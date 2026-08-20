package chatcmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/ui"
)

// ChannelFocusMsg requests a focus switch to a channel the user
// is already in. `At` stamps the user-intent moment; the chat-
// screen's arbiter compares it against the target window's
// `UserTime` to decide whether the switch takes the visible area
// (newer than the current focus) or just flags activity on the
// sidebar (older). Sources that represent a deliberate user
// action — slash commands, sidebar selection — stamp `time.Now()`;
// derived sources (e.g. a join-time landing event) stamp the
// triggering event's time so a freshly-arrived window can't
// out-bid one the user has already moved past.
type ChannelFocusMsg struct {
	Channel domain.ChannelName
	At      time.Time
}

// DMOpenedMsg is fired by `/msg <nick> <body>` and `/query <nick>
// [<body>]`. The chat screen materialises a DM window for
// `Counterpart`, optionally focus-switches, and optionally sends
// `Body` to it. `/query` sets `Focus`; `/msg` leaves it false.
type DMOpenedMsg struct {
	Counterpart *domain.Instance
	Body        string
	Focus       bool
	At          time.Time
}

// DMClosedMsg is fired by `/close` in a DM window. The chat screen
// drops the window, its scrollback and its sidebar entry, and
// forgets it from the set the next run reopens. The conversation
// survives: the event log keeps both directions of it, and
// messaging the counterpart again opens the window on it.
type DMClosedMsg struct {
	Window domain.ChannelName
	At     time.Time
}

// ChannelArg is a command-layer wrapper around domain.ChannelName
// that implements FieldDecoder to ensure the # prefix is present. It
// also accepts RFC 2812 §3.2.1's own JOIN syntax, a comma-separated
// channel list ("#a,#b,#c"): Decode splits on commas, drops any
// empty entry, and prefixes every surviving one; [ChannelArg.Channels]
// splits the decoded argument back into its individual names.
type ChannelArg string

// Decode implements command.FieldDecoder. A trailing or doubled
// comma must not manufacture a bare "#" channel: an entry that is
// empty once trimmed is dropped, never prefixed, and an argument
// that decodes to no channel at all (",", ",,", "") is refused.
func (c *ChannelArg) Decode(raw string) error {
	parts := strings.Split(raw, ",")
	channels := make([]string, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !domain.HasChannelPrefix(domain.ChannelName(part)) {
			part = domain.ChannelPrefix + part
		}
		channels = append(channels, part)
	}

	if len(channels) == 0 {
		return fmt.Errorf("no channel name given")
	}

	*c = ChannelArg(strings.Join(channels, ","))
	return nil
}

// String returns the decoded argument as a plain string: a single
// channel name, or a comma-separated list for a multi-target JOIN.
func (c ChannelArg) String() string { return string(c) }

// Channels splits a decoded argument into its individual channel
// names. A single-channel argument yields a one-element slice.
func (c ChannelArg) Channels() []domain.ChannelName {
	parts := strings.Split(string(c), ",")
	channels := make([]domain.ChannelName, len(parts))
	for i, part := range parts {
		channels[i] = domain.ChannelName(part)
	}
	return channels
}

// JoinCommand represents `/join <channel>[,<channel>...] [key]`.
// The optional key is required when a keyed (`+k`) channel is
// named, and applies to every channel in the list.
type JoinCommand struct {
	Channel ChannelArg `arg:"channel" help:"Channel to join or create, or a comma-separated list of up to 10 to join at once"`
	Key     string     `arg:"" optional:"" help:"Channel key, if the channel has +k"`
}

// Sources implements command.Completer.
func (JoinCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"channel": channelsSource}
}

// ToCommand builds the wire-protocol command for `/join`. The CLI
// grammar reads the channel argument and the key argument as
// separate, space-delimited tokens, so a space after a comma in the
// channel list ("/join #a, #b") splits into Channel "#a," and Key
// "#b" before Decode ever runs: the intended second channel becomes
// a key. RFC 2812's own list syntax has no space in it
// ("/join #a,#b"); a key that itself looks like a channel name is
// refused here, since a real key starting with "#" is far less
// likely than this typo.
func (c JoinCommand) ToCommand(_ Context) (protocol.Command, error) {
	if domain.HasChannelPrefix(domain.ChannelName(c.Key)) {
		return nil, fmt.Errorf("%q looks like a channel, not a key; join a list with no space after the comma, e.g. \"#a,#b\"", c.Key)
	}

	return protocol.Join{Channels: c.Channel.Channels(), Key: c.Key}, nil
}

