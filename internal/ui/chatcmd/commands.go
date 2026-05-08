package chatcmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui"
)

// ChannelFocusMsg requests a focus switch to a channel the user
// is already in.
type ChannelFocusMsg struct {
	Channel domain.ChannelName
}

// DMOpenedMsg is fired by `/msg <nick> <body>` and `/query <nick>
// [<body>]`. The chat screen materialises a DM window for
// `Counterpart`, optionally focus-switches, and optionally sends
// `Body` via `SendMessageAs`. `/query` sets `Focus`; `/msg`
// leaves it false.
type DMOpenedMsg struct {
	Counterpart *domain.Instance
	Body        string
	Focus       bool
	At          time.Time
}

// ChannelArg is a command-layer wrapper around domain.ChannelName
// that implements FieldDecoder to ensure the # prefix is present.
type ChannelArg string

// Decode implements command.FieldDecoder.
func (c *ChannelArg) Decode(raw string) error {
	if !strings.HasPrefix(raw, domain.ChannelPrefix) {
		raw = domain.ChannelPrefix + raw
	}

	*c = ChannelArg(raw)
	return nil
}

// String returns the channel name as a plain string.
func (c ChannelArg) String() string { return string(c) }

// JoinCommand represents `/join <channel>`.
type JoinCommand struct {
	Channel ChannelArg `arg:"channel" help:"Channel to join or create"`
}

// Sources implements command.Completer.
func (JoinCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"channel": channelsSource}
}

// ToCommand builds the wire-protocol command for `/join`.
func (c JoinCommand) ToCommand(_ Context) (protocol.Command, error) {
	return protocol.Join{Channel: domain.ChannelName(c.Channel.String())}, nil
}

// Run implements Command.
func (c JoinCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		if msg := sendCommand(rc, c, "join"); msg != nil {
			return msg
		}

		return ChannelFocusMsg{Channel: domain.ChannelName(c.Channel.String())}
	}
}

// RunTool implements ToolCommand.
func (c JoinCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	return sendToolCommand(ctx, tc, c, "joined "+c.Channel.String())
}

// PartCommand represents `/part [message]`.
type PartCommand struct {
	Message []string `arg:"" optional:"" nargs:"1" help:"Optional farewell message"`
}

// ToCommand builds the wire-protocol command for `/part`.
func (c PartCommand) ToCommand(rc Context) (protocol.Command, error) {
	return protocol.Part{
		Channel: rc.Active,
		Reason:  strings.TrimSpace(strings.Join(c.Message, " ")),
	}, nil
}

// Run implements Command.
func (c PartCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("part")
	}

	return func() tea.Msg {
		return sendCommand(rc, c, "part")
	}
}

// RunTool implements ToolCommand.
func (c PartCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	return sendToolCommand(ctx, tc, c, "parted "+string(tc.Channel))
}

// ListCommand represents `/list`.
type ListCommand struct{}

// Run implements Command. The user-side `/list` queries the
// session for the channel directory and returns the entries to
// the chat-screen handler, which builds and persists one
// `domain.ListReply` per entry plus a closing `ListEnd`.
func (ListCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		entries, err := rc.Session.DirectoryChannels(rc.Ctx)
		if err != nil {
			return errorEvent("list", err)
		}

		return ListResult{Entries: entries}
	}
}

