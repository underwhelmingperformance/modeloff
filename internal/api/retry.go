package api

import (
	"context"
	"errors"
	"net/http"

	openai "github.com/openai/openai-go/v3"
)

// Retryable reports whether err describes a transient upstream
// condition, so that the same request sent again later has a real
// chance of succeeding. Three cases qualify:
//
//   - HTTP 429, the provider asking the caller to come back later;
//   - any HTTP 5xx, a failure on the provider's side;
//   - an expired deadline, which is the local half of the same thing:
//     the request was still outstanding when its time ran out.
//
// Everything else is a decision the upstream made about this
// particular request: a malformed body, a rejected key, an unknown
// model, a refusal, a content filter. Sending it again would produce
// the same answer.
//
// A cancelled context is never retryable, whatever else the chain
// carries. Cancellation is the client being torn down, so there is
// nobody left for a second attempt to answer.
func Retryable(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return retryableStatus(apiErr.StatusCode)
	}

	return false
}

// retryableStatus reports whether an HTTP status from the provider
// describes a condition that may have passed by the next attempt.
func retryableStatus(status int) bool {
	if status == http.StatusTooManyRequests {
		return true
	}

	return status >= http.StatusInternalServerError
}
