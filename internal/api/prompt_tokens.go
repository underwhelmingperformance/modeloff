package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	openai "github.com/openai/openai-go/v3"

	"github.com/laney/modeloff/internal/domain"
)

// promptTokenRatios records, per model, how many prompt tokens the
// provider counted for the request bytes it received, so an estimate
// converges on the model's own tokeniser. Before the first
// observation the estimate rests on OpenRouter's four-byte
// normalising rule, which is a metric for comparing models and not
// the count any of them enforces its window with.
//
// A model's running totals are a size-weighted mean of every
// observation: no smoothing constant to pick, and a large request
// counts for more than a small one. The floor is separate and is what
// a refusal for length writes. A refusal is a hard fact about a
// request size the model will not take, so the floor only ever raises
// an estimate, and it is what makes the turn after a refusal compact.
type promptTokenRatios struct {
	mu     sync.Mutex
	models map[domain.ModelID]promptTokenRatio
}

type promptTokenRatio struct {
	bytes       int64
	tokens      int64
	floorBytes  int64
	floorTokens int64
}

func newPromptTokenRatios() *promptTokenRatios {
	return &promptTokenRatios{models: make(map[domain.ModelID]promptTokenRatio)}
}

// observe records that a request of `bytes` cost `tokens` prompt
// tokens. A provider that reports no usage leaves the ratio alone.
func (r *promptTokenRatios) observe(modelID domain.ModelID, bytes int, tokens int64) {
	if bytes <= 0 || tokens <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	ratio := r.models[modelID]
	ratio.bytes += int64(bytes)
	ratio.tokens += tokens
	r.models[modelID] = ratio
}

// reject records that the provider refused a request of `bytes` for
// exceeding a window of `limitTokens`, which means the model counts
// more than `limitTokens` for that many bytes.
func (r *promptTokenRatios) reject(modelID domain.ModelID, bytes, limitTokens int) {
	if bytes <= 0 || limitTokens < 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	ratio := r.models[modelID]
	tokens := int64(limitTokens) + 1
	if ratio.floorBytes > 0 && ratio.floorTokens*int64(bytes) >= tokens*ratio.floorBytes {
		return
	}
	ratio.floorBytes = int64(bytes)
	ratio.floorTokens = tokens
	r.models[modelID] = ratio
}

// estimate returns the prompt tokens a request of `bytes` is expected
// to cost. With no observation for the model it falls back to the
// four-byte rule, which is what [RenderedEventRequest.EstimatedPromptTokens]
// applies.
func (r *promptTokenRatios) estimate(modelID domain.ModelID, bytes int) int {
	if bytes <= 0 {
		return 0
	}

	r.mu.Lock()
	ratio := r.models[modelID]
	r.mu.Unlock()

	estimate := (bytes + 3) / 4
	if ratio.bytes > 0 {
		estimate = scaleTokens(bytes, ratio.tokens, ratio.bytes)
	}
	if ratio.floorBytes > 0 {
		estimate = max(estimate, scaleTokens(bytes, ratio.floorTokens, ratio.floorBytes))
	}

	return estimate
}

// scaleTokens returns `bytes * tokens / perBytes`, rounded up so an
// estimate never falls below the ratio it came from.
func scaleTokens(bytes int, tokens, perBytes int64) int {
	return int((int64(bytes)*tokens + perBytes - 1) / perBytes)
}

// EstimatePromptTokens returns what `request` is expected to cost
// under `modelID`, calibrated from the prompt-token counts this
// client has seen the provider report for that model.
func (c *OpenRouterClient) EstimatePromptTokens(
	modelID domain.ModelID,
	request RenderedEventRequest,
) int {
	return c.ratios.estimate(modelID, len(request.Body))
}

// RecordPromptRejection records a refusal for length so every later
// estimate for `modelID` puts a request of `requestBytes` above
// `limitTokens`.
func (c *OpenRouterClient) RecordPromptRejection(
	modelID domain.ModelID,
	requestBytes, limitTokens int,
) {
	c.ratios.reject(modelID, requestBytes, limitTokens)
}

// classifyCompletionError marks a provider refusal for length with
// [ErrPromptTooLong], leaving every other error as it arrived.
//
// OpenAI names the condition in the error's `code`. OpenRouter
// answers with a numeric code and puts the condition in the message
// ("This endpoint's maximum context length is N tokens. However, you
// requested about M tokens"), so the message is the only signal it
// gives and matching it is what the classification rests on.
func classifyCompletionError(err error) error {
	if !promptTooLong(err) {
		return err
	}

	return fmt.Errorf("%w: %w", ErrPromptTooLong, err)
}

func promptTooLong(err error) bool {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return false
	}
	if apiErr.Code == "context_length_exceeded" {
		return true
	}

	message := strings.ToLower(apiErr.Message)
	for _, phrase := range []string{"context length", "maximum context", "too many tokens"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}

	return false
}