// RunTool implements ToolCommand. Models invoke `/list` as a
// tool to enumerate the public channel directory. The reply
// shape mirrors the user-side path: per-entry `domain.ListReply`
// events are persisted into the model's invocation channel so
// the result is durable in the events log, and the same data
// is returned via `ToolResultPayload` for the immediate-next-
// turn context.
func (ListCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	entries, err := tc.Session.DirectoryChannels(ctx)
	if err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	now := time.Now()
	for _, e := range entries {
		reply := domain.ListReply{
			Channel: e.Channel,
			Members: e.Members,
			Topic:   e.Topic,
			At:      now,
		}

		if _, logErr := tc.Session.LogEvent(ctx, tc.Channel, reply); logErr != nil {
			return session.ToolResultPayload{OK: false, Error: logErr.Error()}
		}
	}

	if _, logErr := tc.Session.LogEvent(ctx, tc.Channel, domain.ListEnd{At: now}); logErr != nil {
		return session.ToolResultPayload{OK: false, Error: logErr.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "listed known channels",
		Data:    entries,
	}
}

// AddModelCommand represents `/add-model [model] [--persona text]`.
type AddModelCommand struct {
	Model   string   `arg:"" optional:"" help:"Model to invite"`
	Persona []string `optional:"" help:"Optional persona"`
}

// Sources implements command.Completer.
func (AddModelCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{
		"model":   liveModelsSource,
		"persona": personasSource,
	}
}

// Run implements Command.
func (c AddModelCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("add-model")
	}

	if c.Model == "" {
		return usageCmd("add-model", "/add-model <model-id> [--persona <text>]")
	}

	return func() tea.Msg {
		if err := rc.Session.AddModel(rc.Ctx, rc.Active, domain.ModelID(c.Model), strings.Join(c.Persona, " ")); err != nil {
			return errorEvent("add-model", err)
		}

		return nil
	}
}

// InviteCommand represents `/invite <nick> [channel]`.
type InviteCommand struct {
	Nick    string     `arg:"" optional:"" help:"Nick to invite"`
	Channel ChannelArg `arg:"channel" optional:"" help:"Channel to invite them to"`
}

// Sources implements command.Completer.
func (InviteCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// ToCommand builds the wire-protocol command for `/invite`.
func (c InviteCommand) ToCommand(rc Context) (protocol.Command, error) {
	ch := rc.Active
	if c.Channel != "" {
		ch = domain.ChannelName(c.Channel.String())
	}

	return protocol.Invite{Nick: domain.Nick(c.Nick), Channel: ch}, nil
}

// Run implements Command.
func (c InviteCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" && c.Channel == "" {
		return noChannelCmd("invite")
	}

	if strings.TrimSpace(c.Nick) == "" {
		return usageCmd("invite", "/invite <nick> [channel]")
	}

	return func() tea.Msg {
		return sendCommand(rc, c, "invite")
	}
}

// RunTool implements ToolCommand.
func (c InviteCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" && c.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if strings.TrimSpace(c.Nick) == "" {
		return session.ToolResultPayload{OK: false, Error: "target nick is required"}
	}

	ch := tc.Channel
	if c.Channel != "" {
		ch = domain.ChannelName(c.Channel.String())
	}

	return sendToolCommand(ctx, tc, c, "invited "+c.Nick+" to "+string(ch))
}

// KickCommand represents `/kick <nick>`.
type KickCommand struct {
	Nick string `arg:"" help:"Nick to kick"`
}

// Sources implements command.Completer.
func (KickCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": activeMembersSource}
}

// ToCommand builds the wire-protocol command for `/kick`.
func (c KickCommand) ToCommand(rc Context) (protocol.Command, error) {
	return protocol.Kick{Nick: domain.Nick(c.Nick), Channel: rc.Active}, nil
}

// Run implements Command.
func (c KickCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("kick")
	}

	return func() tea.Msg {
		return sendCommand(rc, c, "kick")
	}
}

// RunTool implements ToolCommand.
func (c KickCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	return sendToolCommand(ctx, tc, c, "kicked "+c.Nick+" from "+string(tc.Channel))
}

// MsgCommand represents `/msg <target> <message>` where `target`
// is either a `#`-prefixed channel name or a bare nick. For a
// channel target, the actor must already be a member of the
// channel; for a nick target, the message is sent to that user
// directly. The message body is required — `/msg` is a send
// command, not a window-opening one. Use `/query <nick>` to open
// a blank DM window without sending. `/msg` does not focus-switch;
// the chat screen auto-creates a DM window in the sidebar (without
// focusing) when a send goes to a nick the user has no open
// window for.
type MsgCommand struct {
	Target string   `arg:"" help:"#channel or nick to message"`
	Body   []string `arg:"" optional:"" nargs:"1" help:"Message text"`
}

