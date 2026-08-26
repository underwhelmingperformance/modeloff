package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// turnPrompt is what one `SendEvents` call was given: the transcript
// the model reads against, and the traffic it is being asked about.
// A test asserts on `triggers` to pin what the model is told is new.
type turnPrompt struct {
	history  []string
	triggers []string
}

// countingAPI is an [api.Client] that records every `SendEvents` call
// and answers each one from `errs`, so a test can fail the first
// attempt and let the next succeed. A call past the end of `errs`
// succeeds with silence. The embedded [apitest.Fake] answers every
// other method with its ordinary defaults; nothing in this file
// exercises them.
type countingAPI struct {
	apitest.Fake

	mu    sync.Mutex
	calls []turnPrompt
	errs  []error
}

func (c *countingAPI) SendEvents(
	_ context.Context,
	_ domain.ModelID,
	_ domain.InstanceID,
	_ api.SystemPrompt,
	history api.TurnHistory,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, turnPrompt{history: bodies(history.Messages()), triggers: bodies(events)})

	if len(c.calls) <= len(c.errs) {
		return api.CompletionResult{}, c.errs[len(c.calls)-1]
	}

	return api.CompletionResult{}, nil
}

// bodies reduces wire messages to their text, which is all the
// dispatch tests distinguish them by.
func bodies(msgs []protocol.IRCMessage) []string {
	if len(msgs) == 0 {
		return nil
	}

	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Body
	}

	return out
}

func (c *countingAPI) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.calls)
}

// prompts returns the turns the client has asked for so far.
func (c *countingAPI) prompts() []turnPrompt {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]turnPrompt(nil), c.calls...)
}

// upstreamError builds the `*openai.Error` shape the SDK surfaces for
// a non-2xx chat completion, wrapped the way the api package wraps it.
func upstreamError(t *testing.T, status int) error {
	return upstreamErrorWithRetryAfter(t, status, "")
}

func upstreamErrorWithRetryAfter(t *testing.T, status int, retryAfter string) error {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
	require.NoError(t, err)
	headers := make(http.Header)
	if retryAfter != "" {
		headers.Set("Retry-After", retryAfter)
	}

	return fmt.Errorf("chat completion: %w", &openai.Error{
		StatusCode: status,
		Request:    req,
		Response:   &http.Response{StatusCode: status, Header: headers},
	})
}

type retryWaitRequest struct {
	delay   time.Duration
	release chan struct{}
}

type recordingRetryWaiter struct {
	waits chan retryWaitRequest
}

func newRecordingRetryWaiter() *recordingRetryWaiter {
	return &recordingRetryWaiter{waits: make(chan retryWaitRequest)}
}

