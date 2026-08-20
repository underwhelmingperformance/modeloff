package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"
)

// apiError builds the `*openai.Error` the SDK returns for a non-2xx
// response. Request and Response are populated because `Error()`
// reads both.
func apiError(t *testing.T, status int) error {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
	require.NoError(t, err)

	return &openai.Error{
		StatusCode: status,
		Request:    req,
		Response:   &http.Response{StatusCode: status},
	}
}

// TestRetryable covers the classification a caller schedules a second
// attempt on. The rule is narrow on purpose: a later identical request
// has to have a real chance of succeeding, which rules out anything
// the upstream decided about the request itself.
func TestRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want bool
	}{
		{
			name: "no error",
			err:  func(*testing.T) error { return nil },
			want: false,
		},
		{
			name: "rate limited",
			err:  func(t *testing.T) error { return apiError(t, http.StatusTooManyRequests) },
			want: true,
		},
		{
			name: "upstream server error",
			err:  func(t *testing.T) error { return apiError(t, http.StatusInternalServerError) },
			want: true,
		},
		{
			name: "upstream gateway timeout",
			err:  func(t *testing.T) error { return apiError(t, http.StatusGatewayTimeout) },
			want: true,
		},
		{
			name: "wrapped rate limit",
			err: func(t *testing.T) error {
				return fmt.Errorf("chat completion: %w", apiError(t, http.StatusTooManyRequests))
			},
			want: true,
		},
		{
			name: "bad request",
			err:  func(t *testing.T) error { return apiError(t, http.StatusBadRequest) },
			want: false,
		},
		{
			name: "unauthorised",
			err:  func(t *testing.T) error { return apiError(t, http.StatusUnauthorized) },
			want: false,
		},
		{
			name: "model not found",
			err:  func(t *testing.T) error { return apiError(t, http.StatusNotFound) },
			want: false,
		},
		{
			name: "deadline expired",
			err:  func(*testing.T) error { return fmt.Errorf("chat completion: %w", context.DeadlineExceeded) },
			want: true,
		},
		{
			name: "cancelled",
			err:  func(*testing.T) error { return fmt.Errorf("chat completion: %w", context.Canceled) },
			want: false,
		},
		{
			name: "model refused",
			err:  func(*testing.T) error { return &ErrModelRefused{Reason: "no"} },
			want: false,
		},
		{
			name: "content filtered",
			err:  func(*testing.T) error { return ErrContentFiltered },
			want: false,
		},
		{
			name: "unclassified",
			err:  func(*testing.T) error { return errors.New("something else") },
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, Retryable(tc.err(t)))
		})
	}
}

// TestRetryable_cancellation_wins_over_a_deadline pins the ordering
// for a shutdown that cancels a turn already past its own deadline.
// The client is going away, so nothing about it is worth a second
// attempt.
func TestRetryable_cancellation_wins_over_a_deadline(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("%w: %w", context.Canceled, context.DeadlineExceeded)

	require.False(t, Retryable(err))
}