// Sources implements command.Completer.
func (MsgCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"target": msgTargetSource}
}

// msgTargetSource suggests both #channels and known nicks for
// the `/msg` target arg. Channel suggestions sort first; nicks
// follow.
func msgTargetSource(ctx CompletionContext, st command.InvocationState[CompletionContext]) command.SuggestionResult {
	chRes := channelsSource(ctx, st)
	nickRes := instancesSource(ctx, st)

	merged := make([]command.Suggestion, 0, len(chRes.Suggestions)+len(nickRes.Suggestions))
	merged = append(merged, chRes.Suggestions...)
	merged = append(merged, nickRes.Suggestions...)

	return command.SuggestionResult{Suggestions: merged}
}

// ToCommand builds the wire-protocol command for `/msg` against a
// channel target. The nick-target branch of `/msg` does not have a
// protocol counterpart yet (DM materialisation is still
// chat-screen-side); callers must pre-check the target shape
// before invoking.
func (c MsgCommand) ToCommand(rc Context) (protocol.Command, error) {
	body := strings.TrimSpace(strings.Join(c.Body, " "))
	target := domain.ChannelName(c.Target)

	if domain.InferChannelKind(target) != domain.KindChannel {
		return nil, fmt.Errorf("/msg nick-target is not a wire-protocol command")
	}

	if !c.actorInChannel(rc.Actor, target) {
		return nil, notInChannelError(target)
	}

	return protocol.PrivMsg{Target: target, Body: body}, nil
}

// Run implements Command. For a channel target, the actor must
// already be a member; for a nick target, the nick is resolved
// to its `*Instance` and the message is sent to the
// counterpart's `InstanceID`. The chat screen observes the
// resulting `domain.Message` event and auto-creates a DM window
// in the sidebar if one does not already exist for that target.
// No focus switch in either case.
func (c MsgCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		body := strings.TrimSpace(strings.Join(c.Body, " "))
		if body == "" {
			return errorEvent("msg", fmt.Errorf("message body is required"))
		}

		target := domain.ChannelName(c.Target)

		if domain.InferChannelKind(target) == domain.KindChannel {
			if msg := sendCommand(rc, c, "msg"); msg != nil {
				return msg
			}

			// Synthesise the user's outgoing message for local
			// rendering: the session's echo gate suppresses the
			// user-actor copy on the wire.
			return domain.Message{
				Target:     domain.NormaliseChannelName(target),
				From:       rc.Actor.Nick(),
				InstanceID: rc.Actor.ID(),
				Body:       body,
				At:         time.Now(),
			}
		}

		nick := domain.Nick(c.Target)

		resolved, err := rc.Session.ResolveNick(rc.Ctx, nick)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchNick) {
				return errorEvent("msg", domain.UnknownNickError{Nick: nick})
			}

			return errorEvent("msg", fmt.Errorf("resolve nick: %w", err))
		}

		// The chat screen handler materialises the DM window
		// (creating it if missing) and routes the body through
		// `SendMessageAs`, in that order, so the rendered message
		// always lands in an existing sidebar entry. Focus stays
		// where the user had it — `/msg` is a send command, not a
		// window-opening one.
		return DMOpenedMsg{
			Counterpart: resolved,
			Body:        body,
			Focus:       false,
			At:          time.Now(),
		}
	}
}

// actorInChannel reports whether `actor` is a member of `target`.
// The membership snapshot is read from the actor's joined-channel
// map; the same precondition is enforced server-side in
// [session.Session.SendMessageAs], but pre-checking lets the
// chat-screen surface a typed "not a member" error before going
// over the wire.
func (MsgCommand) actorInChannel(actor *domain.Instance, target domain.ChannelName) bool {
	target = domain.NormaliseChannelName(target)

	channels := actor.Channels()
	if channels == nil {
		return false
	}

	_, ok := channels.Get(target)
	return ok
}

// notInChannelError formats the not-a-member rejection. Kept as
// a helper so the user-side and model-side paths surface the
// same wording.
func notInChannelError(target domain.ChannelName) error {
	return fmt.Errorf("not a member of %s", target)
}