func (w *recordingRetryWaiter) Wait(ctx context.Context, delay time.Duration) error {
	wait := retryWaitRequest{delay: delay, release: make(chan struct{})}

	select {
	case w.waits <- wait:
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case <-wait.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryTestDelay is the flat wait the dispatch tests run the retry
// policy at. Jitter is off so the test asserts on when the second
// attempt lands, not on a range.
const retryTestDelay = 3 * time.Second

// newRetryTestClient attaches a model-client whose turns go to `upstream`
// and whose retry waits a flat [retryTestDelay].
func newRetryTestClient(t *testing.T, sess *fakeSession, upstream api.Client) *ModelClient {
	t.Helper()

	return newRetryTestClientWithTools(t, sess, upstream, nil)
}

func newRetryTestClientWithTools(
	t *testing.T,
	sess *fakeSession,
	upstream api.Client,
	tools *ToolRegistry,
) *ModelClient {
	t.Helper()

	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	mc := New(Config{
		Instance:        inst,
		Attachment:      protocol.NewAttachment(),
		Session:         sess,
		APIClient:       func() api.Client { return upstream },
		Tools:           tools,
		LifetimeContext: context.Background,
	})
	mc.retry = retryPolicy{Delay: retryTestDelay}

	require.NoError(t, mc.Attach(t.Context()))

	return mc
}

// TestDispatch_does_not_replay_a_turn_after_a_tool_executed pins the
// boundary between an upstream request that is safe to repeat and a
// tool-calling turn that has already changed local state. A transient
// continuation failure happens after the tool ran, so redispatching
// the original batch would execute the same tool twice.
func TestDispatch_does_not_replay_a_turn_after_a_tool_executed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			turns   int
			effects int
		)

		tools := NewToolRegistry(ToolSpec{
			Definition: api.ToolDefinition{Name: "effect"},
			Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
				effects++

				return ToolResultPayload{OK: true, Summary: "effect applied"}, nil
			},
		})
		upstream := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				turns++

				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
					ID:   "effect-1",
					Name: "effect",
					Args: json.RawMessage(`{}`),
				}}}, nil
			},
			ContinueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
				return api.CompletionResult{}, upstreamError(t, http.StatusServiceUnavailable)
			},
		}

		sess := newFakeSession()
		mc := newRetryTestClientWithTools(t, sess, upstream, tools)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("apply it")

		synctest.Wait()
		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, struct {
			turns   int
			effects int
		}{
			turns:   1,
			effects: 1,
		}, struct {
			turns   int
			effects int
		}{
			turns:   turns,
			effects: effects,
		})
	})
}

func TestDispatch_retries_a_continuation_in_place(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type upstreamAttempt struct {
			Kind   string
			Values []string
		}
		var attempts []upstreamAttempt
		var effects []string
		tools := NewToolRegistry(ToolSpec{
			Definition: api.ToolDefinition{Name: "effect"},
			Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
				effects = append(effects, "effect")

				return ToolResultPayload{OK: true, Summary: "effect applied"}, nil
			},
		})

		upstream := &apitest.Fake{
			SendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ api.SystemPrompt, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				attempts = append(attempts, upstreamAttempt{Kind: "initial", Values: bodies(events)})

				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
					ID:   "effect-1",
					Name: "effect",
					Args: json.RawMessage(`{}`),
				}}}, nil
			},
			ContinueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
				attempts = append(attempts, upstreamAttempt{
					Kind: "continuation",
					Values: []string{
						results[0].ToolCallID,
						results[0].Content,
					},
				})
				if len(attempts) == 2 {
					return api.CompletionResult{}, upstreamError(t, http.StatusServiceUnavailable)
				}

				return api.CompletionResult{}, nil
			},
		}

		sess := newFakeSession()
		mc := newRetryTestClientWithTools(t, sess, upstream, tools)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("try it")

		synctest.Wait()
		time.Sleep(2 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, struct {
			Attempts []upstreamAttempt
			Effects  []string
		}{
			Attempts: []upstreamAttempt{
				{Kind: "initial", Values: []string{"try it"}},
				{
					Kind: "continuation",
					Values: []string{
						"effect-1",
						`{"ok":true,"summary":"effect applied"}`,
					},
				},
				{
					Kind: "continuation",
					Values: []string{
						"effect-1",
						`{"ok":true,"summary":"effect applied"}`,
					},
				},
			},
			Effects: []string{"effect"},
		}, struct {
			Attempts []upstreamAttempt
			Effects  []string
		}{
			Attempts: attempts,
			Effects:  effects,
		})
	})
}

