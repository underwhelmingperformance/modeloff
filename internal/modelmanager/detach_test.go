package modelmanager_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

// spyMemoryDeleter is a memory.Store that also implements
// memory.InstanceDeleter, recording every id DeleteInstance is
// called with. Every other method is a no-op: these tests only care
// about whether Forget reaches the deleter, not about memory content.
type spyMemoryDeleter struct {
	mu      sync.Mutex
	deleted []domain.InstanceID
	err     error
}

func (s *spyMemoryDeleter) Read(context.Context, domain.InstanceID) ([]memory.Entry, error) {
	return nil, nil
}

func (s *spyMemoryDeleter) Write(context.Context, domain.InstanceID, memory.Entry) error {
	return nil
}

func (s *spyMemoryDeleter) Delete(context.Context, domain.InstanceID, string) error {
	return nil
}

func (s *spyMemoryDeleter) Reset(context.Context) error {
	return nil
}

func (s *spyMemoryDeleter) DeleteInstance(_ context.Context, id domain.InstanceID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.deleted = append(s.deleted, id)

	return s.err
}

func (s *spyMemoryDeleter) deletedIDs() []domain.InstanceID {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]domain.InstanceID(nil), s.deleted...)
}

func (s *spyMemoryDeleter) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.err = err
}

var (
	_ memory.Store           = (*spyMemoryDeleter)(nil)
	_ memory.InstanceDeleter = (*spyMemoryDeleter)(nil)
)

type blockingHistoryStore struct {
	*storemod.SQLiteStore

	started chan struct{}
	release chan struct{}
}

func (s *blockingHistoryStore) EventsBefore(
	ctx context.Context,
	ch domain.ChannelName,
	before *int64,
	n int,
) ([]domain.StoredEvent, error) {
	close(s.started)

	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return s.SQLiteStore.EventsBefore(ctx, ch, before, n)
}

// attachTestInstance saves and attaches a model instance under fx's
// manager, returning it for the caller to detach or forget.
func attachTestInstance(t *testing.T, fx *managerFixture, sess *session.Session, id domain.InstanceID) *domain.Instance {
	t.Helper()

	inst := domain.NewModelInstance(id, "botty", "test/model", "", nil)
	require.NoError(t, fx.store.SaveInstance(t.Context(), inst))

	_, err := fx.mgr.Attach(t.Context(), sess, inst)
	require.NoError(t, err)

	return inst
}

func TestManager_Forget_deletes_the_instances_memory_collection(t *testing.T) {
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{APIClient: &apitest.Fake{}, Memory: spy})

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := attachTestInstance(t, fx, sess, "inst-botty")
	require.NoError(t, fx.store.DeleteInstanceByID(t.Context(), inst.ID()))

	fx.mgr.Forget(protocol.ClientID(inst.ID()))
	pending, err := fx.store.ListPendingMemoryDeletions(t.Context())
	require.NoError(t, err)

	require.Equal(t, struct {
		deleted []domain.InstanceID
		pending []domain.InstanceID
	}{
		deleted: []domain.InstanceID{"inst-botty"},
		pending: nil,
	}, struct {
		deleted []domain.InstanceID
		pending []domain.InstanceID
	}{
		deleted: spy.deletedIDs(),
		pending: pending,
	})
}

func TestManager_Forget_keeps_the_marker_when_the_index_is_unavailable(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	fx := newTestManager(t, modelmanager.Config{
		Store:     s,
		APIClient: &apitest.Fake{},
		Memory:    memory.NewStoreAdapter(s),
	})

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := attachTestInstance(t, fx, sess, "inst-botty")
	require.NoError(t, fx.store.DeleteInstanceByID(t.Context(), inst.ID()))
	require.NoError(t, fx.store.WriteMemory(t.Context(), inst.ID(), "late", "orphan", time.Unix(1, 0)))

	fx.mgr.Forget(protocol.ClientID(inst.ID()))
	pending, err := fx.store.ListPendingMemoryDeletions(t.Context())
	require.NoError(t, err)
	memories, err := fx.store.ReadMemories(t.Context(), inst.ID())
	require.NoError(t, err)
	require.Equal(t, struct {
		memories []storemod.MemoryEntry
		pending  []domain.InstanceID
	}{
		memories: nil,
		pending:  []domain.InstanceID{inst.ID()},
	}, struct {
		memories []storemod.MemoryEntry
		pending  []domain.InstanceID
	}{
		memories: memories,
		pending:  pending,
	})
}