// QueryCommand represents `/query <nick> [<body>]`. It opens (or
// re-focuses) a direct-message window with the resolved nick and
// optionally sends a trailing body. Mirrors irssi's behaviour:
// `/query mike` opens a blank query window and switches focus to
// it; `/query mike hello` does the same and additionally sends
// `hello`.
//
// `/query` is purely a UI affordance — the session has no notion
// of "opening" a DM. The chat screen handles `QueryOpenedEvent`
// by inserting the DM into its sidebar cache, focus-switching,
// and (when `Body` is non-empty) routing through `SendMessageAs`.
type QueryCommand struct {
	Nick string   `arg:"" help:"Nick to open a direct message with"`
	Body []string `arg:"" optional:"" nargs:"-1" help:"Optional message text"`
}

// Sources implements command.Completer.
func (QueryCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// Run implements Command.
func (c QueryCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		resolved, err := rc.Session.ResolveNick(rc.Ctx, nick)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchNick) {
				return errorEvent("query", domain.UnknownNickError{Nick: nick})
			}

			return errorEvent("query", fmt.Errorf("resolve nick: %w", err))
		}

		return DMOpenedMsg{
			Counterpart: resolved,
			Body:        strings.TrimSpace(strings.Join(c.Body, " ")),
			Focus:       true,
			At:          time.Now(),
		}
	}
}

// RunTool implements ToolCommand. Models call this as the `msg`
// tool to send a message addressed to either a `#`-channel they
// are in or to a peer's nick. There is no UI window involved
// and no "open DM" step — DMs are stateless on the server side,
// and the conversation lives in the events log.
func (c MsgCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	body := strings.TrimSpace(strings.Join(c.Body, " "))
	if body == "" {
		return session.ToolResultPayload{OK: false, Error: "message body is required"}
	}

	target := domain.ChannelName(c.Target)

	if domain.InferChannelKind(target) == domain.KindChannel {
		return sendToolCommand(ctx, tc, c, "messaged "+c.Target)
	}

	resolved, err := tc.Session.ResolveNick(ctx, domain.Nick(c.Target))
	if err != nil {
		if errors.Is(err, store.ErrNoSuchNick) {
			return session.ToolResultPayload{OK: false, Error: domain.UnknownNickError{Nick: domain.Nick(c.Target)}.Error()}
		}

		return session.ToolResultPayload{OK: false, Error: fmt.Errorf("resolve nick: %w", err).Error()}
	}

	if _, err := tc.Session.SendMessageAs(ctx, tc.Actor, domain.ChannelName(resolved.ID()), body); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "messaged " + c.Target,
	}
}

// NickCommand represents `/nick <new_nick>`.
type NickCommand struct {
	Nick string `arg:"new-nick" help:"New nickname"`
}

// ToCommand builds the wire-protocol command for `/nick`.
func (c NickCommand) ToCommand(_ Context) (protocol.Command, error) {
	return protocol.Nick{New: domain.Nick(c.Nick)}, nil
}

// Run implements Command. Persisting the chosen nick to config so
// it survives a restart is a chat-screen-side concern; the wire
// nick change goes via the protocol client.
func (c NickCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.UserNick = string(nick)
		}); err != nil {
			return errorEvent("nick", err)
		}

		return sendCommand(rc, c, "nick")
	}
}

// RunTool implements ToolCommand.
func (c NickCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	return sendToolCommand(ctx, tc, c, "changed nick to "+c.Nick)
}

// TopicCommand represents `/topic [text]`. An empty topic clears it.
type TopicCommand struct {
	Topic []string `arg:"" optional:"" help:"Topic text"`
}

// ToCommand builds the wire-protocol command for `/topic <body>`.
// The bare `/topic` (display) variant is not a wire command; the
// branch in [TopicCommand.Run] reads it locally and returns a
// [TopicInfoResult].
func (c TopicCommand) ToCommand(rc Context) (protocol.Command, error) {
	return protocol.Topic{Channel: rc.Active, Body: strings.Join(c.Topic, " ")}, nil
}

