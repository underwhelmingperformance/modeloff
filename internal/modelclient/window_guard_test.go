package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

type blockingMemoryStore struct {
	started chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (s *blockingMemoryStore) Read(context.Context, domain.InstanceID) ([]memory.Entry, error) {
	s.once.Do(func() { close(s.started) })
	<-s.proceed
	return nil, nil
}

func (*blockingMemoryStore) Write(context.Context, domain.InstanceID, memory.Entry) error {
	return nil
}

func (*blockingMemoryStore) Delete(context.Context, domain.InstanceID, string) error {
	return nil
}

func (*blockingMemoryStore) Reset(context.Context) error { return nil }

type revocableWindowGuard struct{ valid atomic.Bool }

func (g *revocableWindowGuard) Valid(context.Context) bool { return g.valid.Load() }
func (g *revocableWindowGuard) RunWithAuthority(_ context.Context, operation func() error) error {
	if !g.valid.Load() {
		return protocol.ErrWindowAuthorityChanged
	}

	return operation()
}
func (*revocableWindowGuard) Context(context.Context) (protocol.WindowContext, error) {
	return nil, nil
}
func (g *revocableWindowGuard) Send(
	ctx context.Context,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	if !g.valid.Load() {
		return protocol.Response{}, protocol.ErrWindowAuthorityChanged
	}

	return client.Send(ctx, cmd)
}

type expiringWindowGuard struct{ valid atomic.Bool }

func (g *expiringWindowGuard) Valid(context.Context) bool {
	return g.valid.Swap(false)
}
func (g *expiringWindowGuard) RunWithAuthority(_ context.Context, operation func() error) error {
	if !g.valid.Load() {
		return protocol.ErrWindowAuthorityChanged
	}

	return operation()
}
func (*expiringWindowGuard) Context(context.Context) (protocol.WindowContext, error) {
	return nil, nil
}
func (g *expiringWindowGuard) Send(
	ctx context.Context,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	if !g.valid.Load() {
		return protocol.Response{}, protocol.ErrWindowAuthorityChanged
	}

	return client.Send(ctx, cmd)
}

type commandRecordingClient struct {
	protocol.Client

	commands []protocol.Command
}

func (*commandRecordingClient) Identity() protocol.ClientID { return "inst-botty" }
func (c *commandRecordingClient) Send(
	_ context.Context,
	cmd protocol.Command,
) (protocol.Response, error) {
	c.commands = append(c.commands, cmd)

	return protocol.Response{}, nil
}

type lockingWindowGuard struct {
	mu    sync.Mutex
	valid bool
}

func (g *lockingWindowGuard) Valid(context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.valid
}

func (g *lockingWindowGuard) RunWithAuthority(_ context.Context, operation func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.valid {
		return protocol.ErrWindowAuthorityChanged
	}

	return operation()
}

func (g *lockingWindowGuard) Send(
	ctx context.Context,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	g.mu.Lock()
	valid := g.valid
	g.mu.Unlock()

	if !valid {
		return protocol.Response{}, protocol.ErrWindowAuthorityChanged
	}

	return client.Send(ctx, cmd)
}

func (*lockingWindowGuard) Context(context.Context) (protocol.WindowContext, error) {
	return nil, nil
}

type blockingMemoryExecutor struct {
	phase   blockingMemoryPhase
	started chan struct{}
}

type blockingMemoryPhase uint8

const (
	blockWritePreparation blockingMemoryPhase = iota
	blockDeleteFinish
	blockSearch
)

type blockingMemoryEffect struct {
	started chan struct{}
}

func (*blockingMemoryEffect) Commit(context.Context) error { return nil }

func (e *blockingMemoryEffect) Finish(ctx context.Context) {
	close(e.started)
	<-ctx.Done()
}

func (m *blockingMemoryExecutor) PrepareWriteMemory(
	ctx context.Context,
	_, _ string,
	_ bool,
) (memory.PreparedMutation, error) {
	if m.phase != blockWritePreparation {
		return memoryEffectFunc(func(context.Context) error { return nil }), nil
	}

	close(m.started)
	<-ctx.Done()

	return nil, ctx.Err()
}

func (m *blockingMemoryExecutor) PrepareDeleteMemory(
	context.Context,
	string,
) (memory.PreparedMutation, error) {
	if m.phase == blockDeleteFinish {
		return &blockingMemoryEffect{started: m.started}, nil
	}

	return memoryEffectFunc(func(context.Context) error { return nil }), nil
}

func (m *blockingMemoryExecutor) SearchMemory(
	ctx context.Context,
	_ string,
	_ int,
) ([]memory.SearchResult, error) {
	if m.phase == blockSearch {
		close(m.started)
		<-ctx.Done()

		return nil, ctx.Err()
	}

	return nil, nil
}

func TestToolContext_Send_rechecks_window_authority_at_command_submission(t *testing.T) {
	guard := &expiringWindowGuard{}
	guard.valid.Store(true)
	client := &commandRecordingClient{}
	toolCtx := NewToolContext(guard, nil, client, nil)

	initiallyValid := guard.Valid(t.Context())
	response, err := toolCtx.Send(t.Context(), protocol.Nick{New: "renamed"})

	type assertionSnapshot struct {
		InitiallyValid bool
		Response       protocol.Response
		WindowClosed   bool
		Commands       []protocol.Command
	}

	require.Equal(t, assertionSnapshot{
		InitiallyValid: true,
		WindowClosed:   true,
	}, assertionSnapshot{
		InitiallyValid: initiallyValid,
		Response:       response,
		WindowClosed:   errors.Is(err, errDispatchWindowClosed),
		Commands:       client.commands,
	})
}

func TestExecuteTools_rechecks_window_authority_before_accepting_memory_work(t *testing.T) {
	tests := []struct {
		name       string
		call       api.PendingToolCall
		withSearch bool
	}{
		{
			name: "write",
			call: api.PendingToolCall{
				ID: "write-1", Name: "write_memory",
				Args: json.RawMessage(`{"key":"decision","content":"ship it","pinned":false}`),
			},
		},
		{
			name: "delete",
			call: api.PendingToolCall{
				ID: "delete-1", Name: "delete_memory",
				Args: json.RawMessage(`{"key":"decision"}`),
			},
		},
		{
			name: "search",
			call: api.PendingToolCall{
				ID: "search-1", Name: "search_memory",
				Args: json.RawMessage(`{"query":"decision","limit":5}`),
			},
			withSearch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guard := &expiringWindowGuard{}
			guard.valid.Store(true)
			memories := newFakeMemoryExecutor()

			outcome, executeErr := executeTools(
				t.Context(), newFakeSession(), NewToolContext(guard, nil, nil, nil),
				memoryToolRegistry(memories, tt.withSearch),
				[]api.PendingToolCall{tt.call}, nil,
			)

			type assertionSnapshot struct {
				Outcome      toolBatchOutcome
				WindowClosed bool
				Written      map[string]memory.Entry
				Deleted      []string
			}

			require.Equal(t, assertionSnapshot{
				Outcome: toolBatchOutcome{
					results:  []api.ToolResult{},
					executed: true,
				},
				WindowClosed: true,
				Written:      map[string]memory.Entry{},
			}, assertionSnapshot{
				Outcome:      outcome,
				WindowClosed: errors.Is(executeErr, errDispatchWindowClosed),
				Written:      memories.written,
				Deleted:      memories.deleted,
			})
		})
	}
}

