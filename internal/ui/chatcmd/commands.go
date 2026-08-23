package chatcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
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

// ChannelJoinFocusMsg requests focus for a JOIN whose protocol
// events can reach the chat screen before or after the command
// result. The chat screen retains the request until the JOIN has
// created the window.
type ChannelJoinFocusMsg struct {
	Channel domain.ChannelName
	At      time.Time
}

// DMOpenedMsg is fired by `/msg <nick> <body>` and `/query <nick>
// [<body>]`. The chat screen materialises a DM window for
// `Counterpart`, optionally focus-switches, and optionally sends
// `Body` to it. `/query` sets `Focus`; `/msg` leaves it false.
type DMOpenedMsg struct {
	CounterpartID   domain.InstanceID
	CounterpartNick domain.Nick
	Body            string
	Focus           bool
	At              time.Time
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
// that implements UnmarshalText to ensure the # prefix is
// present. It also accepts RFC 2812 §3.2.1's own JOIN syntax, a
// comma-separated channel list ("#a,#b,#c"): UnmarshalText splits on
// commas, drops any empty entry, and prefixes every surviving one;
// [ChannelArg.Channels] splits the decoded argument back into its
// individual names.
type ChannelArg string

// UnmarshalText decodes a channel argument. A trailing or doubled
// comma must not manufacture a bare "#" channel: an entry that is
// empty once trimmed is dropped, never prefixed, and an argument
// that decodes to no channel at all (",", ",,", "") is refused.
func (c *ChannelArg) UnmarshalText(text []byte) error {
	raw := string(text)
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

// CommandText stores one free-text value from a slash command or tool call.
type CommandText string

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *CommandText) UnmarshalText(text []byte) error {
	*t = CommandText(text)

	return nil
}

// String returns the command's single text value.
func (t CommandText) String() string {
	return string(t)
}

// MessageBodies contains the independently emitted plain messages
// in one msg or me tool call. Slash commands map their raw remainder
// into one element.
type MessageBodies []string

const (
	maxToolMessages   = 4
	maxToolReplySpans = 32
)

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *MessageBodies) UnmarshalText(text []byte) error {
	*b = append(*b, string(text))

	return nil
}

// UnmarshalJSON retains the structured array accepted by tool calls.
func (b *MessageBodies) UnmarshalJSON(data []byte) error {
	return unmarshalStringArray(data, (*[]string)(b))
}

// Validate enforces the per-call bound and IRC body rules for msg.
func (b MessageBodies) Validate() error {
	return validateMessageBodies(protocol.ReplyMessage, []string(b))
}

// ActionBodies is the plain-text representation accepted by me.
type ActionBodies []string

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *ActionBodies) UnmarshalText(text []byte) error {
	*b = append(*b, string(text))

	return nil
}

// UnmarshalJSON retains the structured array accepted by tool calls.
func (b *ActionBodies) UnmarshalJSON(data []byte) error {
	return unmarshalStringArray(data, (*[]string)(b))
}

func unmarshalStringArray(data []byte, target *[]string) error {
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}

	*target = values
	return nil
}

// Validate enforces the per-call bound and IRC body rules for me.
func (b ActionBodies) Validate() error {
	return validateMessageBodies(protocol.ReplyAction, []string(b))
}

