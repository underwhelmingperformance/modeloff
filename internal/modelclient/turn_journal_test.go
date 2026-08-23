package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

type recordingTurnJournal struct {
	turn          store.ModelTurn
	entries       []store.ModelTurnEntry
	beginErr      error
	appendErrKind store.ModelTurnEntryKind
	appendErr     error
}

// blockingTurnJournal parks inside the admission write until it is
// released, so a test can see what a turn did while the journal had
// accepted nothing.
type blockingTurnJournal struct {
	recordingTurnJournal

	release chan struct{}
}

func (j *blockingTurnJournal) BeginModelTurn(
	ctx context.Context,
	guard protocol.WindowGuard,
	turn store.ModelTurn,
	input store.ModelTurnEntry,
) (store.ModelTurnRecorder, error) {
	<-j.release

	return j.recordingTurnJournal.BeginModelTurn(ctx, guard, turn, input)
}

type contextBoundTurnJournal struct {
	entered chan context.Context
	release chan struct{}
	errors  []error
}

func (j *contextBoundTurnJournal) AppendModelTurnEntry(
	ctx context.Context,
	_ store.ModelTurnEntry,
) error {
	select {
	case j.entered <- ctx:
	default:
	}

	select {
	case <-j.release:
		return nil
	case <-ctx.Done():
		j.errors = append(j.errors, ctx.Err())

		return ctx.Err()
	}
}

func (j *recordingTurnJournal) BeginModelTurn(
	_ context.Context,
	_ protocol.WindowGuard,
	turn store.ModelTurn,
	input store.ModelTurnEntry,
) (store.ModelTurnRecorder, error) {
	if j.beginErr != nil {
		return nil, j.beginErr
	}

	j.turn = turn
	j.entries = append(j.entries, input)
	return j, nil
}

func (j *recordingTurnJournal) AppendModelTurnEntry(
	_ context.Context,
	entry store.ModelTurnEntry,
) error {
	if entry.Kind == j.appendErrKind {
		if j.appendErr != nil {
			return j.appendErr
		}

		return errors.New("journal unavailable")
	}

	entry.Data = append(json.RawMessage(nil), entry.Data...)
	j.entries = append(j.entries, entry)
	return nil
}

// testJournalWriter returns a writer for one turn that `recorder` has
// already admitted, so a test can drive the entries a turn produces
// without an admission round trip. Call `writer.queue.stop` before
// reading what the recorder received.
func testJournalWriter(recorder store.ModelTurnRecorder) *turnJournalWriter {
	return &turnJournalWriter{
		queue: newJournalQueue(context.Background()),
		turn:  &journalTurn{recorder: recorder},
		now:   func() time.Time { return time.Time{} },
	}
}

// sequenced numbers the entries a turn is expected to produce, so a
// test states what the turn records and the numbering is checked
// against the order the journal received them in.
func sequenced(entries []store.ModelTurnEntry) []store.ModelTurnEntry {
	for i := range entries {
		entries[i].Seq = i
	}

	return entries
}

func journalJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func TestModelTurnInput_preserves_the_provider_request_body(t *testing.T) {
	wire := api.RenderedEventRequest{
		Method:  "POST",
		Path:    "/api/v1/chat/completions",
		Headers: map[string]string{"x-session-id": "inst-botty"},
		Body:    json.RawMessage(`{"content":"stored <memory>& mood"}`),
	}
	data := journalJSON(t, journalInput(wire))
	var stored struct {
		Request struct {
			Method  string            `json:"method"`
			Path    string            `json:"path"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
		} `json:"request"`
	}
	err := json.Unmarshal(data, &stored)

	require.Equal(t, struct {
		Error   error
		Method  string
		Path    string
		Headers map[string]string
		Body    string
	}{
		Method:  "POST",
		Path:    "/api/v1/chat/completions",
		Headers: map[string]string{"x-session-id": "inst-botty"},
		Body:    string(wire.Body),
	}, struct {
		Error   error
		Method  string
		Path    string
		Headers map[string]string
		Body    string
	}{
		Error:   err,
		Method:  stored.Request.Method,
		Path:    stored.Request.Path,
		Headers: stored.Request.Headers,
		Body:    stored.Request.Body,
	})
}

func TestDispatchToInstance_sends_an_initial_request_recorded_before_authority_changes(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	guard := &revocableWindowGuard{}
	guard.valid.Store(true)
	journal := &recordingTurnJournal{}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	trigger := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: "hello", At: sess.Now(),
	}
	var requests []turnPrompt
	var effects []string
	call := api.PendingToolCall{ID: "effect-1", Name: "effect", Args: json.RawMessage(`{}`)}
	upstream := &apitest.Fake{SendEventsFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		_ api.SystemPrompt,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (api.CompletionResult, error) {
		requests = append(requests, turnPrompt{history: bodies(history), triggers: bodies(events)})
		guard.valid.Store(false)

		return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{call}}, nil
	}}
	definition := api.ToolDefinition{Name: "effect"}
	tools := NewToolRegistry(ToolSpec{
		Definition: definition,
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			effects = append(effects, "effect")

			return ToolResultPayload{OK: true}, nil
		},
	})
	mc := New(Config{
		Instance: inst, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     tools, Journal: journal,
	})
	window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
		guard: guard, events: []protocol.IRCMessage{trigger},
	})
	mc.Wait()

	prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
	rendered, renderErr := api.RenderEventRequest(
		inst.ModelID, inst.ID(), prompt, nil, []protocol.IRCMessage{trigger}, definition,
	)
	require.NoError(t, renderErr)
	require.ErrorIs(t, err, errDispatchWindowClosed)
	require.Equal(t, struct {
		Requests []turnPrompt
		Effects  []string
		Entries  []store.ModelTurnEntry
	}{
		Requests: []turnPrompt{{triggers: []string{"hello"}}},
		Entries: sequenced([]store.ModelTurnEntry{
			{
				Kind: store.ModelTurnInput,
				Data: journalJSON(t, journalInput(rendered)),
				At:   sess.Now(),
			},
			{
				Kind: store.ModelTurnAssistant,
				Data: journalJSON(t, modelTurnAssistant{
					ToolCalls: journalToolCalls([]api.PendingToolCall{call}),
				}),
				At: sess.Now(),
			},
			{
				Kind: store.ModelTurnOutcome,
				Data: journalJSON(t, modelTurnOutcome{
					Error: errDispatchWindowClosed.Error(),
				}),
				At: sess.Now(),
			},
		}),
	}, struct {
		Requests []turnPrompt
		Effects  []string
		Entries  []store.ModelTurnEntry
	}{
		Requests: requests,
		Effects:  effects,
		Entries:  journal.entries,
	})
}

func TestDispatchToInstance_journals_the_complete_provider_turn(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	journal := &recordingTurnJournal{}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	trigger := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev",
		Body:   "hello",
		At:     sess.Now(),
	}
	definition := api.ToolDefinition{
		Name:        "effect",
		Description: "Record an effect.",
		Parameters:  map[string]any{"type": "object"},
	}
	call := api.PendingToolCall{ID: "call-1", Name: "effect", Args: json.RawMessage(`{}`)}
	first := api.CompletionResult{
		PendingToolCalls: []api.PendingToolCall{call},
		RequestID:        "request-1",
		Usage:            api.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
	}
	second := api.CompletionResult{
		RequestID: "request-2",
		Usage:     api.Usage{PromptTokens: 120, CompletionTokens: 5, TotalTokens: 125},
	}
	continuedRequest := api.RenderedEventRequest{
		Method:  "POST",
		Path:    "/api/v1/chat/completions",
		Headers: map[string]string{"x-session-id": "inst-botty"},
		Body:    json.RawMessage(`{"messages":[{"role":"tool","content":"{\"ok\":true}"}]}`),
	}
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			return first, nil
		},
		ContinueWithToolResultsFn: func(
			context.Context,
			*api.Conversation,
			[]api.ToolResult,
		) (api.CompletionResult, error) {
			return second, nil
		},
		RenderToolResultRequestFn: func(
			*api.Conversation,
			[]api.ToolResult,
		) (api.RenderedEventRequest, error) {
			return continuedRequest, nil
		},
	}
	registry := NewToolRegistry(ToolSpec{
		Definition: definition,
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			return ToolResultPayload{OK: true}, nil
		},
	})
	mc := New(Config{
		Instance:  inst,
		Session:   sess,
		APIClient: func() api.Client { return upstream },
		Tools:     registry,
		Journal:   journal,
	})
	window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api:    upstream,
		window: window,
		target: protocol.ChannelTarget("#dev"),
		guard:  validWindowGuard{window: window},
		events: []protocol.IRCMessage{trigger},
	})
	mc.Wait()
	require.NoError(t, err)

	prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
	renderedRequest, err := api.RenderEventRequest(
		"test/model",
		"inst-botty",
		prompt,
		make([]protocol.IRCMessage, 0),
		[]protocol.IRCMessage{trigger},
		definition,
	)
	require.NoError(t, err)
	toolResults := []api.ToolResult{{ToolCallID: "call-1", Content: `{"ok":true}`}}
	require.Equal(t, store.ModelTurn{
		InstanceID: "inst-botty",
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    "test/model",
		StartedAt:  sess.Now(),
	}, journal.turn)
	require.Equal(t, sequenced([]store.ModelTurnEntry{
		{
			Kind: store.ModelTurnInput,
			Data: journalJSON(t, journalInput(renderedRequest)),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnAssistant,
			Data: journalJSON(t, modelTurnAssistant{
				ToolCalls: journalToolCalls([]api.PendingToolCall{call}),
				RequestID: "request-1",
				Usage:     first.Usage,
			}),
			At: sess.Now(),
		},
		{
			Kind: store.ModelTurnToolResults,
			Data: journalJSON(t, modelTurnToolResults{Results: toolResults}),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnInput,
			Data: journalJSON(t, journalInput(continuedRequest)),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnAssistant,
			Data: journalJSON(t, modelTurnAssistant{
				RequestID: "request-2",
				Usage:     second.Usage,
			}),
			At: sess.Now(),
		},
		{
			Kind: store.ModelTurnOutcome,
			Data: journalJSON(t, modelTurnOutcome{
				ToolTurnCount: 1,
				PassReason:    "model_pass",
			}),
			At: sess.Now(),
		},
	}), journal.entries)
}

func TestDispatchToInstance_retries_a_continuation_without_repeating_tool_effects(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	journal := &recordingTurnJournal{}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	trigger := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#dev", Body: "apply it", At: sess.Now(),
	}
	definition := api.ToolDefinition{
		Name: "effect", Description: "Record an effect.",
		Parameters: map[string]any{"type": "object"},
	}
	call := api.PendingToolCall{ID: "call-1", Name: "effect", Args: json.RawMessage(`{}`)}
	conversation := &api.Conversation{}
	first := api.CompletionResult{
		PendingToolCalls: []api.PendingToolCall{call},
		Conversation:     conversation,
		RequestID:        "request-1",
		Usage:            api.Usage{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110},
	}
	second := api.CompletionResult{
		RequestID: "request-2",
		Usage:     api.Usage{PromptTokens: 120, CompletionTokens: 5, TotalTokens: 125},
	}
	continuedRequest := api.RenderedEventRequest{
		Method: "POST", Path: "/api/v1/chat/completions",
		Headers: map[string]string{"x-session-id": "inst-botty"},
		Body:    json.RawMessage(`{"messages":[{"role":"tool","content":"{\"ok\":true}"}]}`),
	}
	toolResults := []api.ToolResult{{ToolCallID: "call-1", Content: `{"ok":true}`}}
	type continuationAttempt struct {
		SameConversation bool
		ToolResults      []api.ToolResult
	}
	var (
		effects              []string
		continuationAttempts []continuationAttempt
	)
	waiter := newRecordingRetryWaiter()
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			return first, nil
		},
		ContinueWithToolResultsFn: func(
			_ context.Context,
			gotConversation *api.Conversation,
			results []api.ToolResult,
		) (api.CompletionResult, error) {
			continuationAttempts = append(continuationAttempts, continuationAttempt{
				SameConversation: gotConversation == conversation,
				ToolResults:      append([]api.ToolResult(nil), results...),
			})
			if len(continuationAttempts) == 1 {
				return api.CompletionResult{}, upstreamErrorWithRetryAfter(
					t, http.StatusServiceUnavailable, "10",
				)
			}

			return second, nil
		},
		RenderToolResultRequestFn: func(
			*api.Conversation,
			[]api.ToolResult,
		) (api.RenderedEventRequest, error) {
			return continuedRequest, nil
		},
	}
	registry := NewToolRegistry(ToolSpec{
		Definition: definition,
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			effects = append(effects, "effect")

			return ToolResultPayload{OK: true}, nil
		},
	})
	mc := New(Config{
		Instance: inst, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     registry, Journal: journal,
	})
	mc.retry = retryPolicy{Delay: retryTestDelay, Waiter: waiter}
	window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

	done := make(chan error, 1)
	go func() {
		dispatchErr := mc.dispatchToInstance(t.Context(), turnRequest{
			api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
			guard: validWindowGuard{window: window}, events: []protocol.IRCMessage{trigger},
		})
		mc.Wait()

		done <- dispatchErr
	}()
	wait := <-waiter.waits

	prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
	renderedRequest, err := api.RenderEventRequest(
		"test/model", "inst-botty", prompt, nil,
		[]protocol.IRCMessage{trigger}, definition,
	)
	require.NoError(t, err)
	require.Equal(t, struct {
		Delay                time.Duration
		Effects              []string
		ContinuationAttempts []continuationAttempt
	}{
		Delay:                10 * time.Second,
		Effects:              []string{"effect"},
		ContinuationAttempts: []continuationAttempt{{SameConversation: true, ToolResults: toolResults}},
	}, struct {
		Delay                time.Duration
		Effects              []string
		ContinuationAttempts []continuationAttempt
	}{
		Delay:                wait.delay,
		Effects:              effects,
		ContinuationAttempts: continuationAttempts,
	})

	close(wait.release)
	require.NoError(t, <-done)
	wantEntries := sequenced([]store.ModelTurnEntry{
		{
			Kind: store.ModelTurnInput,
			Data: journalJSON(t, journalInput(renderedRequest)),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnAssistant,
			Data: journalJSON(t, modelTurnAssistant{
				ToolCalls: journalToolCalls([]api.PendingToolCall{call}),
				RequestID: "request-1", Usage: first.Usage,
			}),
			At: sess.Now(),
		},
		{
			Kind: store.ModelTurnToolResults,
			Data: journalJSON(t, modelTurnToolResults{Results: toolResults}),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnInput,
			Data: journalJSON(t, journalInput(continuedRequest)),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnInput,
			Data: journalJSON(t, journalInput(continuedRequest)),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnAssistant,
			Data: journalJSON(t, modelTurnAssistant{
				RequestID: "request-2", Usage: second.Usage,
			}),
			At: sess.Now(),
		},
		{
			Kind: store.ModelTurnOutcome,
			Data: journalJSON(t, modelTurnOutcome{
				ToolTurnCount: 1, PassReason: "model_pass",
			}),
			At: sess.Now(),
		},
	})
	require.Equal(t, struct {
		Effects              []string
		ContinuationAttempts []continuationAttempt
		JournalEntries       []store.ModelTurnEntry
	}{
		Effects: []string{"effect"},
		ContinuationAttempts: []continuationAttempt{
			{SameConversation: true, ToolResults: toolResults},
			{SameConversation: true, ToolResults: toolResults},
		},
		JournalEntries: wantEntries,
	}, struct {
		Effects              []string
		ContinuationAttempts []continuationAttempt
		JournalEntries       []store.ModelTurnEntry
	}{
		Effects:              effects,
		ContinuationAttempts: continuationAttempts,
		JournalEntries:       journal.entries,
	})
}

func TestDispatchToInstance_journals_malformed_tool_arguments_and_correction(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	journal := &recordingTurnJournal{}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	trigger := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Body: "hello", At: sess.Now(),
	}
	definition := api.ToolDefinition{
		Name: "effect", Description: "Record an effect.",
		Parameters: map[string]any{"type": "object"},
	}
	call := api.PendingToolCall{
		ID: "call-1", Name: "effect", Args: json.RawMessage(`{`),
	}
	first := api.CompletionResult{
		PendingToolCalls: []api.PendingToolCall{call},
		RequestID:        "request-1",
		Usage:            api.Usage{TotalTokens: 10},
	}
	second := api.CompletionResult{
		RequestID: "request-2",
		Usage:     api.Usage{TotalTokens: 12},
	}
	toolResults := []api.ToolResult{{
		ToolCallID: "call-1",
		Content:    `{"ok":false,"error":"unexpected end of JSON input"}`,
	}}
	continuedRequest := api.RenderedEventRequest{
		Method: "POST", Path: "/chat/completions",
		Body: json.RawMessage(`{"messages":[{"role":"tool","content":"invalid arguments"}]}`),
	}
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			return first, nil
		},
		ContinueWithToolResultsFn: func(
			_ context.Context,
			_ *api.Conversation,
			got []api.ToolResult,
		) (api.CompletionResult, error) {
			require.Equal(t, toolResults, got)
			return second, nil
		},
		RenderToolResultRequestFn: func(
			*api.Conversation,
			[]api.ToolResult,
		) (api.RenderedEventRequest, error) {
			return continuedRequest, nil
		},
	}
	registry := NewToolRegistry(ToolSpec{
		Definition: definition,
		Execute: func(_ context.Context, _ ToolContext, args json.RawMessage) (ToolResultPayload, error) {
			var decoded map[string]any
			if err := json.Unmarshal(args, &decoded); err != nil {
				return ToolResultPayload{}, err
			}

			return ToolResultPayload{OK: true}, nil
		},
	})
	mc := New(Config{
		Instance: inst, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     registry, Journal: journal,
	})
	window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
		guard: validWindowGuard{window: window}, events: []protocol.IRCMessage{trigger},
	})
	mc.Wait()
	require.NoError(t, err)

	prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
	rendered, err := api.RenderEventRequest(
		"test/model", "inst-botty", prompt, nil,
		[]protocol.IRCMessage{trigger}, definition,
	)
	require.NoError(t, err)
	require.Equal(t, sequenced([]store.ModelTurnEntry{
		{
			Kind: store.ModelTurnInput,
			Data: journalJSON(t, journalInput(rendered)),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnAssistant,
			Data: journalJSON(t, modelTurnAssistant{
				ToolCalls: []modelTurnToolCall{{ID: "call-1", Name: "effect", Args: "{"}},
				RequestID: "request-1",
				Usage:     first.Usage,
			}),
			At: sess.Now(),
		},
		{
			Kind: store.ModelTurnToolResults,
			Data: journalJSON(t, modelTurnToolResults{Results: toolResults}),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnInput,
			Data: journalJSON(t, journalInput(continuedRequest)),
			At:   sess.Now(),
		},
		{
			Kind: store.ModelTurnAssistant,
			Data: journalJSON(t, modelTurnAssistant{
				RequestID: "request-2", Usage: second.Usage,
			}),
			At: sess.Now(),
		},
		{
			Kind: store.ModelTurnOutcome,
			Data: journalJSON(t, modelTurnOutcome{
				ToolTurnCount: 1, PassReason: "model_pass",
			}),
			At: sess.Now(),
		},
	}), journal.entries)
}

func TestDispatchToInstance_journals_completed_tools_before_a_later_failure(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	journal := &recordingTurnJournal{}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	trigger := protocol.IRCMessage{Kind: protocol.KindPrivMsg, Body: "hello", At: sess.Now()}
	calls := []api.PendingToolCall{
		{ID: "call-1", Name: "succeed", Args: json.RawMessage(`{}`)},
		{ID: "call-2", Name: "fail", Args: json.RawMessage(`{}`)},
		{ID: "call-3", Name: "after", Args: json.RawMessage(`{}`)},
	}
	result := api.CompletionResult{
		PendingToolCalls: calls,
		RequestID:        "request-1",
		Usage:            api.Usage{TotalTokens: 10},
	}
	upstream := &apitest.Fake{SendEventsFn: func(
		context.Context,
		domain.ModelID,
		domain.InstanceID,
		api.SystemPrompt,
		[]protocol.IRCMessage,
		[]protocol.IRCMessage,
	) (api.CompletionResult, error) {
		return result, nil
	}}
	executionErr := errors.New("effect unavailable")
	succeedDefinition := api.ToolDefinition{
		Name: "succeed", Description: "Succeed.", Parameters: map[string]any{"type": "object"},
	}
	failDefinition := api.ToolDefinition{
		Name: "fail", Description: "Fail.", Parameters: map[string]any{"type": "object"},
	}
	afterDefinition := api.ToolDefinition{
		Name: "after", Description: "Run after failure.", Parameters: map[string]any{"type": "object"},
	}
	definitions := []api.ToolDefinition{succeedDefinition, failDefinition, afterDefinition}
	var effects []string
	registry := NewToolRegistry(
		ToolSpec{
			Definition: succeedDefinition,
			Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
				effects = append(effects, "succeed")
				return ToolResultPayload{OK: true}, nil
			},
		},
		ToolSpec{
			Definition: failDefinition,
			Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
				effects = append(effects, "fail")
				return ToolResultPayload{}, &ToolExecutionError{Tool: "fail", Err: executionErr}
			},
		},
		ToolSpec{
			Definition: afterDefinition,
			Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
				effects = append(effects, "after")
				return ToolResultPayload{OK: true}, nil
			},
		},
	)
	mc := New(Config{
		Instance: inst, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     registry, Journal: journal,
	})
	window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
		guard: validWindowGuard{window: window}, events: []protocol.IRCMessage{trigger},
	})
	mc.Wait()

	require.ErrorIs(t, err, executionErr)
	prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
	rendered, renderErr := api.RenderEventRequest(
		"test/model", "inst-botty", prompt, nil,
		[]protocol.IRCMessage{trigger}, definitions...,
	)
	require.NoError(t, renderErr)
	toolResults := []api.ToolResult{{ToolCallID: "call-1", Content: `{"ok":true}`}}
	require.Equal(t, struct {
		Effects []string
		Entries []store.ModelTurnEntry
	}{
		Effects: []string{"succeed", "fail"},
		Entries: sequenced([]store.ModelTurnEntry{
			{
				Kind: store.ModelTurnInput,
				Data: journalJSON(t, journalInput(rendered)),
				At:   sess.Now(),
			},
			{
				Kind: store.ModelTurnAssistant,
				Data: journalJSON(t, modelTurnAssistant{
					ToolCalls: journalToolCalls(calls), RequestID: "request-1", Usage: result.Usage,
				}),
				At: sess.Now(),
			},
			{
				Kind: store.ModelTurnToolResults,
				Data: journalJSON(t, modelTurnToolResults{Results: toolResults}),
				At:   sess.Now(),
			},
			{
				Kind: store.ModelTurnOutcome,
				Data: journalJSON(t, modelTurnOutcome{
					ToolTurnCount: 1,
					Error:         "execute tool fail: effect unavailable",
				}),
				At: sess.Now(),
			},
		}),
	}, struct {
		Effects []string
		Entries []store.ModelTurnEntry
	}{
		Effects: effects,
		Entries: journal.entries,
	})
}

func TestDispatchToInstance_stops_after_the_last_executed_tool_batch(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	journal := &recordingTurnJournal{}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	call := api.PendingToolCall{ID: "call", Name: "effect", Args: json.RawMessage(`{}`)}
	initialResult := api.CompletionResult{
		PendingToolCalls: []api.PendingToolCall{call},
		RequestID:        "request-1",
	}
	continuations := make([]api.CompletionResult, maxToolLoopTurns-1)
	for i := range continuations {
		continuations[i] = api.CompletionResult{
			PendingToolCalls: []api.PendingToolCall{call},
			RequestID:        fmt.Sprintf("request-%d", i+2),
		}
	}
	results := append([]api.CompletionResult{initialResult}, continuations...)
	var continueCalls int
	continuedRequest := api.RenderedEventRequest{
		Method: "POST", Path: "/chat/completions",
		Body: json.RawMessage(`{"messages":[{"role":"tool","content":"{\"ok\":true}"}]}`),
	}
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			return initialResult, nil
		},
		ContinueWithToolResultsFn: func(
			context.Context,
			*api.Conversation,
			[]api.ToolResult,
		) (api.CompletionResult, error) {
			continueCalls++
			if continueCalls > len(continuations) {
				return api.CompletionResult{RequestID: "unexecuted"}, nil
			}

			return continuations[continueCalls-1], nil
		},
		RenderToolResultRequestFn: func(
			*api.Conversation,
			[]api.ToolResult,
		) (api.RenderedEventRequest, error) {
			return continuedRequest, nil
		},
	}
	definition := api.ToolDefinition{
		Name: "effect", Description: "Record an effect.",
		Parameters: map[string]any{"type": "object"},
	}
	var toolCalls int
	registry := NewToolRegistry(ToolSpec{
		Definition: definition,
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			toolCalls++
			return ToolResultPayload{OK: true}, nil
		},
	})
	mc := New(Config{
		Instance: inst, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     registry, Journal: journal,
	})
	window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
		guard: validWindowGuard{window: window},
	})
	mc.Wait()
	require.NoError(t, err)

	prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
	rendered, renderErr := api.RenderEventRequest(
		"test/model", "inst-botty", prompt, nil, nil, definition,
	)
	require.NoError(t, renderErr)
	toolResult := []api.ToolResult{{ToolCallID: "call", Content: `{"ok":true}`}}
	wantEntries := []store.ModelTurnEntry{{
		Kind: store.ModelTurnInput,
		Data: journalJSON(t, journalInput(rendered)),
		At:   sess.Now(),
	}}
	for i, result := range results {
		wantEntries = append(wantEntries,
			store.ModelTurnEntry{
				Kind: store.ModelTurnAssistant,
				Data: journalJSON(t, modelTurnAssistant{
					ToolCalls: journalToolCalls([]api.PendingToolCall{call}),
					RequestID: result.RequestID,
				}),
				At: sess.Now(),
			},
			store.ModelTurnEntry{
				Kind: store.ModelTurnToolResults,
				Data: journalJSON(t, modelTurnToolResults{Results: toolResult}),
				At:   sess.Now(),
			},
		)
		if i < len(results)-1 {
			wantEntries = append(wantEntries, store.ModelTurnEntry{
				Kind: store.ModelTurnInput,
				Data: journalJSON(t, journalInput(continuedRequest)),
				At:   sess.Now(),
			})
		}
	}
	wantEntries = append(wantEntries, store.ModelTurnEntry{
		Kind: store.ModelTurnOutcome,
		Data: journalJSON(t, modelTurnOutcome{
			ToolTurnCount: maxToolLoopTurns,
			PassReason:    observability.PassReasonToolLoopExhausted,
		}),
		At: sess.Now(),
	})
	require.Equal(t, struct {
		ContinueCalls int
		ToolCalls     int
		Entries       []store.ModelTurnEntry
	}{
		ContinueCalls: maxToolLoopTurns - 1,
		ToolCalls:     maxToolLoopTurns,
		Entries:       sequenced(wantEntries),
	}, struct {
		ContinueCalls int
		ToolCalls     int
		Entries       []store.ModelTurnEntry
	}{
		ContinueCalls: continueCalls,
		ToolCalls:     toolCalls,
		Entries:       journal.entries,
	})
}

func TestDispatchToInstance_journals_rejected_tool_batches(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	journal := &recordingTurnJournal{}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	call := api.PendingToolCall{ID: "unknown-call", Name: "unknown", Args: json.RawMessage(`{}`)}
	results := make([]api.CompletionResult, maxToolLoopTurns)
	for i := range results {
		results[i] = api.CompletionResult{
			PendingToolCalls: []api.PendingToolCall{call},
			RequestID:        fmt.Sprintf("request-%d", i+1),
		}
	}
	nextResult := 1
	continuedRequest := api.RenderedEventRequest{
		Method: "POST", Path: "/chat/completions",
		Body: json.RawMessage(`{"messages":[{"role":"tool","content":"{\"ok\":false}"}]}`),
	}
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			return results[0], nil
		},
		ContinueWithToolResultsFn: func(
			context.Context,
			*api.Conversation,
			[]api.ToolResult,
		) (api.CompletionResult, error) {
			if nextResult == len(results) {
				return api.CompletionResult{RequestID: "unexpected-continuation"}, nil
			}
			result := results[nextResult]
			nextResult++

			return result, nil
		},
		RenderToolResultRequestFn: func(
			*api.Conversation,
			[]api.ToolResult,
		) (api.RenderedEventRequest, error) {
			return continuedRequest, nil
		},
	}
	definition := api.ToolDefinition{
		Name: "effect", Description: "Record an effect.",
		Parameters: map[string]any{"type": "object"},
	}
	registry := NewToolRegistry(ToolSpec{
		Definition: definition,
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			t.Fatal("the rejected tool batch executed an effect")

			return ToolResultPayload{}, nil
		},
	})
	mc := New(Config{
		Instance: inst, Session: sess,
		APIClient: func() api.Client { return upstream },
		Tools:     registry, Journal: journal,
	})
	window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

	err := mc.dispatchToInstance(t.Context(), turnRequest{
		api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
		guard: validWindowGuard{window: window},
	})
	mc.Wait()
	require.NoError(t, err)

	prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
	rendered, renderErr := api.RenderEventRequest(
		"test/model", "inst-botty", prompt, nil, nil, definition,
	)
	require.NoError(t, renderErr)
	toolResults := []api.ToolResult{{
		ToolCallID: "unknown-call",
		Content:    `{"ok":false,"error":"unknown tool \"unknown\""}`,
	}}
	wantEntries := []store.ModelTurnEntry{{
		Kind: store.ModelTurnInput,
		Data: journalJSON(t, journalInput(rendered)),
		At:   sess.Now(),
	}}
	for i, result := range results {
		wantEntries = append(wantEntries,
			store.ModelTurnEntry{
				Kind: store.ModelTurnAssistant,
				Data: journalJSON(t, modelTurnAssistant{
					ToolCalls: journalToolCalls([]api.PendingToolCall{call}),
					RequestID: result.RequestID,
				}),
				At: sess.Now(),
			},
			store.ModelTurnEntry{
				Kind: store.ModelTurnToolResults,
				Data: journalJSON(t, modelTurnToolResults{Results: toolResults}),
				At:   sess.Now(),
			},
		)
		if i < len(results)-1 {
			wantEntries = append(wantEntries, store.ModelTurnEntry{
				Kind: store.ModelTurnInput,
				Data: journalJSON(t, journalInput(continuedRequest)),
				At:   sess.Now(),
			})
		}
	}
	wantEntries = append(wantEntries, store.ModelTurnEntry{
		Kind: store.ModelTurnOutcome,
		Data: journalJSON(t, modelTurnOutcome{
			ToolTurnCount: maxToolLoopTurns,
			PassReason:    observability.PassReasonToolLoopExhausted,
		}),
		At: sess.Now(),
	})
	require.Equal(t, sequenced(wantEntries), journal.entries)
}

func TestDispatchToInstance_journals_provider_choice_failures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		result      api.CompletionResult
		upstreamErr error
		outcome     modelTurnOutcome
		wantError   error
	}{
		{
			name: "refusal",
			result: api.CompletionResult{
				Refusal: "I cannot do that", ResponseReceived: true,
				RequestID: "request-refused", Usage: api.Usage{TotalTokens: 11},
			},
			upstreamErr: &api.ErrModelRefused{Reason: "I cannot do that"},
			outcome: modelTurnOutcome{
				PassReason: observability.PassReasonModelRefused,
			},
		},
		{
			name: "content filter",
			result: api.CompletionResult{
				ResponseReceived: true, RequestID: "request-filtered",
				Usage: api.Usage{TotalTokens: 15},
			},
			upstreamErr: api.ErrContentFiltered,
			outcome: modelTurnOutcome{
				PassReason: observability.PassReasonContentFiltered,
			},
		},
		{
			name: "length",
			result: api.CompletionResult{
				AssistantText: "partial", ResponseReceived: true,
				RequestID: "request-truncated", Usage: api.Usage{TotalTokens: 25},
			},
			upstreamErr: api.ErrResponseTruncated,
			outcome: modelTurnOutcome{
				Error: api.ErrResponseTruncated.Error(),
			},
			wantError: api.ErrResponseTruncated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sess := newFakeSession()
			journal := &recordingTurnJournal{}
			inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
			upstream := &apitest.Fake{SendEventsFn: func(
				context.Context,
				domain.ModelID,
				domain.InstanceID,
				api.SystemPrompt,
				[]protocol.IRCMessage,
				[]protocol.IRCMessage,
			) (api.CompletionResult, error) {
				return tt.result, tt.upstreamErr
			}}
			mc := New(Config{
				Instance: inst, Session: sess,
				APIClient: func() api.Client { return upstream },
				Tools:     NewToolRegistry(), Journal: journal,
			})
			window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

			err := mc.dispatchToInstance(t.Context(), turnRequest{
				api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
				guard: validWindowGuard{window: window},
			})
			mc.Wait()
			if tt.wantError == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tt.wantError)
			}

			prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
			rendered, renderErr := api.RenderEventRequest(
				"test/model", "inst-botty", prompt, nil, nil,
			)
			require.NoError(t, renderErr)
			require.Equal(t, sequenced([]store.ModelTurnEntry{
				{
					Kind: store.ModelTurnInput,
					Data: journalJSON(t, journalInput(rendered)),
					At:   sess.Now(),
				},
				{
					Kind: store.ModelTurnAssistant,
					Data: journalJSON(t, modelTurnAssistant{
						Text: tt.result.AssistantText, Refusal: tt.result.Refusal,
						RequestID: tt.result.RequestID, Usage: tt.result.Usage,
					}),
					At: sess.Now(),
				},
				{
					Kind: store.ModelTurnOutcome,
					Data: journalJSON(t, tt.outcome),
					At:   sess.Now(),
				},
			}), journal.entries)
		})
	}
}

// TestTurnJournal_writes_use_the_journal_lifecycle covers what bounds
// a journal write. The writer takes no turn context, so a turn that
// has already been cancelled still records how it ended; cancelling
// the journal's own context is what ends the writes.
func TestTurnJournal_writes_use_the_journal_lifecycle(t *testing.T) {
	t.Parallel()

	live := &contextBoundTurnJournal{
		entered: make(chan context.Context, 1),
		release: make(chan struct{}),
	}
	close(live.release)
	liveWriter := testJournalWriter(live)
	liveWriter.assistant(api.CompletionResult{})
	liveWriter.queue.stop()

	journalContext, cancelJournal := context.WithCancel(context.Background())
	cancelJournal()
	cancelled := &contextBoundTurnJournal{
		entered: make(chan context.Context, 1),
		release: make(chan struct{}),
	}
	cancelledWriter := testJournalWriter(cancelled)
	cancelledWriter.queue.writeContext = journalContext
	cancelledWriter.assistant(api.CompletionResult{})
	cancelledWriter.queue.stop()

	_, liveDeadline := (<-live.entered).Deadline()
	type assertionSnapshot struct {
		LiveWriteHasDeadline bool
		LiveErrors           []error
		CancelledErrors      []error
	}

	require.Equal(t, assertionSnapshot{
		LiveWriteHasDeadline: true,
		CancelledErrors:      []error{context.Canceled},
	}, assertionSnapshot{
		LiveWriteHasDeadline: liveDeadline,
		LiveErrors:           live.errors,
		CancelledErrors:      cancelled.errors,
	})
}

// TestTurnJournal_a_refused_write_leaves_the_turn_alone covers the
// journal's separation from the turn's result. The journal is the
// operator's record of provider traffic, so a store that refuses a
// write changes nothing about what the model did or whether the turn
// may be dispatched again. The two refusals differ in what the
// journal does next: a store that failed this write may accept the
// next one, while a turn whose window has closed would refuse every
// later entry, so its remaining entries are dropped.
func TestTurnJournal_a_refused_write_leaves_the_turn_alone(t *testing.T) {
	t.Parallel()

	// turnEntries names the entries this turn produces and the
	// journal accepts, so a case says which of them it expects to
	// find. Sequences 1 and 4 belong to the two assistant entries
	// the journal refuses.
	type turnEntries struct {
		initial     store.ModelTurnEntry
		toolResults store.ModelTurnEntry
		continued   store.ModelTurnEntry
		outcome     store.ModelTurnEntry
	}

	type journalRefusal struct {
		name        string
		beginErr    error
		appendErr   error
		wantEntries func(turnEntries) []store.ModelTurnEntry
	}

	tests := []journalRefusal{
		{
			name:     "admission refused",
			beginErr: errors.New("journal unavailable"),
			wantEntries: func(turnEntries) []store.ModelTurnEntry {
				return nil
			},
		},
		{
			name:      "store unavailable",
			appendErr: errors.New("journal unavailable"),
			wantEntries: func(produced turnEntries) []store.ModelTurnEntry {
				return []store.ModelTurnEntry{
					produced.initial, produced.toolResults,
					produced.continued, produced.outcome,
				}
			},
		},
		{
			name:      "window closed",
			appendErr: store.ErrModelTurnClosed,
			wantEntries: func(produced turnEntries) []store.ModelTurnEntry {
				return []store.ModelTurnEntry{produced.initial}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sess := newFakeSession()
			journal := &recordingTurnJournal{
				beginErr:      tt.beginErr,
				appendErrKind: store.ModelTurnAssistant,
				appendErr:     tt.appendErr,
			}
			continuedRequest := api.RenderedEventRequest{
				Method: "POST", Path: "/chat/completions",
				Body: json.RawMessage(`{"messages":[{"role":"tool"}]}`),
			}
			inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
			upstream := &apitest.Fake{
				SendEventsFn: func(
					context.Context,
					domain.ModelID,
					domain.InstanceID,
					api.SystemPrompt,
					[]protocol.IRCMessage,
					[]protocol.IRCMessage,
				) (api.CompletionResult, error) {
					return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
						ID: "call-1", Name: "effect", Args: json.RawMessage(`{}`),
					}}}, nil
				},
				ContinueWithToolResultsFn: func(
					context.Context,
					*api.Conversation,
					[]api.ToolResult,
				) (api.CompletionResult, error) {
					return api.CompletionResult{}, nil
				},
				RenderToolResultRequestFn: func(
					*api.Conversation,
					[]api.ToolResult,
				) (api.RenderedEventRequest, error) {
					return continuedRequest, nil
				},
			}
			var toolCalls int
			registry := NewToolRegistry(ToolSpec{
				Definition: api.ToolDefinition{
					Name: "effect", Description: "Record an effect.",
					Parameters: map[string]any{"type": "object"},
				},
				Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
					toolCalls++

					return ToolResultPayload{OK: true}, nil
				},
			})
			mc := New(Config{
				Instance: inst, Session: sess,
				APIClient: func() api.Client { return upstream },
				Tools:     registry, Journal: journal,
			})
			window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

			err := mc.dispatchToInstance(t.Context(), turnRequest{
				api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
				guard: validWindowGuard{window: window},
			})
			mc.Wait()

			prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
			rendered, renderErr := api.RenderEventRequest(
				inst.ModelID, inst.ID(), prompt, nil, nil, registry.Definitions()...,
			)
			require.NoError(t, renderErr)
			type assertionSnapshot struct {
				DispatchErr error
				ToolCalls   int
				Entries     []store.ModelTurnEntry
			}

			require.Equal(t, assertionSnapshot{
				ToolCalls: 1,
				Entries: tt.wantEntries(turnEntries{
					initial: store.ModelTurnEntry{
						Kind: store.ModelTurnInput, Seq: 0,
						Data: journalJSON(t, journalInput(rendered)),
						At:   sess.Now(),
					},
					toolResults: store.ModelTurnEntry{
						Kind: store.ModelTurnToolResults, Seq: 2,
						Data: journalJSON(t, modelTurnToolResults{
							Results: []api.ToolResult{{
								ToolCallID: "call-1", Content: `{"ok":true}`,
							}},
						}),
						At: sess.Now(),
					},
					continued: store.ModelTurnEntry{
						Kind: store.ModelTurnInput, Seq: 3,
						Data: journalJSON(t, journalInput(continuedRequest)),
						At:   sess.Now(),
					},
					outcome: store.ModelTurnEntry{
						Kind: store.ModelTurnOutcome, Seq: 5,
						Data: journalJSON(t, modelTurnOutcome{
							ToolTurnCount: 1, PassReason: observability.PassReasonModelPass,
						}),
						At: sess.Now(),
					},
				}),
			}, assertionSnapshot{
				DispatchErr: err,
				ToolCalls:   toolCalls,
				Entries:     journal.entries,
			})
		})
	}
}

// TestTurnJournal_a_turn_does_not_wait_for_the_store covers the
// reason journal writes are queued. Admitting a turn opens a store
// transaction, takes the actor's authority locks and runs the actor's
// retention pass. The turn hands the entry over and makes its
// provider call while all of that is still in flight.
func TestTurnJournal_a_turn_does_not_wait_for_the_store(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		journal := &blockingTurnJournal{release: make(chan struct{})}
		inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
		var upstreamCalls int
		upstream := &apitest.Fake{SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			upstreamCalls++

			return api.CompletionResult{}, nil
		}}
		mc := New(Config{
			Instance: inst, Session: sess,
			APIClient: func() api.Client { return upstream },
			Tools:     NewToolRegistry(), Journal: journal,
		})
		window := testChannelContext(domain.NewChannelWindow("#dev", sess.Now()))

		done := make(chan error, 1)
		go func() {
			done <- mc.dispatchToInstance(t.Context(), turnRequest{
				api: upstream, window: window, target: protocol.ChannelTarget("#dev"),
			})
		}()
		synctest.Wait()

		type turnProgress struct {
			UpstreamCalls  int
			TurnFinished   bool
			JournalEntries []store.ModelTurnEntry
		}
		blockedProgress := turnProgress{
			UpstreamCalls:  upstreamCalls,
			TurnFinished:   len(done) == 1,
			JournalEntries: journal.entries,
		}

		close(journal.release)
		mc.Wait()

		prompt := buildSystemPrompt(window, inst.Nick(), inst.Persona())
		rendered, renderErr := api.RenderEventRequest(
			inst.ModelID, inst.ID(), prompt, nil, nil,
		)
		require.NoError(t, renderErr)
		type assertionSnapshot struct {
			WhileBlocked turnProgress
			DispatchErr  error
			Entries      []store.ModelTurnEntry
		}

		require.Equal(t, assertionSnapshot{
			WhileBlocked: turnProgress{UpstreamCalls: 1, TurnFinished: true},
			Entries: sequenced([]store.ModelTurnEntry{
				{
					Kind: store.ModelTurnInput,
					Data: journalJSON(t, journalInput(rendered)),
					At:   sess.Now(),
				},
				{
					Kind: store.ModelTurnAssistant,
					Data: journalJSON(t, modelTurnAssistant{}),
					At:   sess.Now(),
				},
				{
					Kind: store.ModelTurnOutcome,
					Data: journalJSON(t, modelTurnOutcome{
						PassReason: observability.PassReasonModelPass,
					}),
					At: sess.Now(),
				},
			}),
		}, assertionSnapshot{
			WhileBlocked: blockedProgress,
			DispatchErr:  <-done,
			Entries:      journal.entries,
		})
	})
}
