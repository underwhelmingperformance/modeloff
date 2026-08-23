package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	openai "github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"
)

// apiError builds the `*openai.Error` the SDK returns for a non-2xx
// response. Request and Response are populated because `Error()`
// reads both.
func apiError(t *testing.T, status int) error {
	return apiErrorWithRetryHeader(t, status, "")
}

func apiErrorWithRetryHeader(t *testing.T, status int, retry string) error {
	t.Helper()

	return apiErrorWithHeaders(t, status, http.Header{"X-Should-Retry": []string{retry}})
}

func apiErrorWithHeaders(t *testing.T, status int, headers http.Header) error {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
	require.NoError(t, err)

	return &openai.Error{
		StatusCode: status,
		Request:    req,
		Response: &http.Response{
			StatusCode: status,
			Header:     headers,
		},
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		err  func(t *testing.T) error
		want time.Duration
		ok   bool
	}{
		{
			name: "seconds",
			err: func(t *testing.T) error {
				return apiErrorWithHeaders(t, http.StatusTooManyRequests,
					http.Header{"Retry-After": []string{"10"}})
			},
			want: 10 * time.Second,
			ok:   true,
		},
		{
			name: "milliseconds take precedence",
			err: func(t *testing.T) error {
				return apiErrorWithHeaders(t, http.StatusTooManyRequests, http.Header{
					"Retry-After-Ms": []string{"1500"},
					"Retry-After":    []string{"10"},
				})
			},
			want: 1500 * time.Millisecond,
			ok:   true,
		},
		{
			name: "http date",
			err: func(t *testing.T) error {
				return apiErrorWithHeaders(t, http.StatusServiceUnavailable,
					http.Header{"Retry-After": []string{now.Add(30 * time.Second).Format(time.RFC1123)}})
			},
			want: 30 * time.Second,
			ok:   true,
		},
		{
			name: "wrapped response",
			err: func(t *testing.T) error {
				return fmt.Errorf("chat completion: %w", apiErrorWithHeaders(
					t, http.StatusTooManyRequests, http.Header{"Retry-After": []string{"4"}},
				))
			},
			want: 4 * time.Second,
			ok:   true,
		},
		{
			name: "elapsed date does not produce a negative wait",
			err: func(t *testing.T) error {
				return apiErrorWithHeaders(t, http.StatusServiceUnavailable,
					http.Header{"Retry-After": []string{now.Add(-time.Minute).Format(time.RFC1123)}})
			},
			ok: true,
		},
		{
			name: "malformed header",
			err: func(t *testing.T) error {
				return apiErrorWithHeaders(t, http.StatusTooManyRequests,
					http.Header{"Retry-After": []string{"later"}})
			},
		},
		{
			name: "no provider response",
			err: func(*testing.T) error {
				return &url.Error{Op: "Post", URL: "https://openrouter.ai", Err: errors.New("offline")}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := RetryAfter(tc.err(t), now)

			require.Equal(t, struct {
				Delay time.Duration
				OK    bool
			}{
				Delay: tc.want,
				OK:    tc.ok,
			}, struct {
				Delay time.Duration
				OK    bool
			}{
				Delay: got,
				OK:    ok,
			})
		})
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
			name: "request timeout",
			err:  func(t *testing.T) error { return apiError(t, http.StatusRequestTimeout) },
			want: true,
		},
		{
			name: "conflict",
			err:  func(t *testing.T) error { return apiError(t, http.StatusConflict) },
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
			name: "response requests retry",
			err: func(t *testing.T) error {
				return apiErrorWithRetryHeader(t, http.StatusBadRequest, "true")
			},
			want: true,
		},
		{
			name: "response refuses retry",
			err: func(t *testing.T) error {
				return apiErrorWithRetryHeader(t, http.StatusInternalServerError, "false")
			},
			want: false,
		},
		{
			name: "connection failed before a response",
			err: func(*testing.T) error {
				return fmt.Errorf("chat completion: %w", &url.Error{
					Op: "Post", URL: "https://openrouter.ai/api/v1/chat/completions",
					Err: errors.New("connection unavailable"),
				})
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