func validateMessageBodies(kind protocol.ReplyKind, bodies []string) error {
	if len(bodies) > maxToolMessages {
		name := "body"
		if kind == protocol.ReplyAction {
			name = "action"
		}

		return &command.TooManyValuesError{Name: name, Maximum: maxToolMessages, Actual: len(bodies)}
	}

	for _, body := range bodies {
		if err := protocol.ValidateMessageBody(kind.CommandName(), body, time.Time{}); err != nil {
			return err
		}
	}

	return nil
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
	intentAt := rc.Session.Now()

	return func() tea.Msg {
		cmd, err := c.ToCommand(rc)
		if err != nil {
			return rc.errorResult("join", err)
		}

		resp, err := rc.Client.Send(ctx, cmd)
		if len(resp.Events) == 0 {
			if err != nil {
				return rc.errorResult("join", err)
			}
			if resp.Err != nil {
				return rc.errorResult("join", resp.Err)
			}
			return nil
		}

		outcome := newJoinOutcome(resp.Events)
		if len(outcome.Refused) > 0 || err != nil {
			target, _ := rc.ActiveName()
			reply := ReplyEvents{
				Events: []domain.ProtocolEvent{
					domain.SystemNotice{Target: target, Text: outcome.Text(), At: time.Now()},
				},
			}
			if err != nil {
				errorEvent := rc.errorEvent("join", err)
				reply.Error = &errorEvent
			}

			return reply
		}

		focus, ok := outcome.Focus()
		if !ok {
			return nil
		}

		return ChannelJoinFocusMsg{Channel: focus, At: intentAt}
	}
}

// RunTool implements ToolCommand. A multi-target JOIN's partial
// success (some channels joined, some refused) is reported as
// success: OK reflects whether anything joined at all, and Summary
// names every channel's outcome, giving the model the per-channel
// detail behind a single verdict.
func (c JoinCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	cmd, err := c.ToCommand(toolContext(tc))
	if err != nil {
		return toolRefusal(err)
	}

	resp, err := tc.Send(ctx, cmd)
	if len(resp.Events) == 0 {
		if err != nil {
			return toolExecutionFailure(err)
		}
		if resp.Err != nil {
			return toolRefusal(resp.Err)
		}
		return toolResult(modelclient.ToolResultPayload{OK: false, Error: "no channel was joined"})
	}

	outcome := newJoinOutcome(resp.Events)
	if err != nil {
		return toolExecutionFailure(fmt.Errorf("%s: %w", outcome.Text(), err))
	}
	if len(outcome.Joined) == 0 {
		return toolResult(modelclient.ToolResultPayload{OK: false, Error: outcome.Text()})
	}

	return toolResult(modelclient.ToolResultPayload{OK: true, Summary: outcome.Text()})
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
	Message CommandText `arg:"" optional:"" passthrough:"all" help:"Optional farewell message"`
}

// ToCommand builds the wire-protocol command for `/part`.
func (c PartCommand) ToCommand(rc Context) (protocol.Command, error) {
	channel, _ := rc.ActiveName()

	return protocol.Part{
		Channel: channel,
		Reason:  strings.TrimSpace(c.Message.String()),
	}, nil
}

// Run implements Command.
func (c PartCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == nil {
		return noChannelCmd("part")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "part")
	}
}

// RunTool implements ToolCommand.
func (c PartCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	ch, ok := toolChannel(tc)
	if !ok {
		return toolResult(noActiveChannel())
	}

	return sendToolCommand(ctx, tc, c, "parted "+string(ch))
}

// ListCommand represents `/list`.
type ListCommand struct{}