func TestManager_DetachAndForget_waits_for_the_dispatch_goroutine(t *testing.T) {
	baseCtx, cancelBase := context.WithCancel(t.Context())
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &apitest.Fake{
		SendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, api.SystemPrompt, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			close(started)
			<-release

			return api.CompletionResult{}, nil
		},
	}
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{
		APIClient:   fake,
		Memory:      spy,
		BaseContext: func() context.Context { return baseCtx },
	})
	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := attachTestInstance(t, fx, sess, "inst-botty")
	client := sess.LookupClient(protocol.ClientID(inst.ID()))
	require.NotNil(t, client)
	resp, err := client.Send(t.Context(), protocol.Join{Channels: []domain.ChannelName{"#room"}})
	require.NoError(t, err)
	require.Equal(t, protocol.Response{Events: []domain.ProtocolEvent{
		domain.JoinedChannel{Channel: "#room"},
	}}, resp)
	require.NoError(t, sess.PokeNow(t.Context()))
	<-started

	require.NoError(t, fx.store.DeleteInstanceByID(t.Context(), inst.ID()))
	fx.mgr.DetachAndForget(protocol.ClientID(inst.ID()))
	lateMemory := storemod.MemoryEntry{
		Key:     "late",
		Content: "written while dispatch was stopping",
		At:      time.Unix(1, 0),
	}
	require.NoError(t, fx.store.WriteMemory(t.Context(), inst.ID(), lateMemory.Key, lateMemory.Content, lateMemory.At))
	memories, err := fx.store.ReadMemories(t.Context(), inst.ID())
	require.NoError(t, err)
	pending, err := fx.store.ListPendingMemoryDeletions(t.Context())
	require.NoError(t, err)
	require.Equal(t, struct {
		deleted  []domain.InstanceID
		memories []storemod.MemoryEntry
		pending  []domain.InstanceID
	}{
		deleted:  nil,
		memories: []storemod.MemoryEntry{lateMemory},
		pending:  []domain.InstanceID{inst.ID()},
	}, struct {
		deleted  []domain.InstanceID
		memories []storemod.MemoryEntry
		pending  []domain.InstanceID
	}{
		deleted:  spy.deletedIDs(),
		memories: memories,
		pending:  pending,
	})

	cancelBase()
	close(release)
	require.NoError(t, fx.mgr.DetachAll(t.Context()))
	pending, err = fx.store.ListPendingMemoryDeletions(t.Context())
	require.NoError(t, err)
	memories, err = fx.store.ReadMemories(t.Context(), inst.ID())
	require.NoError(t, err)
	require.Equal(t, struct {
		deleted  []domain.InstanceID
		memories []storemod.MemoryEntry
		pending  []domain.InstanceID
	}{
		deleted:  []domain.InstanceID{inst.ID()},
		memories: nil,
		pending:  nil,
	}, struct {
		deleted  []domain.InstanceID
		memories []storemod.MemoryEntry
		pending  []domain.InstanceID
	}{
		deleted:  spy.deletedIDs(),
		memories: memories,
		pending:  pending,
	})
}

// TestManager_DetachAll_does_not_delete_memory_collections pins the
// negative half of the InstanceDeleter connection point: DetachAll's
// bulk release runs for every client attached at shutdown regardless
// of why the process is exiting, and none of those clients had their
// instance row deleted just because the process is stopping. Only an
// explicit Forget may reach the deleter.
func TestManager_DetachAll_does_not_delete_memory_collections(t *testing.T) {
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{APIClient: &apitest.Fake{}, Memory: spy})

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	attachTestInstance(t, fx, sess, "inst-botty")

	require.NoError(t, fx.mgr.DetachAll(t.Context()))

	require.Equal(t, []domain.InstanceID(nil), spy.deletedIDs())
}