// TestDispatch_retries_a_turn_whose_outcome_journal_write_failed
// covers the journal's separation from the turn's result. A turn lost
// to a transient upstream failure is dispatched once more, and a
// journal that refuses the outcome entry does not take that retry
// away. The two refusals are the ones a live turn meets: the actor
// left the window the turn runs in, and the write did not finish in
// time.
func TestDispatch_retries_a_turn_whose_outcome_journal_write_failed(t *testing.T) {
	type outcomeRefusal struct {
		name      string
		appendErr error
	}

	tests := []outcomeRefusal{
		{name: "window closed", appendErr: store.ErrModelTurnClosed},
		{name: "write timed out", appendErr: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess := newFakeSession()
				upstream := &countingAPI{errs: []error{
					upstreamError(t, http.StatusServiceUnavailable),
				}}
				journal := &recordingTurnJournal{
					appendErrKind: store.ModelTurnOutcome,
					appendErr:     tt.appendErr,
				}
				inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
				mc := New(Config{
					Instance: inst, Attachment: protocol.NewAttachment(), Session: sess,
					APIClient: func() api.Client { return upstream },
					Journal:   journal, LifetimeContext: context.Background,
				})
				mc.retry = retryPolicy{Delay: retryTestDelay}
				require.NoError(t, mc.Attach(t.Context()))
				t.Cleanup(mc.Detach)

				sess.sub.events <- channelMessage("dispatch me")
				synctest.Wait()
				time.Sleep(4 * retryTestDelay)
				synctest.Wait()

				require.Equal(t, []turnPrompt{
					{triggers: []string{"dispatch me"}},
					{triggers: []string{"dispatch me"}},
				}, upstream.prompts())
			})
		})
	}
}

func TestDispatch_does_not_replay_a_tool_execution_failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var (
			turns   int
			effects int
		)

		tools := NewToolRegistry(ToolSpec{
			Definition: api.ToolDefinition{Name: "effect"},
			Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
				effects++

				return ToolResultPayload{}, &ToolExecutionError{Tool: "effect", Err: context.DeadlineExceeded}
			},
		})
		upstream := &apitest.Fake{
			SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				turns++

				return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{{
					ID:   "effect-1",
					Name: "effect",
					Args: json.RawMessage(`{}`),
				}}}, nil
			},
		}

		sess := newFakeSession()
		mc := newRetryTestClientWithTools(t, sess, upstream, tools)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("apply it")

		synctest.Wait()
		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, struct {
			turns   int
			effects int
		}{
			turns:   1,
			effects: 1,
		}, struct {
			turns   int
			effects int
		}{
			turns:   turns,
			effects: effects,
		})
	})
}

// channelMessage is the delivery that raises a turn in `#dev`.
func channelMessage(body string) protocol.Delivery {
	return messageIn("#dev", body)
}

// messageIn is the delivery that raises a turn in the named window.
func messageIn(channel domain.ChannelName, body string) protocol.Delivery {
	return protocol.Delivery{Event: domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"), Target: channel,
		Body: body, At: time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
	}}
}

func TestDispatch_does_not_repeat_a_first_DM_loaded_from_scrollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		message := domain.Message{
			Source: domain.ClientSource("", "testuser"), Target: "inst-botty",
			Body: "hello", At: time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
		}
		sess := newFakeSession()
		sess.instances = map[domain.InstanceID]*domain.Instance{
			"": domain.NewUserInstance("testuser"),
		}
		upstream := &countingAPI{}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- protocol.Delivery{Event: message}
		synctest.Wait()
		second := message
		second.Body = "again"
		second.At = second.At.Add(time.Second)
		sess.sub.events <- protocol.Delivery{Event: second}
		synctest.Wait()

		require.Equal(t, []turnPrompt{
			{triggers: []string{"hello"}},
			{history: []string{"hello"}, triggers: []string{"again"}},
		}, upstream.prompts())
	})
}

// TestDispatch_retries_a_turn_lost_to_a_transient_failure pins the
// single re-dispatch in quiet conditions: nothing else arrives for
// the window, so the delay runs out and the same triggers are asked
// about again. Without it a 429 loses the turn, and the model never
// answers a message the channel has seen, with nothing but a span to
// say why.
func TestDispatch_retries_a_turn_lost_to_a_transient_failure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusTooManyRequests)}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anyone about?")

		synctest.Wait()
		require.Equal(t, 1, upstream.callCount(), "the first attempt runs at once")

		// The scheduler's timer is the only thing left to run, so the
		// bubble's clock advances to it.
		time.Sleep(retryTestDelay)
		synctest.Wait()

		require.Equal(t, []turnPrompt{
			{triggers: []string{"anyone about?"}},
			{triggers: []string{"anyone about?"}},
		}, upstream.prompts(),
			"the re-dispatch asks about the traffic the failed turn never answered")
	})
}