// ToCommand builds the wire-protocol command for `/list`.
func (ListCommand) ToCommand(ctx Context) (protocol.Command, error) {
	return protocol.List{Window: ctx.activeWindowTarget()}, nil
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
func (c ListCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	cmd, err := c.ToCommand(toolContext(tc))
	if err != nil {
		return toolRefusal(err)
	}

	resp, err := tc.Send(ctx, cmd)
	if err != nil {
		return toolExecutionFailure(err)
	}
	if resp.Err != nil {
		return toolRefusal(resp.Err)
	}

	return toolResult(modelclient.ToolResultPayload{
		OK:      true,
		Summary: "listed known channels",
		Data:    listEntries(resp.Events),
	})
}

func listEntries(events []protocol.Event) []domain.ChannelDirectoryEntry {
	entries := make([]domain.ChannelDirectoryEntry, 0, len(events))
	for _, evt := range events {
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

	return entries
}

// AddModelCommand represents `/add-model [model] [--persona value]`.
type AddModelCommand struct {
	Model   string   `arg:"" optional:"" help:"Model to invite"`
	Persona []string `optional:"" help:"Persona ID or literal text"`
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
	channel, _ := rc.ActiveName()

	return protocol.AddModel{
		Channel: channel,
		Model:   domain.ModelID(c.Model),
		Persona: strings.Join(c.Persona, " "),
	}, nil
}

// Run implements Command.
func (c AddModelCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == nil {
		return noChannelCmd("add-model")
	}

	if c.Model == "" {
		return usageCmd("add-model", "/add-model <model-id> [--persona <id-or-text>]")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "add-model")
	}
}

// RunTool implements ToolCommand.
func (c AddModelCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	ch, ok := toolChannel(tc)
	if !ok {
		return toolResult(noActiveChannel())
	}

	if c.Model == "" {
		return toolResult(modelclient.ToolResultPayload{OK: false, Error: "model is required"})
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
	window := rc.activeWindowTarget()
	ch, _ := rc.ActiveName()
	if c.Channel != "" {
		ch = domain.ChannelName(c.Channel.String())
	}

	return protocol.Invite{
		Nick: domain.Nick(c.Nick), Channel: ch, Window: window,
	}, nil
}

// Run implements Command.
func (c InviteCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == nil && c.Channel == "" {
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
func (c InviteCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	ch, ok := toolChannel(tc)
	if !ok && c.Channel == "" {
		return toolResult(noActiveChannel())
	}

	if strings.TrimSpace(c.Nick) == "" {
		return toolResult(modelclient.ToolResultPayload{OK: false, Error: "target nick is required"})
	}

	if c.Channel != "" {
		ch = domain.ChannelName(c.Channel.String())
	}

	return sendToolCommand(ctx, tc, c, "invited "+c.Nick+" to "+string(ch))
}

// KillCommand represents `/kill <nick> [reason]`.
type KillCommand struct {
	Nick   string      `arg:"" help:"Nick to disconnect"`
	Reason CommandText `arg:"" optional:"" passthrough:"all" help:"Optional reason; defaults to 'No reason given'."`
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
	r := strings.TrimSpace(c.Reason.String())
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
func (c KillCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
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
	channel, _ := rc.ActiveName()

	return protocol.Kick{Nick: domain.Nick(c.Nick), Channel: channel}, nil
}

// Run implements Command.
func (c KickCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == nil {
		return noChannelCmd("kick")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "kick")
	}
}

// RunTool implements ToolCommand.
func (c KickCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	ch, ok := toolChannel(tc)
	if !ok {
		return toolResult(noActiveChannel())
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
	Target string              `arg:"" help:"#channel or nick to message"`
	Body   MessageBodies       `arg:"" passthrough:"all" xor:"content" max:"4" tool:"One to 4 non-empty plain messages to send in order. Each array element becomes a separate IRC message." help:"Plain message text"`
	Spans  protocol.ReplySpans `cli:"-" xor:"content" max:"32" tool:"One to 32 styled spans for one IRC message. Each span has non-empty text and optional style (bold, italic, underline, reverse, strike, fg, bg); fg and bg use palette values 0..15." help:"Styled IRC message spans"`
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
// Channel membership remains a server-side send gate, so command
// construction does not need a client-side actor snapshot.
func (c MsgCommand) ToCommand(_ Context) (protocol.Command, error) {
	body, err := c.cliBody()
	if err != nil {
		return nil, err
	}
	target := protocol.ParseMsgTarget(c.Target)

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
		body, err := c.cliBody()
		if err != nil {
			return rc.errorResult("msg", err)
		}

		target := domain.ChannelName(c.Target)

		if domain.InferChannelKind(target) == domain.KindChannel {
			return sendCommand(ctx, rc, c, "msg")
		}

		nick := domain.Nick(c.Target)

		id, resolvedNick, err := rc.Session.ResolveNick(ctx, nick)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchNick) {
				return rc.errorResult("msg", domain.UnknownNickError{Nick: nick, At: time.Now()})
			}

			return rc.errorResult("msg", fmt.Errorf("resolve nick: %w", err))
		}

		// The chat screen handler materialises the DM window
		// (creating it if missing) and then sends the body to it,
		// in that order, so the rendered message always lands in
		// an existing sidebar entry. Focus stays where the user
		// had it — `/msg` is a send command, not a window-opening
		// one.
		return DMOpenedMsg{
			CounterpartID:   id,
			CounterpartNick: resolvedNick,
			Body:            body,
			Focus:           false,
			At:              time.Now(),
		}
	}
}

func (c MsgCommand) cliBody() (string, error) {
	if len(c.Body) == 0 {
		return "", domain.NoTextToSendError{Command: protocol.PrivMsg{}.Name()}
	}

	body := c.Body[0]
	if err := protocol.ValidateMessageBody(protocol.PrivMsg{}.Name(), body, time.Time{}); err != nil {
		return "", err
	}

	return body, nil
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
	Nick string      `arg:"" help:"Nick to open a direct message with"`
	Body CommandText `arg:"" optional:"" passthrough:"all" help:"Optional message text"`
}

// Sources implements command.Completer.
func (QueryCommand) Sources() map[string]command.SuggestionSource[CompletionContext] {
	return map[string]command.SuggestionSource[CompletionContext]{"nick": instancesSource}
}

// Run implements Command.
func (c QueryCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	intentAt := rc.Session.Now()

	return func() tea.Msg {
		nick := domain.Nick(c.Nick)

		id, resolvedNick, err := rc.Session.ResolveNick(ctx, nick)
		if err != nil {
			if errors.Is(err, store.ErrNoSuchNick) {
				return rc.errorResult("query", domain.UnknownNickError{Nick: nick, At: time.Now()})
			}

			return rc.errorResult("query", fmt.Errorf("resolve nick: %w", err))
		}

		return DMOpenedMsg{
			CounterpartID:   id,
			CounterpartNick: resolvedNick,
			Body:            strings.TrimSpace(c.Body.String()),
			Focus:           true,
			At:              intentAt,
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
	if rc.Active == nil {
		return usageCmd("close", "no window to close")
	}

	switch rc.Active.Kind() {
	case domain.KindStatus:
		return usageCmd("close", "&modeloff stays open for the session")
	case domain.KindDM:
		window := rc.Active.Name()

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
// The tool accepts either a plain `body` or styled `spans`. The
// whole body array is validated before the first send. Each
// element then becomes its own PRIVMSG in array order; spans render
// as one styled message.
func (c MsgCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	messages, err := renderReplyMessages(protocol.ReplyMessage, c.Body, c.Spans)
	if err != nil {
		return toolRefusal(err)
	}

	target := protocol.ParseMsgTarget(c.Target)
	return sendToolMessages(ctx, tc, protocol.ReplyMessage, messages, func(body string) (protocol.Response, error) {
		return tc.Send(ctx, protocol.PrivMsg{Target: target, Body: body})
	}, func(resp protocol.Response) error {
		if _, isNick := target.(protocol.NickTarget); !isNick {
			return nil
		}

		message, ok := responseMessage(resp)
		if !ok {
			return &MissingMessageResponseError{Tool: "msg"}
		}

		target = protocol.ClientTarget(message.Target)

		return nil
	}, "messaged "+c.Target, "messages to "+c.Target)
}

func responseMessage(resp protocol.Response) (domain.Message, bool) {
	for _, event := range resp.Events {
		if message, ok := event.(domain.Message); ok {
			return message, true
		}
	}

	return domain.Message{}, false
}

// MissingMessageResponseError reports a successful message send whose
// response did not identify the persisted message.
type MissingMessageResponseError struct {
	Tool string
}

func (e *MissingMessageResponseError) Error() string {
	return e.Tool + " response did not contain the persisted message"
}

func sendToolMessages(
	ctx context.Context,
	tc modelclient.ToolContext,
	kind protocol.ReplyKind,
	messages []renderedMessage,
	send func(string) (protocol.Response, error),
	afterSend func(protocol.Response) error,
	singleSummary string,
	multipleSummary string,
) modelclient.ToolOutcome {
	for index, message := range messages {
		if err := tc.PaceMessage(ctx, message.pacingText); err != nil {
			return toolExecutionFailure(&ToolBatchError{Kind: kind, Sent: index, Total: len(messages), Err: err})
		}

		resp, err := send(message.body)
		if err != nil {
			return toolExecutionFailure(&ToolBatchError{Kind: kind, Sent: index, Total: len(messages), Err: err})
		}
		if resp.Err != nil {
			failure := &ToolBatchError{Kind: kind, Sent: index, Total: len(messages), Err: resp.Err}
			return toolRefusal(failure)
		}
		if afterSend != nil {
			if err := afterSend(resp); err != nil {
				return toolExecutionFailure(&ToolBatchError{Kind: kind, Sent: index + 1, Total: len(messages), Err: err})
			}
		}
	}

	if len(messages) == 1 {
		return toolResult(modelclient.ToolResultPayload{OK: true, Summary: singleSummary})
	}

	return toolResult(modelclient.ToolResultPayload{OK: true, Summary: fmt.Sprintf("sent %d %s", len(messages), multipleSummary)})
}

// ToolBatchError reports a failure after zero or more messages from a
// tool batch have already been sent.
type ToolBatchError struct {
	Kind  protocol.ReplyKind
	Sent  int
	Total int
	Err   error
}

func (e *ToolBatchError) Error() string {
	if e.Sent == 0 {
		return e.Err.Error()
	}

	noun := "messages"
	if e.Kind == protocol.ReplyAction {
		noun = "actions"
	}

	return fmt.Sprintf("sent %d of %d %s before failure: %s", e.Sent, e.Total, noun, e.Err)
}

func (e *ToolBatchError) Unwrap() error {
	return e.Err
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
			return rc.errorResult("nick", err)
		}

		return sendCommand(ctx, rc, c, "nick")
	}
}

// RunTool implements ToolCommand.
func (c NickCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	return sendToolCommand(ctx, tc, c, "changed nick to "+c.Nick)
}

// ModeCommand carries one or more channel-mode changes. The slash command
// accepts RFC 2812's compound form and UnmarshalText converts it into the same
// declarative representation that the model tool receives.
type ModeCommand struct {
	Changes ModeChanges `arg:"" passthrough:"all" max:"16" tool:"One to 16 ordered channel-mode changes." help:"Mode flags and parameters, e.g. +ov-i alice bob"`
}

// ToCommand builds the wire-protocol command for `/mode`.
func (c ModeCommand) ToCommand(rc Context) (protocol.Command, error) {
	changes, err := c.Changes.protocolChanges()
	if err != nil {
		return nil, err
	}

	channel, _ := rc.ActiveName()

	return protocol.ChannelMode{Channel: channel, Changes: changes}, nil
}

// Run implements Command.
func (c ModeCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == nil {
		return noChannelCmd("mode")
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "mode")
	}
}

// RunTool implements ToolCommand.
func (c ModeCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	ch, ok := toolChannel(tc)
	if !ok {
		return toolResult(noActiveChannel())
	}

	return sendToolCommand(ctx, tc, c, "mode change on "+string(ch))
}

// TopicCommand represents `/topic [text]`. An empty topic clears it.
type TopicCommand struct {
	Topic *CommandText `arg:"" optional:"" passthrough:"all" help:"Topic text"`
}

// ToCommand builds the wire-protocol command for `/topic <body>`.
// The bare `/topic` (display) variant is not a wire command; the
// branch in [TopicCommand.Run] reads it locally and returns a
// [TopicInfoResult].
func (c TopicCommand) ToCommand(rc Context) (protocol.Command, error) {
	channel, _ := rc.ActiveName()
	body := ""
	if c.Topic != nil {
		body = c.Topic.String()
	}

	return protocol.Topic{Channel: channel, Body: body}, nil
}

// Run implements Command.
func (c TopicCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == nil {
		return noChannelCmd("topic")
	}

	if c.Topic == nil {
		issuingChannel, _ := rc.Active.(*domain.ChannelWindow)

		return func() tea.Msg {
			channel := rc.Active.Name()
			response, err := rc.Client.Send(ctx, protocol.TopicQuery{Channel: channel})
			if err != nil {
				return rc.errorResult("topic", err)
			}
			if response.Err != nil {
				return rc.errorResult("topic", response.Err)
			}

			topic, ok := topicFromResponse(response)
			if !ok {
				return rc.errorResult("topic", fmt.Errorf("server returned no topic for %s", channel))
			}
			if issuingChannel == nil {
				return rc.errorResult("topic", fmt.Errorf("server returned a topic for non-channel %s", channel))
			}

			return TopicInfoResult{Topic: topic}
		}
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "topic")
	}
}

// RunTool implements ToolCommand.
func (c TopicCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	ch, ok := toolChannel(tc)
	if !ok {
		return toolResult(noActiveChannel())
	}

	if c.Topic == nil {
		response, err := tc.Send(ctx, protocol.TopicQuery{Channel: ch})
		if err != nil {
			return toolExecutionFailure(err)
		}
		if response.Err != nil {
			return toolRefusal(response.Err)
		}

		topic, ok := topicFromResponse(response)
		if !ok {
			return toolResult(modelclient.ToolResultPayload{OK: false, Error: fmt.Sprintf("server returned no topic for %s", ch)})
		}

		return toolResult(modelclient.ToolResultPayload{
			OK:      true,
			Summary: "returned current topic",
			Data:    topic,
		})
	}

	return sendToolCommand(ctx, tc, c, "updated topic for "+string(ch))
}

func topicFromResponse(response protocol.Response) (domain.TopicInfo, bool) {
	return singleResponseEvent[domain.TopicInfo](response.Events)
}

// MeCommand represents `/me <action>`.
type MeCommand struct {
	Action ActionBodies        `arg:"" passthrough:"all" xor:"content" max:"4" tool:"One to 4 non-empty plain actions to send in order. Each array element becomes a separate IRC action." help:"Plain action text"`
	Spans  protocol.ReplySpans `cli:"-" xor:"content" max:"32" tool:"One to 32 styled spans for one IRC action. Each span has non-empty text and optional style (bold, italic, underline, reverse, strike, fg, bg); fg and bg use palette values 0..15." help:"Styled IRC action spans"`
}

type noMessageTargetError struct {
	Window domain.ChannelName
}

func (e noMessageTargetError) Error() string {
	return fmt.Sprintf("window %q has no message target", e.Window)
}

// ToCommand builds the wire-protocol command for `/me`. The action
// goes to the window the user is in, which
// [protocol.TargetForWindow] turns into a channel or a counterpart.
func (c MeCommand) ToCommand(rc Context) (protocol.Command, error) {
	body, err := c.cliBody()
	if err != nil {
		return nil, err
	}
	if rc.Active == nil {
		return nil, fmt.Errorf("me requires an active conversation")
	}
	target, ok := protocol.TargetForWindow(rc.Active)
	if !ok {
		return nil, noMessageTargetError{Window: rc.Active.Name()}
	}

	return protocol.Action{
		Target: target,
		Body:   body,
	}, nil
}

// Run implements Command.
func (c MeCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	if rc.Active == nil || rc.Active.Kind() == domain.KindStatus {
		return noChannelCmd("me")
	}

	if _, err := c.cliBody(); err != nil {
		var noText domain.NoTextToSendError
		if errors.As(err, &noText) {
			return usageCmd("me", "/me <action>")
		}

		return func() tea.Msg { return rc.errorResult("me", err) }
	}

	return func() tea.Msg {
		return sendCommand(ctx, rc, c, "me")
	}
}

func (c MeCommand) cliBody() (string, error) {
	if len(c.Action) == 0 {
		return "", domain.NoTextToSendError{Command: protocol.Action{}.Name()}
	}

	body := c.Action[0]
	if err := protocol.ValidateMessageBody(protocol.Action{}.Name(), body, time.Time{}); err != nil {
		return "", err
	}

	return body, nil
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
func (c MeCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	if tc.Target == nil {
		return toolResult(modelclient.ToolResultPayload{OK: false, Error: "no active window"})
	}

	messages, err := renderReplyMessages(protocol.ReplyAction, c.Action, c.Spans)
	if err != nil {
		return toolRefusal(err)
	}

	return sendToolMessages(ctx, tc, protocol.ReplyAction, messages, func(body string) (protocol.Response, error) {
		return tc.Send(ctx, protocol.Action{Target: tc.Target, Body: body})
	}, nil, "sent action to "+tc.Target.String(), "actions to "+tc.Target.String())
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
	return protocol.Whois{Nick: domain.Nick(c.Nick), Window: ctx.activeWindowTarget()}, nil
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
func (c WhoisCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	cmd, err := c.ToCommand(toolContext(tc))
	if err != nil {
		return toolRefusal(err)
	}

	resp, err := tc.Send(ctx, cmd)
	if err != nil {
		return toolExecutionFailure(err)
	}
	if resp.Err != nil {
		return toolRefusal(resp.Err)
	}

	whois, ok := singleResponseEvent[domain.Whois](resp.Events)
	if !ok {
		return toolExecutionFailure(&MissingWhoisResponseError{Nick: domain.Nick(c.Nick)})
	}

	return toolResult(modelclient.ToolResultPayload{
		OK:      true,
		Summary: "returned details for " + c.Nick,
		Data:    whois,
	})
}

func singleResponseEvent[T any](events []protocol.Event) (T, bool) {
	var zero T
	if len(events) != 1 {
		return zero, false
	}

	for _, event := range events {
		value, ok := event.(T)

		return value, ok
	}

	return zero, false
}

// MissingWhoisResponseError reports a successful WHOIS command whose
// response did not contain the requested snapshot.
type MissingWhoisResponseError struct {
	Nick domain.Nick
}

func (e *MissingWhoisResponseError) Error() string {
	return "WHOIS response did not contain details for " + string(e.Nick)
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
func (HelpCommand) RunTool(_ context.Context, _ modelclient.ToolContext) modelclient.ToolOutcome {
	return toolResult(modelclient.ToolResultPayload{
		OK:      true,
		Summary: "available command tools include join, part, list, invite, kick, msg, nick, topic, me, whois, help, and quit",
	})
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
	Message CommandText `arg:"" optional:"" passthrough:"all" help:"Optional farewell message"`
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
	msg := strings.TrimSpace(c.Message.String())
	if msg == "" {
		return defaultQuitMessage
	}

	return msg
}

// RunTool implements ToolCommand.
func (c QuitCommand) RunTool(ctx context.Context, tc modelclient.ToolContext) modelclient.ToolOutcome {
	return sendToolCommand(ctx, tc, c, "shut down and left all channels")
}

// PersonasCommand represents `/personas`.
type PersonasCommand struct{}

// Run implements Command.
func (PersonasCommand) Run(ctx context.Context, rc Context) tea.Cmd {
	return func() tea.Msg {
		personas, err := rc.Manager.ListPersonas(ctx)
		if err != nil {
			return rc.errorResult("personas", err)
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
			return rc.errorResult("regenerate-personas", err)
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
func (c PassCommand) RunTool(ctx context.Context, _ modelclient.ToolContext) modelclient.ToolOutcome {
	reason := strings.TrimSpace(c.Reason)
	if reason == "" {
		reason = "no reason given"
	}

	trace.SpanFromContext(ctx).SetAttributes(attribute.String("pass.reason", reason))

	return toolResult(modelclient.ToolResultPayload{OK: true, Summary: "passed: " + reason})
}