// Run implements Command.
func (c TopicCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("topic")
	}

	if len(c.Topic) == 0 {
		return func() tea.Msg {
			w, err := rc.Session.GetWindow(rc.Ctx, rc.Active)
			if err != nil {
				return errorEvent("topic", err)
			}

			cw, ok := w.(*domain.ChannelWindow)
			if !ok {
				return errorEvent("topic", fmt.Errorf("%s is not a channel", rc.Active))
			}

			return TopicInfoResult{Window: cw}
		}
	}

	return func() tea.Msg {
		return sendCommand(rc, c, "topic")
	}
}

// RunTool implements ToolCommand.
func (c TopicCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if len(c.Topic) == 0 {
		w, err := tc.Session.GetWindow(ctx, tc.Channel)
		if err != nil {
			return session.ToolResultPayload{OK: false, Error: err.Error()}
		}

		cw, ok := w.(*domain.ChannelWindow)
		if !ok {
			return session.ToolResultPayload{OK: false, Error: fmt.Errorf("%s is not a channel", tc.Channel).Error()}
		}

		return session.ToolResultPayload{
			OK:      true,
			Summary: "returned current topic",
			Data:    cw,
		}
	}

	return sendToolCommand(ctx, tc, c, "updated topic for "+string(tc.Channel))
}

// MeCommand represents `/me <action>`.
type MeCommand struct {
	Action []string `arg:"" nargs:"1" help:"Action text"`
}

// ToCommand builds the wire-protocol command for `/me`.
func (c MeCommand) ToCommand(rc Context) (protocol.Command, error) {
	return protocol.Action{
		Target: rc.Active,
		Body:   strings.TrimSpace(strings.Join(c.Action, " ")),
	}, nil
}

// Run implements Command.
func (c MeCommand) Run(rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("me")
	}

	body := strings.TrimSpace(strings.Join(c.Action, " "))
	if body == "" {
		return usageCmd("me", "/me <action>")
	}

	return func() tea.Msg {
		if msg := sendCommand(rc, c, "me"); msg != nil {
			return msg
		}

		// Synthesise the user's outgoing /me action for local
		// rendering: the session's echo gate suppresses the
		// user-actor copy on the wire.
		return domain.Message{
			Target:     rc.Active,
			From:       rc.Actor.Nick(),
			InstanceID: rc.Actor.ID(),
			Body:       body,
			Action:     true,
			At:         time.Now(),
		}
	}
}

// RunTool implements ToolCommand.
func (c MeCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if tc.Channel == "" {
		return session.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	return sendToolCommand(ctx, tc, c, "sent action to "+string(tc.Channel))
}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick string `arg:"" help:"Nick to look up"`
}

// Sources implements command.Completer.
func (WhoisCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// Run implements Command.
func (c WhoisCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		inst, err := rc.Session.Whois(rc.Ctx, domain.Nick(c.Nick))
		if err != nil {
			return errorEvent("whois", domain.UnknownNickError{Nick: domain.Nick(c.Nick)})
		}

		return WhoisResult{Instance: inst}
	}
}

// RunTool implements ToolCommand.
func (c WhoisCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	inst, err := tc.Session.Whois(ctx, domain.Nick(c.Nick))
	if err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	if _, err := tc.Session.LogEvent(ctx, tc.Channel, domain.Whois{
		Target:   tc.Channel,
		Instance: inst,
		At:       time.Now(),
	}); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "returned details for " + c.Nick,
		Data:    inst,
	}
}

// HelpCommand represents `/help`.
type HelpCommand struct{}

// Run implements Command.
func (HelpCommand) Run(_ Context) tea.Cmd {
	return func() tea.Msg { return HelpResult{} }
}

// RunTool implements ToolCommand.
func (HelpCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if _, err := tc.Session.LogEvent(ctx, tc.Channel, domain.Help{
		Target: tc.Channel,
		At:     time.Now(),
	}); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "available command tools include join, part, list, invite, kick, msg, nick, topic, me, whois, help, and quit",
	}
}

