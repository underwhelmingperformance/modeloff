package modelmanager

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

func TestManager_background_finaliser_removes_a_completed_client(t *testing.T) {
	mgr := New(Config{BaseContext: t.Context})
	id := protocol.ClientID("inst-finished")
	client := modelclient.New(modelclient.Config{
		Instance: domain.NewModelInstance(domain.InstanceID(id), "botty", "test/model", "", nil),
	})
	entry := &drainingClient{client: client, done: make(chan struct{})}
	mgr.draining[id] = entry

	go mgr.finishDrain(id, entry)
	<-entry.done

	mgr.clientsMu.Lock()
	draining := make(map[protocol.ClientID]*drainingClient, len(mgr.draining))
	maps.Copy(draining, mgr.draining)
	mgr.clientsMu.Unlock()
	require.Empty(t, draining)
}

func TestManager_DetachAll_ends_the_background_lifecycle_after_a_clean_drain(t *testing.T) {
	appContext, cancelApp := context.WithCancel(t.Context())
	baseContextCalls := 0
	mgr := New(Config{BaseContext: func() context.Context {
		baseContextCalls++

		return appContext
	}})

	cancelApp()
	before := mgr.lifecycleContext.Err()
	drainErr := mgr.DetachAll(t.Context())

	type assertionSnapshot struct {
		BaseContextCalls int
		Before           error
		DrainErr         error
		After            error
	}

	require.Equal(t, assertionSnapshot{
		BaseContextCalls: 1,
		After:            context.Canceled,
	}, assertionSnapshot{
		BaseContextCalls: baseContextCalls,
		Before:           before,
		DrainErr:         drainErr,
		After:            mgr.lifecycleContext.Err(),
	})
}

func TestManager_DetachAll_ends_the_background_lifecycle_on_timeout(t *testing.T) {
	mgr := New(Config{BaseContext: t.Context})
	id := protocol.ClientID("inst-blocked")
	mgr.draining[id] = &drainingClient{done: make(chan struct{})}
	drainContext, cancelDrain := context.WithCancel(t.Context())
	cancelDrain()

	err := mgr.DetachAll(drainContext)
	var drainErr *DrainTimeoutError
	require.ErrorAs(t, err, &drainErr)
	type assertionSnapshot struct {
		Abandoned []protocol.ClientID
		Cause     error
		Lifecycle error
	}

	require.Equal(t, assertionSnapshot{
		Abandoned: []protocol.ClientID{id},
		Cause:     context.Canceled,
		Lifecycle: context.Canceled,
	}, assertionSnapshot{
		Abandoned: drainErr.Abandoned,
		Cause:     drainErr.Err,
		Lifecycle: mgr.lifecycleContext.Err(),
	})
}

func TestManager_DetachAll_does_not_rejoin_an_abandoned_finaliser(t *testing.T) {
	mgr := New(Config{BaseContext: t.Context})
	id := protocol.ClientID("inst-blocked")
	entry := &drainingClient{done: make(chan struct{})}
	mgr.draining[id] = entry
	drainContext, cancelDrain := context.WithCancel(t.Context())
	cancelDrain()

	firstErr := mgr.DetachAll(drainContext)
	secondErr := mgr.DetachAll(t.Context())

	var drainErr *DrainTimeoutError
	require.ErrorAs(t, firstErr, &drainErr)
	type assertionSnapshot struct {
		Abandoned      []protocol.ClientID
		EntryAbandoned bool
		StillDraining  map[protocol.ClientID]*drainingClient
		SecondErr      error
	}

	require.Equal(t, assertionSnapshot{
		Abandoned:      []protocol.ClientID{id},
		EntryAbandoned: true,
		StillDraining:  map[protocol.ClientID]*drainingClient{id: entry},
	}, assertionSnapshot{
		Abandoned:      drainErr.Abandoned,
		EntryAbandoned: entry.abandoned,
		StillDraining:  mgr.draining,
		SecondErr:      secondErr,
	})
}

func TestManager_abandoned_finaliser_does_not_delete_memory(t *testing.T) {
	ctx := t.Context()
	backing := storetest.NewMemoryStore(t)
	id := domain.InstanceID("inst-blocked")
	inst := domain.NewModelInstance(id, "botty", "test/model", "", nil)
	restored := storemod.MemoryEntry{
		Key:     "late",
		Content: "written before the stalled turn returned",
		At:      time.Unix(1, 0).UTC(),
	}
	require.NoError(t, backing.SaveInstance(ctx, inst))
	require.NoError(t, backing.DeleteInstanceByID(ctx, id))
	require.NoError(t, backing.WriteMemory(
		ctx, id, restored.Key, restored.Content, restored.At, restored.Pinned,
	))

	mgr := New(Config{
		Store:       backing,
		Memory:      memory.NewStoreAdapter(backing),
		BaseContext: t.Context,
	})
	client := modelclient.New(modelclient.Config{Instance: inst})
	entry := &drainingClient{
		client:    client,
		forget:    true,
		abandoned: true,
		done:      make(chan struct{}),
	}
	mgr.draining[protocol.ClientID(id)] = entry

	mgr.finishDrain(protocol.ClientID(id), entry)

	memories, err := backing.ReadMemories(ctx, id)
	require.NoError(t, err)
	pending, err := backing.ListPendingMemoryDeletions(ctx)
	require.NoError(t, err)
	type assertionSnapshot struct {
		Memories []storemod.MemoryEntry
		Pending  []domain.InstanceID
		Draining map[protocol.ClientID]*drainingClient
	}

	require.Equal(t, assertionSnapshot{
		Memories: []storemod.MemoryEntry{restored},
		Pending:  []domain.InstanceID{id},
		Draining: map[protocol.ClientID]*drainingClient{},
	}, assertionSnapshot{
		Memories: memories,
		Pending:  pending,
		Draining: mgr.draining,
	})
}