func TestDispatch_honours_retry_after_before_replaying_an_initial_request(t *testing.T) {
	t.Parallel()

	attempts := make(chan turnPrompt)
	var attempt int
	upstream := &apitest.Fake{SendEventsFn: func(
		_ context.Context,
		_ domain.ModelID,
		_ domain.InstanceID,
		_ api.SystemPrompt,
		history []protocol.IRCMessage,
		events []protocol.IRCMessage,
	) (api.CompletionResult, error) {
		attempt++
		attempts <- turnPrompt{history: bodies(history), triggers: bodies(events)}
		if attempt == 1 {
			return api.CompletionResult{}, upstreamErrorWithRetryAfter(
				t, http.StatusTooManyRequests, "10",
			)
		}

		return api.CompletionResult{}, nil
	}}
	waiter := newRecordingRetryWaiter()
	sess := newFakeSession()
	mc := newRetryTestClient(t, sess, upstream)
	mc.retry = retryPolicy{Delay: retryTestDelay, Waiter: waiter}
	t.Cleanup(mc.Detach)

	sess.sub.events <- channelMessage("anyone about?")
	first := <-attempts
	wait := <-waiter.waits

	select {
	case unexpected := <-attempts:
		t.Fatalf("request repeated before retry delay was released: %#v", unexpected)
	default:
	}
	type assertionSnapshot struct {
		Prompt turnPrompt
		Delay  time.Duration
	}

	require.Equal(t, assertionSnapshot{
		Prompt: turnPrompt{triggers: []string{"anyone about?"}},
		Delay:  10 * time.Second,
	}, assertionSnapshot{
		Prompt: first,
		Delay:  wait.delay,
	})

	close(wait.release)
	second := <-attempts

	require.Equal(t, turnPrompt{triggers: []string{"anyone about?"}}, second)
}

// TestDispatch_retries_a_failing_turn_only_once pins the bound. An
// upstream that answers the same way twice is not having a moment,
// and a loop here would spend the user's credits on a conversation
// that has moved on.
func TestDispatch_retries_a_failing_turn_only_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{
			upstreamError(t, http.StatusBadGateway),
			upstreamError(t, http.StatusBadGateway),
			upstreamError(t, http.StatusBadGateway),
		}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anyone about?")

		synctest.Wait()
		time.Sleep(retryTestDelay)
		synctest.Wait()

		// Well past a second delay, in case another one were pending.
		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, 2, upstream.callCount())
	})
}

// TestDispatch_does_not_retry_a_refusal covers the other side of the
// classification: a status the upstream decided about this request
// would answer the same way next time, so the turn stays failed.
func TestDispatch_does_not_retry_a_refusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusBadRequest)}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anyone about?")

		synctest.Wait()
		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, 1, upstream.callCount())
	})
}

// TestDispatch_keeps_draining_while_a_retry_is_pending pins the rule
// the whole design turns on: the delay runs on a goroutine of its own,
// so the dispatch loop stays on its select. A message arriving during
// the wait takes its turn straight away, without queueing behind the
// pending re-dispatch.
//
// The new message is in a different window, so it has no bearing on
// the pending turn. `#dev`'s re-dispatch still lands on time.
func TestDispatch_keeps_draining_while_a_retry_is_pending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusTooManyRequests)}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anyone about?")

		synctest.Wait()
		require.Equal(t, 1, upstream.callCount())

		// Still inside the retry delay.
		time.Sleep(retryTestDelay / 3)

		sess.sub.events <- messageIn("#ops", "unrelated window")
		synctest.Wait()

		require.Equal(t, 2, upstream.callCount(), "the new message did not wait for the pending retry")

		time.Sleep(retryTestDelay)
		synctest.Wait()

		require.Equal(t, 3, upstream.callCount(), "the retry still lands after its delay")
	})
}

