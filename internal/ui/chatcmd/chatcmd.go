// Package chatcmd defines the concrete slash-command types for the
// chat screen. It consumes the generic command library and binds it
// to the application's session layer and Bubble Tea runtime.
package chatcmd

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/richtext"
)

// Command is the typed command interface for the chat screen.
type Command = command.Command[Context, tea.Cmd]

// Parser is the typed parser for the chat screen.
type Parser = command.Parser[CompletionContext, Context, tea.Cmd]

// Context carries the dependencies a command needs to execute.
// Client is the protocol-side client handle the caller dispatches
// commands through (the user-client for chat-screen invocations).
// It is guaranteed non-nil at construction in `runContext`
// (chat_commands.go). The cancellation
// context is threaded as an explicit first parameter to [Command.Run]
// and not carried on the struct.
type Context struct {
	Session    modelclient.SessionAPI
	Manager    modelclient.ManagerAPI
	Config     config.Store
	Active     domain.Window
	Client     protocol.Client
	Invocation command.Invocation[CompletionContext]
}

// ActiveName returns the active window's addressable name. Its
// second result distinguishes no active window from the user's
// self-DM, whose name is the empty string.
func (rc Context) ActiveName() (domain.ChannelName, bool) {
	if rc.Active == nil {
		return "", false
	}

	return rc.Active.Name(), true
}

func (rc Context) activeWindowTarget() protocol.WindowTarget {
	if rc.Active == nil {
		return nil
	}
	switch rc.Active.Kind() {
	case domain.KindChannel:
		return protocol.ChannelWindowTarget(rc.Active.Name())
	case domain.KindDM:
		return protocol.DirectWindowTarget(domain.InstanceID(rc.Active.Name()))
	}

	return nil
}

// HelpResult signals that the help screen should be shown.
type HelpResult struct{}

// ClearResult signals that the current window should be cleared.
type ClearResult struct{}

// PokeRequested signals that the user asked the session to poke idle
// channels now. The automatic schedule is session-owned and runs
// without UI involvement.
type PokeRequested struct{}

// CommandResult binds a delayed slash-command result to the exact window in
// which the command ran.
type CommandResult struct {
	IssuingWindow         domain.Window
	IssuingWindowRevision uint64
	Message               tea.Msg
}

// TopicInfoResult carries the actor-projected current topic metadata
// for display. DM and status windows never produce one because
// `/topic` rejects non-channel targets at the command layer.
type TopicInfoResult struct {
	Topic domain.TopicInfo
}

// CommandErrorResult carries a delayed command error inside a
// [CommandResult].
type CommandErrorResult struct {
	Error domain.ErrorEvent
}

// UsageError indicates a command was invoked incorrectly. Usage
// carries the human-readable usage string (e.g. "/add-model <model-id>").
type UsageError struct {
	Command string
	Usage   string
}

// NoChannelError indicates a command requires an active channel but
// none is set.
type NoChannelError struct {
	Command string
}

// APIKeySetResult signals that the API key was updated.
type APIKeySetResult struct {
	Reset bool
}

// PokeIntervalSetResult signals that the poke interval was updated.
type PokeIntervalSetResult struct {
	Interval time.Duration
	Reset    bool
}

// DrainTimeoutSetResult signals that the shutdown drain timeout was
// updated.
type DrainTimeoutSetResult struct {
	Timeout time.Duration
	Reset   bool
}

// SmallModelSetResult signals that the small model was updated.
type SmallModelSetResult struct {
	ModelID domain.ModelID
	Reset   bool
}

// HighlightWordsSetResult signals that the highlight words were
// updated.
type HighlightWordsSetResult struct {
	Words []string
	Reset bool
}

// BaseURLSetResult signals that the API base URL was updated.
type BaseURLSetResult struct {
	URL   string
	Reset bool
}

// EmbeddingModelSetResult signals that the embedding model was
// updated.
type EmbeddingModelSetResult struct {
	ModelID domain.ModelID
	Reset   bool
}

