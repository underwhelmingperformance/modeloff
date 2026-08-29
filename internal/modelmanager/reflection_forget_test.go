package modelmanager

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
)

// recordingInstanceDeleter is a memory.Store that also implements
// memory.InstanceDeleter, recording every id DeleteInstance is called
// with. Every other method is a no-op: these tests only care about when
// Forget reaches the deleter, not about memory content.
type recordingInstanceDeleter struct {
	mu      sync.Mutex
	deleted []domain.InstanceID
}

func (d *recordingInstanceDeleter) Read(context.Context, domain.InstanceID) ([]memory.Entry, error) {
	return nil, nil
}

func (d *recordingInstanceDeleter) Write(context.Context, domain.InstanceID, memory.Entry) error {
	return nil
}

func (d *recordingInstanceDeleter) Delete(context.Context, domain.InstanceID, string) error {
	return nil
}

func (d *recordingInstanceDeleter) Reset(context.Context) error {
	return nil
}

func (d *recordingInstanceDeleter) DeleteInstance(_ context.Context, id domain.InstanceID) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.deleted = append(d.deleted, id)

	return nil
}

func (d *recordingInstanceDeleter) deletedIDs() []domain.InstanceID {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]domain.InstanceID(nil), d.deleted...)
}

var (
	_ memory.Store           = (*recordingInstanceDeleter)(nil)
	_ memory.InstanceDeleter = (*recordingInstanceDeleter)(nil)
)

// blockedReflections is an api client whose persona reflections block
// until the test releases them, ignoring the run's cancellation the way
// an upstream call already handed to the network does. Each call
// reports itself on started before blocking, and the nth call blocks on
// the nth release channel.
type blockedReflections struct {
	apitest.Fake

	mu       sync.Mutex
	calls    int
	started  chan struct{}
	releases []chan struct{}
}

func newBlockedReflections(releases ...chan struct{}) *blockedReflections {
	return &blockedReflections{
		Fake: apitest.Fake{
			ListModelsFn: func(context.Context) ([]api.ModelInfo, error) {
				return []api.ModelInfo{{
					ID:                  "test/reflection",
					SupportedParameters: []string{"tools", "structured_outputs"},
				}}, nil
			},
		},
		started:  make(chan struct{}),
		releases: releases,
	}
}

func (b *blockedReflections) ReflectPersona(
	ctx context.Context,
	_ domain.ModelID,
	_ domain.InstanceID,
	_ api.ReflectionInput,
	_ ...api.ToolDefinition,
) (api.ReflectionExploration, error) {
	b.mu.Lock()
	call := b.calls
	b.calls++
	b.mu.Unlock()

	b.started <- struct{}{}
	<-b.releases[call]

	return api.ReflectionExploration{}, ctx.Err()
}

type timedOutDisableCase struct {
	name     string
	reenable bool
}

type reflectionForgetEffect struct {
	DisableTimedOut       bool
	DeletedBeforeReleases []domain.InstanceID
	DeletedAfterLater     []domain.InstanceID
	DeletedAfterFirst     []domain.InstanceID
}