// TestDispatch_same_window_replacement_inherits_the_pending_retry_delay
// checks the chronology and delay when new traffic replaces a failed
// batch.
//
// `fileBatch` files every delivery into the window's ring as it walks
// the burst, so a failed turn's triggers are transcript from that
// moment on. The replacement keeps the failed traffic in chronological
// history and uses only the new traffic as explicit triggers. It
// still waits out the provider's Retry-After before reaching the wire.
func TestDispatch_same_window_replacement_inherits_the_pending_retry_delay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamErrorWithRetryAfter(
			t, http.StatusTooManyRequests, "10",
		)}}
		waiter := newRecordingRetryWaiter()

		mc := newRetryTestClient(t, sess, upstream)
		mc.retry = retryPolicy{Delay: retryTestDelay, Waiter: waiter}
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anyone about?")
		wait := <-waiter.waits

		sess.sub.events <- channelMessage("still here?")
		synctest.Wait()
		require.Equal(t, []turnPrompt{
			{triggers: []string{"anyone about?"}},
		}, upstream.prompts())

		close(wait.release)
		synctest.Wait()

		type assertionSnapshot struct {
			Delay   time.Duration
			Prompts []turnPrompt
		}

		require.Equal(t, assertionSnapshot{
			Delay: 10 * time.Second,
			Prompts: []turnPrompt{
				{triggers: []string{"anyone about?"}},
				{history: []string{"anyone about?"}, triggers: []string{"still here?"}},
			},
		}, assertionSnapshot{
			Delay:   wait.delay,
			Prompts: upstream.prompts(),
		})
	})
}

func TestRedispatchMerge_keeps_failed_traffic_in_chronological_history(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	failed := domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"), Target: "#dev",
		Body: "failed", At: at,
	}
	current := domain.Message{
		Source: domain.ClientSource("inst-bob", "bob"), Target: "#dev",
		Body: "current", At: at.Add(time.Second),
	}
	failedIRC, _ := protocol.FromChannelEvent(failed)
	currentIRC, _ := protocol.FromChannelEvent(current)
	failedCause := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1},
	})
	currentCause := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{2}, SpanID: trace.SpanID{2},
	})

	pending := redispatchSet{}
	token := &turnBatch{
		channel: "#dev", events: []protocol.IRCMessage{failedIRC},
		triggers: []protocol.IRCMessage{failedIRC},
		causes:   []trace.SpanContext{failedCause}, retried: true,
	}
	pending.hold(token)
	queued := pending.merge(&turnBatch{
		channel: "#dev", history: []domain.StoredEvent{{Event: failed}},
		events: []protocol.IRCMessage{currentIRC}, triggers: []protocol.IRCMessage{currentIRC},
		causes: []trace.SpanContext{currentCause},
	})
	merged, claimed := pending.claim(token)

	type assertionSnapshot struct {
		Queued  *turnBatch
		Claimed bool
		Batch   turnBatch
	}

	require.Equal(t, assertionSnapshot{
		Claimed: true,
		Batch: turnBatch{
			channel: "#dev", history: []domain.StoredEvent{{Event: failed}},
			events: []protocol.IRCMessage{currentIRC}, triggers: []protocol.IRCMessage{currentIRC},
			causes:  []trace.SpanContext{currentCause},
			retried: true,
		},
	}, assertionSnapshot{
		Queued:  queued,
		Claimed: claimed,
		Batch:   *merged,
	})
}