// Run implements Command. Success is silent: the channels that
// joined show up through the ordinary JOIN broadcast, the same way
// any other member sees them. A refusal is not on that bus, so a
// partial or total refusal is reported as a system notice naming
// which channels joined and which were refused, and why; focus does
// not move for that case. On full success it focuses the first
// channel joined, the way a single-channel `/join` always has.
//
// The focus target comes from the server's reply and not from what
// the user typed. Channel names compare case-insensitively, so
// `/join #DEV` against an existing `#dev` joins `#dev`, and focusing
// the typed spelling would find no window and leave the user where
// they were with the channel joined behind their back.
func (c JoinCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		cmd, err := c.ToCommand(rc)
		if err != nil {
			return rc.errorEvent("join", err)
		}

		resp, err := rc.Client.Send(ctx, cmd)
		if err != nil {
			return rc.errorEvent("join", err)
		}

		if len(resp.Events) == 0 {
			if resp.Err != nil {
				return rc.errorEvent("join", resp.Err)
			}
			return nil
		}

		outcome := newJoinOutcome(resp.Events)
		if len(outcome.Refused) > 0 {
			return ReplyEvents{domain.SystemNotice{Target: rc.Active, Text: outcome.Text(), At: time.Now()}}
		}

		focus, ok := outcome.Focus()
		if !ok {
			return nil
		}

		return ChannelFocusMsg{Channel: focus, At: time.Now()}
	}
}

// RunTool implements ToolCommand. A multi-target JOIN's partial
// success (some channels joined, some refused) is reported as
// success: OK reflects whether anything joined at all, and Summary
// names every channel's outcome, giving the model the per-channel
// detail behind a single verdict.
func (c JoinCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	cmd, err := c.ToCommand(toolContext(tc))
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	resp, err := tc.Client.Send(ctx, cmd)
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	if len(resp.Events) == 0 {
		if resp.Err != nil {
			return modelclient.ToolResultPayload{OK: false, Error: resp.Err.Error()}
		}
		return modelclient.ToolResultPayload{OK: false, Error: "no channel was joined"}
	}

	outcome := newJoinOutcome(resp.Events)
	if len(outcome.Joined) == 0 {
		return modelclient.ToolResultPayload{OK: false, Error: outcome.Text()}
	}

	return modelclient.ToolResultPayload{OK: true, Summary: outcome.Text()}
}

// joinOutcome partitions a JOIN's Response.Events into which
// channels joined and which were refused. RFC 2812 answers each
// target in a multi-target JOIN with its own reply; both the
// tool-result payload and the chat-screen's notice carry a single
// string, so [joinOutcome.Text] renders the whole partition into
// that one line.
type joinOutcome struct {
	Joined  []string
	Refused []string
}

func newJoinOutcome(events []protocol.Event) joinOutcome {
	var o joinOutcome

	for _, ev := range events {
		switch e := ev.(type) {
		case domain.JoinedChannel:
			o.Joined = append(o.Joined, string(e.Channel))
		case error:
			o.Refused = append(o.Refused, e.Error())
		}
	}

	return o
}

// Focus returns the channel a successful `/join` moves focus to:
// the first one the server confirmed, spelled the way the server
// spells it. The second return is false when the reply confirmed
// none, and there is then nowhere to move to.
func (o joinOutcome) Focus() (domain.ChannelName, bool) {
	if len(o.Joined) == 0 {
		return "", false
	}

	return domain.ChannelName(o.Joined[0]), true
}