// ClearCommand represents `/clear`.
type ClearCommand struct{}

// Run implements Command.
func (ClearCommand) Run(_ Context) tea.Cmd {
	return func() tea.Msg { return ClearResult{} }
}

// QuitCommand represents `/quit [message]`.
type QuitCommand struct {
	Message []string `arg:"" optional:"" nargs:"1" help:"Optional farewell message"`
}

// Run implements Command.
func (c QuitCommand) Run(_ Context) tea.Cmd {
	msg := c.quitMessage()

	return func() tea.Msg {
		return ui.QuitRequestedMsg{Message: msg}
	}
}

// defaultQuitMessage is used when the user types /quit without a
// farewell message.
const defaultQuitMessage = "leaving"

func (c QuitCommand) quitMessage() string {
	msg := strings.TrimSpace(strings.Join(c.Message, " "))
	if msg == "" {
		return defaultQuitMessage
	}

	return msg
}

func (c QuitCommand) executeQuit(ctx context.Context, sess *session.Session, actor *domain.Instance) error {
	return sess.QuitAs(ctx, actor, c.quitMessage())
}

// RunTool implements ToolCommand.
func (c QuitCommand) RunTool(ctx context.Context, tc session.ToolContext) session.ToolResultPayload {
	if err := c.executeQuit(ctx, tc.Session, tc.Actor); err != nil {
		return session.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return session.ToolResultPayload{
		OK:      true,
		Summary: "shut down and left all channels",
	}
}

// PersonasCommand represents `/personas`.
type PersonasCommand struct{}

// Run implements Command.
func (PersonasCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		personas, err := rc.Session.ListPersonas(rc.Ctx)
		if err != nil {
			return errorEvent("personas", err)
		}

		return PersonasListResult{Personas: personas}
	}
}

// RegeneratePersonasCommand represents `/regenerate-personas`.
type RegeneratePersonasCommand struct{}

// Run implements Command.
func (RegeneratePersonasCommand) Run(rc Context) tea.Cmd {
	return func() tea.Msg {
		personas, err := rc.Session.RegeneratePersonas(rc.Ctx)
		if err != nil {
			return errorEvent("regenerate-personas", err)
		}

		return PersonasRegeneratedResult{Count: len(personas)}
	}
}

// ConfigCommand is a group node whose children are the individual
// config keys. Each subcommand has its own args and Run method.
type ConfigCommand struct {
	Reset           bool                  `optional:"" help:"Reset the selected setting to its default"`
	APIKey          APIKeyConfig          `cmd:"" name:"api-key" help:"Activate OpenRouter immediately."`
	BaseURL         BaseURLConfig         `cmd:"" name:"base-url" help:"Set the API base URL."`
	PokeInterval    PokeIntervalConfig    `cmd:"" name:"poke-interval" help:"Set the background poke cadence."`
	SmallModel      SmallModelConfig      `cmd:"" name:"small-model" help:"Set the model used for lightweight tasks."`
	EmbeddingModel  EmbeddingModelConfig  `cmd:"" name:"embedding-model" help:"Set the embedding model."`
	Highlight       HighlightConfig       `cmd:"" help:"Set words that trigger visual highlighting."`
	TimestampFormat TimestampFormatConfig `cmd:"" name:"timestamp-format" help:"Set or disable timestamp formatting."`
	Persona         PersonaConfig         `cmd:"" help:"Define a custom persona."`
}

// APIKeyConfig represents `/config api-key <value>`.
type APIKeyConfig struct {
	Value string `arg:"" optional:"" help:"OpenRouter API key"`
}