func TestDispatch_DM_history_failure_never_calls_provider_with_incomplete_context(t *testing.T) {
	tests := []struct {
		name      string
		failReply bool
	}{
		{name: "scrollback read fails"},
		{name: "reply read fails", failReply: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sentinel := fmt.Errorf("history unavailable: %w", context.DeadlineExceeded)
				var unavailable atomic.Bool
				unavailable.Store(true)

				earlier := inboundDM("earlier", dmAt.Add(-time.Minute))
				first := inboundDM("first", dmAt)
				second := inboundDM("second", dmAt.Add(time.Second))
				sess := newFakeSession()
				sess.instances = map[domain.InstanceID]*domain.Instance{
					"inst-alice": domain.NewModelInstance("inst-alice", "alice", "test/model", "", nil),
				}
				sess.sub.scrollback = func(
					context.Context,
					protocol.WindowTarget,
					int,
				) ([]protocol.ScrollbackEntry, error) {
					if unavailable.Load() && !tc.failReply {
						return nil, sentinel
					}

					return []protocol.ScrollbackEntry{{Event: earlier}}, nil
				}
				sess.sub.replies = func(
					_ context.Context,
					window protocol.WindowTarget,
					_ int,
				) ([]protocol.ReplyEntry, error) {
					_, direct := protocol.DirectWindowPeer(window)
					if unavailable.Load() && tc.failReply && direct {
						return nil, sentinel
					}

					return nil, nil
				}
				upstream := &countingAPI{}
				mc := newRetryTestClient(t, sess, upstream)
				t.Cleanup(mc.Detach)

				sess.sub.events <- protocol.Delivery{Event: first}
				synctest.Wait()
				time.Sleep(4 * retryTestDelay)
				synctest.Wait()
				unavailable.Store(false)
				sess.sub.events <- protocol.Delivery{Event: second}
				synctest.Wait()

				source := domain.ClientSource("inst-botty", "botty")
				type assertionSnapshot struct {
					Prompts []turnPrompt
					Events  []domain.ProtocolEvent
				}

				require.Equal(t, assertionSnapshot{
					Prompts: []turnPrompt{{
						history: []string{"earlier", "first"}, triggers: []string{"second"},
					}},
					Events: []domain.ProtocolEvent{
						domain.ModelUnavailableError{Source: source, At: sess.Now()},
						domain.ModelDispatchStarted{Source: source, At: sess.Now()},
						domain.ModelDispatchDone{Source: source, At: sess.Now()},
					},
				}, assertionSnapshot{
					Prompts: upstream.prompts(),
					Events:  sess.emittedEvents(),
				})
			})
		})
	}
}

func TestDispatch_merged_traffic_does_not_reset_the_retry_allowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{
			upstreamError(t, http.StatusTooManyRequests),
			upstreamError(t, http.StatusTooManyRequests),
		}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("first")
		synctest.Wait()
		time.Sleep(retryTestDelay / 3)

		sess.sub.events <- channelMessage("second")
		synctest.Wait()
		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, []turnPrompt{
			{triggers: []string{"first"}},
			{history: []string{"first"}, triggers: []string{"second"}},
		}, upstream.prompts())
	})
}

func TestDispatch_rejoin_cancels_the_previous_window_retry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusTooManyRequests)}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("before part")
		synctest.Wait()

		at := time.Date(2025, 6, 15, 12, 1, 0, 0, time.UTC)
		sess.sub.events <- protocol.Delivery{Event: domain.Part{
			Source: domain.ClientSource(mc.instance.ID(), "botty"), Target: "#dev", At: at,
		}}
		sess.sub.events <- protocol.Delivery{Event: domain.Join{
			Source: domain.ClientSource(mc.instance.ID(), "botty"), Target: "#dev", At: at.Add(time.Second),
		}}
		sess.sub.events <- messageIn("#dev", "after rejoin")
		synctest.Wait()

		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, []turnPrompt{
			{triggers: []string{"before part"}},
			{triggers: []string{"", "after rejoin"}},
		}, upstream.prompts())
	})
}

