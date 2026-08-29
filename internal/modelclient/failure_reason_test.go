package modelclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability/oteltest"
)

type failureReasonCase struct {
	name string
	err  error
}

type failureReasonEffect struct {
	Reason domain.ModelFailureReason
	Text   string
}

// providerError builds the error the OpenAI SDK returns for a
// non-2xx response.
func providerError(status int) error {
	return &openai.Error{
		StatusCode: status,
		Response:   &http.Response{StatusCode: status},
	}
}

// TestModelFailureReason pins one reason per thing the operator reading
// the line would do about it. Two failures share a reason only when they
// call for the same action, which is why 401 and 403 are told apart: a
// provider returns 403 when it accepts the key and refuses the request
// anyway, and sending somebody to check a key the provider just accepted
// is sending them to the wrong place.
func TestModelFailureReason(t *testing.T) {
	cases := []failureReasonCase{
		{
			name: "a prompt the context window cannot hold",
			err: fmt.Errorf("plan turn context: %w", &ContextWindowExceededError{
				ContextLength: 8192, PromptTokens: 12000,
			}),
		},
		{
			name: "a key the provider will not accept",
			err:  fmt.Errorf("dispatch: %w", providerError(http.StatusUnauthorized)),
		},
		{
			name: "a model the provider will not serve this account",
			err:  providerError(http.StatusForbidden),
		},
		{
			name: "an account out of credit",
			err:  providerError(http.StatusPaymentRequired),
		},
		{
			name: "a model the provider does not serve",
			err:  providerError(http.StatusNotFound),
		},
		{
			name: "a provider asking for fewer requests",
			err:  providerError(http.StatusTooManyRequests),
		},
		{
			name: "a request the provider would not accept",
			err:  providerError(http.StatusBadRequest),
		},
		{
			name: "a provider that is over capacity",
			err:  providerError(http.StatusServiceUnavailable),
		},
		{
			name: "a connection that never reached the provider",
			err:  &url.Error{Op: "Post", Err: errors.New("connection refused")},
		},
		{
			name: "a fault in the dispatch path",
			err:  errors.New("read turn context: database is locked"),
		},
	}

	want := []failureReasonEffect{
		{
			Reason: domain.ModelFailureContextWindow,
			Text: `model "botty" has more context than its window holds, ` +
				`and a retry will not clear it`,
		},
		{
			Reason: domain.ModelFailureAuth,
			Text:   `model "botty" was refused by its provider: check the configured API key`,
		},
		{
			Reason: domain.ModelFailureForbidden,
			Text: `model "botty" was refused by its provider: the key is accepted ` +
				`but this account cannot use this model`,
		},
		{
			Reason: domain.ModelFailureNoCredit,
			Text: `model "botty" was refused by its provider for payment: ` +
				`add credit to the account`,
		},
		{
			Reason: domain.ModelFailureUnknownModel,
			Text: `model "botty" names a model its provider does not serve: ` +
				`pick another with /add-model`,
		},
		{
			Reason: domain.ModelFailureRateLimited,
			Text: `model "botty" was rate limited by its provider, and the ` +
				`turn was given up`,
		},
		{
			Reason: domain.ModelFailureBadRequest,
			Text: `model "botty" sent a request its provider would not accept, ` +
				`which is a fault in this application and not in the configuration`,
		},
		{
			Reason: domain.ModelFailureUpstream,
			Text: `model "botty" could not complete its provider request: ` +
				`the provider failed on its own side`,
		},
		{
			Reason: domain.ModelFailureUnavailable,
			Text:   `model "botty" unavailable for dispatch`,
		},
		{
			Reason: domain.ModelFailureUnavailable,
			Text:   `model "botty" unavailable for dispatch`,
		},
	}

	got := make([]failureReasonEffect, 0, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.name, func(*testing.T) {
			reason := modelFailureReason(testCase.err)
			failure := domain.ModelUnavailableError{
				Source: domain.ClientSource("inst-botty", "botty"),
				Reason: reason,
			}
			got = append(got, failureReasonEffect{Reason: reason, Text: failure.Error()})
		})
	}

	require.Equal(t, want, got)
}

// dispatchFailureCase is one upstream status. Attempts is how many
// upstream calls fail: two for a status the retry policy replays, one
// for the rest.
type dispatchFailureCase struct {
	Name     string
	Status   int
	Attempts int
}

// dispatchFailureEffect records the events the window received, the text
// each failure notice renders, and the prompts the provider was sent.
type dispatchFailureEffect struct {
	Events  []domain.ProtocolEvent
	Texts   []string
	Prompts []turnPrompt
}