// Run implements Command.
func (c APIKeyConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if err := rc.Session.SetAPIKey(rc.Ctx, "", config.DefaultBaseURL); err != nil {
				return errorEvent("config api-key", err)
			}

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.APIKey = ""
			}); err != nil {
				return errorEvent("config api-key", err)
			}

			return APIKeySetResult{Reset: true}
		}
	}

	if strings.TrimSpace(c.Value) == "" {
		return usageCmd("config", "/config api-key <value>")
	}

	return func() tea.Msg {
		cfg, err := rc.Config.Load(rc.Ctx)
		if err != nil {
			return errorEvent("config api-key", err)
		}

		if err := rc.Session.SetAPIKey(rc.Ctx, c.Value, cfg.BaseURL); err != nil {
			return errorEvent("config api-key", err)
		}

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.APIKey = c.Value
		}); err != nil {
			return errorEvent("config api-key", err)
		}

		return APIKeySetResult{}
	}
}

// BaseURLConfig represents `/config base-url <url>`.
type BaseURLConfig struct {
	URL string `arg:"" optional:"" help:"API base URL"`
}

// Run implements Command.
func (c BaseURLConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if err := rc.Session.SetBaseURL(rc.Ctx, config.DefaultBaseURL); err != nil {
				return errorEvent("config base-url", err)
			}

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.BaseURL = config.DefaultBaseURL
			}); err != nil {
				return errorEvent("config base-url", err)
			}

			return BaseURLSetResult{URL: config.DefaultBaseURL, Reset: true}
		}
	}

	if strings.TrimSpace(c.URL) == "" {
		return usageCmd("config", "/config base-url <url>")
	}

	return func() tea.Msg {
		if err := rc.Session.SetBaseURL(rc.Ctx, c.URL); err != nil {
			return errorEvent("config base-url", err)
		}

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.BaseURL = c.URL
		}); err != nil {
			return errorEvent("config base-url", err)
		}

		return BaseURLSetResult{URL: c.URL}
	}
}

// PokeIntervalConfig represents `/config poke-interval <duration>`.
type PokeIntervalConfig struct {
	Duration string `arg:"" optional:"" help:"Poke interval (e.g. 5m, 1h)"`
}

// Sources implements command.Completer.
func (PokeIntervalConfig) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{
		"duration": command.LiteralSource[CompletionContext](
			command.Suggestion{Value: "5m", Label: "5m", Detail: "Fast poke cadence"},
			command.Suggestion{Value: "10m", Label: "10m", Detail: "Balanced poke cadence"},
			command.Suggestion{Value: "30m", Label: "30m", Detail: "Quiet channels"},
			command.Suggestion{Value: "1h", Label: "1h", Detail: "Very low activity"},
		),
	}
}

// Run implements Command.
func (c PokeIntervalConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.PokeInterval = config.DefaultPokeInterval
			}); err != nil {
				return errorEvent("config poke-interval", err)
			}

			return PokeIntervalSetResult{Interval: config.DefaultPokeInterval, Reset: true}
		}
	}

	if strings.TrimSpace(c.Duration) == "" {
		return usageCmd("config", "/config poke-interval <duration>")
	}

	return func() tea.Msg {
		interval, err := time.ParseDuration(c.Duration)
		if err != nil {
			return errorEvent("config poke-interval", domain.InvalidDurationError{
				Input: c.Duration,
				Err:   err,
			})
		}

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.PokeInterval = interval
		}); err != nil {
			return errorEvent("config poke-interval", err)
		}

		return PokeIntervalSetResult{Interval: interval}
	}
}

// SmallModelConfig represents `/config small-model <model-id>`.
type SmallModelConfig struct {
	ModelID string `arg:"" optional:"" help:"Model ID for lightweight tasks"`
}

// Run implements Command.
func (c SmallModelConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			rc.Session.SetSmallModel(rc.Ctx, config.DefaultSmallModel)

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.SmallModel = config.DefaultSmallModel
			}); err != nil {
				return errorEvent("config small-model", err)
			}

			return SmallModelSetResult{ModelID: config.DefaultSmallModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return usageCmd("config", "/config small-model <model-id>")
	}

	return func() tea.Msg {
		modelID := domain.ModelID(c.ModelID)
		rc.Session.SetSmallModel(rc.Ctx, modelID)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.SmallModel = modelID
		}); err != nil {
			return errorEvent("config small-model", err)
		}

		return SmallModelSetResult{ModelID: modelID}
	}
}

