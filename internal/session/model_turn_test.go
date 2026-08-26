package session

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

type blockedModelTurnStore struct {
	Store

	entered chan struct{}
	release chan struct{}
}

type blockedQuitPropagationStore struct {
	Store

	deleted chan struct{}
	release chan struct{}
}

type blockedInstanceDeletionStore struct {
	Store

	deleted chan struct{}
	release chan struct{}
}

type blockedSeparateProjectionStore struct {
	Store

	attempted chan []storemod.ChannelScrollbackRecord
	release   chan struct{}
}

type routedPassiveClient struct {
	protocol.Client

	id   protocol.ClientID
	sess *Session
	sub  protocol.Subscription
}

func (c *routedPassiveClient) Identity() protocol.ClientID { return c.id }
func (c *routedPassiveClient) Send(
	ctx context.Context,
	cmd protocol.Command,
) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}

func (c *routedPassiveClient) Events() <-chan protocol.Delivery {
	return c.sub.Events()
}

func (s *blockedInstanceDeletionStore) CommitInstanceDeletion(
	ctx context.Context,
	deletion storemod.InstanceDeletion,
) (storemod.CommittedInstanceDeletion, error) {
	committed, err := s.Store.CommitInstanceDeletion(ctx, deletion)
	if err != nil {
		return storemod.CommittedInstanceDeletion{}, err
	}

	close(s.deleted)

	select {
	case <-s.release:
		return committed, nil
	case <-ctx.Done():
		return storemod.CommittedInstanceDeletion{}, ctx.Err()
	}
}