// TestDispatch_tells_the_window_why_a_turn_could_not_run pins three
// things about the notice a failed turn raises: it carries the reason
// the failure classifies to, one notice covers a turn however many
// attempts it took, and it follows the final `ModelDispatchDone`.
//
// [TestModelFailureReason] calls the classifier directly, so it would
// pass even if dispatch dropped the notice. This drives the dispatch
// loop and reads what the window received.
func TestDispatch_tells_the_window_why_a_turn_could_not_run(t *testing.T) {
	cases := []dispatchFailureCase{
		{Name: "a key the provider will not accept", Status: http.StatusUnauthorized, Attempts: 1},
		{Name: "a model this account cannot use", Status: http.StatusForbidden, Attempts: 1},
		{Name: "an account out of credit", Status: http.StatusPaymentRequired, Attempts: 1},
		{Name: "a model the provider does not serve", Status: http.StatusNotFound, Attempts: 1},
		{Name: "a request the provider would not accept", Status: http.StatusBadRequest, Attempts: 1},
		{Name: "a provider asking for fewer requests", Status: http.StatusTooManyRequests, Attempts: 2},
		{Name: "a provider that is over capacity", Status: http.StatusServiceUnavailable, Attempts: 2},
	}

	reasons := []domain.ModelFailureReason{
		domain.ModelFailureAuth,
		domain.ModelFailureForbidden,
		domain.ModelFailureNoCredit,
		domain.ModelFailureUnknownModel,
		domain.ModelFailureBadRequest,
		domain.ModelFailureRateLimited,
		domain.ModelFailureUpstream,
	}

	// The rendered line is asserted beside the reason because the notice
	// is raised only once the turn has been abandoned. A line offering
	// the operator a further wait would say the opposite of what
	// happened, and a line counting attempts would be wrong when the
	// retry was never scheduled.
	texts := []string{
		`model "botty" was refused by its provider: check the configured API key`,
		`model "botty" was refused by its provider: the key is accepted but ` +
			`this account cannot use this model`,
		`model "botty" was refused by its provider for payment: add credit to the account`,
		`model "botty" names a model its provider does not serve: pick another with /add-model`,
		`model "botty" sent a request its provider would not accept, which is a ` +
			`fault in this application and not in the configuration`,
		`model "botty" was rate limited by its provider, and the turn was given up`,
		`model "botty" could not complete its provider request: the provider ` +
			`failed on its own side`,
	}

	for i, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess := newFakeSession()
				errs := make([]error, 0, testCase.Attempts)
				for range testCase.Attempts {
					errs = append(errs, upstreamError(t, testCase.Status))
				}
				upstream := &countingAPI{errs: errs}
				mc := newRetryTestClient(t, sess, upstream)
				t.Cleanup(mc.Detach)

				sess.sub.events <- channelMessage("anything at all")
				synctest.Wait()
				time.Sleep(4 * retryTestDelay)
				synctest.Wait()

				source := domain.ClientSource("inst-botty", "botty")
				want := make([]domain.ProtocolEvent, 0, 2*testCase.Attempts+1)
				for range testCase.Attempts {
					want = append(want,
						domain.ModelDispatchStarted{Source: source, At: sess.Now()},
						domain.ModelDispatchDone{Source: source, At: sess.Now()},
					)
				}
				want = append(want, domain.ModelUnavailableError{
					Source: source, Reason: reasons[i], At: sess.Now(),
				})

				prompts := make([]turnPrompt, 0, testCase.Attempts)
				for range testCase.Attempts {
					prompts = append(prompts, turnPrompt{
						triggers: []string{"anything at all"},
					})
				}
				got := dispatchFailureEffect{
					Events: sess.emittedEvents(), Prompts: upstream.prompts(),
				}
				for _, event := range got.Events {
					failure, ok := event.(domain.ModelUnavailableError)
					if ok {
						got.Texts = append(got.Texts, failure.Error())
					}
				}

				require.Equal(t, dispatchFailureEffect{
					Events: want, Texts: []string{texts[i]}, Prompts: prompts,
				}, got)
			})
		})
	}
}

// turnNoticeSpanEffect records the span each failure notice was emitted
// under.
type turnNoticeSpanEffect struct {
	Spans []trace.SpanContext
}

// TestDispatch_raises_the_notice_under_the_turn_that_failed pins the
// trace link between a failed turn and the line an operator reads about
// it. `runBatch` raises the notice after the turn's span has closed, and
// a delivery carries whatever span its emitting context holds.
func TestDispatch_raises_the_notice_under_the_turn_that_failed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder, provider := oteltest.NewSpanRecorder(t)
		sess := newFakeSession()
		sess.tracer = provider
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusForbidden)}}

		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		sess.sub.events <- channelMessage("anything at all")
		synctest.Wait()

		turn := oteltest.FindSpan(t, recorder, "modelclient.dispatch_turn")
		require.True(t, turn.SpanContext().IsValid(),
			"the recorder must supply a real span for the comparison to mean anything")

		require.Equal(t,
			turnNoticeSpanEffect{Spans: []trace.SpanContext{turn.SpanContext()}},
			turnNoticeSpanEffect{Spans: sess.failureSpanContexts()},
		)
	})
}

// renamedTurnEffect records the nick each notice named.
type renamedTurnEffect struct {
	Nicks []domain.Nick
}

// TestDispatch_names_the_nick_the_failed_turn_ran_under covers a model
// that renames itself while the turn that fails is still running.
//
// `mc.nick()` reads the subscription, which a NICK has already moved by
// the time the turn is abandoned and the notice raised. The prompt, the
// span attributes and the dispatch events all used the earlier nick.
func TestDispatch_names_the_nick_the_failed_turn_ran_under(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess := newFakeSession()
		upstream := &countingAPI{errs: []error{upstreamError(t, http.StatusForbidden)}}
		mc := newRetryTestClient(t, sess, upstream)
		t.Cleanup(mc.Detach)

		// The rename lands while the turn is upstream, which is where a
		// model's own `nick` tool call would put it.
		upstream.beforeReturn = func() { sess.sub.nick = "renamed" }

		sess.sub.events <- channelMessage("anything at all")
		synctest.Wait()

		var nicks []domain.Nick
		for _, event := range sess.emittedEvents() {
			failure, ok := event.(domain.ModelUnavailableError)
			if ok {
				nicks = append(nicks, failure.Source.Nick())
			}
		}

		require.Equal(t,
			renamedTurnEffect{Nicks: []domain.Nick{"botty"}},
			renamedTurnEffect{Nicks: nicks},
		)
	})
}