func TestManager_DetachAll_refuses_later_attachments(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{APIClient: &apitest.Fake{}})
	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	require.NoError(t, fx.mgr.DetachAll(t.Context()))

	inst := domain.NewModelInstance("inst-late", "late", "test/model", "", nil)
	client, err := fx.mgr.Attach(t.Context(), sess, inst)
	var draining *modelmanager.ManagerDrainingError
	require.ErrorAs(t, err, &draining)
	require.Equal(t, struct {
		client   protocol.Client
		draining *modelmanager.ManagerDrainingError
	}{
		client:   nil,
		draining: &modelmanager.ManagerDrainingError{InstanceID: inst.ID()},
	}, struct {
		client   protocol.Client
		draining *modelmanager.ManagerDrainingError
	}{
		client:   client,
		draining: draining,
	})
}

func TestManager_DetachAll_interrupts_an_attachment_loading_history(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fx := newTestManager(t, modelmanager.Config{APIClient: &apitest.Fake{}})
		blocking := &blockingHistoryStore{
			SQLiteStore: fx.store,
			started:     make(chan struct{}),
			release:     make(chan struct{}),
		}
		sess := session.New(t.Context, blocking, fx.mgr, nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

		inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
		inst.JoinChannel("#room", fixedTime)
		require.NoError(t, fx.store.SaveInstance(t.Context(), inst))

		type attachResult struct {
			client protocol.Client
			err    error
		}
		attached := make(chan attachResult, 1)
		go func() {
			client, err := fx.mgr.Attach(t.Context(), sess, inst)
			attached <- attachResult{client: client, err: err}
		}()
		<-blocking.started

		drained := make(chan error, 1)
		go func() { drained <- fx.mgr.DetachAll(t.Context()) }()
		synctest.Wait()
		close(blocking.release)

		result := <-attached
		var draining *modelmanager.ManagerDrainingError
		require.ErrorAs(t, result.err, &draining)
		require.Equal(t, struct {
			client    protocol.Client
			draining  *modelmanager.ManagerDrainingError
			drainErr  error
			connected bool
		}{
			client:   nil,
			draining: &modelmanager.ManagerDrainingError{InstanceID: inst.ID()},
		}, struct {
			client    protocol.Client
			draining  *modelmanager.ManagerDrainingError
			drainErr  error
			connected bool
		}{
			client:    result.client,
			draining:  draining,
			drainErr:  <-drained,
			connected: sess.LookupClient(protocol.ClientID(inst.ID())) != nil,
		})
	})
}

func TestManager_Detach_does_not_delete_memory(t *testing.T) {
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{APIClient: &apitest.Fake{}, Memory: spy})
	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := attachTestInstance(t, fx, sess, "inst-botty")

	fx.mgr.Detach(protocol.ClientID(inst.ID()))

	require.Equal(t, []domain.InstanceID(nil), spy.deletedIDs())
}

type pendingDeletionFailureStore struct {
	*storemod.SQLiteStore

	err error
}

func (s *pendingDeletionFailureStore) DeleteInstanceByID(context.Context, domain.InstanceID) error {
	return s.err
}

func TestManager_Start_excludes_a_pending_instance_when_its_retry_fails(t *testing.T) {
	ctx := t.Context()
	backing := storetest.NewMemoryStore(t)
	active := domain.NewModelInstance("inst-active", "active", "test/model", "", nil)
	pending := domain.NewModelInstance("inst-pending", "pending", "test/model", "", nil)
	require.NoError(t, backing.SaveInstance(ctx, active))
	require.NoError(t, backing.SaveInstance(ctx, pending))
	require.NoError(t, backing.MarkInstancePendingDeletion(ctx, pending.ID()))

	deleteErr := errors.New("delete pending instance")
	failing := &pendingDeletionFailureStore{SQLiteStore: backing, err: deleteErr}
	spy := &spyMemoryDeleter{}
	mgr := modelmanager.New(modelmanager.Config{
		Store:       failing,
		Memory:      spy,
		APIClient:   &apitest.Fake{},
		BaseContext: t.Context,
		Pacer:       &modelclient.Pacer{},
	})
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })
	sess := session.New(t.Context, failing, mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	err := mgr.Start(ctx, sess)
	require.ErrorIs(t, err, deleteErr)
	pendingIDs, listErr := backing.ListPendingInstanceDeletions(ctx)
	require.NoError(t, listErr)

	require.Equal(t, struct {
		activeConnected  bool
		pendingConnected bool
		pendingIDs       []domain.InstanceID
		forgotten        []domain.InstanceID
	}{
		activeConnected:  true,
		pendingConnected: false,
		pendingIDs:       []domain.InstanceID{pending.ID()},
		forgotten:        nil,
	}, struct {
		activeConnected  bool
		pendingConnected bool
		pendingIDs       []domain.InstanceID
		forgotten        []domain.InstanceID
	}{
		activeConnected:  sess.LookupClient(protocol.ClientID(active.ID())) != nil,
		pendingConnected: sess.LookupClient(protocol.ClientID(pending.ID())) != nil,
		pendingIDs:       pendingIDs,
		forgotten:        spy.deletedIDs(),
	})
}