func TestDispatch_does_not_run_a_closed_batch_without_a_later_trigger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{}
		at := time.Date(2025, 6, 15, 12, 1, 0, 0, time.UTC)
		sess.sub.events <- channelMessage("before part")
		sess.sub.events <- protocol.Delivery{Event: domain.Part{
			Source: domain.ClientSource("inst-botty", "botty"), Target: "#dev", At: at,
		}}
		sess.sub.events <- protocol.Delivery{Event: domain.Join{
			Source: domain.ClientSource("inst-botty", "botty"), Target: "#dev", At: at.Add(time.Second),
		}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)
		synctest.Wait()

		require.Equal(t, []turnPrompt(nil), upstream.prompts())
	})
}

func TestDispatch_keeps_an_invitation_after_a_kick_in_the_same_burst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		sess.windows = map[domain.ChannelName]*domain.ChannelWindow{
			"#dev": domain.NewChannelWindow("#dev", sess.Now()),
		}
		upstream := &countingAPI{}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- protocol.Delivery{Event: domain.Kicked{
			Source: domain.ClientSource("inst-alice", "alice"), Target: "#dev",
			Subject: "botty", SubjectIsSelf: true, At: sess.Now(),
		}}
		sess.sub.events <- protocol.Delivery{Event: domain.Invited{
			Source: domain.ClientSource("inst-alice", "alice"), Target: "#dev",
			Invitee: "botty", At: sess.Now(),
		}}
		synctest.Wait()

		source := domain.ClientSource("inst-botty", "botty")
		type assertionSnapshot struct {
			Prompts []turnPrompt
			Events  []domain.ProtocolEvent
		}

		require.Equal(t, assertionSnapshot{
			Prompts: []turnPrompt{{triggers: []string{""}}},
			Events: []domain.ProtocolEvent{
				domain.ModelDispatchStarted{Source: source, At: sess.Now()},
				domain.ModelDispatchDone{Source: source, At: sess.Now()},
			},
		}, assertionSnapshot{
			Prompts: upstream.prompts(),
			Events:  sess.emittedEvents(),
		})
	})
}

// TestDispatch_abandons_a_pending_retry_on_teardown pins that a
// scheduled re-dispatch never outlives the client. `Detach` joins the
// dispatch goroutine and the scheduler alongside it, so shutdown does
// not wait out a delay for a client that has gone.
func TestDispatch_abandons_a_pending_retry_on_teardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusServiceUnavailable)}}

		mc := newRetryTestClient(t, sess, upstream)

		sess.sub.events <- channelMessage("anyone about?")

		synctest.Wait()
		require.Equal(t, 1, upstream.callCount())

		// The retry is still waiting out its delay. Detach joins it
		// without the clock ever reaching the timer.
		mc.Detach()

		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, 1, upstream.callCount())
	})
}

type replayableCase struct {
	name string
	err  error
	want bool
}

type replayableEffect struct {
	Replayable bool
}

// TestReplayableTurnError covers which failed turns the dispatch loop
// hands back for a second attempt. A refusal for length is the case
// [api.Retryable] declines and the dispatch loop still wants: the
// refusal raises the model's recorded token ratio, so the second
// attempt plans a smaller request.
func TestReplayableTurnError(t *testing.T) {
	t.Parallel()

	tests := []replayableCase{
		{name: "no failure", err: nil, want: false},
		{
			name: "a refusal for length",
			err:  fmt.Errorf("send events: %w", api.ErrPromptTooLong),
			want: true,
		},
		{
			name: "an expired deadline",
			err:  fmt.Errorf("send events: %w", context.DeadlineExceeded),
			want: true,
		},
		{
			name: "a cancelled turn",
			err:  fmt.Errorf("send events: %w", context.Canceled),
			want: false,
		},
		{name: "an ordinary refusal", err: errors.New("unknown model"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, replayableEffect{Replayable: tc.want}, replayableEffect{
				Replayable: replayableTurnError(tc.err),
			})
		})
	}
}
