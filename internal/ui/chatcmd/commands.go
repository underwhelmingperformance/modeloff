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

// JoinCommand represents `/join <channel> [key]`. The optional
// key is required when the channel carries `+k`.
type JoinCommand struct {
	Channel ChannelArg `arg:"channel" help:"Channel to join or create"`
	Key     string     `arg:"" optional:"" help:"Channel key, if the channel has +k"`
}

// Sources implements command.Completer.
func (JoinCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"channel": channelsSource}
}

// ToCommand builds the wire-protocol command for `/join`.
func (c JoinCommand) ToCommand(_ Context) (protocol.Command, error) {
	return protocol.Join{Channel: domain.ChannelName(c.Channel.String()), Key: c.Key}, nil
}

// Run implements Command.
func (c JoinCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		if msg := sendCommand(ctx, rc, c, "join"); msg != nil {
			return msg
		}

		return ChannelFocusMsg{Channel: domain.ChannelName(c.Channel.String()), At: time.Now()}
	}
}

// RunTool implements ToolCommand.
func (c JoinCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
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
	if tc.Channel == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	return sendToolCommand(ctx, tc, c, "parted "+string(tc.Channel))
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
	if tc.Channel == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if c.Model == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "model is required"}
	}

	return sendToolCommand(ctx, tc, c, "added "+c.Model+" to "+string(tc.Channel))
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
	if tc.Channel == "" && c.Channel == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if strings.TrimSpace(c.Nick) == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "target nick is required"}
	}

	ch := tc.Channel
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
	if tc.Channel == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
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
	Target string              `arg:"" help:"#channel or nick to message"`
	Body   []string            `arg:"" optional:"" nargs:"1" help:"Plain message text. Provide either body or spans, not both."`
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
func (c MsgCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		body := strings.TrimSpace(strings.Join(c.Body, " "))
		if body == "" {
			return errorEvent("msg", fmt.Errorf("message body is required"))
		}

		target := domain.ChannelName(c.Target)

		if domain.InferChannelKind(target) == domain.KindChannel {
			return sendCommand(ctx, rc, c, "msg")
		}

		nick := domain.Nick(c.Target)

		resolved, err := rc.Session.ResolveNick(ctx, nick)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchNick) {
				return errorEvent("msg", domain.UnknownNickError{Nick: nick, At: time.Now()})
			}

			return errorEvent("msg", fmt.Errorf("resolve nick: %w", err))
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
				return errorEvent("query", domain.UnknownNickError{Nick: nick, At: time.Now()})
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

	target, err := resolveMsgTarget(ctx, tc, c.Target)
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	resp, sendErr := tc.Client.Send(ctx, protocol.PrivMsg{Target: target, Body: body})
	return resolveSendResult(resp, sendErr, "messaged "+c.Target)
}