func TestManager_Start_retries_pending_instance_deletion(t *testing.T) {
	ctx := t.Context()
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{APIClient: &apitest.Fake{}, Memory: spy})
	pending := domain.NewModelInstance("inst-pending", "pending", "test/model", "", nil)
	require.NoError(t, fx.store.SaveInstance(ctx, pending))
	require.NoError(t, fx.store.MarkInstancePendingDeletion(ctx, pending.ID()))

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
	require.NoError(t, fx.mgr.Start(ctx, sess))

	pendingIDs, err := fx.store.ListPendingInstanceDeletions(ctx)
	require.NoError(t, err)
	pendingMemoryIDs, err := fx.store.ListPendingMemoryDeletions(ctx)
	require.NoError(t, err)
	instances, err := fx.store.ListInstances(ctx)
	require.NoError(t, err)

	require.Equal(t, struct {
		connected bool
		pending   []domain.InstanceID
		memory    []domain.InstanceID
		instances []*domain.Instance
		forgotten []domain.InstanceID
	}{
		connected: false,
		pending:   nil,
		memory:    nil,
		instances: []*domain.Instance{},
		forgotten: []domain.InstanceID{pending.ID()},
	}, struct {
		connected bool
		pending   []domain.InstanceID
		memory    []domain.InstanceID
		instances []*domain.Instance
		forgotten []domain.InstanceID
	}{
		connected: sess.LookupClient(protocol.ClientID(pending.ID())) != nil,
		pending:   pendingIDs,
		memory:    pendingMemoryIDs,
		instances: instances,
		forgotten: spy.deletedIDs(),
	})
}

func TestManager_Start_retries_pending_memory_deletion(t *testing.T) {
	ctx := t.Context()
	deleteErr := errors.New("delete indexed memory")
	spy := &spyMemoryDeleter{err: deleteErr}
	fx := newTestManager(t, modelmanager.Config{APIClient: &apitest.Fake{}, Memory: spy})
	inst := domain.NewModelInstance("inst-gone", "gone", "test/model", "", nil)
	require.NoError(t, fx.store.SaveInstance(ctx, inst))
	require.NoError(t, fx.store.DeleteInstanceByID(ctx, inst.ID()))

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	err := fx.mgr.Start(ctx, sess)
	require.ErrorIs(t, err, deleteErr)
	pending, listErr := fx.store.ListPendingMemoryDeletions(ctx)
	require.NoError(t, listErr)
	require.Equal(t, struct {
		pending []domain.InstanceID
		deleted []domain.InstanceID
	}{
		pending: []domain.InstanceID{inst.ID()},
		deleted: []domain.InstanceID{inst.ID()},
	}, struct {
		pending []domain.InstanceID
		deleted []domain.InstanceID
	}{
		pending: pending,
		deleted: spy.deletedIDs(),
	})

	spy.setError(nil)
	require.NoError(t, fx.mgr.Start(ctx, sess))
	pending, listErr = fx.store.ListPendingMemoryDeletions(ctx)
	require.NoError(t, listErr)
	require.Equal(t, struct {
		pending []domain.InstanceID
		deleted []domain.InstanceID
	}{
		pending: nil,
		deleted: []domain.InstanceID{inst.ID(), inst.ID()},
	}, struct {
		pending []domain.InstanceID
		deleted []domain.InstanceID
	}{
		pending: pending,
		deleted: spy.deletedIDs(),
	})
}