// Text renders the outcome as one semicolon-separated line. It uses
// no newlines, which a message body may not carry
// ([protocol.ValidateReplyPart]) and which [errors.Join] would have
// produced from the refused list.
func (o joinOutcome) Text() string {
	var parts []string

	if len(o.Joined) > 0 {
		parts = append(parts, "joined "+strings.Join(o.Joined, ", "))
	}
	if len(o.Refused) > 0 {
		parts = append(parts, strings.Join(o.Refused, "; "))
	}

	return strings.Join(parts, "; ")
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
func (c PartCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("part")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "part")
	}
}

// RunTool implements ToolCommand.
func (c PartCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	ch, ok := toolChannel(tc)
	if !ok {
		return noActiveChannel()
	}

	return sendToolCommand(ctx, tc, c, "parted "+string(ch))
}

// ListCommand represents `/list`.
type ListCommand struct{}

// ToCommand builds the wire-protocol command for `/list`.
func (ListCommand) ToCommand(_ Context) (protocol.Command, error) {
	return protocol.List{}, nil
}

// Run implements Command. The dispatcher returns one
// `domain.ListReply` per channel followed by a closing
// `domain.ListEnd` in `Response.Events`; `sendCommand` delivers
// the whole slice to the chat-screen, which renders each event
// through the generic bus-event path.
func (c ListCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "list")
	}
}

// RunTool implements ToolCommand. Models invoke `/list` as a
// tool to enumerate the public channel directory. The wire `LIST`
// the dispatcher serves records the reply in the model's private
// reply log — its own memory of the lookup — and the same data
// rides back in `ToolResultPayload.Data` for the immediate turn.
func (c ListCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	entries, err := c.fetch(ctx, tc.Client)
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return modelclient.ToolResultPayload{
		OK:      true,
		Summary: "listed known channels",
		Data:    entries,
	}
}

// fetch issues the wire `LIST` and assembles the directory
// entries from the per-channel `domain.ListReply` events the
// dispatcher returns. The closing `domain.ListEnd` is consumed
// but ignored — its presence in `Response.Events` is the
// dispatcher's signal that the list is complete; callers don't
// need to forward it.
func (ListCommand) fetch(ctx context.Context, client protocol.Client) ([]domain.ChannelDirectoryEntry, error) {
	resp, err := client.Send(ctx, protocol.List{})
	if err != nil {
		return nil, err
	}

	if resp.Err != nil {
		return nil, resp.Err
	}

	entries := make([]domain.ChannelDirectoryEntry, 0, len(resp.Events))
	for _, evt := range resp.Events {
		reply, ok := evt.(domain.ListReply)
		if !ok {
			continue
		}

		entries = append(entries, domain.ChannelDirectoryEntry{
			Channel: reply.Channel,
			Members: reply.Members,
			Topic:   reply.Topic,
		})
	}

	return entries, nil
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

// ToCommand builds the wire-protocol command for `/add-model`.
func (c AddModelCommand) ToCommand(rc Context) (protocol.Command, error) {
	return protocol.AddModel{
		Channel: rc.Active,
		Model:   domain.ModelID(c.Model),
		Persona: strings.Join(c.Persona, " "),
	}, nil
}

// Run implements Command.
func (c AddModelCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("add-model")
	}

	if c.Model == "" {
		return usageCmd("add-model", "/add-model <model-id> [--persona <text>]")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "add-model")
	}
}

// RunTool implements ToolCommand.
func (c AddModelCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	ch, ok := toolChannel(tc)
	if !ok {
		return noActiveChannel()
	}

	if c.Model == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "model is required"}
	}

	return sendToolCommand(ctx, tc, c, "added "+c.Model+" to "+string(ch))
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
func (c InviteCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" && c.Channel == "" {
		return noChannelCmd("invite")
	}

	if strings.TrimSpace(c.Nick) == "" {
		return usageCmd("invite", "/invite <nick> [channel]")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "invite")
	}
}

// RunTool implements ToolCommand.
func (c InviteCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	ch, ok := toolChannel(tc)
	if !ok && c.Channel == "" {
		return noActiveChannel()
	}

	if strings.TrimSpace(c.Nick) == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "target nick is required"}
	}

	if c.Channel != "" {
		ch = domain.ChannelName(c.Channel.String())
	}

	return sendToolCommand(ctx, tc, c, "invited "+c.Nick+" to "+string(ch))
}