func TestExecuteTools_does_not_hold_window_authority_during_slow_memory_work(t *testing.T) {
	tests := []struct {
		name          string
		phase         blockingMemoryPhase
		call          api.PendingToolCall
		withSearch    bool
		wantCancelled bool
	}{
		{
			name:  "write preparation",
			phase: blockWritePreparation,
			call: api.PendingToolCall{
				ID: "write-1", Name: "write_memory",
				Args: json.RawMessage(`{"key":"decision","content":"ship it","pinned":false}`),
			},
			wantCancelled: true,
		},
		{
			name:  "delete index cleanup",
			phase: blockDeleteFinish,
			call: api.PendingToolCall{
				ID: "delete-1", Name: "delete_memory",
				Args: json.RawMessage(`{"key":"decision"}`),
			},
		},
		{
			name:  "search",
			phase: blockSearch,
			call: api.PendingToolCall{
				ID: "search-1", Name: "search_memory",
				Args: json.RawMessage(`{"query":"decision","limit":5}`),
			},
			withSearch:    true,
			wantCancelled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guard := &lockingWindowGuard{valid: true}
			memories := &blockingMemoryExecutor{
				phase:   tt.phase,
				started: make(chan struct{}),
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)

			type executionResult struct {
				outcome toolBatchOutcome
				err     error
			}
			executed := make(chan executionResult, 1)
			go func() {
				outcome, executeErr := executeTools(
					ctx, newFakeSession(), NewToolContext(guard, nil, nil, nil),
					memoryToolRegistry(memories, tt.withSearch),
					[]api.PendingToolCall{tt.call}, nil,
				)
				executed <- executionResult{outcome: outcome, err: executeErr}
			}()

			<-memories.started
			authorityUnlocked := guard.mu.TryLock()
			if authorityUnlocked {
				guard.mu.Unlock()
			}
			cancel()
			result := <-executed

			type assertionSnapshot struct {
				AuthorityUnlocked bool
				Executed          bool
				Cancelled         bool
			}

			require.Equal(t, assertionSnapshot{
				AuthorityUnlocked: true,
				Executed:          true,
				Cancelled:         tt.wantCancelled,
			}, assertionSnapshot{
				AuthorityUnlocked: authorityUnlocked,
				Executed:          result.outcome.executed,
				Cancelled:         errors.Is(result.err, context.Canceled),
			})
		})
	}
}