func (s *blockedSeparateProjectionStore) AppendChannelScrollback(
	ctx context.Context,
	records []storemod.ChannelScrollbackRecord,
) ([]int64, error) {
	if len(records) == 0 {
		return s.Store.AppendChannelScrollback(ctx, records)
	}

	select {
	case s.attempted <- slices.Clone(records):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return s.Store.AppendChannelScrollback(ctx, records)
}

func TestSession_quit_commits_recipient_projection_with_deletion(t *testing.T) {
	testSessionQuitCommitsRecipientProjectionWithDeletion(t, false)
}

func TestSession_anonymous_quit_commits_masked_projection_with_deletion(t *testing.T) {
	testSessionQuitCommitsRecipientProjectionWithDeletion(t, true)
}

func testSessionQuitCommitsRecipientProjectionWithDeletion(t *testing.T, anonymous bool) {
	t.Helper()

	synctest.Test(t, func(t *testing.T) {
		sess, backing := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		if anonymous {
			window, err := sess.loadChannelWindow(ctx, "#dev")
			require.NoError(t, err)
			window.Modes.Anonymous = true
			require.NoError(t, sess.persistChannelWindow(ctx, window))
		}

		peer := seedInstanceRow(t, backing, instanceSpec{
			Nick: "peer", ModelID: "test/model",
		})
		peerClient := &passiveClient{id: protocol.ClientID(peer.ID())}
		peerSub, err := subscribeTestClient(ctx, t, sess, peerClient,
			protocol.SubscribeOptions{ReplayHistory: true})
		require.NoError(t, err)
		peerClient.sub = peerSub
		peerSub.Activate()
		require.NoError(t, joinAs(ctx, sess, peer, "#dev", ""))

		actor, actorClient := seedPassiveInstance(t, sess, "botty", "test/model")
		actorClient.sub.Activate()
		require.NoError(t, joinAs(ctx, sess, actor, "#dev", ""))
		synctest.Wait()
		drainDeliveries(peerClient)

		auditBefore, err := backing.EventsBefore(ctx, "#dev", nil, 100)
		require.NoError(t, err)
		projectionBefore, err := backing.ChannelScrollback(ctx, peer.ID(), "#dev", 100)
		require.NoError(t, err)

		blocked := &blockedSeparateProjectionStore{
			Store: sess.store, attempted: make(chan []storemod.ChannelScrollbackRecord),
			release: make(chan struct{}),
		}
		sess.store = blocked

		type quitResult struct {
			Response protocol.Response
			Err      error
		}
		result := make(chan quitResult, 1)
		go func() {
			response, quitErr := sess.Handle(ctx, actorClient, protocol.Quit{Reason: "gone"})
			result <- quitResult{Response: response, Err: quitErr}
		}()

		var (
			actualResult   quitResult
			pendingRecords []storemod.ChannelScrollbackRecord
			separateAppend bool
		)
		select {
		case actualResult = <-result:
		case pendingRecords = <-blocked.attempted:
			separateAppend = true
		}

		_, actorErr := backing.GetInstanceByID(ctx, actor.ID())
		audit, auditErr := backing.EventsBefore(ctx, "#dev", nil, 100)
		projection, projectionErr := backing.ChannelScrollback(ctx, peer.ID(), "#dev", 100)

		if separateAppend {
			close(blocked.release)
			actualResult = <-result
		} else {
			close(blocked.release)
		}
		synctest.Wait()

		quit := domain.Quit{
			Source:  domain.ClientSource(actor.ID(), actor.Nick()),
			Message: "gone",
			At:      fixedTime,
		}
		var projectedEvent domain.PersistableEvent = quit
		var deliveryEvent protocol.Event = quit
		deliveryTargets := []domain.ChannelName{"#dev"}
		if anonymous {
			part := domain.Part{
				Source: domain.AnonymousSource(), Target: "#dev", Message: "gone", At: fixedTime,
			}
			projectedEvent = part
			deliveryEvent = part
			deliveryTargets = nil
		}
		wantAudit := append(slices.Clone(auditBefore), domain.StoredEvent{
			ID: auditBefore[len(auditBefore)-1].ID + 1, Event: projectedEvent,
		})
		wantProjection := append(slices.Clone(projectionBefore), domain.StoredEvent{
			ID: projectionBefore[len(projectionBefore)-1].ID + 1, Event: projectedEvent,
		})
		type assertionSnapshot struct {
			Result          quitResult
			SeparateAppend  bool
			PendingRecords  []storemod.ChannelScrollbackRecord
			ActorDeleted    bool
			AuditError      error
			Audit           []domain.StoredEvent
			ProjectionError error
			Projection      []domain.StoredEvent
			Deliveries      []protocol.Delivery
		}

		require.Equal(t, assertionSnapshot{
			ActorDeleted: true,
			Audit:        wantAudit,
			Projection:   wantProjection,
			Deliveries: []protocol.Delivery{{
				Event: deliveryEvent, Targets: deliveryTargets,
			}},
		}, assertionSnapshot{
			Result:          actualResult,
			SeparateAppend:  separateAppend,
			PendingRecords:  pendingRecords,
			ActorDeleted:    actorErr != nil,
			AuditError:      auditErr,
			Audit:           audit,
			ProjectionError: projectionErr,
			Projection:      projection,
			Deliveries:      collectProtocolDeliveries(peerClient),
		})
	})
}

func TestSession_quit_serialises_committed_projection_with_replay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, backing := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		peer := seedInstanceRow(t, backing, instanceSpec{
			Nick: "peer", ModelID: "test/model",
		})
		peerClient := &passiveClient{id: protocol.ClientID(peer.ID())}
		peerSub, err := subscribeTestClient(ctx, t, sess, peerClient,
			protocol.SubscribeOptions{ReplayHistory: true})
		require.NoError(t, err)
		peerClient.sub = peerSub
		require.NoError(t, joinAs(ctx, sess, peer, "#dev", ""))
		joinedWindow, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		joinedMembers := joinedWindow.Members.Clone()

		actor, actorClient := seedPassiveInstance(t, sess, "botty", "test/model")
		actorClient.sub.Activate()
		require.NoError(t, joinAs(ctx, sess, actor, "#dev", ""))

		blocked := &blockedQuitPropagationStore{
			Store: sess.store, deleted: make(chan struct{}), release: make(chan struct{}),
		}
		sess.store = blocked

		type quitResult struct {
			Response protocol.Response
			Err      error
		}
		result := make(chan quitResult, 1)
		go func() {
			response, quitErr := sess.Handle(ctx, actorClient, protocol.Quit{Reason: "gone"})
			result <- quitResult{Response: response, Err: quitErr}
		}()
		<-blocked.deleted

		serverSub := peerSub.(*serverClient)
		projectionUnlocked := serverSub.replayMu.TryLock()
		if projectionUnlocked {
			serverSub.replayMu.Unlock()
		}
		close(blocked.release)
		actualResult := <-result

		snapshot, snapshotErr := peerSub.Scrollback(
			ctx, protocol.ChannelWindowTarget("#dev"), 100,
		)
		peerSub.Activate()
		synctest.Wait()

		peerJoin := domain.Join{
			Source: domain.ClientSource(peer.ID(), peer.Nick()), Target: "#dev", At: fixedTime,
		}
		actorJoin := domain.Join{
			Source: domain.ClientSource(actor.ID(), actor.Nick()), Target: "#dev", At: fixedTime,
		}
		quit := domain.Quit{
			Source: domain.ClientSource(actor.ID(), actor.Nick()), Message: "gone", At: fixedTime,
		}
		type assertionSnapshot struct {
			Result             quitResult
			ProjectionUnlocked bool
			SnapshotError      error
			Snapshot           []protocol.ScrollbackEntry
			Deliveries         []protocol.Delivery
		}

		require.Equal(t, assertionSnapshot{
			Snapshot: []protocol.ScrollbackEntry{},
			Deliveries: []protocol.Delivery{
				{Event: peerJoin},
				{Event: domain.NamesReplyEvent{
					Channel: "#dev", Members: joinedMembers, At: fixedTime,
				}},
				{Event: domain.NamesEnd{Channel: "#dev", At: fixedTime}},
				{Event: actorJoin},
				{Event: quit, Targets: []domain.ChannelName{"#dev"}},
			},
		}, assertionSnapshot{
			Result:             actualResult,
			ProjectionUnlocked: projectionUnlocked,
			SnapshotError:      snapshotErr,
			Snapshot:           snapshot,
			Deliveries:         collectProtocolDeliveries(peerClient),
		})
	})
}