// ReflectionModeSetResult signals that the reflection mode was updated.
type ReflectionModeSetResult struct {
	Mode  config.ReflectionMode
	Reset bool
}

// ReflectionModelSetResult signals that the reflection model was updated.
// An empty model means reflection follows the small-model setting.
type ReflectionModelSetResult struct {
	ModelID domain.ModelID
	Reset   bool
}

// TimestampFormatSetResult signals that the timestamp format was
// updated.
type TimestampFormatSetResult struct {
	Format *string
	Reset  bool
}

// PersonaTemplatesResult is the chat-screen-side dispatch marker
// for a `/templates` reply. The named-slice shape keeps the
// result distinguishable from a bare `[]domain.PersonaTemplate` in a
// type switch.
type PersonaTemplatesResult []domain.PersonaTemplate

// PersonaTemplatesRegeneratedResult reports how many generated
// templates were replaced.
type PersonaTemplatesRegeneratedResult struct {
	Count int
}

// PersonaSetResult signals that a persona was saved.
type PersonaSetResult struct {
	ID string
}

// PersonaResetResult signals that user-defined personas were removed.
type PersonaResetResult struct {
	Count int
}

// PersonaAction identifies the operation reported by a [PersonaResult].
type PersonaAction string

const (
	// PersonaInspected reports a read-only inspection.
	PersonaInspected PersonaAction = "inspected"
	// PersonaReset reports a reset to revision zero.
	PersonaReset PersonaAction = "reset"
	// PersonaRolledBack reports a move to an ancestor revision.
	PersonaRolledBack PersonaAction = "rolled_back"
	// PersonaDescribed reports a revision an operator wrote by giving the
	// instance a replacement description.
	PersonaDescribed PersonaAction = "described"
)

// PersonaResult carries one complete operator inspection after the
// requested action.
type PersonaResult struct {
	Action     PersonaAction
	Inspection domain.PersonaInspection
}

// errorEvent builds the failure payload for a command run from this context.
func (rc Context) errorEvent(operation string, err error) domain.ErrorEvent {
	target, _ := rc.ActiveName()

	return domain.ErrorEvent{Operation: operation, Err: err, Target: target, At: time.Now()}
}

func (rc Context) errorResult(operation string, err error) tea.Msg {
	return CommandErrorResult{
		Error: rc.errorEvent(operation, err),
	}
}

// protocolCommand is implemented by any chatcmd that translates to a
// wire command, exposing it via `ToCommand`. Purely UI-side commands
// (help, clear, …) do not implement it.
type protocolCommand interface {
	ToCommand(rc Context) (protocol.Command, error)
}

// ReplyEvents carries the full slice of confirmation events the
// dispatcher synthesised in `Response.Events` and an optional later
// execution error. Its enclosing [CommandResult] identifies the
// window in which the command was issued.
// The chat-screen unpacks it into a [tea.Sequence] that re-delivers
// each event as its own message, so every confirmation reaches the
// per-event render arms in dispatcher order. For `Invite` this is a
// [domain.Inviting] or a [domain.SystemNotice]; for `Whois` a
// [domain.Whois]; for `List` one [domain.ListReply] per channel
// followed by a closing [domain.ListEnd]. A `PrivMsg` / `Action`
// confirmation reaches the user-client over the bus via
// echo-message, so the chat-screen drops [domain.Message] from this
// slice.
type ReplyEvents struct {
	Events []domain.ProtocolEvent
	Error  *domain.ErrorEvent
}