func TestDispatch_rechecks_window_authority_after_prompt_preparation(t *testing.T) {
	sess := newFakeSession()
	upstream := &countingAPI{}
	memories := &blockingMemoryStore{started: make(chan struct{}), proceed: make(chan struct{})}
	guard := &revocableWindowGuard{}
	guard.valid.Store(true)
	journal := &recordingTurnJournal{}

	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	inst.JoinChannel("#dev", time.Time{})
	window := domain.NewChannelWindow("#dev", time.Time{})
	window.Members.Add(inst)

	mc := New(Config{
		Instance:        inst,
		Attachment:      protocol.NewAttachment(),
		Session:         sess,
		APIClient:       func() api.Client { return upstream },
		Memory:          memories,
		Tools:           NewToolRegistry(),
		Journal:         journal,
		LifetimeContext: context.Background,
	})

	result := make(chan error, 1)
	go func() {
		result <- mc.dispatchToInstance(t.Context(), turnRequest{
			api:    upstream,
			window: testChannelContext(window),
			target: protocol.ChannelTarget("#dev"),
			guard:  guard,
		})
	}()

	<-memories.started
	guard.valid.Store(false)
	close(memories.proceed)

	require.ErrorIs(t, <-result, errDispatchWindowClosed)
	mc.Wait()
	type assertionSnapshot struct {
		UpstreamCalls int
		Journal       []store.ModelTurnEntry
	}

	require.Equal(t, assertionSnapshot{}, assertionSnapshot{
		UpstreamCalls: upstream.callCount(),
		Journal:       journal.entries,
	})
}

func TestRunTurn_rechecks_window_authority_before_tools(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			close(started)
			<-proceed

			return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
				ID: "effect-1", Name: "effect", Args: json.RawMessage(`{}`),
			}}}, nil
		},
	}

	var effects atomic.Int64
	tools := NewToolRegistry(ToolSpec{
		Definition: api.ToolDefinition{Name: "effect"},
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			effects.Add(1)
			return ToolResultPayload{OK: true}, nil
		},
	})

	guard := &revocableWindowGuard{}
	guard.valid.Store(true)
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	result := make(chan error, 1)

	go func() {
		_, err := runTurn(t.Context(), runTurnRequest{
			apiClient: upstream,
			session:   newFakeSession(),
			instance:  inst,
			target:    protocol.ChannelTarget("#dev"),
			registry:  tools,
			guard:     guard,
		})
		result <- err
	}()

	<-started
	guard.valid.Store(false)
	close(proceed)

	require.ErrorIs(t, <-result, errDispatchWindowClosed)
	require.Equal(t, int64(0), effects.Load())
}

func TestRunTurn_upstream_failure_after_revocation_is_window_closed(t *testing.T) {
	started := make(chan struct{})
	proceed := make(chan struct{})
	upstreamFailure := errors.New("upstream failed")
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			close(started)
			<-proceed

			return api.CompletionResult{}, upstreamFailure
		},
	}

	guard := &revocableWindowGuard{}
	guard.valid.Store(true)
	result := make(chan error, 1)

	go func() {
		_, err := runTurn(t.Context(), runTurnRequest{
			apiClient: upstream,
			session:   newFakeSession(),
			instance: domain.NewModelInstance(
				"inst-botty", "botty", "test/model", "", nil,
			),
			target:   protocol.ChannelTarget("#dev"),
			registry: NewToolRegistry(),
			guard:    guard,
		})
		result <- err
	}()

	<-started
	guard.valid.Store(false)
	close(proceed)

	require.ErrorIs(t, <-result, errDispatchWindowClosed)
}