func TestWindowGuard_rechecks_command_authority_on_the_writer(t *testing.T) {
	sess, backing := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, backing.SaveInstance(ctx, botty))
	client := &routedPassiveClient{
		id: protocol.ClientID(botty.ID()), sess: sess,
	}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)
	client.sub = sub
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))

	guard, err := sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)
	kickResponse, err := userClient(t, sess).Send(ctx, protocol.Kick{
		Nick: "botty", Channel: "#dev",
	})
	require.NoError(t, err)
	require.NoError(t, kickResponse.Err)

	response, sendErr := guard.Send(ctx, client, protocol.Nick{New: "renamed"})
	stored, storedErr := backing.GetInstanceByID(ctx, botty.ID())
	type assertionSnapshot struct {
		Response         protocol.Response
		AuthorityChanged bool
		LiveNick         domain.Nick
		StoredError      error
		StoredNick       domain.Nick
	}

	require.Equal(t, assertionSnapshot{
		AuthorityChanged: true,
		LiveNick:         "botty",
		StoredNick:       "botty",
	}, assertionSnapshot{
		Response:         response,
		AuthorityChanged: errors.Is(sendErr, protocol.ErrWindowAuthorityChanged),
		LiveNick:         botty.Nick(),
		StoredError:      storedErr,
		StoredNick:       stored.Nick(),
	})
}

func (s *blockedQuitPropagationStore) CommitInstanceDeletion(
	ctx context.Context,
	deletion storemod.InstanceDeletion,
) (storemod.CommittedInstanceDeletion, error) {
	committed, err := s.Store.CommitInstanceDeletion(ctx, deletion)
	if err == nil {
		close(s.deleted)
	}
	select {
	case <-s.release:
		return committed, err
	case <-ctx.Done():
		return storemod.CommittedInstanceDeletion{}, ctx.Err()
	}
}

func (s *blockedModelTurnStore) BeginModelTurn(
	ctx context.Context,
	turn storemod.ModelTurn,
	input storemod.ModelTurnEntry,
) (storemod.ModelTurnID, error) {
	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	return s.Store.BeginModelTurn(ctx, turn, input)
}

func TestSession_model_turn_admission_orders_with_channel_revocation(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
	guard, err := client.sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)

	blocked := &blockedModelTurnStore{
		Store: sess.store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked

	type beginResult struct {
		Recorder storemod.ModelTurnRecorder
		Err      error
	}
	begun := make(chan beginResult, 1)
	go func() {
		recorder, beginErr := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
			InstanceID: botty.ID(),
			Window:     protocol.ChannelWindowTarget("#dev"),
			ModelID:    botty.ModelID,
			StartedAt:  fixedTime,
		}, storemod.ModelTurnEntry{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"before part"}`),
			At:   fixedTime,
		})
		begun <- beginResult{Recorder: recorder, Err: beginErr}
	}()
	<-blocked.entered

	parted := make(chan error, 1)
	go func() {
		resp, sendErr := sess.Handle(ctx, client, protocol.Part{
			Channel: "#dev", Reason: "leaving",
		})
		parted <- errors.Join(sendErr, resp.Err)
	}()
	close(blocked.release)

	begin := <-begun
	require.NoError(t, begin.Err)
	require.NoError(t, <-parted)

	appendErr := begin.Recorder.AppendModelTurnEntry(ctx, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnAssistant,
		Seq:  1,
		Data: []byte(`{"text":"after part"}`),
		At:   fixedTime,
	})
	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)

	type assertionSnapshot struct {
		GuardValid bool
		AppendErr  error
		EntriesErr error
		Entries    []storemod.ModelTurnEntry
	}

	require.Equal(t, assertionSnapshot{
		AppendErr: storemod.ErrModelTurnClosed,
		Entries: []storemod.ModelTurnEntry{{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"before part"}`),
			At:   fixedTime,
		}},
	}, assertionSnapshot{
		GuardValid: guard.Valid(ctx),
		AppendErr:  appendErr,
		EntriesErr: entriesErr,
		Entries:    entries,
	})
}

