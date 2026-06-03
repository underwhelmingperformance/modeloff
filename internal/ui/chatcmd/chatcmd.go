// Package chatcmd defines the concrete slash-command types for the
// chat screen. It consumes the generic command library and binds it
// to the application's session layer and Bubble Tea runtime.
package chatcmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
// Actor is the `*domain.Instance` for the caller — the user's handle
// for slash-command invocations. Client is the protocol-side client
// handle the caller dispatches commands through (the user-client
// for chat-screen invocations). Both are guaranteed non-nil at
// construction in `runContext` (chat_commands.go). The cancellation
// context is threaded as an explicit first parameter to [Command.Run]
// and not carried on the struct.
type Context struct {
	Session    modelclient.SessionAPI
	Manager    modelclient.ManagerAPI
	Config     config.Store
	Active     domain.ChannelName
	Actor      *domain.Instance
	Client     protocol.Client
	Invocation command.Invocation[CompletionContext]
}

// updateConfig loads the current configuration, applies fn, and saves
// the result. It returns the updated configuration.
func (rc Context) updateConfig(ctx context.Context, fn func(*config.Config)) (config.Config, error) {
	cfg, err := rc.Config.Load(ctx)
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}

	fn(&cfg)

	if err := rc.Config.Save(ctx, cfg); err != nil {
		return config.Config{}, fmt.Errorf("save config: %w", err)
	}

	return cfg, nil
}

// HelpResult signals that the help screen should be shown.
type HelpResult struct{}

// ClearResult signals that the current window should be cleared.
type ClearResult struct{}

// WhoisResult is the chat-screen-side dispatch marker for a
// `/whois` reply. It embeds the dispatcher's [domain.Whois]
// snapshot so the renderer reads the snapshot fields directly,
// and the wrapping struct keeps the result distinguishable from
// a `Whois` event arriving on the protocol bus through any
// other path. `/whois` produces an `UnknownNickError` when the
// nick does not resolve rather than a `WhoisResult` carrying a
// zero snapshot.
type WhoisResult struct {
	domain.Whois
}

// TopicInfoResult carries the current topic metadata for
// display. `Window` is the typed `*ChannelWindow` so the UI
// can read `Topic` / `TopicSetBy` / `TopicSetAt` directly off
// the handle. DM and status windows never produce a
// `TopicInfoResult` — `/topic` rejects non-channel targets at
// the command layer.
type TopicInfoResult struct {
	Window *domain.ChannelWindow
}

// ListResult is the chat-screen-side dispatch marker for a
// `/list` reply. The named-slice shape keeps the result
// distinguishable from a bare `[]ChannelDirectoryEntry` in a
// type switch while letting handlers iterate it directly without
// an `.Entries` indirection.
type ListResult []domain.ChannelDirectoryEntry

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

// TimestampFormatSetResult signals that the timestamp format was
// updated.
type TimestampFormatSetResult struct {
	Format *string
	Reset  bool
}

// PersonasListResult is the chat-screen-side dispatch marker
// for a `/personas` reply. The named-slice shape keeps the
// result distinguishable from a bare `[]domain.Persona` in a
// type switch.
type PersonasListResult []domain.Persona

// PersonasRegeneratedResult signals that personas were regenerated.
type PersonasRegeneratedResult struct {
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

func errorEvent(operation string, err error) domain.ErrorEvent {
	return domain.ErrorEvent{Operation: operation, Err: err, At: time.Now()}
}

// protocolCommand is implemented by any chatcmd that translates to a
// wire command, exposing it via `ToCommand`. Purely UI-side commands
// (help, clear, …) do not implement it.
type protocolCommand interface {
	ToCommand(rc Context) (protocol.Command, error)
}

// sendCommand routes a migrated command through the protocol
// client. On any failure (translation, transport, or the
// dispatcher's typed `Response.Err`) it returns a
// [domain.ErrorEvent] for the chat-screen to render. On success
// it returns the first event the dispatcher synthesised in
// `Response.Events` — for `PrivMsg` and `Action` the canonical
// [domain.Message] the session persisted, which the chat-screen
// renders inline; for `Invite` a [domain.ModelInvited] (or a
// [domain.SystemNotice] when the target nick is unknown). Commands
// whose handler does not populate `Response.Events` (Topic, Kick,
// Nick, …) return `nil`, leaving the caller to follow up with
// whatever post-success `tea.Msg` it wants.
//
// Only `resp.Events[0]` is surfaced, so a handler returning
// multiple events would lose all but the first; today only `List`
// produces several, and it uses its own `fetch` path rather than
// `sendCommand`.
func sendCommand(ctx context.Context, rc Context, c protocolCommand, operation string) tea.Msg {
	cmd, err := c.ToCommand(rc)
	if err != nil {
		return errorEvent(operation, err)
	}

	resp, err := rc.Client.Send(ctx, cmd)
	if err != nil {
		return errorEvent(operation, err)
	}

	if resp.Err != nil {
		return errorEvent(operation, resp.Err)
	}

	if len(resp.Events) > 0 {
		return resp.Events[0]
	}

	return nil
}

// toolContext adapts a [modelclient.ToolContext] to the [Context]
// that `ToCommand` reads from, so the same translation method serves
// both `Run` (chat-screen) and `RunTool` (model). The returned
// context carries the actor, the active channel, and the protocol
// client — every field `ToCommand` implementations consult. The
// cancellation context is threaded separately to the wire send.
func toolContext(tc modelclient.ToolContext) Context {
	return Context{
		Session: tc.Session,
		Manager: tc.Manager,
		Active:  tc.Channel,
		Actor:   tc.Actor,
		Client:  tc.Client,
	}
}

// sendToolCommand routes a migrated command through the model's
// protocol client and assembles the [modelclient.ToolResultPayload]
// the LLM tool-result protocol expects. Errors at any of the three
// failure points (translation, transport, dispatcher) collapse to
// `OK: false` with the error string. Success returns `OK: true`
// with the caller-supplied summary so the model sees a stable
// confirmation line.
//
// Typed errors are flattened to strings; the LLM tool-result
// protocol carries strings only, so callers cannot `errors.As` over
// the result.
func sendToolCommand(ctx context.Context, tc modelclient.ToolContext, c protocolCommand, summary string) modelclient.ToolResultPayload {
	cmd, err := c.ToCommand(toolContext(tc))
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	resp, err := tc.Client.Send(ctx, cmd)
	if err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: err.Error()}
	}

	if resp.Err != nil {
		return modelclient.ToolResultPayload{OK: false, Error: resp.Err.Error()}
	}

	return modelclient.ToolResultPayload{OK: true, Summary: summary}
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

	if strings.TrimSpace(part.Body) != "" {
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

func cloneReplyColour(colour *uint8) *uint8 {
	if colour == nil {
		return nil
	}
	value := *colour
	return &value
}
