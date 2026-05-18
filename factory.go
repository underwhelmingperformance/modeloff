package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
)

// modelClientRegistry satisfies [session.ModelClientFactory] by
// constructing per-instance [modelclient.ModelClient] handles,
// holding them by id, and detaching on request. The owning
// [session.Session] reference arrives as a parameter on each
// `Attach` call, so the registry can be built before the session
// it serves.
type modelClientRegistry struct {
	apiClient   api.Client
	memStore    memory.Store
	tools       *modelclient.ToolRegistry
	baseContext func() context.Context

	mu      sync.Mutex
	clients map[protocol.ClientID]*modelclient.ModelClient
}

func newModelClientRegistry(
	apiClient api.Client,
	memStore memory.Store,
	tools *modelclient.ToolRegistry,
	baseContext func() context.Context,
) *modelClientRegistry {
	return &modelClientRegistry{
		apiClient:   apiClient,
		memStore:    memStore,
		tools:       tools,
		baseContext: baseContext,
		clients:     make(map[protocol.ClientID]*modelclient.ModelClient),
	}
}

// Attach constructs (or returns the existing handle for) the
// model-client backing `inst` and attaches it to `sess`.
// Idempotent on a repeat call for the same identity.
func (r *modelClientRegistry) Attach(ctx context.Context, sess *session.Session, inst *domain.Instance) (protocol.Client, error) {
	id := protocol.ClientID(inst.ID())

	r.mu.Lock()
	if existing, ok := r.clients[id]; ok {
		r.mu.Unlock()
		return existing, nil
	}

	mc := modelclient.New(inst, sess, r.apiClient, r.memStore, r.tools, r.baseContext)
	r.clients[id] = mc
	r.mu.Unlock()

	if err := mc.Attach(ctx); err != nil {
		r.mu.Lock()
		delete(r.clients, id)
		r.mu.Unlock()
		return nil, fmt.Errorf("attach model client %q: %w", id, err)
	}

	return mc, nil
}

// Detach releases the model-client for `id`. Idempotent on an
// unknown id.
func (r *modelClientRegistry) Detach(id protocol.ClientID) {
	r.mu.Lock()
	mc, ok := r.clients[id]
	if ok {
		delete(r.clients, id)
	}
	r.mu.Unlock()

	if !ok {
		return
	}

	mc.Detach()
}