func TestSession_failed_departure_keeps_the_channel_turn_open(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
	guard, err := client.sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)
	recorder, err := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    botty.ModelID,
		StartedAt:  fixedTime,
	}, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"before part"}`),
		At:   fixedTime,
	})
	require.NoError(t, err)

	departureErr := errors.New("commit departure failed")
	sess.store = &failingChannelDepartureStore{Store: sess.store, err: departureErr}
	resp, sendErr := sess.Handle(ctx, client, protocol.Part{Channel: "#dev"})
	require.ErrorIs(t, sendErr, departureErr)
	require.Equal(t, protocol.Response{}, resp)

	assistant := storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnAssistant,
		Data: []byte(`{"text":"still current"}`),
		At:   fixedTime,
	}
	require.NoError(t, recorder.AppendModelTurnEntry(ctx, assistant))

	entries, err := sqliteStore.ModelTurnEntries(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, []storemod.ModelTurnEntry{
		{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"before part"}`),
			At:   fixedTime,
		},
		assistant,
	}, entries)
	require.True(t, guard.Valid(ctx))
}

func TestSession_model_turn_uses_the_canonical_channel_for_revocation(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#Dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
	guard, err := client.sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)
	recorder, err := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    botty.ModelID,
		StartedAt:  fixedTime,
	}, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"mixed case"}`),
		At:   fixedTime,
	})
	require.NoError(t, err)
	require.NotNil(t, recorder)

	resp, sendErr := sess.Handle(ctx, client, protocol.Part{Channel: "#dev"})
	require.NoError(t, sendErr)
	require.NoError(t, resp.Err)

	appendErr := recorder.AppendModelTurnEntry(ctx, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnAssistant,
		Seq:  1,
		Data: []byte(`{"text":"too late"}`),
		At:   fixedTime,
	})
	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)

	type assertionSnapshot struct {
		AppendErr  error
		EntriesErr error
		Entries    []storemod.ModelTurnEntry
	}

	require.Equal(t, assertionSnapshot{
		AppendErr: storemod.ErrModelTurnClosed,
		Entries: []storemod.ModelTurnEntry{{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"mixed case"}`),
			At:   fixedTime,
		}},
	}, assertionSnapshot{
		AppendErr:  appendErr,
		EntriesErr: entriesErr,
		Entries:    entries,
	})
}