// sendCommand routes a migrated command through the protocol client.
// Translation and transport failures return a [CommandErrorResult].
// A dispatcher response returns every event from `Response.Events` and
// its optional typed `Response.Err` together in [ReplyEvents], so the
// chat-screen renders the complete response in order. Commands whose
// handler returns neither events nor an error return `nil`, leaving the
// caller to follow up with its post-success `tea.Msg`. JOIN's
// partial-success shape has per-target outcomes, so [JoinCommand.Run]
// talks to the client directly and skips this helper.
func sendCommand(ctx context.Context, rc Context, c protocolCommand, operation string) tea.Msg {
	cmd, err := c.ToCommand(rc)
	if err != nil {
		return rc.errorResult(operation, err)
	}

	resp, err := rc.Client.Send(ctx, cmd)
	if err != nil {
		return rc.errorResult(operation, err)
	}

	if len(resp.Events) > 0 {
		reply := ReplyEvents{Events: resp.Events}
		if resp.Err != nil {
			event := rc.errorEvent(operation, resp.Err)
			reply.Error = &event
		}

		return reply
	}

	if resp.Err != nil {
		return rc.errorResult(operation, resp.Err)
	}

	return nil
}

// toolContext adapts a [modelclient.ToolContext] to the [Context]
// that `ToCommand` reads from, so the same translation method serves
// both `Run` (chat-screen) and `RunTool` (model). The returned
// context carries the active window and protocol client: every field
// `ToCommand` implementation consults. The
// cancellation context is threaded separately to the wire send.
//
// `Active` is the name of the window the call is running in, which a
// tool context carries as an address (see
// [modelclient.ToolContext]). A caller with no window leaves it
// empty, and the tools that need one have already refused by the
// time this runs.
func toolContext(tc modelclient.ToolContext) Context {
	name, ok := protocol.WindowName(tc.Target)
	var window domain.Window
	if ok {
		window = domain.WindowKey(name)
	}

	return Context{
		Session: tc.Session,
		Manager: tc.Manager,
		Active:  window,
		Client:  tc.Client,
	}
}

// toolChannel returns the channel the call is running in. The second
// return is false when the window is not a channel, which covers a
// DM, the status window and a caller with no window at all.
func toolChannel(tc modelclient.ToolContext) (domain.ChannelName, bool) {
	ch, ok := tc.Target.(protocol.ChannelTarget)

	return domain.ChannelName(ch), ok
}

// noActiveChannel is the refusal a channel-scoped tool returns when
// the call is not running in a channel.
func noActiveChannel() modelclient.ToolResultPayload {
	return modelclient.ToolResultPayload{OK: false, Error: "no active channel"}
}

// sendToolCommand routes a migrated command through the model's
// protocol client and assembles the [modelclient.ToolResultPayload]
// the LLM tool-result protocol expects. Translation and dispatcher
// refusals become `OK: false` results the model can correct. A
// transport or session execution failure aborts the tool loop. A
// [domain.SystemNotice] in `Response.Events`, where `Response.Err` is
// nil, also becomes `OK: false`. This is the shape `inviteAs` uses for
// an unknown target nick, and the notice text tells the model what
// went wrong. Success returns `OK: true`
// with the caller-supplied summary so the model sees a stable
// confirmation line.
//
// Typed errors are flattened to strings; the LLM tool-result
// protocol carries strings only, so callers cannot `errors.As` over
// the result.
func toolResult(payload modelclient.ToolResultPayload) modelclient.ToolOutcome {
	return modelclient.ToolOutcome{Payload: payload}
}

func toolRefusal(err error) modelclient.ToolOutcome {
	return toolResult(modelclient.ToolResultPayload{OK: false, Error: err.Error()})
}

func toolExecutionFailure(err error) modelclient.ToolOutcome {
	return modelclient.ToolOutcome{ExecutionError: err}
}

func sendToolCommand(ctx context.Context, tc modelclient.ToolContext, c protocolCommand, summary string) modelclient.ToolOutcome {
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

	if notice, ok := replyFailureNotice(resp.Events); ok {
		return toolResult(modelclient.ToolResultPayload{OK: false, Error: notice.Text})
	}

	return toolResult(modelclient.ToolResultPayload{OK: true, Summary: summary})
}

