package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
)

// ErrNoAPIKey is returned by operations that require an OpenRouter
// API key when one has not yet been configured. Callers can use
// `errors.Is(err, modelclient.ErrNoAPIKey)` to distinguish this
// validation outcome from an upstream failure, e.g. to suppress
// user-facing notices while the user is still in onboarding.
var ErrNoAPIKey = errors.New("api key not configured")

// ErrModelListUnavailable is returned when the OpenRouter model
// catalogue could not be fetched on the most recent attempt and
// the session has no fresh list to validate against. `/add-model`
// and other operations that need an authoritative model list
// short-circuit with this sentinel. Callers can use
// `errors.Is(err, modelclient.ErrModelListUnavailable)` to surface
// a dedicated user notice.
var ErrModelListUnavailable = errors.New("model list unavailable")

// MemoryExecutor prepares memory mutations and executes read-only
// searches on behalf of a model instance. Preparation may perform slow
// embedding work, but it must not change persistent memory state.
type MemoryExecutor interface {
	PrepareWriteMemory(
		ctx context.Context,
		key string,
		content string,
		pinned bool,
	) (memory.PreparedMutation, error)
	PrepareDeleteMemory(ctx context.Context, key string) (memory.PreparedMutation, error)
	SearchMemory(ctx context.Context, query string, limit int) ([]memory.SearchResult, error)
}

type memoryEffectFunc func(context.Context) error

func (f memoryEffectFunc) Commit(ctx context.Context) error {
	return f(ctx)
}

func (memoryEffectFunc) Finish(context.Context) {}

// instanceMemory closes over an InstanceID and memory.Store to
// implement MemoryExecutor. Keying by identity (not nick) means
// memories survive a model instance's `/nick` rename.
type instanceMemory struct {
	instanceID domain.InstanceID
	store      memory.Store

	// now is the clock a written entry's At is stamped from. Callers
	// construct instanceMemory with the same session clock the rest
	// of a dispatch turn already reads (Session.Now, the SessionAPI
	// method mc.sess.Now already calls elsewhere), not time.Now
	// directly, so a frozen test clock stamps memories the same way
	// it stamps every other event in that turn.
	now func() time.Time
}

func (m *instanceMemory) PrepareWriteMemory(
	ctx context.Context,
	key string,
	content string,
	pinned bool,
) (memory.PreparedMutation, error) {
	entry := memory.Entry{Key: key, Content: content, Pinned: pinned, At: m.now()}
	if preparer, ok := m.store.(memory.MutationPreparer); ok {
		return preparer.PrepareWrite(ctx, m.instanceID, entry)
	}

	return memoryEffectFunc(func(ctx context.Context) error {
		return m.store.Write(ctx, m.instanceID, entry)
	}), nil
}

func (m *instanceMemory) PrepareDeleteMemory(
	ctx context.Context,
	key string,
) (memory.PreparedMutation, error) {
	if preparer, ok := m.store.(memory.MutationPreparer); ok {
		return preparer.PrepareDelete(ctx, m.instanceID, key)
	}

	return memoryEffectFunc(func(ctx context.Context) error {
		return m.store.Delete(ctx, m.instanceID, key)
	}), nil
}

func (m *instanceMemory) WriteMemory(
	ctx context.Context,
	key string,
	content string,
	pinned bool,
) error {
	effect, err := m.PrepareWriteMemory(ctx, key, content, pinned)
	if err != nil {
		return err
	}
	if err := effect.Commit(ctx); err != nil {
		return err
	}

	effect.Finish(ctx)

	return nil
}

func (m *instanceMemory) DeleteMemory(ctx context.Context, key string) error {
	effect, err := m.PrepareDeleteMemory(ctx, key)
	if err != nil {
		return err
	}
	if err := effect.Commit(ctx); err != nil {
		return err
	}

	effect.Finish(ctx)

	return nil
}

func (m *instanceMemory) SearchMemory(ctx context.Context, query string, limit int) ([]memory.SearchResult, error) {
	searcher, ok := m.store.(memory.Searcher)
	if !ok || !searcher.Searchable() {
		return nil, fmt.Errorf("semantic search is not configured")
	}

	return searcher.Search(ctx, m.instanceID, query, limit)
}

