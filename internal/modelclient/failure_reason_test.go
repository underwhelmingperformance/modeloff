package modelclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
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
			Text: `model "botty" is being rate limited by its provider: ` +
				`it will answer again shortly`,
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