// EmbeddingModelConfig represents `/config embedding-model <model-id>`.
type EmbeddingModelConfig struct {
	ModelID string `arg:"" optional:"" help:"Model ID for embeddings"`
}

// Run implements Command.
func (c EmbeddingModelConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.EmbeddingModel = config.DefaultEmbeddingModel
			}); err != nil {
				return errorEvent("config embedding-model", err)
			}

			return EmbeddingModelSetResult{ModelID: config.DefaultEmbeddingModel, Reset: true}
		}
	}

	if strings.TrimSpace(c.ModelID) == "" {
		return usageCmd("config", "/config embedding-model <model-id>")
	}

	return func() tea.Msg {
		modelID := domain.ModelID(c.ModelID)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.EmbeddingModel = modelID
		}); err != nil {
			return errorEvent("config embedding-model", err)
		}

		return EmbeddingModelSetResult{ModelID: modelID}
	}
}

// HighlightConfig represents `/config highlight <word> [<word>...]`.
type HighlightConfig struct {
	Words []string `arg:"" optional:"" help:"Words to highlight"`
}

// Run implements Command.
func (c HighlightConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			words := append([]string(nil), config.DefaultHighlightWords...)

			if _, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.HighlightWords = words
			}); err != nil {
				return errorEvent("config highlight", err)
			}

			return HighlightWordsSetResult{Words: words, Reset: true}
		}
	}

	if len(c.Words) == 0 {
		return usageCmd("config", "/config highlight <word> [<word>...]")
	}

	return func() tea.Msg {
		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.HighlightWords = c.Words
		}); err != nil {
			return errorEvent("config highlight", err)
		}

		return HighlightWordsSetResult{Words: c.Words}
	}
}

// TimestampFormatConfig represents `/config timestamp-format [<format>...]`.
type TimestampFormatConfig struct {
	Format []string `arg:"" optional:"" help:"Timestamp format"`
}

// Run implements Command.
func (c TimestampFormatConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			cfg, err := rc.updateConfig(func(cfg *config.Config) {
				cfg.TimestampFormat = nil
			})
			if err != nil {
				return errorEvent("config timestamp-format", err)
			}

			return TimestampFormatSetResult{Format: cfg.TimestampFormat, Reset: true}
		}
	}

	return func() tea.Msg {
		format := normaliseTimestampFormat(c.Format)

		if _, err := rc.updateConfig(func(cfg *config.Config) {
			cfg.TimestampFormat = format
		}); err != nil {
			return errorEvent("config timestamp-format", err)
		}

		return TimestampFormatSetResult{Format: format}
	}
}

// PersonaConfig represents `/config persona <id> <description...>`.
type PersonaConfig struct {
	ID          string   `arg:"" optional:"" help:"Persona identifier"`
	Description []string `arg:"" optional:"" help:"Persona description"`
}

// Run implements Command.
func (c PersonaConfig) Run(rc Context) tea.Cmd {
	if rc.configResetRequested() {
		return func() tea.Msg {
			count, err := rc.Session.ResetPersonas(rc.Ctx)
			if err != nil {
				return errorEvent("config persona", err)
			}

			return PersonaResetResult{Count: count}
		}
	}

	if strings.TrimSpace(c.ID) == "" {
		return usageCmd("config persona", "/config persona <id> <description...>")
	}

	desc := strings.TrimSpace(strings.Join(c.Description, " "))
	if desc == "" {
		return usageCmd("config persona", "/config persona <id> <description...>")
	}

	return func() tea.Msg {
		if err := rc.Session.SetPersona(rc.Ctx, c.ID, desc); err != nil {
			return errorEvent("config persona", err)
		}

		return PersonaSetResult{ID: c.ID}
	}
}

func normaliseTimestampFormat(parts []string) *string {
	if len(parts) == 0 {
		disabled := ""
		return &disabled
	}

	joined := strings.TrimSpace(strings.Join(parts, " "))
	if joined == `""` || joined == `''` {
		disabled := ""
		return &disabled
	}

	return &joined
}
