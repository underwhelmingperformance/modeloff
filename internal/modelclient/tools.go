package modelclient

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
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
	ResolveNick(ctx context.Context, nick domain.Nick) (domain.InstanceID, domain.Nick, error)
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
	SetReflectionMode(ctx context.Context, mode config.ReflectionMode) error
	SetReflectionModel(modelID domain.ModelID)
	SetPersona(ctx context.Context, id string, description string) error
	ListPersonas(ctx context.Context) ([]domain.Persona, error)
	RegeneratePersonas(ctx context.Context) ([]domain.Persona, error)
	ResetPersonas(ctx context.Context) (int, error)
	InspectPersona(
		ctx context.Context,
		nick domain.Nick,
	) (domain.PersonaInspection, error)
	ResetPersona(
		ctx context.Context,
		nick domain.Nick,
	) (domain.PersonaInspection, error)
	RollbackPersona(
		ctx context.Context,
		nick domain.Nick,
		target domain.PersonaRevisionID,
	) (domain.PersonaInspection, error)
	SetInstancePersona(
		ctx context.Context,
		nick domain.Nick,
		description string,
	) (domain.PersonaInspection, error)

	// HasAPIKey reports whether an API key is configured. `/config`
	// small-model validation uses it to decide between validating
	// against the catalogue now and deferring until a key exists.
	HasAPIKey() bool

	// EnsureStructuredOutputModel validates that modelID supports
	// the app's strict structured-output contract, lazily loading
	// the model catalogue if needed. It is a no-op returning nil
	// when no API key is configured.
	EnsureStructuredOutputModel(ctx context.Context, modelID domain.ModelID) error

	// EnsureToolCapableModel validates that modelID supports tool
	// calling, on the same terms. `/config reflection-model` uses it:
	// a reflection run explores with the recall tools before it
	// proposes, so a model without them fails every attempt.
	EnsureToolCapableModel(ctx context.Context, modelID domain.ModelID) error

	// EmbeddingSearchable reports whether the memory store's
	// embedding endpoint is currently reachable, and the error from
	// its most recent probe. `/config` embedding-model uses it to
	// report the outcome of the probe a successful config change
	// already triggers. The model catalogue has no embedding models
	// in it, so validating the id against it is not an option.
	EmbeddingSearchable() (bool, error)
}

// ToolContext carries the backend context for a model tool call.
// Client is the protocol-side handle the tool dispatches commands
// through;
// it is the model-client handle for model invocations and the
// user-client for user-driven tool calls.
//
// Target addresses the window the call is running in, as a
// [protocol.MsgTarget] built by the dispatch loop from that window's
// key. The address is what keeps a caller with no window apart from a
// caller in the DM with the user: the user's [domain.InstanceID] is
// empty, so an empty key names that conversation, while a nil Target
// names nothing and every tool that needs a window refuses it. A tool
// that needs the window's name asks [protocol.WindowName]; a tool
// that needs a channel asserts [protocol.ChannelTarget].
//
// Callers must populate Client before invoking any tool whose
// `RunTool` routes through the wire protocol; a nil Client crashes
// with a nil-pointer dereference at `tc.Send`.
//
// The window authority a call runs under is a constructor argument
// ([NewToolContext]), so a tool invocation always has one and a
// caller cannot reach the wire by leaving it out.
type ToolContext struct {
	Session    SessionAPI
	Manager    ManagerAPI
	Target     protocol.MsgTarget
	Client     protocol.Client
	Pace       func(context.Context, string) error
	guard      protocol.WindowGuard
	projection providerTargetProjection
}

// NewToolContext binds one tool invocation to the authority in
// `guard`. Every command the invocation sends and every store
// operation it runs is checked against that authority first.
func NewToolContext(
	guard protocol.WindowGuard,
	session SessionAPI,
	client protocol.Client,
	target protocol.MsgTarget,
) ToolContext {
	return ToolContext{
		Session: session,
		Target:  target,
		Client:  client,
		guard:   guard,
	}
}

// PaceMessage waits before one chat message when the caller supplied
// a typing pacer. Direct tool invocations leave Pace nil and send
// immediately.
func (tc ToolContext) PaceMessage(ctx context.Context, body string) error {
	if tc.Pace != nil {
		if err := tc.Pace(ctx, body); err != nil {
			return err
		}
	}

	return tc.authorise(ctx)
}

func (tc ToolContext) authorise(ctx context.Context) error {
	if !tc.guard.Valid(ctx) {
		return errDispatchWindowClosed
	}

	return nil
}

// Send submits a protocol command under the turn's window authority.
func (tc ToolContext) Send(
	ctx context.Context,
	cmd protocol.Command,
) (protocol.Response, error) {
	cmd = tc.projection.bindCommand(cmd)

	response, err := tc.guard.Send(ctx, tc.Client, cmd)
	if errors.Is(err, protocol.ErrWindowAuthorityChanged) {
		return protocol.Response{}, errDispatchWindowClosed
	}

	return response, err
}

func (tc ToolContext) runWithAuthority(ctx context.Context, operation func() error) error {
	err := tc.guard.RunWithAuthority(ctx, operation)
	if errors.Is(err, protocol.ErrWindowAuthorityChanged) {
		return errDispatchWindowClosed
	}

	return err
}

// ToolResultPayload is the common tool result envelope returned to
// models.
type ToolResultPayload struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary,omitempty"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ToolOutcome separates a model-facing result from an execution
// failure that must abort the current tool loop.
type ToolOutcome struct {
	Payload        ToolResultPayload
	ExecutionError error
}

// ToolExecutionError reports an infrastructure failure while running
// a named tool. Model-correctable refusals use ToolResultPayload.
type ToolExecutionError struct {
	Tool string
	Err  error
}

func (e *ToolExecutionError) Error() string {
	return "execute tool " + e.Tool + ": " + e.Err.Error()
}

func (e *ToolExecutionError) Unwrap() error {
	return e.Err
}

// ToolSpec describes a model-callable tool and how to execute it.
// RequiredCapabilities and RequiredKind mirror the command grammar's
// `caps:` / `kind:` tags. [ToolRegistry.Filter] keeps the advertised
// set stable across windows, while the executor applies RequiredKind
// to each call.
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
// `caps` may call. Window-specific tools remain in the registry so
// the provider sees the same tool prefix in channel and DM turns;
// the executor refuses a call made from the wrong window.
func (r *ToolRegistry) Filter(caps command.CapabilityHolder) *ToolRegistry {
	if r == nil {
		return nil
	}

	specs := make([]ToolSpec, 0, len(r.order))
	for _, spec := range r.order {
		if !command.Holds(caps, spec.RequiredCapabilities) {
			continue
		}

		specs = append(specs, spec)
	}

	return NewToolRegistry(specs...)
}

func searchEnabled(store memory.Store) bool {
	searcher, ok := store.(memory.Searcher)

	return ok && searcher.Searchable()
}