// KillCommand represents `/kill <nick> [reason]`.
type KillCommand struct {
	Nick   string   `arg:"" help:"Nick to disconnect"`
	Reason []string `arg:"" optional:"" help:"Optional reason; defaults to 'No reason given'."`
}

// Sources implements command.Completer.
func (KillCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// ToCommand builds the wire-protocol command for `/kill`.
func (c KillCommand) ToCommand(_ Context) (protocol.Command, error) {
	return protocol.Kill{Nick: domain.Nick(c.Nick), Reason: c.killReason()}, nil
}

const defaultKillReason = "No reason given"

func (c KillCommand) killReason() string {
	r := strings.TrimSpace(strings.Join(c.Reason, " "))
	if r == "" {
		return defaultKillReason
	}

	return r
}

// Run implements Command.
func (c KillCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "kill")
	}
}

// RunTool implements ToolCommand.
func (c KillCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	return sendToolCommand(ctx, tc, c, "killed "+c.Nick)
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
func (c KickCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("kick")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "kick")
	}
}

// RunTool implements ToolCommand.
func (c KickCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	ch, ok := toolChannel(tc)
	if !ok {
		return noActiveChannel()
	}

	return sendToolCommand(ctx, tc, c, "kicked "+c.Nick+" from "+string(ch))
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
	Target string               `arg:"" help:"#channel or nick to message"`
	Body   []string             `arg:"" optional:"" nargs:"1" help:"Plain message text. Provide either body or spans, not both."`
	Spans  []protocol.ReplySpan `optional:"" help:"Styled spans for IRC formatting. Each span has text and optional style (bold, italic, underline, reverse, strike, fg, bg as palette 0..15). Provide either body or spans, not both."`
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

// ToCommand builds the wire-protocol command for `/msg`. The target
// is read the way a server reads `<msgtarget>`: a `#`-prefixed value
// is a channel, anything else is a nick the dispatcher resolves. A
// channel the actor is not in is refused here so the chat-screen can
// surface a typed error without going over the wire.
func (c MsgCommand) ToCommand(rc Context) (protocol.Command, error) {
	body := strings.TrimSpace(strings.Join(c.Body, " "))
	target := protocol.ParseMsgTarget(c.Target)

	if ch, ok := target.(protocol.ChannelTarget); ok && !c.actorInChannel(rc.Actor, domain.ChannelName(ch)) {
		return nil, notInChannelError(domain.ChannelName(ch))
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
func (c MsgCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		body := strings.TrimSpace(strings.Join(c.Body, " "))
		if body == "" {
			return rc.errorEvent("msg", fmt.Errorf("message body is required"))
		}

		target := domain.ChannelName(c.Target)

		if domain.InferChannelKind(target) == domain.KindChannel {
			return sendCommand(ctx, rc, c, "msg")
		}

		nick := domain.Nick(c.Target)

		resolved, err := rc.Session.ResolveNick(ctx, nick)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchNick) {
				return rc.errorEvent("msg", domain.UnknownNickError{Nick: nick, At: time.Now()})
			}

			return rc.errorEvent("msg", fmt.Errorf("resolve nick: %w", err))
		}

		// The chat screen handler materialises the DM window
		// (creating it if missing) and then sends the body to it,
		// in that order, so the rendered message always lands in
		// an existing sidebar entry. Focus stays where the user
		// had it — `/msg` is a send command, not a window-opening
		// one.
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
// map; the same precondition is enforced server-side, but pre-
// checking lets the chat-screen surface a typed "not a member"
// error before going over the wire.
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
// and (when `Body` is non-empty) sending the body to it.
type QueryCommand struct {
	Nick string   `arg:"" help:"Nick to open a direct message with"`
	Body []string `arg:"" optional:"" nargs:"-1" help:"Optional message text"`
}

// Sources implements command.Completer.
func (QueryCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// Run implements Command.
func (c QueryCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		resolved, err := rc.Session.ResolveNick(ctx, nick)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchNick) {
				return rc.errorEvent("query", domain.UnknownNickError{Nick: nick, At: time.Now()})
			}

			return rc.errorEvent("query", fmt.Errorf("resolve nick: %w", err))
		}

		return DMOpenedMsg{
			Counterpart: resolved,
			Body:        strings.TrimSpace(strings.Join(c.Body, " ")),
			Focus:       true,
			At:          time.Now(),
		}
	}
}

