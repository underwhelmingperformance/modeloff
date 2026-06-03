package modelclient

import (
	"context"
	"encoding/json"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/protocol"
)

// SessionAPI is the session-side dependency surface the chatcmd
// tool grammar reads through [ToolContext.Session]. The concrete
// `*session.Session` satisfies it implicitly; defining it here lets
// the chatcmd and modelclient packages stay independent of the
// session package's symbol set.
type SessionAPI interface {
	GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error)
	LogEvent(ctx context.Context, ch domain.ChannelName, event domain.PersistableEvent) (domain.StoredEvent, error)
	ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error)
	Now() time.Time
}

// ManagerAPI is the manager-side dependency surface the chatcmd
// tool grammar reads through [ToolContext.Manager]. The concrete
// `*modelmanager.Manager` satisfies it implicitly; defining it
// here lets the chatcmd and modelclient packages stay independent
// of the modelmanager package's symbol set.
type ManagerAPI interface {
	SetAPIKey(ctx context.Context, apiKey, baseURL string) error
	SetBaseURL(ctx context.Context, baseURL string) error
	SetSmallModel(ctx context.Context, modelID domain.ModelID)
	SetPersona(ctx context.Context, id string, description string) error
	ListPersonas(ctx context.Context) ([]domain.Persona, error)
	RegeneratePersonas(ctx context.Context) ([]domain.Persona, error)
	ResetPersonas(ctx context.Context) (int, error)
}

// ToolContext carries the backend context for a model tool call.
// Actor is the `*domain.Instance` for the caller — models dispatched
// by the session receive their own handle, and the user's own
// `/`-command tool invocations receive the user handle. Client is
// the protocol-side handle the tool dispatches commands through;
// it is the model-client handle for model invocations and the
// user-client for user-driven tool calls.
//
// Callers must populate Client before invoking any tool whose
// `RunTool` routes through the wire protocol; a nil Client crashes
// with a nil-pointer dereference at `tc.Client.Send`.
type ToolContext struct {
	Session SessionAPI
	Manager ManagerAPI
	Actor   *domain.Instance
	Channel domain.ChannelName
	Client  protocol.Client
}

// ToolResultPayload is the common tool result envelope returned to
// models.
type ToolResultPayload struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary,omitempty"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ToolSpec describes a model-callable tool and how to execute it.
// RequiredCapabilities and RequiredKind mirror the command grammar's
// `caps:` / `kind:` tags so [ToolRegistry.Filter] can present a model
// only the tools it can actually use in the current window.
type ToolSpec struct {
	Definition           api.ToolDefinition
	Execute              func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error)
	RequiredCapabilities []command.Capability
	RequiredKind         *domain.ChannelKind
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

// Filter returns a registry holding only the tools a holder with
// `caps` may call in a window of `kind`. A tool is dropped when the
// holder lacks one of its RequiredCapabilities, or when its
// RequiredKind names a different window. Tools with neither
// requirement always pass.
func (r *ToolRegistry) Filter(caps command.CapabilityHolder, kind domain.ChannelKind) *ToolRegistry {
	if r == nil {
		return nil
	}

	specs := make([]ToolSpec, 0, len(r.order))
	for _, spec := range r.order {
		if spec.RequiredKind != nil && *spec.RequiredKind != kind {
			continue
		}

		if !command.Holds(caps, spec.RequiredCapabilities) {
			continue
		}

		specs = append(specs, spec)
	}

	return NewToolRegistry(specs...)
}

func searchEnabled(store memory.Store) bool {
	_, ok := store.(memory.Searcher)

	return ok
}