func TestSession_model_turn_to_user_closes_when_the_user_quits(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	guard, err := client.sub.GuardWindow(
		ctx,
		protocol.DirectWindowTarget(protocol.UserClientID),
	)
	require.NoError(t, err)
	recorder, err := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.DirectWindowTarget(protocol.UserClientID),
		ModelID:    botty.ModelID,
		StartedAt:  fixedTime,
	}, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"private"}`),
		At:   fixedTime,
	})
	require.NoError(t, err)

	resp, sendErr := userClient(t, sess).Send(ctx, protocol.Quit{Reason: "leaving"})
	require.NoError(t, sendErr)
	require.NoError(t, resp.Err)

	err = recorder.AppendModelTurnEntry(ctx, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnAssistant,
		Data: []byte(`{"text":"too late"}`),
		At:   fixedTime,
	})
	require.ErrorIs(t, err, storemod.ErrModelTurnClosed)

	entries, err := sqliteStore.ModelTurnEntries(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSession_peer_deletion_closes_direct_turn_admission_before_quit_propagation(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	target, targetClient := seedPassiveInstance(t, sess, "target", "test/model")
	require.NoError(t, joinAs(ctx, sess, target, "#dev", ""))
	_, peer := seedPassiveInstance(t, sess, "peer", "test/model")
	blocked := &blockedQuitPropagationStore{
		Store: sess.store, deleted: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked

	type killResult struct {
		Response protocol.Response
		Err      error
	}
	killed := make(chan killResult, 1)
	go func() {
		response, err := userClient(t, sess).Send(ctx, protocol.Kill{
			Nick: target.Nick(), Reason: "done",
		})
		killed <- killResult{Response: response, Err: err}
	}()
	<-blocked.deleted

	type guardResult struct {
		Guard protocol.WindowGuard
		Err   error
	}
	guarded := make(chan guardResult, 1)
	go func() {
		guard, guardErr := peer.sub.GuardWindow(
			ctx,
			protocol.DirectWindowTarget(target.ID()),
		)
		guarded <- guardResult{Guard: guard, Err: guardErr}
	}()
	targetSub := targetClient.sub.(*serverClient)
	deleteUnlocked := targetSub.replayMu.TryLock()
	if deleteUnlocked {
		targetSub.replayMu.Unlock()
	}
	require.False(t, deleteUnlocked)
	close(blocked.release)
	result := <-killed
	gotGuard := <-guarded
	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)

	var unknown domain.UnknownNickError
	require.ErrorAs(t, gotGuard.Err, &unknown)
	type assertionSnapshot struct {
		Guard      protocol.WindowGuard
		Response   protocol.Response
		KillError  error
		Entries    []storemod.ModelTurnEntry
		EntriesErr error
	}

	require.Equal(t, assertionSnapshot{
		Response: protocol.Response{},
	}, assertionSnapshot{
		Guard:      gotGuard.Guard,
		Response:   result.Response,
		KillError:  result.Err,
		Entries:    entries,
		EntriesErr: entriesErr,
	})
}

func TestSession_last_member_deletion_closes_invitation_turn_admission(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#solo"))

	botty, bottyClient := seedPassiveInstance(t, sess, "botty", "test/model")
	response, err := userClient(t, sess).Send(ctx, protocol.Invite{
		Nick: botty.Nick(), Channel: "#solo",
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	guard, err := bottyClient.sub.GuardInvitation(ctx, "#solo")
	require.NoError(t, err)

	blocked := &blockedInstanceDeletionStore{
		Store: sess.store, deleted: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked

	type commandResult struct {
		Response protocol.Response
		Err      error
	}
	quit := make(chan commandResult, 1)
	go func() {
		response, quitErr := userClient(t, sess).Send(ctx, protocol.Quit{Reason: "done"})
		quit <- commandResult{Response: response, Err: quitErr}
	}()
	<-blocked.deleted

	type beginResult struct {
		Recorder storemod.ModelTurnRecorder
		Err      error
	}
	begun := make(chan beginResult, 1)
	go func() {
		recorder, beginErr := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
			InstanceID: botty.ID(),
			Window:     protocol.ChannelWindowTarget("#solo"),
			ModelID:    botty.ModelID,
			StartedAt:  fixedTime,
		}, storemod.ModelTurnEntry{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"after deletion"}`),
			At:   fixedTime,
		})
		begun <- beginResult{Recorder: recorder, Err: beginErr}
	}()
	close(blocked.release)

	begin := <-begun
	quitResult := <-quit
	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)
	_, channelErr := sqliteStore.GetWindow(ctx, "#solo")
	var notOnChannel domain.NotOnChannelError

	require.ErrorAs(t, begin.Err, &notOnChannel)
	require.ErrorIs(t, channelErr, storemod.ErrNoSuchChannel)
	type assertionSnapshot struct {
		RecorderPresent bool
		Response        protocol.Response
		QuitError       error
		Entries         []storemod.ModelTurnEntry
		EntriesError    error
	}

	require.Equal(t, assertionSnapshot{
		Response: protocol.Response{},
	}, assertionSnapshot{
		RecorderPresent: begin.Recorder != nil,
		Response:        quitResult.Response,
		QuitError:       quitResult.Err,
		Entries:         entries,
		EntriesError:    entriesErr,
	})
}

func TestSession_invitation_turn_admission_orders_with_channel_revocation(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#solo"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	response, err := userClient(t, sess).Send(ctx, protocol.Invite{
		Nick: botty.Nick(), Channel: "#solo",
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	guard, err := client.sub.GuardInvitation(ctx, "#solo")
	require.NoError(t, err)

	blocked := &blockedModelTurnStore{
		Store: sess.store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked

	type beginResult struct {
		Recorder storemod.ModelTurnRecorder
		Err      error
	}
	begun := make(chan beginResult, 1)
	go func() {
		recorder, beginErr := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
			InstanceID: botty.ID(),
			Window:     protocol.ChannelWindowTarget("#solo"),
			ModelID:    botty.ModelID,
			StartedAt:  fixedTime,
		}, storemod.ModelTurnEntry{
			Kind: storemod.ModelTurnInput,
			Data: []byte(`{"input":"before part"}`),
			At:   fixedTime,
		})
		begun <- beginResult{Recorder: recorder, Err: beginErr}
	}()
	<-blocked.entered

	channelStateUnlocked := sess.channels.mu.TryLock()
	if channelStateUnlocked {
		sess.channels.mu.Unlock()
	}
	close(blocked.release)
	begin := <-begun
	partErr := userPart(ctx, t, sess, "#solo", "done")
	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)
	_, channelErr := sqliteStore.GetWindow(ctx, "#solo")

	type assertionSnapshot struct {
		RecorderPresent                   bool
		BeginError                        error
		ChannelStateLockedDuringAdmission bool
		PartError                         error
		Entries                           []storemod.ModelTurnEntry
		EntriesError                      error
		ChannelAbsent                     bool
		GuardValid                        bool
	}

	require.Equal(t, assertionSnapshot{
		RecorderPresent:                   true,
		ChannelStateLockedDuringAdmission: true,
		ChannelAbsent:                     true,
	}, assertionSnapshot{
		RecorderPresent:                   begin.Recorder != nil,
		BeginError:                        begin.Err,
		ChannelStateLockedDuringAdmission: !channelStateUnlocked,
		PartError:                         partErr,
		Entries:                           entries,
		EntriesError:                      entriesErr,
		ChannelAbsent:                     errors.Is(channelErr, storemod.ErrNoSuchChannel),
		GuardValid:                        guard.Valid(ctx),
	})
}