// CloseCommand represents `/close`, with irssi's `/wc` and
// `/unquery` as aliases. It closes the window in view, and what
// that means follows from what the window is.
//
// A channel window exists because the user is in the channel, so
// closing it parts the channel, the same thing `/wc` does in irssi
// and `/close` in WeeChat. A DM window is client state the server
// holds nothing for, so closing it never reaches the wire: PART
// takes a channel and refuses anything else (RFC 2812 §3.2.2), and
// this is the command a query window has instead. `&modeloff` is
// the client's own view of the server and stays for the session.
type CloseCommand struct{}

// Run implements Command.
func (c CloseCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" {
		return usageCmd("close", "no window to close")
	}

	switch domain.InferChannelKind(rc.Active) {
	case domain.KindStatus:
		return usageCmd("close", "&modeloff stays open for the session")
	case domain.KindDM:
		window := rc.Active

		return func() tea.Msg {
			return DMClosedMsg{Window: window, At: time.Now()}
		}
	}

	return PartCommand{}.Run(ctx, rc)
}

// RunTool implements ToolCommand. Models call this as the `msg`
// tool to send a message addressed to either a `#`-channel they
// are in or to a peer's nick. There is no UI window involved
// and no "open DM" step — DMs are stateless on the server side,
// and the conversation lives in the events log.
//
// The tool accepts either a plain `body` or styled `spans`;
// `renderReplyPart` validates the structural shape (exactly one of
// body/spans, no embedded newlines, spans are non-empty, colour
// values in range) and renders spans into IRC wire control
// characters via `ircfmt`. Validation failure returns an error
// tool-result so the model can self-correct on its next call.
func (c MsgCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	body, err := renderReplyPart(protocol.ReplyPart{
		Kind:  protocol.ReplyMessage,
		Body:  strings.TrimSpace(strings.Join(c.Body, " ")),
		Spans: c.Spans,
	})
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	resp, sendErr := tc.Client.Send(ctx, protocol.PrivMsg{
		Target: protocol.ParseMsgTarget(c.Target),
		Body:   body,
	})

	return resolveSendResult(resp, sendErr, "messaged "+c.Target)
}

// resolveSendResult flattens a `caller.Send` outcome into the
// tool-result envelope the model sees. Send-level errors and gate
// rejections both surface as `OK: false`; a successful send returns
// the caller-supplied summary.
func resolveSendResult(resp protocol.Response, err error, summary string) modelclient.ToolResultPayload {
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	if resp.Err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: resp.Err.Error()}
	}

	return modelclient.ToolResultPayload{OK: true, Summary: summary}
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
func (c NickCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		if _, err := rc.Config.Update(ctx, func(cfg config.Config) config.Config {
			cfg.UserNick = string(nick)
			return cfg
		}); err != nil {
			return rc.errorEvent("nick", err)
		}

		return sendCommand(ctx, rc, c, "nick")
	}
}

// RunTool implements ToolCommand.
func (c NickCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	return sendToolCommand(ctx, tc, c, "changed nick to "+c.Nick)
}

// ModeCommand represents `/mode <flags> [args...]`. Carries one
// or more channel-mode changes in RFC 2812 §3.2.3 compound form;
// flags toggle direction with `+` / `-` prefixes and parametric
// flags consume their argument from the args list left-to-right.
type ModeCommand struct {
	Flags string   `arg:"" help:"Mode flag string, e.g. +ov-i or +k"`
	Args  []string `arg:"" optional:"" help:"Parameters for parametric flags, in flag-string order"`
}

// ToCommand builds the wire-protocol command for `/mode`, parsing
// the compound flag string into a sequence of changes. Shape
// errors (unknown flag, missing parameter, surplus parameter)
// reject before any wire send so the dispatcher and the chatcmd
// surface agree on what's well-formed.
func (c ModeCommand) ToCommand(rc Context) (protocol.Command, error) {
	changes, err := parseChannelModeString(c.Flags, c.Args)
	if err != nil {
		return nil, err
	}

	return protocol.ChannelMode{Channel: rc.Active, Changes: changes}, nil
}

