package session

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/observability"
)

// ToolContext carries the backend context for a model tool call.
type ToolContext struct {
	Session *Session
	Actor   domain.Nick
	Channel domain.ChannelName
}

// ToolResultPayload is the common tool result envelope returned to models.
type ToolResultPayload struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary,omitempty"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ToolSpec describes a model-callable tool and how to execute it.
type ToolSpec struct {
	Definition api.ToolDefinition
	Execute    func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error)
}

// ToolRegistry holds the available tools for a dispatch.
type ToolRegistry struct {
	order  []ToolSpec
	byName map[string]ToolSpec
}

// NewToolRegistry builds a registry from the given specs.
func NewToolRegistry(specs ...ToolSpec) *ToolRegistry {
	registry := &ToolRegistry{
		order:  make([]ToolSpec, 0, len(specs)),
		byName: make(map[string]ToolSpec, len(specs)),
	}

	for _, spec := range specs {
		registry.order = append(registry.order, spec)
		registry.byName[spec.Definition.Name] = spec
	}

	return registry
}

// MergeToolRegistries combines registries in order, with later duplicates
// ignored so earlier entries keep precedence.
func MergeToolRegistries(registries ...*ToolRegistry) *ToolRegistry {
	var specs []ToolSpec
	seen := map[string]struct{}{}

	for _, registry := range registries {
		if registry == nil {
			continue
		}

		for _, spec := range registry.order {
			if _, ok := seen[spec.Definition.Name]; ok {
				continue
			}

			seen[spec.Definition.Name] = struct{}{}
			specs = append(specs, spec)
		}
	}

	return NewToolRegistry(specs...)
}

// Definitions returns the API-facing tool definitions.
func (r *ToolRegistry) Definitions() []api.ToolDefinition {
	if r == nil {
		return nil
	}

	definitions := make([]api.ToolDefinition, 0, len(r.order))

	for _, spec := range r.order {
		definitions = append(definitions, spec.Definition)
	}

	return definitions
}

// Find returns the named tool spec if present.
func (r *ToolRegistry) Find(name string) (ToolSpec, bool) {
	if r == nil {
		return ToolSpec{}, false
	}

	spec, ok := r.byName[name]

	return spec, ok
}

func memoryToolRegistry(mem MemoryExecutor, searchEnabled bool) *ToolRegistry {
	if mem == nil {
		return nil
	}

	specs := []ToolSpec{
		{
			Definition: api.ToolDefinition{
				Name:        "write_memory",
				Description: "Create or update a durable personal memory by key. Prefer updating an existing key over creating near-duplicates. Use short, stable keys that describe the fact clearly (e.g. user_name, preferred_editor). Only store stable facts that may matter in future conversations, not temporary chat context.",
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
					},
					"required":             []string{"key", "content"},
					"additionalProperties": false,
				},
			},
			Execute: func(ctx context.Context, _ ToolContext, rawArgs json.RawMessage) (ToolResultPayload, error) {
				var args struct {
					Key     string `json:"key"`
					Content string `json:"content"`
				}

				if err := json.Unmarshal(rawArgs, &args); err != nil {
					return ToolResultPayload{}, err
				}

				err := mem.WriteMemory(ctx, args.Key, args.Content)
				recordMemoryTool(ctx, "write_memory", err)
				if err != nil {
					return ToolResultPayload{}, err
				}

				return ToolResultPayload{OK: true, Summary: fmt.Sprintf("stored memory %q", args.Key)}, nil
			},
		},
		{
			Definition: api.ToolDefinition{
				Name:        "delete_memory",
				Description: "Delete a stored memory by key when it is outdated, incorrect, or no longer useful.",
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
			Execute: func(ctx context.Context, _ ToolContext, rawArgs json.RawMessage) (ToolResultPayload, error) {
				var args struct {
					Key string `json:"key"`
				}

				if err := json.Unmarshal(rawArgs, &args); err != nil {
					return ToolResultPayload{}, err
				}

				err := mem.DeleteMemory(ctx, args.Key)
				recordMemoryTool(ctx, "delete_memory", err)
				if err != nil {
					return ToolResultPayload{}, err
				}

				return ToolResultPayload{OK: true, Summary: fmt.Sprintf("deleted memory %q", args.Key)}, nil
			},
		},
	}

	if searchEnabled {
		specs = append(specs, ToolSpec{
			Definition: api.ToolDefinition{
				Name:        "search_memory",
				Description: "Search stored memories semantically. Use before guessing or writing duplicate memories. Do not call repeatedly without clear reason. Query when you need prior context that is not visible in the current prompt, or when you suspect a relevant memory already exists.",
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
			Execute: func(ctx context.Context, _ ToolContext, rawArgs json.RawMessage) (ToolResultPayload, error) {
				var args struct {
					Query string `json:"query"`
					Limit int    `json:"limit"`
				}

				if err := json.Unmarshal(rawArgs, &args); err != nil {
					return ToolResultPayload{}, err
				}

				results, err := mem.SearchMemory(ctx, args.Query, args.Limit)
				recordMemoryTool(ctx, "search_memory", err)
				if err != nil {
					return ToolResultPayload{}, err
				}

				return ToolResultPayload{OK: true, Summary: fmt.Sprintf("found %d matching memories", len(results)), Data: results}, nil
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

func searchEnabled(store memory.Store) bool {
	_, ok := store.(memory.Searcher)

	return ok
}