func memoryToolRegistry(mem MemoryExecutor, searchEnabled bool) *ToolRegistry {
	if mem == nil {
		return nil
	}

	specs := []ToolSpec{
		{
			Definition: api.ToolDefinition{
				Name:        "write_memory",
				Description: "Create or update a durable personal memory by key. Prefer updating an existing key over creating near-duplicates. Use short, stable keys that describe the fact clearly (e.g. user_name, preferred_editor). Only store stable facts that may matter in future conversations, not temporary chat context or obvious details already visible in the current prompt. Set pinned for a fact worth keeping in the short memory line even when nothing in the conversation points at it; leave ordinary searchable facts unpinned. Use this to overwrite a stale or incorrect memory by writing the corrected value to the same key. Do not call repeatedly without clear reason.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"key": map[string]any{
							"type":        "string",
							"description": "A short, stable identifier for the memory, such as user_name, favourite_topic, or preferred_editor.",
						},
						"content": map[string]any{
							"type":        "string",
							"description": "The durable fact, preference, or decision to remember.",
						},
						"pinned": map[string]any{
							"type":        "boolean",
							"description": "Whether this fact is preferred for the short memory line ahead of other memories the conversation does not point at.",
						},
					},
					"required":             []string{"key", "content", "pinned"},
					"additionalProperties": false,
				},
			},
			Execute: func(ctx context.Context, toolCtx ToolContext, rawArgs json.RawMessage) (ToolResultPayload, error) {
				var args struct {
					Key     string `json:"key"`
					Content string `json:"content"`
					Pinned  bool   `json:"pinned"`
				}

				if err := json.Unmarshal(rawArgs, &args); err != nil {
					return ToolResultPayload{}, err
				}

				effect, err := mem.PrepareWriteMemory(
					ctx, args.Key, args.Content, args.Pinned,
				)
				if err == nil {
					err = toolCtx.runWithAuthority(ctx, func() error {
						return effect.Commit(ctx)
					})
				}
				if err == nil {
					effect.Finish(ctx)
				}
				recordMemoryTool(ctx, "write_memory", err)
				if err != nil {
					return ToolResultPayload{}, &ToolExecutionError{Tool: "write_memory", Err: err}
				}

				return ToolResultPayload{OK: true, Summary: fmt.Sprintf("stored memory %q", args.Key)}, nil
			},
		},
		{
			Definition: api.ToolDefinition{
				Name:        "delete_memory",
				Description: "Delete a stored memory by key when it is outdated, incorrect, stale, or no longer useful. Do not call repeatedly without clear reason.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"key": map[string]any{
							"type":        "string",
							"description": "The identifier of the memory to remove.",
						},
					},
					"required":             []string{"key"},
					"additionalProperties": false,
				},
			},
			Execute: func(ctx context.Context, toolCtx ToolContext, rawArgs json.RawMessage) (ToolResultPayload, error) {
				var args struct {
					Key string `json:"key"`
				}

				if err := json.Unmarshal(rawArgs, &args); err != nil {
					return ToolResultPayload{}, err
				}

				effect, err := mem.PrepareDeleteMemory(ctx, args.Key)
				if err == nil {
					err = toolCtx.runWithAuthority(ctx, func() error {
						return effect.Commit(ctx)
					})
				}
				if err == nil {
					effect.Finish(ctx)
				}
				recordMemoryTool(ctx, "delete_memory", err)
				if err != nil {
					return ToolResultPayload{}, &ToolExecutionError{Tool: "delete_memory", Err: err}
				}

				return ToolResultPayload{OK: true, Summary: fmt.Sprintf("deleted memory %q", args.Key)}, nil
			},
		},
	}

	if searchEnabled {
		specs = append(specs, ToolSpec{
			Definition: api.ToolDefinition{
				Name:        "search_memory",
				Description: "Search stored memories semantically. Use before guessing or writing duplicate memories. Query when you need prior context that is not visible in the current prompt, or when you suspect a relevant memory already exists. Do not call repeatedly without clear reason.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "A natural-language description of the context you want to recall.",
						},
						"limit": map[string]any{
							"type":        "integer",
							"description": "Maximum number of results to return (1-20).",
						},
					},
					"required":             []string{"query", "limit"},
					"additionalProperties": false,
				},
			},
			Execute: func(ctx context.Context, toolCtx ToolContext, rawArgs json.RawMessage) (ToolResultPayload, error) {
				var args struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}

				if err := json.Unmarshal(rawArgs, &args); err != nil {
					return ToolResultPayload{}, err
				}

				results, err := mem.SearchMemory(ctx, args.Query, args.Limit)
				var authorisedResults []memory.SearchResult
				if err == nil {
					err = toolCtx.runWithAuthority(ctx, func() error {
						authorisedResults = results

						return nil
					})
				}
				recordMemoryTool(ctx, "search_memory", err)
				if err != nil {
					return ToolResultPayload{}, &ToolExecutionError{Tool: "search_memory", Err: err}
				}

				return ToolResultPayload{OK: true, Summary: fmt.Sprintf("found %d matching memories", len(authorisedResults)), Data: authorisedResults}, nil
			},
		})
	}

	return NewToolRegistry(specs...)
}

func recordMemoryTool(ctx context.Context, kind string, err error) {
	span := trace.SpanFromContext(ctx)
	attrs := []attribute.KeyValue{
		attribute.String(observability.AttrMemoryOperation, kind),
		attribute.String(observability.AttrMemoryToolKind, kind),
	}

	result := observability.ResultOK
	if err != nil {
		result = observability.ResultError
	}

	observability.RecordMemoryToolCall(ctx, kind, result)
	attrs = append(attrs, attribute.String(observability.AttrResult, result))
	span.AddEvent("memory.tool_call", trace.WithAttributes(attrs...))
}