func TestSession_instance_deletion_orders_with_direct_turn_admission(t *testing.T) {
	tests := []struct {
		name       string
		turnOwner  string
		wantClosed bool
		wantGone   bool
	}{
		{name: "departing actor owns the turn", turnOwner: "target", wantClosed: true},
		{name: "peer owns the turn", turnOwner: "peer", wantGone: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess, sqliteStore := newTestSession(t)
				ctx := t.Context()

				target, targetClient := seedPassiveInstance(t, sess, "target", "test/model")
				peer, peerClient := seedPassiveInstance(t, sess, "peer", "test/model")

				var (
					owner       *domain.Instance
					ownerClient *passiveClient
					window      protocol.WindowTarget
				)
				if tt.turnOwner == "target" {
					owner = target
					ownerClient = targetClient
					window = protocol.DirectWindowTarget(protocol.UserClientID)
				} else {
					owner = peer
					ownerClient = peerClient
					window = protocol.DirectWindowTarget(target.ID())
				}

				guard, err := ownerClient.sub.GuardWindow(ctx, window)
				require.NoError(t, err)

				blocked := &blockedInstanceDeletionStore{
					Store: sess.store, deleted: make(chan struct{}), release: make(chan struct{}),
				}
				sess.store = blocked

				type commandResult struct {
					Response protocol.Response
					Err      error
				}
				killed := make(chan commandResult, 1)
				go func() {
					response, killErr := userClient(t, sess).Send(ctx, protocol.Kill{
						Nick: target.Nick(), Reason: "done",
					})
					killed <- commandResult{Response: response, Err: killErr}
				}()
				<-blocked.deleted

				type beginResult struct {
					Recorder storemod.ModelTurnRecorder
					Err      error
				}
				begun := make(chan beginResult, 1)
				go func() {
					recorder, beginErr := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
						InstanceID: owner.ID(),
						Window:     window,
						ModelID:    owner.ModelID,
						StartedAt:  fixedTime,
					}, storemod.ModelTurnEntry{
						Kind: storemod.ModelTurnInput,
						Data: []byte(`{"input":"after deletion"}`),
						At:   fixedTime,
					})
					begun <- beginResult{Recorder: recorder, Err: beginErr}
				}()
				targetSub := targetClient.sub.(*serverClient)
				deleteUnlocked := targetSub.replayMu.TryLock()
				if deleteUnlocked {
					targetSub.replayMu.Unlock()
				}
				require.False(t, deleteUnlocked)
				close(blocked.release)

				begin := <-begun
				kill := <-killed
				entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)
				var unknown domain.UnknownNickError
				type assertionSnapshot struct {
					RecorderPresent bool
					BeginClosed     bool
					BeginPeerGone   bool
					Response        protocol.Response
					KillError       error
					Entries         []storemod.ModelTurnEntry
					EntriesError    error
				}

				require.Equal(t, assertionSnapshot{
					BeginClosed:   tt.wantClosed,
					BeginPeerGone: tt.wantGone,
					Response:      protocol.Response{},
				}, assertionSnapshot{
					RecorderPresent: begin.Recorder != nil,
					BeginClosed:     errors.Is(begin.Err, protocol.ErrSubscriptionClosed),
					BeginPeerGone:   errors.As(begin.Err, &unknown),
					Response:        kill.Response,
					KillError:       kill.Err,
					Entries:         entries,
					EntriesError:    entriesErr,
				})
			})
		})
	}
}