// replyFailureNotice reports whether the dispatcher folded a
// failure into `Response.Events` as a [domain.SystemNotice] — the
// shape `inviteAs` uses for an unknown target nick, where the
// command itself succeeds (`Response.Err == nil`) but the action
// did not take effect.
func replyFailureNotice(events []domain.ProtocolEvent) (domain.SystemNotice, bool) {
	for _, evt := range events {
		if notice, ok := evt.(domain.SystemNotice); ok {
			return notice, true
		}
	}

	return domain.SystemNotice{}, false
}

func usageCmd(cmd, usage string) tea.Cmd {
	return func() tea.Msg { return UsageError{Command: cmd, Usage: usage} }
}

func noChannelCmd(command string) tea.Cmd {
	return func() tea.Msg { return NoChannelError{Command: command} }
}

func (rc Context) configResetRequested() bool {
	value, ok := rc.Invocation.ValueAtPath("config")
	if !ok {
		return false
	}

	cfg, ok := value.(ConfigCommand)
	if !ok {
		return false
	}

	return cfg.Reset
}

// renderReplyPart validates a [protocol.ReplyPart] for IRC delivery
// and returns the wire body. Plain text passes through; styled spans
// are encoded into IRC mIRC control characters via `ircfmt`.
func renderReplyPart(part protocol.ReplyPart) (string, error) {
	if err := protocol.ValidateReplyPart(part); err != nil {
		return "", err
	}

	if part.Body != "" {
		return part.Body, nil
	}

	spans := make([]richtext.Span, 0, len(part.Spans))
	for _, span := range part.Spans {
		attrs := richtext.Attrs{}
		if span.Style != nil {
			attrs = replyStyleToAttrs(*span.Style)
		}
		spans = append(spans, richtext.Span{Text: span.Text, Attrs: attrs})
	}

	return ircfmt.Encode(richtext.NewDocumentFromLines([]richtext.Line{{Spans: spans}})), nil
}

type renderedMessage struct {
	body       string
	pacingText string
}

// renderReplyMessages validates the complete body-or-spans batch
// before rendering any message. Plain body elements remain separate;
// spans describe one styled message.
func renderReplyMessages(kind protocol.ReplyKind, bodies []string, spans protocol.ReplySpans) ([]renderedMessage, error) {
	hasBodies := len(bodies) > 0
	hasSpans := len(spans) > 0
	if hasBodies == hasSpans {
		return nil, protocol.ReplyPartShapeError{HasBody: hasBodies, HasSpans: hasSpans}
	}

	if hasBodies {
		if err := validateMessageBodies(kind, bodies); err != nil {
			return nil, err
		}

		messages := make([]renderedMessage, len(bodies))
		for index, body := range bodies {
			messages[index] = renderedMessage{body: body, pacingText: body}
		}

		return messages, nil
	}
	if len(spans) > maxToolReplySpans {
		return nil, &command.TooManyValuesError{Name: "spans", Maximum: maxToolReplySpans, Actual: len(spans)}
	}

	body, err := renderReplyPart(protocol.ReplyPart{Kind: kind, Spans: spans})
	if err != nil {
		return nil, err
	}

	var pacingText strings.Builder
	for _, span := range spans {
		pacingText.WriteString(span.Text)
	}

	return []renderedMessage{{body: body, pacingText: pacingText.String()}}, nil
}

func replyStyleToAttrs(style protocol.ReplyStyle) richtext.Attrs {
	return richtext.Attrs{
		Bold:      style.Bold,
		Italic:    style.Italic,
		Underline: style.Underline,
		Reverse:   style.Reverse,
		Strike:    style.Strike,
		FG:        cloneReplyColour(style.FG),
		BG:        cloneReplyColour(style.BG),
	}
}

func cloneReplyColour(colour *protocol.ReplyPaletteIndex) *uint8 {
	if colour == nil {
		return nil
	}
	value := uint8(*colour)
	return &value
}
