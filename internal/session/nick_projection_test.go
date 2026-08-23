package session

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

type nickProjectionError struct{}

func (*nickProjectionError) Error() string { return "persist nick projection" }

type failingNickProjectionStore struct{ Store }

func (s *failingNickProjectionStore) CommitActorRename(
	ctx context.Context,
	rename storemod.ActorRename,
) (storemod.CommittedActorRename, error) {
	if len(rename.Scrollback) > 0 {
		return storemod.CommittedActorRename{}, &nickProjectionError{}
	}

	return s.Store.CommitActorRename(ctx, rename)
}

type nickProjectionState struct {
	Response           protocol.Response
	LiveUserNick       domain.Nick
	PersistedUserError error
	PersistedUser      comparableInstance
	PersistedModelErr  error
	PersistedModel     comparableInstance
	WindowError        error
	Window             domain.Window
	AuditError         error
	Audit              []domain.StoredEvent
	ProjectionError    error
	Projection         []domain.StoredEvent
	ModelConnected     bool
	Deliveries         []protocol.Delivery
}

func TestSession_nick_projection_failure_rolls_back_the_rename(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, backing := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		model := seedInstanceRow(t, backing, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})
		client := &passiveClient{id: protocol.ClientID(model.ID())}
		subscription, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)
		client.sub = subscription
		joinResponse, err := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{Events: []protocol.Event{
			domain.JoinedChannel{Channel: "#general"},
		}}, joinResponse)
		subscription.Activate()
		synctest.Wait()
		collectSubscriptionDeliveries(subscription)

		auditBefore, err := backing.EventsBefore(ctx, "#general", nil, 100)
		require.NoError(t, err)
		projectionBefore, err := backing.ChannelScrollback(
			ctx, model.ID(), "#general", 100,
		)
		require.NoError(t, err)
		windowBefore, err := backing.GetWindow(ctx, "#general")
		require.NoError(t, err)
		expectedUser := normaliseInstance(userInstance(t, sess))
		expectedModel := normaliseInstance(model)

		sess.store = &failingNickProjectionStore{Store: backing}
		response, sendErr := userClient(t, sess).Send(ctx, protocol.Nick{New: "renamed"})
		synctest.Wait()

		var projectionErr *nickProjectionError
		require.ErrorAs(t, sendErr, &projectionErr)

		persistedUser, userErr := backing.GetInstanceByID(ctx, protocol.UserClientID)
		persistedModel, modelErr := backing.GetInstanceByID(ctx, model.ID())
		window, windowErr := backing.GetWindow(ctx, "#general")
		audit, auditErr := backing.EventsBefore(ctx, "#general", nil, 100)
		projection, projectionErrRead := backing.ChannelScrollback(
			ctx, model.ID(), "#general", 100,
		)

		require.Equal(t, nickProjectionState{
			LiveUserNick:   "testuser",
			PersistedUser:  expectedUser,
			PersistedModel: expectedModel,
			Window:         windowBefore,
			Audit:          auditBefore,
			Projection:     projectionBefore,
			ModelConnected: true,
		}, nickProjectionState{
			Response:           response,
			LiveUserNick:       userNick(t, sess),
			PersistedUserError: userErr,
			PersistedUser:      normaliseInstance(persistedUser),
			PersistedModelErr:  modelErr,
			PersistedModel:     normaliseInstance(persistedModel),
			WindowError:        windowErr,
			Window:             window,
			AuditError:         auditErr,
			Audit:              audit,
			ProjectionError:    projectionErrRead,
			Projection:         projection,
			ModelConnected:     sess.ClientConnected(protocol.ClientID(model.ID())),
			Deliveries:         collectSubscriptionDeliveries(subscription),
		})

		nickChange := domain.NickChange{
			Source:  domain.ClientSource(protocol.UserClientID, "testuser"),
			NewNick: "renamed",
			At:      fixedTime,
		}
		expectedRenamedUser := userInstance(t, sess).Snapshot()
		expectedRenamedUser.SetNick("renamed")
		expectedWindow := windowBefore.(*domain.ChannelWindow).Clone()
		expectedWindow.Members.RenameID(protocol.UserClientID, "renamed")
		expectedAudit := append(slices.Clone(auditBefore), domain.StoredEvent{
			ID: auditBefore[len(auditBefore)-1].ID + 1, Event: nickChange,
		})
		expectedProjection := append(slices.Clone(projectionBefore), domain.StoredEvent{
			ID: projectionBefore[len(projectionBefore)-1].ID + 1, Event: nickChange,
		})

		sess.store = backing
		retryResponse, retryErr := userClient(t, sess).Send(ctx, protocol.Nick{New: "renamed"})
		require.NoError(t, retryErr)
		synctest.Wait()

		persistedUser, userErr = backing.GetInstanceByID(ctx, protocol.UserClientID)
		persistedModel, modelErr = backing.GetInstanceByID(ctx, model.ID())
		window, windowErr = backing.GetWindow(ctx, "#general")
		audit, auditErr = backing.EventsBefore(ctx, "#general", nil, 100)
		projection, projectionErrRead = backing.ChannelScrollback(
			ctx, model.ID(), "#general", 100,
		)

		require.Equal(t, nickProjectionState{
			LiveUserNick:   "renamed",
			PersistedUser:  normaliseInstance(expectedRenamedUser),
			PersistedModel: expectedModel,
			Window:         domain.Window(expectedWindow),
			Audit:          expectedAudit,
			Projection:     expectedProjection,
			ModelConnected: true,
			Deliveries: []protocol.Delivery{{
				Event: nickChange, Targets: []domain.ChannelName{"#general"},
			}},
		}, nickProjectionState{
			Response:           retryResponse,
			LiveUserNick:       userNick(t, sess),
			PersistedUserError: userErr,
			PersistedUser:      normaliseInstance(persistedUser),
			PersistedModelErr:  modelErr,
			PersistedModel:     normaliseInstance(persistedModel),
			WindowError:        windowErr,
			Window:             window,
			AuditError:         auditErr,
			Audit:              audit,
			ProjectionError:    projectionErrRead,
			Projection:         projection,
			ModelConnected:     sess.ClientConnected(protocol.ClientID(model.ID())),
			Deliveries:         collectSubscriptionDeliveries(subscription),
		})
	})
}