// Run implements Command.
func (c ModeCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("mode")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "mode")
	}
}

// RunTool implements ToolCommand.
func (c ModeCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	ch, ok := toolChannel(tc)
	if !ok {
		return noActiveChannel()
	}

	return sendToolCommand(ctx, tc, c, "mode change on "+string(ch))
}

// parseChannelModeString walks `flags` left-to-right, tracking
// sign, and emits one [protocol.ChannelModeChange] per flag rune.
// Parametric flags (`+o`, `+v`, `+l` on add, `+k` on add) consume
// their argument from `args` in order. The function rejects
// unknown flags, missing arguments, and surplus arguments — RFC
// 2812 doesn't pin behaviour on extra trailing args, but the
// stricter rejection makes a typo surface immediately rather than
// silently dropping a half-meant change.
func parseChannelModeString(flags string, args []string) ([]protocol.ChannelModeChange, error) {
	if flags == "" {
		return nil, fmt.Errorf("mode: empty flag string")
	}

	add := true
	argIdx := 0

	var changes []protocol.ChannelModeChange

	for _, r := range flags {
		switch r {
		case '+':
			add = true
			continue
		case '-':
			add = false
			continue
		}

		flag := domain.Mode(r)
		change := protocol.ChannelModeChange{Flag: flag, Add: add}

		needsParam, paramKind := channelModeParamShape(flag, add)
		if needsParam {
			if argIdx >= len(args) {
				return nil, domain.MissingModeParamError{Flag: flag}
			}

			switch paramKind {
			case modeParamTarget:
				change.Target = domain.Nick(args[argIdx])
			case modeParamValue:
				change.Param = args[argIdx]
			}
			argIdx++
		}

		changes = append(changes, change)
	}

	if argIdx < len(args) {
		return nil, fmt.Errorf("mode: %d surplus argument(s)", len(args)-argIdx)
	}

	return changes, nil
}

type modeParamKind int

const (
	modeParamNone modeParamKind = iota
	modeParamTarget
	modeParamValue
)

// channelModeParamShape reports whether a flag in the given
// direction consumes an argument and, if so, whether the argument
// is a nick (a per-member grant) or a free value (a count or a
// key). It reads [domain.ModeArgumentFor], so the set of flags that
// take an argument is the one the dispatcher validates against.
//
// An unknown flag consumes nothing here. The dispatcher is what
// rejects it, with [domain.UnknownModeFlagError], and consuming an
// argument for it would turn that into a confusing surplus-argument
// complaint about the next flag along.
func channelModeParamShape(flag domain.Mode, add bool) (bool, modeParamKind) {
	switch domain.ModeArgumentFor(flag) {
	case domain.ModeArgNick:
		return true, modeParamTarget
	case domain.ModeArgCount, domain.ModeArgText:
		if add {
			return true, modeParamValue
		}
	}
	return false, modeParamNone
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
func (c TopicCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("topic")
	}

	if len(c.Topic) == 0 {
		return func() tea.Msg {
			w, err := rc.Session.GetWindow(ctx, rc.Active)
			if err != nil {
				return rc.errorEvent("topic", err)
			}

			cw, ok := w.(*domain.ChannelWindow)
			if !ok {
				return rc.errorEvent("topic", fmt.Errorf("%s is not a channel", rc.Active))
			}

			return TopicInfoResult{Window: cw}
		}
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "topic")
	}
}

// RunTool implements ToolCommand.
func (c TopicCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	ch, ok := toolChannel(tc)
	if !ok {
		return noActiveChannel()
	}

	if len(c.Topic) == 0 {
		w, err := tc.Session.GetWindow(ctx, ch)
		if err != nil {
			return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
		}

		cw, isChannel := w.(*domain.ChannelWindow)
		if !isChannel {
			return modelclient.ToolResultPayload{OK: false, Error: fmt.Errorf("%s is not a channel", ch).Error()}
		}

		return modelclient.ToolResultPayload{
			OK:      true,
			Summary: "returned current topic",
			Data:    cw,
		}
	}

	return sendToolCommand(ctx, tc, c, "updated topic for "+string(ch))
}