// resolveMsgTarget normalises the model-supplied msg/me target into
// a [domain.ChannelName] the session's send path accepts. A `#`-
// prefixed value is a channel, used as-is. A bare value is first
// looked up as a nick (the model normally sees peers by nick in
// chat events); if no instance owns that nick, the value is assumed
// to already be a DM key (`InstanceID`) and is passed through.
// Unknown values that match neither a channel, nick, nor existing
// instance surface as `UnknownNickError`.
func resolveMsgTarget(ctx context.Context, tc modelclient.ToolContext, raw string) (domain.ChannelName, error) {
	target := domain.ChannelName(raw)

	if domain.InferChannelKind(target) == domain.KindChannel {
		return target, nil
	}

	resolved, err := tc.Session.ResolveNick(ctx, domain.Nick(raw))
	if err == nil {
		return domain.ChannelName(resolved.ID()), nil
	}

	if !errors.Is(err, store.ErrNoSuchNick) {
		return "", fmt.Errorf("resolve nick: %w", err)
	}

	return target, nil
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

		if _, err := rc.updateConfig(ctx, func(cfg *config.Config) {
			cfg.UserNick = string(nick)
		}); err != nil {
			return errorEvent("nick", err)
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
	if tc.Channel == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	return sendToolCommand(ctx, tc, c, "mode change on "+string(tc.Channel))
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
// is a nick (member-mode `+o`/`+v`) or a free value (`+l` int /
// `+k` string).
func channelModeParamShape(flag domain.Mode, add bool) (bool, modeParamKind) {
	switch flag {
	case domain.ModeOperator, domain.ModeChannelVoice:
		return true, modeParamTarget
	case domain.ModeUserLimit, domain.ModeKey:
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
		return sendCommand(ctx, rc, c, "topic")
	}
}

// RunTool implements ToolCommand.
func (c TopicCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	if tc.Channel == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	if len(c.Topic) == 0 {
		w, err := tc.Session.GetWindow(ctx, tc.Channel)
		if err != nil {
			return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
		}

		cw, ok := w.(*domain.ChannelWindow)
		if !ok {
			return modelclient.ToolResultPayload{OK: false, Error: fmt.Errorf("%s is not a channel", tc.Channel).Error()}
		}

		return modelclient.ToolResultPayload{
			OK:      true,
			Summary: "returned current topic",
			Data:    cw,
		}
	}

	return sendToolCommand(ctx, tc, c, "updated topic for "+string(tc.Channel))
}

// MeCommand represents `/me <action>`.
type MeCommand struct {
	Action []string            `arg:"" optional:"" nargs:"1" help:"Plain action text. Provide either action or spans, not both."`
	Spans  []protocol.ReplySpan `optional:"" help:"Styled spans for IRC formatting. Each span has text and optional style (bold, italic, underline, reverse, strike, fg, bg as palette 0..15). Provide either action or spans, not both."`
}

// ToCommand builds the wire-protocol command for `/me`.
func (c MeCommand) ToCommand(rc Context) (protocol.Command, error) {
	return protocol.Action{
		Target: rc.Active,
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
// a `/me`-style Action to the active channel.
func (c MeCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	if tc.Channel == "" {
		return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
	}

	body, err := renderReplyPart(protocol.ReplyPart{
		Kind:  protocol.ReplyAction,
		Body:  strings.TrimSpace(strings.Join(c.Action, " ")),
		Spans: c.Spans,
	})
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	resp, sendErr := tc.Client.Send(ctx, protocol.Action{Target: tc.Channel, Body: body})
	return resolveSendResult(resp, sendErr, "sent action to "+string(tc.Channel))
}

// WhoisCommand represents `/whois <nick>`.
type WhoisCommand struct {
	Nick string `arg:"" help:"Nick to look up"`
}

// Sources implements command.Completer.
func (WhoisCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// ToCommand builds the wire-protocol command for `/whois`.
func (c WhoisCommand) ToCommand(_ Context) (protocol.Command, error) {
	return protocol.Whois{Nick: domain.Nick(c.Nick)}, nil
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

// RunTool implements ToolCommand.
func (c WhoisCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolResultPayload {
	whois, err := c.fetch(ctx, tc.Client, domain.Nick(c.Nick))
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	return modelclient.ToolResultPayload{
		OK:      true,
		Summary: "returned details for " + c.Nick,
		Data:    whois,
	}
}

// fetch issues the wire `WHOIS` and extracts the dispatcher's
// `domain.Whois` snapshot from `Response.Events`. The snapshot
// freezes the instance's identity surface at the moment of
// query — `Nick`, `Persona`, `Channels` — so later renames or
// channel changes don't retro-edit historical renderings.
func (WhoisCommand) fetch(ctx context.Context, client protocol.Client, nick domain.Nick) (domain.Whois, error) {
	resp, err := client.Send(ctx, protocol.Whois{Nick: nick})
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
			return errorEvent("personas", err)
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
			return errorEvent("regenerate-personas", err)
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
