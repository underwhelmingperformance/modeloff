package observability

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// llmScopedAttrs lists the attribute keys that represent LLM-dispatch
// concerns (session-layer outcomes and API transport). Every entry
// must be namespaced under `modeloff.llm.`.
var llmScopedAttrs = map[string]string{
	"AttrPassReason":       AttrPassReason,
	"AttrRetryCount":       AttrRetryCount,
	"AttrToolTurnCount":    AttrToolTurnCount,
	"AttrPromptTokens":     AttrPromptTokens,
	"AttrCompletionTokens": AttrCompletionTokens,
	"AttrTotalTokens":      AttrTotalTokens,
	"AttrReasoningTokens":  AttrReasoningTokens,
	"AttrCachedTokens":     AttrCachedTokens,
	"AttrCacheWriteTokens": AttrCacheWriteTokens,
	"AttrCostCredits":      AttrCostCredits,
	"AttrRequestID":        AttrRequestID,
}

// crossCuttingAttrs lists attributes that intentionally live outside
// the LLM namespace because they apply beyond LLM dispatch. They must
// NOT carry the `modeloff.llm.` prefix.
var crossCuttingAttrs = map[string]string{
	"AttrOperation":        AttrOperation,
	"AttrResult":           AttrResult,
	"AttrErrorKind":        AttrErrorKind,
	"AttrChannel":          AttrChannel,
	"AttrNick":             AttrNick,
	"AttrInstanceID":       AttrInstanceID,
	"AttrModelID":          AttrModelID,
	"AttrChannelKind":      AttrChannelKind,
	"AttrHTTPStatusCode":   AttrHTTPStatusCode,
	"AttrHTTPResponseBody": AttrHTTPResponseBody,
}

func TestLLMAttributeConstants_useLLMNamespace(t *testing.T) {
	for name, value := range llmScopedAttrs {
		require.Truef(t,
			strings.HasPrefix(value, "modeloff.llm."),
			"%s = %q must start with %q",
			name, value, "modeloff.llm.")
	}
}

func TestCrossCuttingAttributeConstants_excludeLLMNamespace(t *testing.T) {
	for name, value := range crossCuttingAttrs {
		require.Falsef(t,
			strings.HasPrefix(value, "modeloff.llm."),
			"%s = %q must not start with %q",
			name, value, "modeloff.llm.")
	}
}