func TestSession_invitation_turn_closes_when_the_channel_is_destroyed(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#solo"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	resp, sendErr := userClient(t, sess).Send(ctx, protocol.Invite{
		Nick: botty.Nick(), Channel: "#solo",
	})
	require.NoError(t, sendErr)
	require.NoError(t, resp.Err)
	guard, err := client.sub.GuardInvitation(ctx, "#solo")
	require.NoError(t, err)
	recorder, err := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.ChannelWindowTarget("#solo"),
		ModelID:    botty.ModelID,
		StartedAt:  fixedTime,
	}, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"invitation"}`),
		At:   fixedTime,
	})
	require.NoError(t, err)

	resp, sendErr = userClient(t, sess).Send(ctx, protocol.Part{Channel: "#solo"})
	require.NoError(t, sendErr)
	require.NoError(t, resp.Err)

	err = recorder.AppendModelTurnEntry(ctx, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnAssistant,
		Data: []byte(`{"text":"too late"}`),
		At:   fixedTime,
	})
	require.ErrorIs(t, err, storemod.ErrModelTurnClosed)

	entries, err := sqliteStore.ModelTurnEntries(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSession_invitation_turn_transfers_to_the_joined_membership(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	response, err := userClient(t, sess).Send(ctx, protocol.Invite{
		Nick: botty.Nick(), Channel: "#dev",
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	guard, err := client.sub.GuardInvitation(ctx, "#dev")
	require.NoError(t, err)
	input := storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"invitation"}`),
		At:   fixedTime,
	}

	recorder, err := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    botty.ModelID,
		StartedAt:  fixedTime,
	}, input)
	require.NoError(t, err)

	response, err = sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#dev"}})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	toolResult := storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnToolResults,
		Seq:  1,
		Data: []byte(`{"results":[{"ok":true}]}`),
		At:   fixedTime,
	}
	appendErr := recorder.AppendModelTurnEntry(ctx, toolResult)
	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)
	joinedGuardValid := guard.Valid(ctx)
	response, err = sess.Handle(ctx, client, protocol.Part{Channel: "#dev"})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	closedAppendErr := recorder.AppendModelTurnEntry(ctx, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnOutcome,
		Seq:  2,
		Data: []byte(`{"outcome":"too late"}`),
		At:   fixedTime,
	})
	closedEntries, closedEntriesErr := sqliteStore.ModelTurnEntries(ctx, 1)

	type assertionSnapshot struct {
		JoinedGuardValid bool
		AppendErr        error
		Entries          []storemod.ModelTurnEntry
		EntriesErr       error
		PartedGuardValid bool
		ClosedAppendErr  error
		ClosedEntries    []storemod.ModelTurnEntry
		ClosedEntriesErr error
	}

	require.Equal(t, assertionSnapshot{
		JoinedGuardValid: joinedGuardValid,
		Entries:          []storemod.ModelTurnEntry{input, toolResult},
		ClosedAppendErr:  storemod.ErrModelTurnClosed,
		ClosedEntries:    []storemod.ModelTurnEntry{input, toolResult},
	}, assertionSnapshot{
		JoinedGuardValid: true,
		AppendErr:        appendErr,
		Entries:          entries,
		EntriesErr:       entriesErr,
		PartedGuardValid: guard.Valid(ctx),
		ClosedAppendErr:  closedAppendErr,
		ClosedEntries:    closedEntries,
		ClosedEntriesErr: closedEntriesErr,
	})
}