func TestRunTurn_stops_journalling_after_a_tool_closes_the_window(t *testing.T) {
	call := api.PendingToolCall{
		ID: "part-1", Name: "part", Args: json.RawMessage(`{}`),
	}
	upstream := &apitest.Fake{SendEventsFn: func(
		context.Context,
		domain.ModelID,
		domain.InstanceID,
		api.SystemPrompt,
		[]protocol.IRCMessage,
		[]protocol.IRCMessage,
	) (api.CompletionResult, error) {
		return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{call}}, nil
	}}
	guard := &revocableWindowGuard{}
	guard.valid.Store(true)
	var effects atomic.Int64
	tools := NewToolRegistry(ToolSpec{
		Definition: api.ToolDefinition{Name: "part"},
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			effects.Add(1)
			guard.valid.Store(false)

			return ToolResultPayload{OK: true}, nil
		},
	})
	recording := &recordingTurnJournal{}
	journal := testJournalWriter(recording)

	_, err := runTurn(t.Context(), runTurnRequest{
		apiClient: upstream,
		session:   newFakeSession(),
		instance:  domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
		target:    protocol.ChannelTarget("#dev"),
		registry:  tools,
		guard:     guard,
		journal:   journal,
	})
	require.ErrorIs(t, err, errDispatchWindowClosed)
	journal.queue.stop()
	type assertionSnapshot struct {
		Effects int64
		Entries []store.ModelTurnEntry
	}

	require.Equal(t, assertionSnapshot{
		Effects: 1,
		Entries: sequenced([]store.ModelTurnEntry{
			{
				Kind: store.ModelTurnAssistant,
				Data: journalJSON(t, modelTurnAssistant{
					ToolCalls: journalToolCalls([]api.PendingToolCall{call}),
				}),
			},
			{
				Kind: store.ModelTurnToolResults,
				Data: journalJSON(t, modelTurnToolResults{Results: []api.ToolResult{{
					ToolCallID: "part-1", Content: `{"ok":true}`,
				}}}),
			},
		}),
	}, assertionSnapshot{
		Effects: effects.Load(),
		Entries: recording.entries,
	})
}

func TestRunTurn_sends_a_continuation_recorded_before_authority_changes(t *testing.T) {
	call := api.PendingToolCall{
		ID: "effect-1", Name: "effect", Args: json.RawMessage(`{}`),
	}
	continuedRequest := api.RenderedEventRequest{
		Method: "POST", Path: "/chat/completions",
		Body: json.RawMessage(`{"messages":[{"role":"tool","content":"{\"ok\":true}"}]}`),
	}
	guard := &revocableWindowGuard{}
	guard.valid.Store(true)
	revokeAfterContinuation := func() { guard.valid.Store(false) }
	var continuations [][]api.ToolResult
	upstream := &apitest.Fake{
		SendEventsFn: func(
			context.Context,
			domain.ModelID,
			domain.InstanceID,
			api.SystemPrompt,
			[]protocol.IRCMessage,
			[]protocol.IRCMessage,
		) (api.CompletionResult, error) {
			return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{call}}, nil
		},
		RenderToolResultRequestFn: func(
			*api.Conversation,
			[]api.ToolResult,
		) (api.RenderedEventRequest, error) {
			return continuedRequest, nil
		},
		ContinueWithToolResultsFn: func(
			_ context.Context,
			_ *api.Conversation,
			results []api.ToolResult,
		) (api.CompletionResult, error) {
			continuations = append(continuations, results)
			revokeAfterContinuation()

			return api.CompletionResult{}, nil
		},
	}
	var toolEffects []string
	tools := NewToolRegistry(ToolSpec{
		Definition: api.ToolDefinition{Name: "effect"},
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			toolEffects = append(toolEffects, "effect")
			return ToolResultPayload{OK: true}, nil
		},
	})
	recorder := &recordingTurnJournal{}
	journal := testJournalWriter(recorder)

	_, err := runTurn(t.Context(), runTurnRequest{
		apiClient: upstream,
		session:   newFakeSession(),
		instance:  domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
		target:    protocol.ChannelTarget("#dev"),
		registry:  tools,
		guard:     guard,
		journal:   journal,
	})

	toolResults := []api.ToolResult{{ToolCallID: "effect-1", Content: `{"ok":true}`}}
	require.ErrorIs(t, err, errDispatchWindowClosed)
	journal.queue.stop()
	type assertionSnapshot struct {
		ToolEffects   []string
		Continuations [][]api.ToolResult
		Entries       []store.ModelTurnEntry
	}

	require.Equal(t, assertionSnapshot{
		ToolEffects: []string{"effect"},
		Continuations: [][]api.ToolResult{
			toolResults,
		},
		Entries: sequenced([]store.ModelTurnEntry{
			{
				Kind: store.ModelTurnAssistant,
				Data: journalJSON(t, modelTurnAssistant{
					ToolCalls: journalToolCalls([]api.PendingToolCall{call}),
				}),
			},
			{
				Kind: store.ModelTurnToolResults,
				Data: journalJSON(t, modelTurnToolResults{Results: toolResults}),
			},
			{
				Kind: store.ModelTurnInput,
				Data: journalJSON(t, journalInput(continuedRequest)),
			},
			{
				Kind: store.ModelTurnAssistant,
				Data: journalJSON(t, modelTurnAssistant{}),
			},
		}),
	}, assertionSnapshot{
		ToolEffects:   toolEffects,
		Continuations: continuations,
		Entries:       recorder.entries,
	})
}
