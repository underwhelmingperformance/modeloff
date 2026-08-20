package modelclient

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
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
// succeeds with silence.
type countingAPI struct {
	mu    sync.Mutex
	calls []turnPrompt
	errs  []error
}

func (c *countingAPI) SendEvents(
	_ context.Context,
	_ domain.ModelID,
	_ domain.InstanceID,
	_ string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls = append(c.calls, turnPrompt{history: bodies(history), triggers: bodies(events)})

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

// triggeredBodies flattens every trigger the model has been shown
// across all turns, in order, which is what the exactly-once rule is
// stated over.
func (c *countingAPI) triggeredBodies() []string {
	var all []string
	for _, p := range c.prompts() {
		all = append(all, p.triggers...)
	}

	return all
}

func (*countingAPI) ListModels(context.Context) ([]api.ModelInfo, error) { return nil, nil }

func (*countingAPI) ContinueWithToolResults(
	context.Context,
	*api.Conversation,
	[]api.ToolResult,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}

func (*countingAPI) GenerateNick(context.Context, domain.ModelID, string, []domain.Nick) (api.NicknameResult, error) {
	return api.NicknameResult{}, nil
}

func (*countingAPI) GeneratePersonas(context.Context, domain.ModelID) ([]domain.Persona, error) {
	return nil, nil
}

// upstreamError builds the `*openai.Error` shape the SDK surfaces for
// a non-2xx chat completion, wrapped the way the api package wraps it.
func upstreamError(t *testing.T, status int) error {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
	require.NoError(t, err)

	return fmt.Errorf("chat completion: %w", &openai.Error{
		StatusCode: status,
		Request:    req,
		Response:   &http.Response{StatusCode: status},
	})
}

// retryTestDelay is the flat wait the dispatch tests run the retry
// policy at. Jitter is off so the test asserts on when the second
// attempt lands, not on a range.
const retryTestDelay = 3 * time.Second

// newRetryTestClient attaches a model-client whose turns go to `upstream`
// and whose retry waits a flat [retryTestDelay].
func newRetryTestClient(t *testing.T, sess *fakeSession, upstream api.Client) *ModelClient {
	t.Helper()

	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	mc := New(inst, sess, func() api.Client { return upstream }, nil, nil, nil, nil, context.Background, nil)
	mc.retry = retryPolicy{Delay: retryTestDelay}

	require.NoError(t, mc.Attach(t.Context()))

	return mc
}

// channelMessage is the delivery that raises a turn in `#dev`.
func channelMessage(body string) protocol.Delivery {
	return messageIn("#dev", body)
}

// messageIn is the delivery that raises a turn in the named window.
func messageIn(channel domain.ChannelName, body string) protocol.Delivery {
	return protocol.Delivery{Event: domain.Message{
		Target:     channel,
		From:       "alice",
		InstanceID: "inst-alice",
		Body:       body,
		At:         time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
	}}
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

// TestDispatch_a_new_turn_supersedes_a_pending_retry pins the
// exactly-once rule a re-dispatch has to keep.
//
// `fileBatch` files every delivery into the window's ring as it walks
// the burst, so a failed turn's triggers are transcript from that
// moment on. A message arriving during the retry delay therefore
// raises a turn whose transcript already carries them, and handing
// the failed batch back afterwards would show the model the same line
// twice: once as history it has read, once as a fresh trigger to
// answer. In a chat window that is the model replying to the same
// message twice.
//
// A turn about to run for the window is the answer to everything
// outstanding in it, so it drops the pending re-dispatch.
func TestDispatch_a_new_turn_supersedes_a_pending_retry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusTooManyRequests)}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anyone about?")

		synctest.Wait()
		require.Equal(t, 1, upstream.callCount())

		// Still inside the retry delay, so the re-dispatch is pending.
		time.Sleep(retryTestDelay / 3)

		sess.sub.events <- channelMessage("still here?")
		synctest.Wait()

		// Well past when the re-dispatch would have fired.
		time.Sleep(4 * retryTestDelay)
		synctest.Wait()

		require.Equal(t, []turnPrompt{
			{triggers: []string{"anyone about?"}},
			{history: []string{"anyone about?"}, triggers: []string{"still here?"}},
		}, upstream.prompts())

		require.Equal(t, []string{"anyone about?", "still here?"}, upstream.triggeredBodies(),
			"every message reaches the model exactly once as a trigger")
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
