package modelmanager_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
)

// spyMemoryDeleter is a memory.Store that also implements
// memory.InstanceDeleter, recording every id DeleteInstance is
// called with. Every other method is a no-op: these tests only care
// about whether Detach reaches the deleter, not about memory content.
type spyMemoryDeleter struct {
	mu      sync.Mutex
	deleted []domain.InstanceID
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

	return nil
}

func (s *spyMemoryDeleter) deletedIDs() []domain.InstanceID {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]domain.InstanceID(nil), s.deleted...)
}

var (
	_ memory.Store           = (*spyMemoryDeleter)(nil)
	_ memory.InstanceDeleter = (*spyMemoryDeleter)(nil)
)

// attachTestInstance saves and attaches a model instance under fx's
// manager, returning it for the caller to Detach.
func attachTestInstance(t *testing.T, fx *managerFixture, sess *session.Session, id domain.InstanceID) *domain.Instance {
	t.Helper()

	inst := domain.NewModelInstance(id, "botty", "test/model", "", nil)
	require.NoError(t, fx.store.SaveInstance(t.Context(), inst))

	_, err := fx.mgr.Attach(t.Context(), sess, inst)
	require.NoError(t, err)

	return inst
}

// TestManager_Detach_deletes_the_instances_memory_collection covers
// the InstanceDeleter connection point (queued finding: deleting an
// instance left its chromem collection behind). Session.releaseClient
// is the only production caller of ModelClientFactory.Detach, and it
// runs for exactly the paths that also delete the instance's store
// row (QUIT, KILL, a send-queue disconnect, a failed ADDMODEL). Wiring
// the deleter here, and not into DetachAll's bulk release at
// shutdown, is what keeps an ordinary drain from wiping every
// attached instance's memories on exit.
func TestManager_Detach_deletes_the_instances_memory_collection(t *testing.T) {
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{APIClient: &fakeAPIClient{}, Memory: spy})

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := attachTestInstance(t, fx, sess, "inst-botty")

	fx.mgr.Detach(protocol.ClientID(inst.ID()))

	require.Equal(t, []domain.InstanceID{"inst-botty"}, spy.deletedIDs())
}

// TestManager_Detach_tolerates_a_memory_store_without_InstanceDeleter
// covers the plain, non-indexed fallback store NewDefaultStore
// returns when the vector index cannot be opened: it satisfies
// memory.Store but not memory.InstanceDeleter, and Detach must not
// fail or panic against one.
func TestManager_Detach_tolerates_a_memory_store_without_InstanceDeleter(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	fx := newTestManager(t, modelmanager.Config{
		Store:     s,
		APIClient: &fakeAPIClient{},
		Memory:    memory.NewStoreAdapter(s),
	})

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := attachTestInstance(t, fx, sess, "inst-botty")

	require.NotPanics(t, func() { fx.mgr.Detach(protocol.ClientID(inst.ID())) })
}

// TestManager_DetachAll_does_not_delete_memory_collections pins the
// negative half of the InstanceDeleter connection point: DetachAll's
// bulk release runs for every client attached at shutdown regardless
// of why the process is exiting, and none of those clients had their
// instance row deleted just because the process is stopping. Unlike
// Detach, which fires only for the paths that also delete the row
// (QUIT, KILL, a send-queue disconnect, a failed ADDMODEL), DetachAll
// must never reach the deleter.
func TestManager_DetachAll_does_not_delete_memory_collections(t *testing.T) {
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{APIClient: &fakeAPIClient{}, Memory: spy})

	sess := session.New(t.Context, fx.store, fx.mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	attachTestInstance(t, fx, sess, "inst-botty")

	require.NoError(t, fx.mgr.DetachAll(t.Context()))

	require.Empty(t, spy.deletedIDs())
}

// TestManager_Detach_unknown_id_is_a_noop pins that Detach's existing
// idempotency on an unknown id extends to the deleter: nothing was
// ever attached, so nothing should be told to delete anything.
func TestManager_Detach_unknown_id_is_a_noop(t *testing.T) {
	spy := &spyMemoryDeleter{}
	fx := newTestManager(t, modelmanager.Config{APIClient: &fakeAPIClient{}, Memory: spy})

	fx.mgr.Detach(protocol.ClientID("inst-nobody"))

	require.Empty(t, spy.deletedIDs())
}