func TestSession_model_turn_admission_refuses_a_revoked_channel(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
	guard, err := client.sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, client, protocol.Part{Channel: "#dev"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	recorder, err := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    botty.ModelID,
		StartedAt:  fixedTime,
	}, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"after part"}`),
		At:   fixedTime,
	})
	require.Error(t, err)
	require.Nil(t, recorder)

	entries, err := sqliteStore.ModelTurnEntries(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, entries)
}

// blockedModelTurnWriteStore parks inside each model-turn write, so a
// test can inspect what the session is still holding while the write
// is in flight.
type blockedModelTurnWriteStore struct {
	Store

	beginEntered  chan struct{}
	beginRelease  chan struct{}
	appendEntered chan struct{}
	appendRelease chan struct{}
}

func (s *blockedModelTurnWriteStore) BeginModelTurn(
	ctx context.Context,
	turn storemod.ModelTurn,
	input storemod.ModelTurnEntry,
) (storemod.ModelTurnID, error) {
	close(s.beginEntered)
	select {
	case <-s.beginRelease:
	case <-ctx.Done():
		return 0, ctx.Err()
	}

	return s.Store.BeginModelTurn(ctx, turn, input)
}

func (s *blockedModelTurnWriteStore) AppendModelTurnEntry(
	ctx context.Context,
	id storemod.ModelTurnID,
	entry storemod.ModelTurnEntry,
) error {
	close(s.appendEntered)
	select {
	case <-s.appendRelease:
	case <-ctx.Done():
		return ctx.Err()
	}

	return s.Store.AppendModelTurnEntry(ctx, id, entry)
}

// TestSession_channel_turn_journal_writes_release_channel_state bounds
// what a journal write holds. `s.channels.mu` covers every live
// channel record, and every command handler that loads one waits on
// it, so a write that keeps it holds up the whole server for the
// length of a database round trip. The departure the admission check
// excludes bumps the window epoch while holding the actor's
// `replayMu`, which the admission holds for its whole call, so the
// channel record can be read under the lock and the write can run
// without it.
func TestSession_channel_turn_journal_writes_release_channel_state(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
	guard, err := client.sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)

	blocked := &blockedModelTurnWriteStore{
		Store:         sess.store,
		beginEntered:  make(chan struct{}),
		beginRelease:  make(chan struct{}),
		appendEntered: make(chan struct{}),
		appendRelease: make(chan struct{}),
	}
	sess.store = blocked

	input := storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"first"}`),
		At:   fixedTime,
	}
	assistant := storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnAssistant,
		Data: []byte(`{"text":"answered"}`),
		At:   fixedTime,
	}

	type beginResult struct {
		Recorder storemod.ModelTurnRecorder
		Err      error
	}
	begun := make(chan beginResult, 1)
	go func() {
		recorder, beginErr := sess.BeginModelTurn(ctx, guard, storemod.ModelTurn{
			InstanceID: botty.ID(),
			Window:     protocol.ChannelWindowTarget("#dev"),
			ModelID:    botty.ModelID,
			StartedAt:  fixedTime,
		}, input)
		begun <- beginResult{Recorder: recorder, Err: beginErr}
	}()

	<-blocked.beginEntered
	freeDuringBegin := sess.channels.mu.TryLock()
	if freeDuringBegin {
		sess.channels.mu.Unlock()
	}
	close(blocked.beginRelease)
	begin := <-begun

	appended := make(chan error, 1)
	go func() {
		appended <- begin.Recorder.AppendModelTurnEntry(ctx, assistant)
	}()

	<-blocked.appendEntered
	freeDuringAppend := sess.channels.mu.TryLock()
	if freeDuringAppend {
		sess.channels.mu.Unlock()
	}
	close(blocked.appendRelease)
	appendErr := <-appended

	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)

	type assertionSnapshot struct {
		ChannelStateFreeDuringBegin  bool
		ChannelStateFreeDuringAppend bool
		BeginError                   error
		AppendError                  error
		Entries                      []storemod.ModelTurnEntry
		EntriesError                 error
	}

	require.Equal(t, assertionSnapshot{
		ChannelStateFreeDuringBegin:  true,
		ChannelStateFreeDuringAppend: true,
		Entries:                      []storemod.ModelTurnEntry{input, assistant},
	}, assertionSnapshot{
		ChannelStateFreeDuringBegin:  freeDuringBegin,
		ChannelStateFreeDuringAppend: freeDuringAppend,
		BeginError:                   begin.Err,
		AppendError:                  appendErr,
		Entries:                      entries,
		EntriesError:                 entriesErr,
	})
}

// foreignWindowGuard satisfies [protocol.WindowGuard] without being a
// guard the session issued. Nothing in the tree constructs one in
// production; the session's actor-bound operations take the exported
// interface, so this is the shape a caller could reach them with.
type foreignWindowGuard struct{}

func (foreignWindowGuard) Valid(context.Context) bool { return true }
func (foreignWindowGuard) Context(context.Context) (protocol.WindowContext, error) {
	return nil, nil
}
func (foreignWindowGuard) RunWithAuthority(_ context.Context, operation func() error) error {
	return operation()
}
func (foreignWindowGuard) Send(
	ctx context.Context,
	client protocol.Client,
	cmd protocol.Command,
) (protocol.Response, error) {
	return client.Send(ctx, cmd)
}

// TestSession_model_turn_refuses_a_guard_it_did_not_issue pins the
// boundary the actor-bound operations sit behind. A guard carries the
// actor and the window it was captured for, and only the session can
// make one. A value that merely answers the interface names no actor,
// so it admits nothing.
func TestSession_model_turn_refuses_a_guard_it_did_not_issue(t *testing.T) {
	sess, sqliteStore := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))

	recorder, beginErr := sess.BeginModelTurn(ctx, foreignWindowGuard{}, storemod.ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    botty.ModelID,
		StartedAt:  fixedTime,
	}, storemod.ModelTurnEntry{
		Kind: storemod.ModelTurnInput,
		Data: []byte(`{"input":"unauthorised"}`),
		At:   fixedTime,
	})

	entries, entriesErr := sqliteStore.ModelTurnEntries(ctx, 1)

	type assertionSnapshot struct {
		RecorderPresent bool
		BeginError      string
		Entries         []storemod.ModelTurnEntry
		EntriesError    error
	}

	require.Equal(t, assertionSnapshot{
		BeginError: "begin model turn: window guard session.foreignWindowGuard " +
			"was not issued by this session",
	}, assertionSnapshot{
		RecorderPresent: recorder != nil,
		BeginError:      beginErr.Error(),
		Entries:         entries,
		EntriesError:    entriesErr,
	})
}