// MeCommand represents `/me <action>`.
type MeCommand struct {
	Action []string             `arg:"" optional:"" nargs:"1" help:"Plain action text. Provide either action or spans, not both."`
	Spans  []protocol.ReplySpan `optional:"" help:"Styled spans for IRC formatting. Each span has text and optional style (bold, italic, underline, reverse, strike, fg, bg as palette 0..15). Provide either action or spans, not both."`
}

// ToCommand builds the wire-protocol command for `/me`. The action
// goes to the window the user is in, which
// [protocol.TargetForWindow] turns into a channel or a counterpart.
func (c MeCommand) ToCommand(rc Context) (protocol.Command, error) {
	return protocol.Action{
		Target: protocol.TargetForWindow(rc.Active),
		Body:   strings.TrimSpace(strings.Join(c.Action, " ")),
	}, nil
}

// Run implements Command.
func (c MeCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == "" {
		return noChannelCmd("me")
	}

	body := strings.TrimSpace(strings.Join(c.Action, " "))
	if body == "" {
		return usageCmd("me", "/me <action>")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "me")
	}
}

// RunTool implements ToolCommand. The action body goes through the
// same validate+render path as `msg`: plain `action` text or styled
// `spans`, exactly one, no newlines, etc. Encoded output is sent as
// a `/me`-style Action addressed at the window the turn is running
// in, which is a channel or, in a DM, the counterpart the turn is
// with.
//
// Any window will do, so this asks only that there be one. A caller
// with no window carries a nil Target, which is a different value
// from every window a client can hold.
func (c MeCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	if tc.Target == nil {
		return modelclient.ToolResultPayload{OK: false, Error: "no active window"}
	}

	body, err := renderReplyPart(protocol.ReplyPart{
		Kind:  protocol.ReplyAction,
		Body:  strings.TrimSpace(strings.Join(c.Action, " ")),
		Spans: c.Spans,
	})
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	resp, sendErr := tc.Client.Send(ctx, protocol.Action{
		Target: tc.Target,
		Body:   body,
	})

	return resolveSendResult(resp, sendErr, "sent action to "+tc.Target.String())
}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick string `arg:"" help:"Nick to look up"`
}

// Sources implements command.Completer.
func (WhoisCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// ToCommand builds the wire-protocol command for `/whois`, carrying
// the issuing window so the dispatcher can stamp it onto the reply's
// render target.
func (c WhoisCommand) ToCommand(ctx Context) (protocol.Command, error) {
	return protocol.Whois{Nick: domain.Nick(c.Nick), Channel: ctx.Active}, nil
}

// Run implements Command. The dispatcher returns the canonical
// `domain.Whois` snapshot in `Response.Events`; `sendCommand`
// delivers it to the chat-screen, which renders the snapshot
// through the generic bus-event path.
func (c WhoisCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "whois")
	}
}

// RunTool implements ToolCommand. The reply is stamped with the
// window the lookup was issued from, so it renders where the model
// asked; a DM window is named by its counterpart's id.
func (c WhoisCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	window, _ := protocol.WindowName(tc.Target)

	whois, err := c.fetch(ctx, tc.Client, domain.Nick(c.Nick), window)
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return modelclient.ToolResultPayload{
		OK:      true,
		Summary: "returned details for " + c.Nick,
		Data:    whois,
	}
}

// fetch issues the wire `WHOIS` from `channel` and extracts the
// dispatcher's `domain.Whois` snapshot from `Response.Events`. The
// snapshot freezes the instance's identity surface at the moment of
// query — `Nick`, `Persona`, `Channels` — so later renames or
// channel changes don't retro-edit historical renderings.
func (WhoisCommand) fetch(ctx context.Context, client protocol.Client, nick domain.Nick, channel domain.ChannelName) (domain.Whois, error) {
	resp, err := client.Send(ctx, protocol.Whois{Nick: nick, Channel: channel})
	if err != nil {
		return domain.Whois{}, err
	}

	if resp.Err != nil {
		return domain.Whois{}, resp.Err
	}

	for _, evt := range resp.Events {
		if whois, ok := evt.(domain.Whois); ok {
			return whois, nil
		}
	}

	return domain.Whois{}, fmt.Errorf("dispatcher returned no Whois event")
}

