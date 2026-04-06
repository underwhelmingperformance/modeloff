// Package observability defines shared attribute keys, metric names,
// and result constants used across the modeloff telemetry layer.
package observability

// OpenTelemetry span attribute keys attached to API and session spans.
const (
	AttrOperation        = "modeloff.operation"
	AttrModelID          = "modeloff.model_id"
	AttrResult           = "modeloff.result"
	AttrChannelKind      = "modeloff.channel_kind"
	AttrPromptTokens     = "modeloff.prompt_tokens"
	AttrCompletionTokens = "modeloff.completion_tokens"
	AttrTotalTokens      = "modeloff.total_tokens"
	AttrReasoningTokens  = "modeloff.reasoning_tokens"
	AttrCachedTokens     = "modeloff.cached_tokens"
	AttrCacheWriteTokens = "modeloff.cache_write_tokens"
	AttrCostCredits      = "modeloff.cost_credits"
	AttrRequestID        = "modeloff.request_id"
	AttrMemoryOperation  = "modeloff.memory.operation"
	AttrMemoryNick       = "modeloff.memory.nick"
	AttrSearchResults    = "modeloff.memory.search_results"
	AttrSearchTopScore   = "modeloff.memory.search_top_score"
)

// OpenTelemetry metric instrument names for counters and histograms.
const (
	MetricOperationCalls      = "modeloff.operation.calls"
	MetricLLMRequests         = "modeloff.llm.requests"
	MetricPromptTokens        = "modeloff.llm.tokens.prompt"
	MetricCompletionTokens    = "modeloff.llm.tokens.completion"
	MetricReasoningTokens     = "modeloff.llm.tokens.reasoning"
	MetricCachedTokens        = "modeloff.llm.tokens.cached"
	MetricCacheWriteTokens    = "modeloff.llm.tokens.cache_write"
	MetricCostCredits         = "modeloff.llm.cost.credits"
	MetricOperationDurationMs = "modeloff.operation.duration.ms"
	MetricRequestDurationMs   = "modeloff.llm.request.duration.ms"
	MetricDroppedLogs         = "modeloff.logs.dropped"
	MetricMemoryOperations    = "modeloff.memory.operations"
	MetricEmbeddingRequests   = "modeloff.memory.embedding.requests"
	MetricEmbeddingDurationMs = "modeloff.memory.embedding.duration.ms"
)

// Values for the AttrResult span attribute, indicating how the
// operation completed.
const (
	ResultOK    = "ok"
	ResultReply = "reply"
	ResultPass  = "pass"
	ResultError = "error"
)