// TestManager_Forget_joins_reflection_after_a_timed_out_disable pins
// that deleting an instance still waits for a reflection run that
// outlived a disable whose stop timed out.
//
// The run holds a snapshot read before the deletion, and its derived
// writes would recreate the instance's indexed memory after the cleanup
// that removes it. A timed-out stop leaves the worker running, and
// re-enabling reflection afterwards starts a second scheduler; the
// deletion has to find the old worker in either state.
func TestManager_Forget_joins_reflection_after_a_timed_out_disable(t *testing.T) {
	cases := []timedOutDisableCase{
		{name: "disable stays off"},
		{name: "reflection re-enabled", reenable: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stored, instance := reflectionSchedulerStore(t)
				firstRelease := make(chan struct{})
				laterRelease := make(chan struct{})
				client := newBlockedReflections(firstRelease, laterRelease)
				deleter := &recordingInstanceDeleter{}
				mgr := New(Config{
					Store: stored, Memory: deleter, APIClient: client,
					InitialAPIKey: "configured", BaseContext: t.Context,
					ReflectionModel: "test/reflection",
				})
				t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })
				releaseFirst := sync.OnceFunc(func() { close(firstRelease) })
				releaseLater := sync.OnceFunc(func() { close(laterRelease) })
				t.Cleanup(releaseFirst)
				t.Cleanup(releaseLater)

				now := mgr.now()
				require.NoError(t, mgr.AppendReflectionEvents(
					t.Context(), instance.ID(),
					reflectionCandidates(1, reflectionSubstantiveThreshold, now), now,
				))
				<-client.started

				expiredCtx, expire := context.WithCancel(t.Context())
				expire()
				disableErr := mgr.SetReflectionMode(expiredCtx, ReflectionDisabled)

				if tc.reenable {
					require.NoError(t, mgr.SetReflectionMode(t.Context(), ReflectionActive))
					<-client.started
				}

				forgotten := make(chan struct{})
				go func() {
					mgr.Forget(protocol.ClientID(instance.ID()))
					close(forgotten)
				}()
				synctest.Wait()
				deletedBeforeReleases := deleter.deletedIDs()

				releaseLater()
				synctest.Wait()
				deletedAfterLater := deleter.deletedIDs()

				releaseFirst()
				<-forgotten
				deletedAfterFirst := deleter.deletedIDs()

				require.Equal(t, reflectionForgetEffect{
					DisableTimedOut:   true,
					DeletedAfterFirst: []domain.InstanceID{instance.ID()},
				}, reflectionForgetEffect{
					DisableTimedOut:       errors.Is(disableErr, context.Canceled),
					DeletedBeforeReleases: deletedBeforeReleases,
					DeletedAfterLater:     deletedAfterLater,
					DeletedAfterFirst:     deletedAfterFirst,
				})
			})
		})
	}
}

type detachAllReflectionEffect struct {
	DeletedBeforeRelease []domain.InstanceID
	DeletedAfterRelease  []domain.InstanceID
	DetachErr            error
}

// TestManager_DetachAll_deletes_memory_only_after_reflection_returns
// pins the drain-time ordering: a finaliser whose instance was deleted
// removes the instance's memory only once the reflection run still
// holding a pre-deletion snapshot has returned.
func TestManager_DetachAll_deletes_memory_only_after_reflection_returns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stored, instance := reflectionSchedulerStore(t)
		release := make(chan struct{})
		client := newBlockedReflections(release)
		deleter := &recordingInstanceDeleter{}
		mgr := New(Config{
			Store: stored, Memory: deleter, APIClient: client,
			InitialAPIKey: "configured", BaseContext: t.Context,
			ReflectionModel: "test/reflection",
		})
		t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })
		releaseRun := sync.OnceFunc(func() { close(release) })
		t.Cleanup(releaseRun)

		sess := session.New(t.Context(), stored, mgr, nil)
		t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
		require.NoError(t, sess.StartModelClients(t.Context()))

		now := mgr.now()
		require.NoError(t, mgr.AppendReflectionEvents(
			t.Context(), instance.ID(),
			reflectionCandidates(1, reflectionSubstantiveThreshold, now), now,
		))
		<-client.started

		require.NoError(t, stored.DeleteInstanceByID(t.Context(), instance.ID()))
		mgr.InstanceDeleted(protocol.ClientID(instance.ID()))

		detached := make(chan error, 1)
		go func() { detached <- mgr.DetachAll(t.Context()) }()
		synctest.Wait()
		deletedBeforeRelease := deleter.deletedIDs()

		releaseRun()
		detachErr := <-detached
		deletedAfterRelease := deleter.deletedIDs()

		require.Equal(t, detachAllReflectionEffect{
			DeletedAfterRelease: []domain.InstanceID{instance.ID()},
		}, detachAllReflectionEffect{
			DeletedBeforeRelease: deletedBeforeRelease,
			DeletedAfterRelease:  deletedAfterRelease,
			DetachErr:            detachErr,
		})
	})
}
