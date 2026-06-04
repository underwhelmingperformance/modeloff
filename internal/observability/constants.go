// Package observability defines shared attribute keys, metric names,
// and result constants used across the modeloff telemetry layer.
package observability

// OpenTelemetry span attribute keys attached to API and session spans.
const (
	// Cross-cutting attributes:
	AttrOperation  = "modeloff.operation"
	AttrResult     = "modeloff.result"
	AttrErrorKind  = "modeloff.error_kind"
	AttrChannel    = "modeloff.channel"
	AttrNick       = "modeloff.nick"
	AttrInstanceID = "modeloff.instance_id"
	AttrModelID    = "modeloff.model_id"

	// Cross-cutting channel attribute (applies beyond LLM dispatch):
	AttrChannelKind = "modeloff.channel_kind"

	// LLM dispatch attributes (session-layer outcomes):
	AttrPassReason    = "modeloff.llm.pass.reason"
	AttrRetryCount    = "modeloff.llm.retry.count"
	AttrToolTurnCount = "modeloff.llm.tool.turn_count"

	// LLM dispatch attributes (API transport):
	AttrPromptTokens     = "modeloff.llm.tokens.prompt"
	AttrCompletionTokens = "modeloff.llm.tokens.completion"
	AttrTotalTokens      = "modeloff.llm.tokens.total"
	AttrReasoningTokens  = "modeloff.llm.tokens.reasoning"
	AttrCachedTokens     = "modeloff.llm.tokens.cached"
	AttrCacheWriteTokens = "modeloff.llm.tokens.cache_write"
	AttrCostCredits      = "modeloff.llm.cost.credits"
	AttrHTTPStatusCode   = "modeloff.http_status_code"
	AttrHTTPResponseBody = "modeloff.http_response_body"
	AttrRequestID        = "modeloff.llm.request.id"

	// Memory attributes:
	AttrMemoryOperation = "modeloff.memory.operation"
	AttrMemoryToolKind  = "modeloff.memory.tool_kind"
	AttrSearchResults   = "modeloff.memory.search_results"
	AttrSearchTopScore  = "modeloff.memory.search_top_score"

	// JoinAutojoinChannels attributes:
	AttrAutojoinCount    = "modeloff.autojoin.count"
	AttrAutojoinFailed   = "modeloff.autojoin.failed"
	AttrAutojoinChannels = "modeloff.autojoin.channels"
)

// OpenTelemetry metric instrument names for counters and histograms.
const (
	// Operation timing:
	MetricOperationCalls      = "modeloff.operation.calls"
	MetricOperationDurationMs = "modeloff.operation.duration.ms"

	// LLM metrics:
	MetricLLMRequests       = "modeloff.llm.requests"
	MetricPromptTokens      = "modeloff.llm.tokens.prompt"
	MetricCompletionTokens  = "modeloff.llm.tokens.completion"
	MetricReasoningTokens   = "modeloff.llm.tokens.reasoning"
	MetricCachedTokens      = "modeloff.llm.tokens.cached"
	MetricCacheWriteTokens  = "modeloff.llm.tokens.cache_write"
	MetricCostCredits       = "modeloff.llm.cost.credits"
	MetricRequestDurationMs = "modeloff.llm.request.duration.ms"

	// Memory metrics:
	MetricMemoryOperations     = "modeloff.memory.operations"
	MetricMemoryToolCalls      = "modeloff.memory.tool.calls"
	MetricMemorySearchResults  = "modeloff.memory.search.results"
	MetricMemorySearchTopScore = "modeloff.memory.search.top_score"
	MetricEmbeddingRequests    = "modeloff.memory.embedding.requests"
	MetricEmbeddingDurationMs  = "modeloff.memory.embedding.duration.ms"

	// Runtime health:
	MetricDroppedLogs         = "modeloff.logs.dropped"
	MetricPersistenceFailures = "modeloff.persistence.failures"
)

// Values for the AttrResult span attribute, indicating how the
// operation completed.
const (
	ResultOK    = "ok"
	ResultTool  = "tool"
	ResultPass  = "pass"
	ResultError = "error"
)

// Values for stable pass_reason attributes.
const (
	PassReasonModelPass         = "model_pass"
	PassReasonModelRefused      = "model_refused"
	PassReasonContentFiltered   = "content_filtered"
	PassReasonToolLoopExhausted = "tool_loop_exhausted"
)

// Values for stable error_kind attributes. These categorise span
// failures so dashboards and alerts can group by kind without parsing
// error strings.
const (
	// ErrorKindTransport indicates a network-level failure reaching
	// the model API (TCP, TLS, DNS, timeouts).
	ErrorKindTransport = "transport"

	// ErrorKindResponseParse indicates the model API responded but
	// the body did not decode into the expected schema.
	ErrorKindResponseParse = "response_parse"

	// ErrorKindHTTPStatus indicates the model API returned a non-2xx
	// status code with an otherwise well-formed body.
	ErrorKindHTTPStatus = "http_status"

	// ErrorKindInvalidResponse indicates the model API returned a
	// well-formed response that violated the structural contract
	// (e.g. neither a reply nor a pass).
	ErrorKindInvalidResponse = "invalid_response"

	// ErrorKindStore indicates a persistence failure (SQLite, event
	// log, persona/instance/channel writes).
	ErrorKindStore = "store"

	// ErrorKindDispatch indicates a session-layer wrapper around an
	// API call failed: the underlying child span will carry the
	// finer-grained transport/response_parse/etc. kind.
	ErrorKindDispatch = "dispatch"

	// ErrorKindClientState indicates a session-layer cached state
	// forbids the operation without an upstream call — e.g. the model
	// list is known-stale from a prior failure, or the API key is not
	// configured. Distinct from `ErrorKindDispatch` (the upstream WAS
	// called) and `ErrorKindValidation` (user-fixable input).
	// Alerting that excludes upstream noise can filter on
	// `AttrErrorKind != ErrorKindClientState`.
	ErrorKindClientState = "client_state"

	// ErrorKindAutojoin indicates one or more channels in the
	// autojoin sequence failed to join. The aggregate
	// session.autojoin span carries this kind; the per-channel
	// session.join children may carry their own kinds.
	ErrorKindAutojoin = "autojoin"

	// ErrorKindValidation indicates a guard refused the request
	// because of a contextual rule (e.g. cannot send messages to the
	// status channel, cannot kick from a DM). The operation did not
	// complete, so the span status is still codes.Error per OTel
	// semantic conventions; the kind discriminates "user-fixable
	// input rejection" from "infrastructure failure". Alerting that
	// wants to ignore user typos should filter on
	// AttrErrorKind != ErrorKindValidation rather than on span
	// status.
	ErrorKindValidation = "validation"

	// ErrorKindNotFound indicates a lookup of a channel, instance,
	// or other named entity returned no result. As with
	// ErrorKindValidation, the span status is codes.Error because
	// the operation did not produce its expected output; the kind
	// distinguishes "name does not resolve" from a real
	// infrastructure error so alerting can filter accordingly.
	ErrorKindNotFound = "not_found"
)