// HelpCommand represents `/help`.
type HelpCommand struct{}

// Run implements Command.
func (HelpCommand) Run(_ context.Context, _ Context) tea.Cmd {
	return func() tea.Msg { return HelpResult{} }
}

// RunTool implements ToolCommand. The command list is a UI
// affordance with no memory value, so it is returned to the model
// for the immediate turn and never persisted.
func (HelpCommand) RunTool(_ context.Context, _ modelclient.ToolContext) modelclient.ToolResultPayload {
	return modelclient.ToolResultPayload{
		OK:      true,
		Summary: "available command tools include join, part, list, invite, kick, msg, nick, topic, me, whois, help, and quit",
	}
}

// ClearCommand represents `/clear`.
type ClearCommand struct{}

// Run implements Command.
func (ClearCommand) Run(_ context.Context, _ Context) tea.Cmd {
	return func() tea.Msg { return ClearResult{} }
}

// PokeCommand represents `/poke`: a manual nudge that asks the
// session to poke idle channels now. The automatic schedule is
// session-owned; this is optional sugar for an on-demand poke.
type PokeCommand struct{}

// Run implements Command.
func (PokeCommand) Run(_ context.Context, _ Context) tea.Cmd {
	return func() tea.Msg { return PokeRequested{} }
}

// QuitCommand represents `/quit [message]`.
type QuitCommand struct {
	Message []string `arg:"" optional:"" nargs:"1" help:"Optional farewell message"`
}

// ToCommand builds the wire-protocol command for `/quit`.
func (c QuitCommand) ToCommand(_ Context) (protocol.Command, error) {
	return protocol.Quit{Reason: c.quitMessage()}, nil
}

// Run implements Command. The user-side `/quit` is a frontend
// concern (lock input, display "Disconnecting…", schedule
// `tea.Quit`) that the chat-screen orchestrates around its own
// state — the wire QUIT fires from the screen's quit handler,
// not from this command. Emitting [ui.QuitRequestedMsg] hands the
// orchestration to that handler.
func (c QuitCommand) Run(_ context.Context, _ Context) tea.Cmd {
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

// RunTool implements ToolCommand.
func (c QuitCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	return sendToolCommand(ctx, tc, c, "shut down and left all channels")
}

// PersonasCommand represents `/personas`.
type PersonasCommand struct{}

// Run implements Command.
func (PersonasCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		personas, err := rc.Manager.ListPersonas(ctx)
		if err != nil {
			return rc.errorEvent("personas", err)
		}

		return PersonasListResult(personas)
	}
}

// RegeneratePersonasCommand represents `/regenerate-personas`.
type RegeneratePersonasCommand struct{}

// Run implements Command.
func (RegeneratePersonasCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		personas, err := rc.Manager.RegeneratePersonas(ctx)
		if err != nil {
			return rc.errorEvent("regenerate-personas", err)
		}

		return PersonasRegeneratedResult{Count: len(personas)}
	}
}

// PassCommand is the model-only `pass` tool. The reason lands on
// the per-tool-call observability span and as the tool result
// summary, distinguishing a deliberate pass from the no-tool-call
// silence.
type PassCommand struct {
	Reason string `arg:"" help:"A brief reason for not replying."`
}

// RunTool records the pass reason on the surrounding execute_tool
// span and returns a stable confirmation summary.
func (c PassCommand) RunTool(ctx context.Context, _ modelclient.ToolContext) modelclient.ToolResultPayload {
	reason := strings.TrimSpace(c.Reason)
	if reason == "" {
		reason = "no reason given"
	}

	trace.SpanFromContext(ctx).SetAttributes(attribute.String("pass.reason", reason))

	return modelclient.ToolResultPayload{OK: true, Summary: "passed: " + reason}
}
