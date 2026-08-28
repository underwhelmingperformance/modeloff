package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	openai "github.com/openai/openai-go/v3"
)

// RetryAfter returns the provider-directed delay from err's response.
// OpenAI-compatible providers use Retry-After-Ms for a
// millisecond delay and Retry-After for either seconds or an HTTP
// date. The millisecond header takes precedence, matching the pinned
// SDK's retry policy.
func RetryAfter(err error, now time.Time) (time.Duration, bool) {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.Response == nil {
		return 0, false
	}

	headers := apiErr.Response.Header
	if delay, ok := retryHeaderDuration(headers.Get("Retry-After-Ms"), time.Millisecond); ok {
		return max(delay, 0), true
	}

	retryAfter := headers.Get("Retry-After")
	if delay, ok := retryHeaderDuration(retryAfter, time.Second); ok {
		return max(delay, 0), true
	}
	if at, parseErr := time.Parse(time.RFC1123, retryAfter); parseErr == nil {
		return max(at.Sub(now), 0), true
	}

	return 0, false
}

func retryHeaderDuration(value string, unit time.Duration) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}

	amount, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}

	return time.Duration(amount * float64(unit)), true
}

// Retryable reports whether err describes a transient upstream
// condition, so that the same request sent again later has a real
// chance of succeeding. These are the cases the pinned SDK retries
// before the caller disables its hidden request loop:
//
//   - a connection failure before any response arrived;
//   - HTTP 408, 409, 429, or any 5xx response;
//   - a response whose x-should-retry header explicitly says true;
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
		if apiErr.Response != nil {
			switch apiErr.Response.Header.Get("x-should-retry") {
			case "true":
				return true
			case "false":
				return false
			}
		}

		return retryableStatus(apiErr.StatusCode)
	}

	var connectionErr *url.Error
	return errors.As(err, &connectionErr)
}

// FailureStatus returns the HTTP status a provider answered a failed
// request with. The second result is false for an error raised before
// any response arrived, and for one raised by the code around the
// request rather than by the provider.
//
// A caller uses this to tell a decision the provider made about the
// request from a fault on this side of it. [Retryable] answers the
// narrower question of whether sending the same request again could
// succeed.
func FailureStatus(err error) (int, bool) {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return 0, false
	}

	return apiErr.StatusCode, true
}

// retryableStatus reports whether an HTTP status from the provider
// describes a condition that may have passed by the next attempt.
func retryableStatus(status int) bool {
	if status == http.StatusRequestTimeout ||
		status == http.StatusConflict ||
		status == http.StatusTooManyRequests {
		return true
	}

	return status >= http.StatusInternalServerError
}
