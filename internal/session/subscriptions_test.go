package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

type blockedScrollbackStore struct {
	Store

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockedProjectionStore struct {
	Store

	appended chan struct{}
	release  chan struct{}
	append   sync.Once
}

type failingProjectionStore struct{ Store }

type blockedDMAppendStore struct {
	Store

	appended chan struct{}
	release  chan struct{}
	once     sync.Once
}

type blockedDMScrollbackStore struct {
	Store

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockedSecondInstanceLookupStore struct {
	Store

	entered chan struct{}
	release chan struct{}
	calls   int
	mu      sync.Mutex
}

type blockedRepliesStore struct {
	Store

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockedDirectoryStore struct {
	Store

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockedWindowStore struct {
	Store

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedWindowStore) GetWindow(
	ctx context.Context,
	name domain.ChannelName,
) (domain.Window, error) {
	window, err := s.Store.GetWindow(ctx, name)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return window, err
}

func collectSubscriptionDeliveries(sub protocol.Subscription) []protocol.Delivery {
	var deliveries []protocol.Delivery

	for {
		select {
		case delivery := <-sub.Events():
			deliveries = append(deliveries, delivery)
		default:
			return deliveries
		}
	}
}

func (s *blockedDirectoryStore) ListWindows(ctx context.Context) ([]domain.Window, error) {
	windows, err := s.Store.ListWindows(ctx)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return windows, err
}

func (s *blockedRepliesStore) InstanceRepliesBefore(
	ctx context.Context,
	id domain.InstanceID,
	before *int64,
	n int,
) ([]store.InstanceReplyRecord, error) {
	replies, err := s.Store.InstanceRepliesBefore(ctx, id, before, n)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return replies, err
}

func (s *blockedRepliesStore) InstanceRepliesForWindowBefore(
	ctx context.Context,
	id domain.InstanceID,
	window protocol.WindowTarget,
	before *int64,
	n int,
) ([]store.InstanceReplyRecord, error) {
	replies, err := s.Store.InstanceRepliesForWindowBefore(ctx, id, window, before, n)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return replies, err
}

func (s *blockedSecondInstanceLookupStore) GetInstanceByID(
	ctx context.Context,
	id domain.InstanceID,
) (*domain.Instance, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()

	inst, err := s.Store.GetInstanceByID(ctx, id)
	if call != 2 {
		return inst, err
	}

	close(s.entered)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return inst, err
}

func (s *blockedDMAppendStore) AppendEvent(
	ctx context.Context,
	target domain.ChannelName,
	event domain.ChannelActivity,
) (int64, error) {
	id, err := s.Store.AppendEvent(ctx, target, event)
	if domain.InferChannelKind(target) == domain.KindDM {
		s.once.Do(func() { close(s.appended) })
		<-s.release
	}

	return id, err
}

func (s *blockedDMScrollbackStore) DMEventsBefore(
	ctx context.Context,
	self, peer domain.InstanceID,
	before *int64,
	limit int,
) ([]domain.StoredEvent, error) {
	events, err := s.Store.DMEventsBefore(ctx, self, peer, before, limit)
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return events, err
}

func (s *blockedProjectionStore) CommitChannelEvent(
	ctx context.Context,
	event store.ChannelEvent,
) (store.CommittedChannelEvent, error) {
	committed, err := s.Store.CommitChannelEvent(ctx, event)
	s.append.Do(func() { close(s.appended) })
	<-s.release

	return committed, err
}

func (s *failingProjectionStore) CommitChannelEvent(
	ctx context.Context,
	event store.ChannelEvent,
) (store.CommittedChannelEvent, error) {
	if len(event.Scrollback) == 0 {
		return s.Store.CommitChannelEvent(ctx, event)
	}

	return store.CommittedChannelEvent{}, errors.New("projection unavailable")
}

func (s *blockedScrollbackStore) ChannelScrollback(
	ctx context.Context,
	actor domain.InstanceID,
	channel domain.ChannelName,
	limit int,
) ([]domain.StoredEvent, error) {
	events, err := s.Store.ChannelScrollback(ctx, actor, channel, limit)
	s.once.Do(func() { close(s.entered) })
	<-s.release

	return events, err
}

func (s *blockedScrollbackStore) ChannelScrollbackBefore(
	ctx context.Context,
	actor domain.InstanceID,
	channel domain.ChannelName,
	before *int64,
	limit int,
) ([]domain.StoredEvent, error) {
	events, err := s.Store.ChannelScrollbackBefore(ctx, actor, channel, before, limit)
	s.once.Do(func() { close(s.entered) })
	<-s.release

	return events, err
}

func TestSession_User_returns_user_client_with_operator_mode(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	user := userClient(t, sess)

	require.NotNil(t, user)
	require.Equal(t, protocol.UserClientID, user.Identity())

	sc := sess.lookupClientHandle(protocol.UserClientID)
	require.NotNil(t, sc)
	require.True(t, sc.HasMode(domain.ModeOperator))
	require.False(t, sc.HasMode(domain.Mode('w')))
}

func TestSession_User_Send_routes_through_Handle(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	resp, err := userClient(t, sess).Send(t.Context(), protocol.Join{Channels: []domain.ChannelName{"#general"}})

	require.NoError(t, err)
	require.Equal(t, protocol.Response{Events: []protocol.Event{domain.JoinedChannel{Channel: "#general"}}}, resp)

	_, ok := userInstance(t, sess).Channels().Get("#general")
	require.True(t, ok)
}

// TestSession_Subscribe_owns_the_identity_it_registers pins the
// attach contract. A subscription's events channel has one reader, so
// the envelope belongs to the client the session allocated it for:
// the same client asking again gets it back, and a different client
// asking for the same identity is refused rather than handed a
// channel it would take deliveries from.
//
// [protocol.UserClientID] is an ordinary identity under all of it.
// The fixture has already registered it, so it is the case where a
// second client asks for one somebody holds.
func TestSession_Subscribe_owns_the_identity_it_registers(t *testing.T) {
	t.Parallel()

	sess, store := newTestSession(t)

	inst := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	owner := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}
	opts := protocol.SubscribeOptions{Attachment: sess.issueAttachment(owner.Identity())}

	first, err := sess.Subscribe(t.Context(), owner, opts)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.Events())

	second, err := sess.Subscribe(t.Context(), owner, opts)
	require.NoError(t, err)
	require.Same(t, first, second, "the owner asking again gets the envelope it holds")
	require.False(t, sess.idHasServerOper(owner.Identity()),
		"repeating a model subscription cannot grant operator mode")

	impostor := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}
	_, err = sess.Subscribe(t.Context(), impostor, opts)
	require.ErrorIs(t, err, ErrIdentityInUse)

	userImpostor := &subscribeFakeClient{id: protocol.UserClientID}
	_, err = sess.Subscribe(t.Context(), userImpostor, protocol.SubscribeOptions{
		UserCredential: sess.userCredential,
	})
	require.ErrorIs(t, err, ErrIdentityInUse,
		"the sentinel identity is held by the client that registered it, like any other")

	require.True(t, sess.idHasServerOper(protocol.UserClientID),
		"the refused attach left the registered client's modes alone")
}

func TestSession_Subscribe_requires_a_pointer_client_handle(t *testing.T) {
	sess, store := newTestSession(t)
	inst := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := nonComparableClient{
		subscribeFakeClient: &subscribeFakeClient{id: protocol.ClientID(inst.ID())},
		state:               []string{"connected"},
	}

	sub, err := sess.Subscribe(t.Context(), client, protocol.SubscribeOptions{
		Attachment: sess.issueAttachment(client.Identity()),
	})

	require.Nil(t, sub)
	require.ErrorIs(t, err, ErrInvalidClientHandle)
	require.False(t, sess.ClientConnected(client.Identity()))
}

func TestSession_Handle_refuses_a_non_pointer_client_handle(t *testing.T) {
	sess, _ := newTestSession(t)
	client := nonComparableClient{
		subscribeFakeClient: &subscribeFakeClient{id: protocol.UserClientID},
		state:               []string{"connected"},
	}

	resp, err := sess.Handle(t.Context(), client, protocol.Nick{New: "renamed"})

	require.Equal(t, protocol.Response{}, resp)
	require.ErrorIs(t, err, ErrInvalidClientHandle)
	require.Equal(t, domain.Nick("testuser"), userNick(t, sess))
}

func TestSession_Subscribe_refuses_changed_delivery_options(t *testing.T) {
	sess, store := newTestSession(t)
	inst := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	owner := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}
	attachment := sess.issueAttachment(owner.Identity())

	first, err := sess.Subscribe(t.Context(), owner, protocol.SubscribeOptions{Attachment: attachment})
	require.NoError(t, err)

	_, err = sess.Subscribe(t.Context(), owner, protocol.SubscribeOptions{
		Attachment:  attachment,
		EchoMessage: true,
	})
	require.ErrorIs(t, err, ErrSubscriptionOptionsChanged)
	require.Same(t, first, sess.lookupClientHandle(owner.Identity()))
	require.False(t, sess.lookupClientHandle(owner.Identity()).echo)
}

func TestSession_Subscribe_uses_the_stores_canonical_instance(t *testing.T) {
	sess, store := newTestSession(t)
	canonical := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(canonical.ID())}

	_, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)
	require.Same(t, canonical, sess.lookupClientHandle(client.Identity()).instance)
}

func TestSession_Subscribe_requires_a_persisted_identity(t *testing.T) {
	sess, _ := newTestSession(t)
	client := &subscribeFakeClient{id: "different-id"}

	_, err := sess.Subscribe(t.Context(), client, protocol.SubscribeOptions{
		Attachment: sess.issueAttachment(client.Identity()),
	})
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.False(t, sess.ClientConnected(client.Identity()))
}

func TestSession_Subscribe_authenticates_the_user_identity(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	factory := newTestModelClientFactory(t, &apitest.Fake{})
	credential := protocol.NewUserCredential()
	sess := New(t.Context(), store, factory, nil, WithUserCredential(credential))
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	user := domain.NewUserInstance("testuser")
	require.NoError(t, store.SaveInstance(t.Context(), user))

	impostor := &subscribeFakeClient{id: protocol.UserClientID}
	for _, supplied := range []*protocol.UserCredential{nil, protocol.NewUserCredential()} {
		_, err := sess.Subscribe(t.Context(), impostor, protocol.SubscribeOptions{
			UserCredential: supplied,
		})
		require.ErrorIs(t, err, ErrInvalidUserCredential)
	}
	require.False(t, sess.ClientConnected(protocol.UserClientID))

	owner := &subscribeFakeClient{id: protocol.UserClientID}
	_, err := sess.Subscribe(t.Context(), owner, protocol.SubscribeOptions{
		UserCredential: credential,
	})
	require.NoError(t, err)
	require.True(t, sess.idHasServerOper(protocol.UserClientID))
}

func TestSession_Subscribe_authenticates_a_model_identity(t *testing.T) {
	sess, store := newTestSession(t)
	inst := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	impostor := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}

	for _, supplied := range []*protocol.Attachment{nil, protocol.NewAttachment()} {
		_, err := sess.Subscribe(t.Context(), impostor, protocol.SubscribeOptions{
			Attachment: supplied,
		})
		require.ErrorIs(t, err, ErrInvalidModelAttachment)
	}
	require.False(t, sess.ClientConnected(protocol.ClientID(inst.ID())))

	client, err := sess.startModelClient(t.Context(), inst)
	require.NoError(t, err)
	require.Equal(t, protocol.ClientID(inst.ID()), client.Identity())
	require.True(t, sess.ClientConnected(client.Identity()))
}

func TestSession_Subscribe_calls_an_external_attachment_verifier_without_the_registry_lock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := storetest.NewMemoryStore(t)
		var sess *Session
		verifier := func(protocol.ClientID, *protocol.Attachment) bool {
			result := make(chan bool, 1)
			go func() {
				result <- sess.ClientConnected(protocol.UserClientID)
			}()

			select {
			case <-result:
				return true
			case <-time.After(time.Second):
				return false
			}
		}
		sess = New(t.Context(), store, newTestModelClientFactory(t, &apitest.Fake{}), nil,
			withModelAttachmentVerifier(verifier))
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

		inst := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		client := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}
		_, err := sess.Subscribe(t.Context(), client, protocol.SubscribeOptions{
			Attachment: protocol.NewAttachment(),
		})
		require.NoError(t, err)
	})
}

func TestSession_Subscribe_rechecks_attachment_when_registering(t *testing.T) {
	base := storetest.NewMemoryStore(t)
	blocked := &blockedSecondInstanceLookupStore{
		Store:   base,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	sess := New(t.Context(), blocked, newTestModelClientFactory(t, &apitest.Fake{}), nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, base.SaveInstance(t.Context(), inst))
	owner := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}
	attachment := sess.issueAttachment(owner.Identity())
	first, err := sess.Subscribe(t.Context(), owner, protocol.SubscribeOptions{Attachment: attachment})
	require.NoError(t, err)

	errCh := make(chan error, 1)
	go func() {
		_, subscribeErr := sess.Subscribe(t.Context(), owner, protocol.SubscribeOptions{Attachment: attachment})
		errCh <- subscribeErr
	}()

	<-blocked.entered
	first.Unsubscribe()
	close(blocked.release)

	require.ErrorIs(t, <-errCh, ErrInvalidModelAttachment)
	require.False(t, sess.ClientConnected(owner.Identity()))
}

func TestSession_Handle_rejects_an_unregistered_owner_of_an_identity(t *testing.T) {
	sess, _ := newTestSession(t)
	impostor := &subscribeFakeClient{id: protocol.UserClientID}

	_, err := sess.Handle(t.Context(), impostor, protocol.Join{
		Channels: []domain.ChannelName{"#stolen"},
	})
	require.ErrorIs(t, err, ErrClientNotConnected)

	_, err = sess.loadChannelWindow(t.Context(), "#stolen")
	require.Error(t, err)
}

func TestSubscription_Scrollback_follows_current_channel_membership(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)

	send := func(command protocol.Command) {
		t.Helper()
		response, sendErr := sess.Handle(ctx, client, command)
		require.NoError(t, sendErr)
		require.NoError(t, response.Err)
	}

	send(protocol.Join{Channels: []domain.ChannelName{"#general"}})
	sub.Activate()
	for range 3 {
		<-sub.Events()
	}
	_, err = userSendMessage(ctx, t, sess, "#general", "first interval")
	require.NoError(t, err)

	first, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"", "first interval"}, scrollbackBodies(first))

	send(protocol.Part{Channel: "#general"})
	_, err = sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
	var notOn domain.NotOnChannelError
	require.ErrorAs(t, err, &notOn)

	send(protocol.Join{Channels: []domain.ChannelName{"#general"}})
	_, err = userSendMessage(ctx, t, sess, "#general", "second interval")
	require.NoError(t, err)

	second, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"", "second interval"}, scrollbackBodies(second))
}

func TestSubscription_actor_bound_capabilities_refuse_a_stale_handle(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	response, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#general"}})
	require.NoError(t, err)
	require.NoError(t, response.Err)

	sub.Unsubscribe()

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "channel scrollback",
			call: func() error {
				_, callErr := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
				return callErr
			},
		},
		{
			name: "direct scrollback",
			call: func() error {
				_, callErr := sub.Scrollback(ctx, protocol.DirectWindowTarget(protocol.UserClientID), 100)
				return callErr
			},
		},
		{
			name: "zero-limit scrollback",
			call: func() error {
				_, callErr := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 0)
				return callErr
			},
		},
		{
			name: "private replies",
			call: func() error {
				_, callErr := sub.Replies(ctx, nil, 100)
				return callErr
			},
		},
		{
			name: "zero-limit private replies",
			call: func() error {
				_, callErr := sub.Replies(ctx, nil, 0)
				return callErr
			},
		},
		{
			name: "channel directory",
			call: func() error {
				_, callErr := sub.DirectoryChannels(ctx)
				return callErr
			},
		},
		{
			name: "window guard",
			call: func() error {
				_, callErr := sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#general"))
				return callErr
			},
		},
		{
			name: "invitation guard",
			call: func() error {
				_, callErr := sub.GuardInvitation(ctx, "#general")
				return callErr
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.call(), protocol.ErrSubscriptionClosed)
		})
	}
}

func TestSubscription_Scrollback_reports_closure_during_a_store_read(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)
	response, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#general"}})
	require.NoError(t, err)
	require.NoError(t, response.Err)

	blocked := &blockedScrollbackStore{
		Store: sess.store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked

	result := make(chan error, 1)
	go func() {
		_, scrollbackErr := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
		result <- scrollbackErr
	}()
	<-blocked.entered

	sub.Unsubscribe()
	close(blocked.release)

	require.ErrorIs(t, <-result, protocol.ErrSubscriptionClosed)
}

func TestSubscription_Scrollback_reports_a_direct_peer_closed_during_read(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()
	peer, peerClient := seedPassiveInstance(t, sess, "botty", "test/model")
	userSub := sess.lookupClientHandle(protocol.UserClientID)

	blocked := &blockedDMScrollbackStore{
		Store: sess.store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked

	type scrollbackResult struct {
		Entries []protocol.ScrollbackEntry
		Err     error
	}
	result := make(chan scrollbackResult, 1)
	go func() {
		entries, err := userSub.Scrollback(
			ctx, protocol.DirectWindowTarget(peer.ID()), 100,
		)
		result <- scrollbackResult{Entries: entries, Err: err}
	}()
	<-blocked.entered

	response, err := sess.Handle(ctx, peerClient, protocol.Quit{Reason: "gone"})
	require.NoError(t, err)
	require.Equal(t, protocol.Response{}, response)
	close(blocked.release)
	read := <-result

	require.ErrorIs(t, read.Err, protocol.ErrWindowAuthorityChanged)
	require.Equal(t, []protocol.ScrollbackEntry(nil), read.Entries)
}

func TestSubscription_GuardWindow_reports_closure_during_a_store_read(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	botty := seedInstanceRow(t, store, instanceSpec{
		Nick: "botty", ModelID: "test/model", Channels: testChannels("#general"),
	})
	window := domain.NewChannelWindow("#general", fixedTime)
	window.Members.Add(botty)
	require.NoError(t, store.SaveWindow(ctx, window))

	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	blocked := &blockedWindowStore{
		Store: sess.store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked

	result := make(chan error, 1)
	go func() {
		_, guardErr := sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#general"))
		result <- guardErr
	}()
	<-blocked.entered

	sub.Unsubscribe()
	close(blocked.release)

	require.ErrorIs(t, <-result, protocol.ErrSubscriptionClosed)
}

func TestWindowGuard_returns_the_actor_visible_channel(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#anon"))
	setChannelModes(t, sess, "#anon", domain.ChannelModes{Anonymous: true})
	require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#anon", "quiet room"))

	sub := sess.lookupClientHandle(protocol.UserClientID)
	require.NotNil(t, sub)
	guard, err := sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#anon"))
	require.NoError(t, err)

	window, err := guard.Context(ctx)
	require.NoError(t, err)
	topic, ok := window.Topic()
	require.True(t, ok)
	require.Equal(t, domain.TopicInfo{
		Target:     "#anon",
		Topic:      "quiet room",
		TopicSetBy: domain.AnonymousNick,
		TopicSetAt: fixedTime,
		At:         fixedTime,
	}, topic)
	require.Equal(t, protocol.ChannelWindowTarget("#anon"), window.Target())

	require.NoError(t, userPart(ctx, t, sess, "#anon", "leaving"))
	require.False(t, guard.Valid(ctx))
	_, err = guard.Context(ctx)
	var notOnChannel domain.NotOnChannelError
	require.ErrorAs(t, err, &notOnChannel)
}

func TestWindowGuard_refuses_an_absent_target(t *testing.T) {
	sess, _ := newTestSession(t)
	sub := sess.lookupClientHandle(protocol.UserClientID)
	require.NotNil(t, sub)

	guard, err := sub.GuardWindow(t.Context(), nil)
	require.Nil(t, guard)
	require.ErrorIs(t, err, protocol.ErrInvalidWindowTarget)
}

func TestWindowGuard_reports_a_closed_subscription_after_creation(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)
	resp, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#general"}})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	guard, err := sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#general"))
	require.NoError(t, err)

	sub.Unsubscribe()

	_, err = guard.Context(ctx)
	require.ErrorIs(t, err, protocol.ErrSubscriptionClosed)
}

func TestWindowGuard_reports_closure_during_its_context_read(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)
	resp, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#general"}})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	guard, err := sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#general"))
	require.NoError(t, err)

	blocked := &blockedWindowStore{
		Store: sess.store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked
	sess.channels.mu.Lock()
	delete(sess.channels.windows, domain.KeyForChannel("#general"))
	sess.channels.mu.Unlock()

	result := make(chan error, 1)
	go func() {
		_, contextErr := guard.Context(ctx)
		result <- contextErr
	}()
	<-blocked.entered

	sub.Unsubscribe()
	close(blocked.release)

	require.ErrorIs(t, <-result, protocol.ErrSubscriptionClosed)
}

func TestInvitationGuard_does_not_reveal_topic_before_join(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#private"))
	require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#private", "members only"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	resp, err := userClient(t, sess).Send(ctx, protocol.Invite{Nick: botty.Nick(), Channel: "#private"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	guard, err := sub.GuardInvitation(ctx, "#private")
	require.NoError(t, err)
	window, err := guard.Context(ctx)
	require.NoError(t, err)
	topic, hasTopic := window.Topic()
	require.Equal(t, protocol.ChannelWindowTarget("#private"), window.Target())
	require.Equal(t, domain.TopicInfo{}, topic)
	require.False(t, hasTopic)
}

func TestInvitationGuard_reports_a_closed_subscription_after_creation(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#private"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	resp, err := userClient(t, sess).Send(ctx, protocol.Invite{Nick: botty.Nick(), Channel: "#private"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	guard, err := sub.GuardInvitation(ctx, "#private")
	require.NoError(t, err)

	sub.Unsubscribe()

	_, err = guard.Context(ctx)
	require.ErrorIs(t, err, protocol.ErrSubscriptionClosed)
}

func TestInvitationGuard_reports_closure_during_its_context_read(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#private"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)
	resp, err := userClient(t, sess).Send(ctx, protocol.Invite{Nick: botty.Nick(), Channel: "#private"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	guard, err := sub.GuardInvitation(ctx, "#private")
	require.NoError(t, err)

	blocked := &blockedWindowStore{
		Store: sess.store, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked
	sess.channels.mu.Lock()
	delete(sess.channels.windows, domain.KeyForChannel("#private"))
	sess.channels.mu.Unlock()

	result := make(chan error, 1)
	go func() {
		_, contextErr := guard.Context(ctx)
		result <- contextErr
	}()
	<-blocked.entered

	sub.Unsubscribe()
	close(blocked.release)

	require.ErrorIs(t, <-result, protocol.ErrSubscriptionClosed)
}

func TestInvitationGuard_does_not_revive_for_a_later_invite(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#room"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	invite := func() {
		resp, inviteErr := userClient(t, sess).Send(ctx, protocol.Invite{
			Nick: botty.Nick(), Channel: "#room",
		})
		require.NoError(t, inviteErr)
		require.NoError(t, resp.Err)
	}
	invite()

	oldGuard, err := sub.GuardInvitation(ctx, "#room")
	require.NoError(t, err)

	resp, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#room"}})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	resp, err = sess.Handle(ctx, client, protocol.Part{Channel: "#room"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	invite()

	newGuard, err := sub.GuardInvitation(ctx, "#room")
	require.NoError(t, err)
	require.False(t, oldGuard.Valid(ctx))
	require.True(t, newGuard.Valid(ctx))
}

func TestInvitationGuard_repeated_invite_preserves_current_authority(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#room"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	invite := func() protocol.Response {
		resp, err := userClient(t, sess).Send(ctx, protocol.Invite{
			Nick: botty.Nick(), Channel: "#room",
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		return resp
	}

	first := invite()
	guard, err := client.sub.GuardInvitation(ctx, "#room")
	require.NoError(t, err)
	_, before, invitedBefore := sess.invitationState(ctx, "#room", botty.ID())

	second := invite()
	_, after, invitedAfter := sess.invitationState(ctx, "#room", botty.ID())

	type result struct {
		Responses     []protocol.Response
		Before        invitationGeneration
		After         invitationGeneration
		InvitedBefore bool
		InvitedAfter  bool
		GuardValid    bool
		Deliveries    []domain.Event
	}
	require.Equal(t, result{
		Responses: []protocol.Response{
			{Events: []protocol.Event{domain.Inviting{Target: "#room", Invitee: "botty", At: fixedTime}}},
			{Events: []protocol.Event{domain.Inviting{Target: "#room", Invitee: "botty", At: fixedTime}}},
		},
		Before:        invitationGeneration{invite: 1},
		After:         invitationGeneration{invite: 1},
		InvitedBefore: true,
		InvitedAfter:  true,
		GuardValid:    true,
		Deliveries: []domain.Event{
			domain.Invited{
				Source: domain.ClientSource(protocol.UserClientID, "testuser"), Target: "#room",
				Invitee: "botty", At: fixedTime,
			},
			domain.Invited{
				Source: domain.ClientSource(protocol.UserClientID, "testuser"), Target: "#room",
				Invitee: "botty", At: fixedTime,
			},
		},
	}, result{
		Responses:     []protocol.Response{first, second},
		Before:        before,
		After:         after,
		InvitedBefore: invitedBefore,
		InvitedAfter:  invitedAfter,
		GuardValid:    guard.Valid(ctx),
		Deliveries:    drainDeliveries(client),
	})
}

func TestSubscription_Replies_discards_a_window_closed_during_read(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	sess := New(t.Context(), backing, newTestModelClientFactory(t, &apitest.Fake{}), nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	asker := seedInstanceRow(t, backing, instanceSpec{
		Nick: "asker", ModelID: "test/model", Channels: testChannels("#dev"),
	})
	seedChannelWithMembers(t, sess, backing, "#dev", "testuser", "asker")
	client := &subscribeFakeClient{id: protocol.ClientID(asker.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)
	_, err = backing.AppendInstanceReply(ctx, asker.ID(), protocol.ChannelWindowTarget("#dev"),
		domain.SystemNotice{Target: "#dev", Text: "private", At: fixedTime})
	require.NoError(t, err)

	blocked := &blockedRepliesStore{
		Store: backing, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked
	result := make(chan []protocol.ReplyEntry, 1)
	resultErr := make(chan error, 1)
	go func() {
		replies, readErr := sub.Replies(ctx, protocol.ChannelWindowTarget("#dev"), 10)
		result <- replies
		resultErr <- readErr
	}()
	<-blocked.entered

	resp, err := sess.Handle(ctx, client, protocol.Part{Channel: "#dev"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	close(blocked.release)

	require.ErrorIs(t, <-resultErr, protocol.ErrWindowAuthorityChanged)
	require.Empty(t, <-result)
}

func TestSubscription_Replies_applies_the_limit_within_the_requested_window(t *testing.T) {
	sess, backing := newTestSession(t)
	ctx := t.Context()
	reader := seedInstanceRow(t, backing, instanceSpec{
		Nick: "reader", ModelID: "test/model", Channels: testChannels("#dev"),
	})
	seedChannelWithMembers(t, sess, backing, "#dev", "testuser", "reader")
	client := &subscribeFakeClient{id: protocol.ClientID(reader.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	devReply := domain.SystemNotice{Target: "#dev", Text: "keep me", At: fixedTime}
	_, err = backing.AppendInstanceReply(
		ctx, reader.ID(), protocol.ChannelWindowTarget("#dev"), devReply,
	)
	require.NoError(t, err)
	for range protocol.MaxScrollbackEntries + 1 {
		_, err = backing.AppendInstanceReply(ctx, reader.ID(), nil,
			domain.SystemNotice{Text: "newer global reply", At: fixedTime})
		require.NoError(t, err)
	}

	replies, err := sub.Replies(ctx, protocol.ChannelWindowTarget("#dev"), 1)
	require.NoError(t, err)
	require.Equal(t, []protocol.ReplyEntry{{
		Window: protocol.ChannelWindowTarget("#dev"), Event: devReply,
	}}, replies)
}

func TestSubscription_DirectoryChannels_rechecks_visibility_after_part(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	sess := New(t.Context(), backing, newTestModelClientFactory(t, &apitest.Fake{}), nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	reader := seedInstanceRow(t, backing, instanceSpec{
		Nick: "reader", ModelID: "test/model", Channels: testChannels("#secret"),
	})
	seedChannelWithMembers(t, sess, backing, "#secret", "testuser", "reader")
	setChannelModes(t, sess, "#secret", domain.ChannelModes{Secret: true})
	client := &subscribeFakeClient{id: protocol.ClientID(reader.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	blocked := &blockedDirectoryStore{
		Store: backing, entered: make(chan struct{}), release: make(chan struct{}),
	}
	sess.store = blocked
	result := make(chan []domain.ChannelDirectoryEntry, 1)
	resultErr := make(chan error, 1)
	go func() {
		entries, readErr := sub.DirectoryChannels(ctx)
		result <- entries
		resultErr <- readErr
	}()
	<-blocked.entered

	resp, err := sess.Handle(ctx, client, protocol.Part{Channel: "#secret"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	close(blocked.release)

	require.NoError(t, <-resultErr)
	require.Empty(t, <-result)
}

func TestSubscription_DirectoryChannels_preserves_visibility_after_a_failed_mode_write(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	failing := &teardownFailureStore{
		Store: backing, saveWindowErr: errors.New("save failed"),
	}
	sess := New(t.Context(), failing, newTestModelClientFactory(t, &apitest.Fake{}), nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#public"))
	reader := seedInstanceRow(t, backing, instanceSpec{
		Nick: "reader", ModelID: "test/model",
	})
	client := &subscribeFakeClient{id: protocol.ClientID(reader.ID())}
	sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	failing.armed.Store(true)
	resp, err := userClient(t, sess).Send(ctx, protocol.ChannelMode{
		Channel: "#public",
		Changes: []protocol.ChannelModeChange{{Flag: domain.ModeSecret, Add: true}},
	})
	require.ErrorIs(t, err, failing.saveWindowErr)
	require.Equal(t, protocol.Response{}, resp)
	modes, exists := sess.channelModes(ctx, "#public")
	require.Equal(t, struct {
		Exists bool
		Modes  domain.ChannelModes
	}{
		Exists: true,
		Modes:  domain.ChannelModes{},
	}, struct {
		Exists bool
		Modes  domain.ChannelModes
	}{Exists: exists, Modes: modes})

	entries, err := sub.DirectoryChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelDirectoryEntry{{
		Channel: "#public", Members: 1,
	}}, entries)
}

func TestSubscription_Scrollback_requires_both_membership_records(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)

	response, err := sess.Handle(ctx, client, protocol.Join{
		Channels: []domain.ChannelName{"#general"},
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)

	botty.LeaveChannels("#general")
	_, err = sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
	var notOn domain.NotOnChannelError
	require.ErrorAs(t, err, &notOn)
}

func TestSubscription_Activate_separates_replayed_and_live_deliveries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)

		response, err := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#general"}})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		sub.Activate()
		for range 3 {
			<-sub.Events()
		}
		_, err = userSendMessage(ctx, t, sess, "#general", "included in snapshot")
		require.NoError(t, err)

		sub.Unsubscribe()
		client = &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err = subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)

		snapshot, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
		require.NoError(t, err)
		require.Equal(t, []string{"", "included in snapshot"}, scrollbackBodies(snapshot))

		_, err = userSendMessage(ctx, t, sess, "#general", "after snapshot")
		require.NoError(t, err)
		sub.Activate()
		synctest.Wait()

		var messages []string
		for {
			select {
			case delivery := <-sub.Events():
				if message, ok := delivery.Event.(domain.Message); ok {
					messages = append(messages, message.Body)
				}
			default:
				require.Equal(t, []string{"after snapshot"}, messages)
				return
			}
		}
	})
}

func TestSubscription_Activate_keeps_its_own_window_close(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)

		response, err := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		_, err = sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
		require.NoError(t, err)

		serverSub := sub.(*serverClient)
		replayed := serverSub.replayRanges["#general"]
		closed := domain.Part{
			Target: "#general", Source: domain.ClientSource(botty.ID(), domain.Nick(botty.Nick())), At: fixedTime,
		}
		require.True(t, serverSub.queue(queuedDelivery{
			delivery: protocol.Delivery{Event: closed},
			scrollbackIDs: map[domain.ChannelName]int64{
				"#general": replayed.last,
			},
		}))

		sub.Activate()
		synctest.Wait()
		var got domain.Part
		for {
			select {
			case delivery := <-sub.Events():
				if part, ok := delivery.Event.(domain.Part); ok {
					got = part
				}
			default:
				require.Equal(t, closed, got)
				return
			}
		}
	})
}

func TestSubscription_Scrollback_orders_a_concurrent_membership_change(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)

	send := func(command protocol.Command) {
		t.Helper()
		response, sendErr := sess.Handle(ctx, client, command)
		require.NoError(t, sendErr)
		require.NoError(t, response.Err)
	}

	send(protocol.Join{Channels: []domain.ChannelName{"#general"}})
	sub.Activate()
	for range 3 {
		<-sub.Events()
	}
	_, err = userSendMessage(ctx, t, sess, "#general", "first interval")
	require.NoError(t, err)

	blocked := &blockedScrollbackStore{
		Store:   sess.store,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	sess.store = blocked

	type scrollbackResult struct {
		entries []protocol.ScrollbackEntry
		err     error
	}
	result := make(chan scrollbackResult, 1)
	go func() {
		entries, scrollbackErr := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
		result <- scrollbackResult{entries: entries, err: scrollbackErr}
	}()
	<-blocked.entered

	changed := make(chan error, 1)
	go func() {
		part, partErr := sess.Handle(ctx, client, protocol.Part{Channel: "#general"})
		if partErr != nil || part.Err != nil {
			changed <- errors.Join(partErr, part.Err)
			return
		}

		join, joinErr := sess.Handle(ctx, client, protocol.Join{Channels: []domain.ChannelName{"#general"}})
		changed <- errors.Join(joinErr, join.Err)
	}()
	select {
	case <-changed:
		t.Fatal("membership changed while the earlier scrollback read held its ordering point")
	default:
	}

	close(blocked.release)

	got := <-result
	require.NoError(t, got.err)
	require.Equal(t, []string{"", "first interval"}, scrollbackBodies(got.entries))
	require.NoError(t, <-changed)
}

func TestSubscription_capped_snapshot_precedes_the_live_queue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const snapshotLimit = 500

		sess, store := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)

		response, err := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)

		for i := range snapshotLimit + 100 {
			require.NoError(t, sess.persistAndEmit(ctx, "#general", domain.Message{Source: domain.ClientSource(

				userInstance(t, sess).ID(), "testuser"), Target: "#general", Body: fmt.Sprintf("message %03d", i), At: fixedTime.Add(time.Duration(i) * time.Nanosecond)}))
		}

		snapshot, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), snapshotLimit)
		require.NoError(t, err)
		require.Empty(t, snapshot)

		sub.Activate()

		bodies := make([]string, 0, snapshotLimit+100)
		for len(bodies) < snapshotLimit+100 {
			delivery := <-sub.Events()
			if message, ok := delivery.Event.(domain.Message); ok {
				bodies = append(bodies, message.Body)
			}
		}

		expectedBodies := make([]string, snapshotLimit+100)
		for i := range expectedBodies {
			expectedBodies[i] = fmt.Sprintf("message %03d", i)
		}
		require.Equal(t, expectedBodies, bodies)
	})
}

func TestSubscription_replay_serialises_projection_and_delivery(t *testing.T) {
	sess, backing := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, backing, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)

	response, err := sess.Handle(ctx, client, protocol.Join{
		Channels: []domain.ChannelName{"#general"},
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)

	blocked := &blockedProjectionStore{
		Store:    sess.store,
		appended: make(chan struct{}),
		release:  make(chan struct{}),
	}
	sess.store = blocked
	user := userInstance(t, sess)

	sent := make(chan error, 1)
	go func() {
		_, sendErr := sess.sendMessageAs(ctx, user, "#general", "committed before enqueue")
		sent <- sendErr
	}()
	<-blocked.appended

	serverSub := sub.(*serverClient)
	require.False(t, serverSub.replayMu.TryLock(),
		"the replay lock must cover the committed projection until its delivery is queued")

	close(blocked.release)
	require.NoError(t, <-sent)
	snapshot, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
	require.NoError(t, err)
	require.Empty(t, snapshot)

	sub.Activate()
	var messages []domain.Message
	for len(messages) == 0 {
		delivery := <-sub.Events()
		if message, ok := delivery.Event.(domain.Message); ok {
			messages = append(messages, message)
		}
	}
	require.Equal(t, []domain.Message{{
		Target: "#general", Source: domain.ClientSource(user.ID(), domain.Nick("testuser")), Body: "committed before enqueue",
		At: fixedTime,
	}}, messages)
	select {
	case delivery := <-sub.Events():
		if message, ok := delivery.Event.(domain.Message); ok {
			t.Fatalf("live message was delivered twice: %#v", message)
		}
	default:
	}
}

func TestSubscription_channel_event_failure_refuses_send_without_a_replay_gap(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		require.NoError(t, meterProvider.Shutdown(context.Background()))
	})

	synctest.Test(t, func(t *testing.T) {
		sess, backing := newTestSession(t)
		ctx := t.Context()
		counter, counterErr := meterProvider.Meter("test").Int64Counter(
			observability.MetricPersistenceFailures,
		)
		require.NoError(t, counterErr)
		sess.persistenceFailures = counter
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		botty := seedInstanceRow(t, backing, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})
		client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)
		joinResponse, joinErr := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, joinErr)
		require.Equal(t, protocol.Response{Events: []protocol.Event{
			domain.JoinedChannel{Channel: "#general"},
		}}, joinResponse)
		sub.Activate()
		synctest.Wait()
		collectSubscriptionDeliveries(sub)
		collectEmittedEvents(t, sess)
		auditBefore, err := backing.EventsBefore(ctx, "#general", nil, 100)
		require.NoError(t, err)
		storedBefore, err := backing.ChannelScrollback(ctx, botty.ID(), "#general", 100)
		require.NoError(t, err)

		sess.store = &failingProjectionStore{Store: sess.store}
		message := domain.Message{
			Source: domain.ClientSource(protocol.UserClientID, "testuser"),
			Target: "#general", Body: "do not leave a replay gap", At: fixedTime,
		}
		response, sendErr := sess.Handle(ctx, userClient(t, sess), protocol.PrivMsg{
			Target: protocol.ChannelTarget("#general"),
			Body:   message.Body,
		})
		synctest.Wait()
		var persistenceErr *MessagePersistenceError
		require.ErrorAs(t, sendErr, &persistenceErr)

		stored, storedErr := backing.ChannelScrollback(ctx, botty.ID(), "#general", 100)
		audit, auditErr := backing.EventsBefore(ctx, "#general", nil, 100)
		window, windowErr := sess.loadChannelWindow(ctx, "#general")
		windowHasModel := windowErr == nil && window.Members.HasInstance(botty)
		modelDeliveries := collectSubscriptionDeliveries(sub)
		userEvents := collectEmittedEvents(t, sess)
		var metrics metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(ctx, &metrics))
		type persistenceFailurePoint struct {
			Attributes []attribute.KeyValue
			Value      int64
		}
		var persistenceFailures []persistenceFailurePoint
		for _, scope := range metrics.ScopeMetrics {
			for _, metric := range scope.Metrics {
				if metric.Name != observability.MetricPersistenceFailures {
					continue
				}

				sum, ok := metric.Data.(metricdata.Sum[int64])
				require.True(t, ok)
				for _, point := range sum.DataPoints {
					persistenceFailures = append(persistenceFailures, persistenceFailurePoint{
						Attributes: point.Attributes.ToSlice(),
						Value:      point.Value,
					})
				}
			}
		}

		require.Equal(t, struct {
			Response        protocol.Response
			SendFailed      bool
			Connected       bool
			ModelInChannel  bool
			AuditError      error
			Audit           []domain.StoredEvent
			StoredError     error
			Stored          []domain.StoredEvent
			WindowError     error
			WindowHasModel  bool
			ModelDeliveries []protocol.Delivery
			UserEvents      []domain.Event
			Failures        []persistenceFailurePoint
		}{
			SendFailed:     true,
			Connected:      true,
			ModelInChannel: true,
			Audit:          auditBefore,
			Stored:         storedBefore,
			WindowHasModel: true,
			Failures: []persistenceFailurePoint{{
				Attributes: []attribute.KeyValue{
					attribute.String(observability.AttrChannel, "#general"),
				},
				Value: 1,
			}},
		}, struct {
			Response        protocol.Response
			SendFailed      bool
			Connected       bool
			ModelInChannel  bool
			AuditError      error
			Audit           []domain.StoredEvent
			StoredError     error
			Stored          []domain.StoredEvent
			WindowError     error
			WindowHasModel  bool
			ModelDeliveries []protocol.Delivery
			UserEvents      []domain.Event
			Failures        []persistenceFailurePoint
		}{
			Response:        response,
			SendFailed:      persistenceErr != nil,
			Connected:       sess.ClientConnected(protocol.ClientID(botty.ID())),
			ModelInChannel:  botty.InChannel("#general"),
			AuditError:      auditErr,
			Audit:           audit,
			StoredError:     storedErr,
			Stored:          stored,
			WindowError:     windowErr,
			WindowHasModel:  windowHasModel,
			ModelDeliveries: modelDeliveries,
			UserEvents:      userEvents,
			Failures:        persistenceFailures,
		})
	})
}

func TestProjectedScrollbackRecords_excludes_private_issuer_replies(t *testing.T) {
	t.Parallel()

	inst := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "", testChannels("#general"),
	)
	sub := fakeServerClient(t, inst)
	sub.replayCapable = true
	at := time.Date(2026, time.August, 24, 10, 0, 0, 0, time.UTC)
	message := domain.Message{
		Source: domain.ClientSource("inst-alice", "alice"),
		Target: "#general",
		Body:   "hello",
		At:     at,
	}
	routes := []routedDelivery{
		{
			sub: sub,
			delivery: protocol.Delivery{Event: domain.TopicInfo{
				Target: "#general", Topic: "release work", At: at,
			}},
		},
		{sub: sub, delivery: protocol.Delivery{Event: message}},
	}

	records, indexes := projectedScrollbackRecords(routes)

	require.Equal(t, struct {
		Records []store.ChannelScrollbackRecord
		Indexes []map[domain.ChannelName]int
	}{
		Records: []store.ChannelScrollbackRecord{{
			InstanceID: "inst-botty", Channel: "#general", Event: message,
		}},
		Indexes: []map[domain.ChannelName]int{nil, {"#general": 0}},
	}, struct {
		Records []store.ChannelScrollbackRecord
		Indexes []map[domain.ChannelName]int
	}{Records: records, Indexes: indexes})
}

func TestSubscription_DM_scrollback_excludes_the_first_live_delivery(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)
	sub.Activate()

	_, err = userSendMessage(ctx, t, sess, domain.ChannelName(botty.ID()), "first live DM")
	require.NoError(t, err)

	delivery := <-sub.Events()
	require.Equal(t, "first live DM", delivery.Event.(domain.Message).Body)

	entries, err := sub.Scrollback(ctx, protocol.DirectWindowTarget(protocol.UserClientID), 100)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSubscription_DM_scrollback_waits_for_the_live_queue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()

		botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)
		sub.Activate()

		blocked := &blockedDMAppendStore{
			Store:    sess.store,
			appended: make(chan struct{}),
			release:  make(chan struct{}),
		}
		sess.store = blocked

		sent := make(chan error, 1)
		go func() {
			_, sendErr := userSendMessage(ctx, t, sess, domain.ChannelName(botty.ID()), "first live DM")
			sent <- sendErr
		}()
		<-blocked.appended

		read := make(chan []protocol.ScrollbackEntry, 1)
		readErr := make(chan error, 1)
		readStarted := make(chan struct{})
		go func() {
			close(readStarted)
			entries, scrollbackErr := sub.Scrollback(ctx, protocol.DirectWindowTarget(protocol.UserClientID), 100)
			read <- entries
			readErr <- scrollbackErr
		}()
		<-readStarted

		select {
		case <-read:
			t.Fatal("scrollback returned between the DM commit and live queue insertion")
		default:
		}

		close(blocked.release)
		synctest.Wait()

		require.NoError(t, <-sent)
		require.NoError(t, <-readErr)
		require.Empty(t, <-read)
	})
}

func TestSubscription_records_and_delivers_a_model_senders_channel_history(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)

		response, err := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		sub.Activate()
		synctest.Wait()
		collectSubscriptionDeliveries(sub)

		response, err = sess.Handle(ctx, client, protocol.PrivMsg{
			Target: protocol.ChannelTarget("#general"),
			Body:   "my own line",
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		sent := domain.Message{
			Source: domain.ClientSource(botty.ID(), botty.Nick()),
			Target: "#general",
			Body:   "my own line",
			At:     fixedTime,
		}
		require.Equal(t, protocol.Response{Events: []protocol.Event{sent}}, response)

		entries, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), 100)
		require.NoError(t, err)
		require.Equal(t, []string{"", "my own line"}, scrollbackBodies(entries))
		synctest.Wait()
		require.Equal(t, []protocol.Delivery{{
			Event:       sent,
			HistoryOnly: true,
		}}, collectSubscriptionDeliveries(sub))
	})
}

func TestSubscription_channel_departure_commits_projection_and_revokes_context(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, eventStore := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		actor := seedInstanceRow(t, eventStore, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})
		actorClient := &subscribeFakeClient{id: protocol.ClientID(actor.ID())}
		actorSub, err := subscribeTestClient(ctx, t, sess, actorClient,
			protocol.SubscribeOptions{ReplayHistory: true})
		require.NoError(t, err)
		response, err := sess.Handle(ctx, actorClient, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{Events: []protocol.Event{
			domain.JoinedChannel{Channel: "#general"},
		}}, response)
		actorSub.Activate()
		_, _, staleAuthorities, admitted := admitScopedDelivery(
			channelScope{"#general"}, actorSub.(*serverClient),
		)
		require.True(t, admitted)
		stalePoke := routedDelivery{
			sub: actorSub.(*serverClient),
			delivery: protocol.Delivery{Event: domain.PokeEvent{
				Channel: "#general", At: fixedTime,
			}},
			membershipAuthorities: staleAuthorities,
		}

		observer := seedInstanceRow(t, eventStore, instanceSpec{
			Nick: "observer", ModelID: "test/model",
		})
		observerClient := &subscribeFakeClient{id: protocol.ClientID(observer.ID())}
		observerSub, err := subscribeTestClient(ctx, t, sess, observerClient,
			protocol.SubscribeOptions{ReplayHistory: true})
		require.NoError(t, err)
		response, err = sess.Handle(ctx, observerClient, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{Events: []protocol.Event{
			domain.JoinedChannel{Channel: "#general"},
		}}, response)
		observerSub.Activate()
		synctest.Wait()
		collectSubscriptionDeliveries(actorSub)
		collectSubscriptionDeliveries(observerSub)

		_, err = eventStore.AppendInstanceReply(
			ctx, actor.ID(), protocol.ChannelWindowTarget("#general"),
			domain.TopicInfo{Target: "#general", Topic: "release", At: fixedTime},
		)
		require.NoError(t, err)
		turnID, err := eventStore.BeginModelTurn(ctx, store.ModelTurn{
			InstanceID: actor.ID(),
			Window:     protocol.ChannelWindowTarget("#general"),
			ModelID:    actor.ModelID,
			StartedAt:  fixedTime,
		}, store.ModelTurnEntry{
			Kind: store.ModelTurnInput,
			Data: []byte(`{"input":"channel context"}`),
			At:   fixedTime,
		})
		require.NoError(t, err)

		response, err = sess.Handle(ctx, actorClient, protocol.Part{
			Channel: "#general", Reason: "bye",
		})
		require.NoError(t, err)
		require.Equal(t, protocol.Response{}, response)
		sess.enqueueRoutes(ctx, []routedDelivery{stalePoke})
		synctest.Wait()

		part := domain.Part{
			Source: domain.ClientSource(actor.ID(), actor.Nick()),
			Target: "#general", Message: "bye", At: fixedTime,
		}
		actorScrollback, actorScrollbackErr := eventStore.ChannelScrollback(
			ctx, actor.ID(), "#general", 10,
		)
		observerScrollback, observerScrollbackErr := eventStore.ChannelScrollback(
			ctx, observer.ID(), "#general", 10,
		)
		replies, repliesErr := eventStore.InstanceRepliesBefore(ctx, actor.ID(), nil, 10)
		turnEntries, turnEntriesErr := eventStore.ModelTurnEntries(ctx, turnID)
		storedActor, storedActorErr := eventStore.GetInstanceByID(ctx, actor.ID())
		storedWindow, storedWindowErr := eventStore.GetWindow(ctx, "#general")
		storedChannel, storedIsChannel := storedWindow.(*domain.ChannelWindow)

		type departureState struct {
			ActorDeliveries         []protocol.Delivery
			ObserverDeliveries      []protocol.Delivery
			ActorScrollbackError    error
			ActorScrollback         []domain.StoredEvent
			ObserverScrollbackError error
			ObserverScrollback      []domain.StoredEvent
			RepliesError            error
			Replies                 []store.InstanceReplyRecord
			TurnEntriesError        error
			TurnEntries             []store.ModelTurnEntry
			LiveActorInChannel      bool
			StoredActorError        error
			StoredActorInChannel    bool
			StoredWindowError       error
			StoredWindowIsChannel   bool
			StoredWindowHasActor    bool
		}
		require.Equal(t, departureState{
			ActorDeliveries: []protocol.Delivery{{Event: part}},
			ObserverDeliveries: []protocol.Delivery{{
				Event: part,
			}},
			ObserverScrollback: []domain.StoredEvent{
				{ID: 3, Event: domain.Join{
					Source: domain.ClientSource(observer.ID(), observer.Nick()),
					Target: "#general", At: fixedTime,
				}},
				{ID: 4, Event: part},
			},
			TurnEntries: []store.ModelTurnEntry{{
				Kind: store.ModelTurnInput,
				Data: []byte(`{"input":"channel context"}`),
				At:   fixedTime,
			}},
			StoredWindowIsChannel: true,
		}, departureState{
			ActorDeliveries:         collectSubscriptionDeliveries(actorSub),
			ObserverDeliveries:      collectSubscriptionDeliveries(observerSub),
			ActorScrollbackError:    actorScrollbackErr,
			ActorScrollback:         actorScrollback,
			ObserverScrollbackError: observerScrollbackErr,
			ObserverScrollback:      observerScrollback,
			RepliesError:            repliesErr,
			Replies:                 replies,
			TurnEntriesError:        turnEntriesErr,
			TurnEntries:             turnEntries,
			LiveActorInChannel:      actor.InChannel("#general"),
			StoredActorError:        storedActorErr,
			StoredActorInChannel:    storedActor.InChannel("#general"),
			StoredWindowError:       storedWindowErr,
			StoredWindowIsChannel:   storedIsChannel,
			StoredWindowHasActor:    storedIsChannel && storedChannel.Members.HasInstance(actor),
		})
	})
}

func TestEnqueueRoutes_rejects_a_previous_membership_interval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, eventStore := newTestSession(t)
		ctx := t.Context()
		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		actor := seedInstanceRow(t, eventStore, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})
		client := &subscribeFakeClient{id: protocol.ClientID(actor.ID())}
		sub, err := subscribeTestClient(ctx, t, sess, client, protocol.SubscribeOptions{})
		require.NoError(t, err)
		joinResponse, joinErr := sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		require.NoError(t, joinErr)
		require.Equal(t, protocol.Response{Events: []protocol.Event{
			domain.JoinedChannel{Channel: "#general"},
		}}, joinResponse)
		sub.Activate()
		synctest.Wait()
		collectSubscriptionDeliveries(sub)

		serverSub := sub.(*serverClient)
		_, _, staleAuthorities, admitted := admitScopedDelivery(
			channelScope{"#general"}, serverSub,
		)
		require.True(t, admitted)
		stale := routedDelivery{
			sub: serverSub,
			delivery: protocol.Delivery{Event: domain.PokeEvent{
				Channel: "#general", At: fixedTime,
			}},
			membershipAuthorities: staleAuthorities,
		}

		partResponse, partErr := sess.Handle(ctx, client, protocol.Part{Channel: "#general"})
		joinResponse, joinErr = sess.Handle(ctx, client, protocol.Join{
			Channels: []domain.ChannelName{"#general"},
		})
		synctest.Wait()
		collectSubscriptionDeliveries(sub)

		queued := sess.enqueueRoutes(ctx, []routedDelivery{stale})
		synctest.Wait()

		require.Equal(t, struct {
			PartResponse protocol.Response
			PartError    error
			JoinResponse protocol.Response
			JoinError    error
			Queued       []routedDelivery
			Deliveries   []protocol.Delivery
		}{
			JoinResponse: protocol.Response{Events: []protocol.Event{
				domain.JoinedChannel{Channel: "#general"},
			}},
			Queued: []routedDelivery{},
		}, struct {
			PartResponse protocol.Response
			PartError    error
			JoinResponse protocol.Response
			JoinError    error
			Queued       []routedDelivery
			Deliveries   []protocol.Delivery
		}{
			PartResponse: partResponse,
			PartError:    partErr,
			JoinResponse: joinResponse,
			JoinError:    joinErr,
			Queued:       queued,
			Deliveries:   collectSubscriptionDeliveries(sub),
		})
	})
}

func TestScopedDeliveryCandidate_rejects_a_rejoined_membership_interval(t *testing.T) {
	tests := []struct {
		name  string
		scope deliveryScope
	}{
		{name: "channel", scope: channelScope{"#general"}},
		{
			name: "shared channels",
			scope: sharedChannelsScope{
				channels: []domain.ChannelName{"#general", "#other"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actor := domain.NewModelInstance(
				"inst-botty", "botty", "test/model", "", testChannels("#general"),
			)
			sub := fakeServerClient(t, actor)
			sub.windowEpochs = make(map[domain.ChannelName]uint64)
			candidate := scopedDeliveryCandidateFor(tt.scope, sub)

			actor.LeaveChannels("#general")
			sub.bumpWindowEpoch("#general")
			actor.JoinChannel("#general", fixedTime)
			sub.bumpWindowEpoch("#general")

			type admissionResult struct {
				targets     []domain.ChannelName
				direct      bool
				authorities []membershipAuthority
				eligible    bool
			}
			targets, direct, authorities, eligible := candidate.admit(sub)
			got := admissionResult{
				targets: targets, direct: direct,
				authorities: authorities, eligible: eligible,
			}

			require.Equal(t, admissionResult{}, got)
		})
	}
}

func TestAdmitScopedDelivery_does_not_lock_unrelated_scopes(t *testing.T) {
	type admissionResult struct {
		targets     []domain.ChannelName
		direct      bool
		authorities []membershipAuthority
		eligible    bool
	}
	tests := []struct {
		name     string
		scope    deliveryScope
		expected admissionResult
	}{
		{name: "channel", scope: channelScope{"#other"}},
		{
			name: "shared channels",
			scope: sharedChannelsScope{
				channels: []domain.ChannelName{"#other"},
			},
		},
		{
			name:  "direct target",
			scope: clientScope{client: "inst-botty"},
			expected: admissionResult{
				direct: true, eligible: true,
			},
		},
		{name: "operator", scope: operatorsScope{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				actor := domain.NewModelInstance(
					"inst-botty", "botty", "test/model", "", testChannels("#general"),
				)
				sub := fakeServerClient(t, actor)
				sub.windowEpochs = make(map[domain.ChannelName]uint64)
				if _, ok := tt.scope.(operatorsScope); ok {
					sub.connectionActive = true
					sub.connectionGeneration = 1
					sub.modes = make(map[domain.Mode]struct{})
					sub.setMode(domain.ModeOperator, true)
					tt.expected.eligible = true
				}
				result := make(chan admissionResult, 1)

				sub.replayMu.Lock()
				go func() {
					targets, direct, authorities, eligible := admitScopedDelivery(tt.scope, sub)
					result <- admissionResult{
						targets: targets, direct: direct,
						authorities: authorities, eligible: eligible,
					}
				}()
				synctest.Wait()

				select {
				case got := <-result:
					sub.replayMu.Unlock()
					require.Equal(t, tt.expected, got)
				default:
					sub.replayMu.Unlock()
					t.Fatal("scope admission waited for an unrelated replay lock")
				}
			})
		})
	}
}

func TestQueueRoutesLocked_returns_only_accepted_routes(t *testing.T) {
	overflowed := fakeServerClient(t, domain.NewModelInstance(
		"inst-overflowed", "overflowed", "test/model", "", nil,
	))
	overflowed.events = make(chan protocol.Delivery)
	overflowed.outWake = make(chan struct{}, 1)
	overflowed.outbox = make([]queuedDelivery, sendQAllowance)

	accepted := fakeServerClient(t, domain.NewModelInstance(
		"inst-accepted", "accepted", "test/model", "", nil,
	))
	accepted.events = make(chan protocol.Delivery, 1)
	accepted.outWake = make(chan struct{}, 1)

	overflowedRoute := routedDelivery{
		sub: overflowed,
		delivery: protocol.Delivery{Event: domain.PokeEvent{
			Channel: "#general", At: fixedTime,
		}},
	}
	acceptedRoute := routedDelivery{
		sub: accepted,
		delivery: protocol.Delivery{Event: domain.PokeEvent{
			Channel: "#other", At: fixedTime,
		}},
	}
	routes := []routedDelivery{overflowedRoute, acceptedRoute}

	queued, disconnected := queueRoutesLocked(routes, make([]map[domain.ChannelName]int, len(routes)), nil, 0)

	require.Equal(t, struct {
		Queued          []routedDelivery
		Disconnected    []*serverClient
		AcceptedEvent   protocol.Delivery
		OverflowedQueue []queuedDelivery
	}{
		Queued:          []routedDelivery{acceptedRoute},
		Disconnected:    []*serverClient{overflowed},
		AcceptedEvent:   acceptedRoute.delivery,
		OverflowedQueue: make([]queuedDelivery, sendQAllowance),
	}, struct {
		Queued          []routedDelivery
		Disconnected    []*serverClient
		AcceptedEvent   protocol.Delivery
		OverflowedQueue []queuedDelivery
	}{
		Queued:          queued,
		Disconnected:    disconnected,
		AcceptedEvent:   <-accepted.events,
		OverflowedQueue: overflowed.outbox,
	})
}

func TestSubscription_excludes_a_model_senders_first_DM_from_its_seed_and_delivers_it_for_history(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, store := newTestSession(t)
		ctx := t.Context()

		botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
		client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
		sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
			ReplayHistory: true,
		})
		require.NoError(t, err)
		sub.Activate()
		synctest.Wait()

		response, err := sess.Handle(ctx, client, protocol.PrivMsg{
			Target: protocol.NickTarget(userNick(t, sess)),
			Body:   "hello from a new DM",
		})
		require.NoError(t, err)
		require.NoError(t, response.Err)
		sent := domain.Message{
			Source: domain.ClientSource(botty.ID(), botty.Nick()),
			Body:   "hello from a new DM",
			At:     fixedTime,
		}
		require.Equal(t, protocol.Response{Events: []protocol.Event{sent}}, response)

		entries, err := sub.Scrollback(ctx, protocol.DirectWindowTarget(protocol.UserClientID), 100)
		require.NoError(t, err)
		require.Empty(t, entries)

		entries, err = sub.Scrollback(ctx, protocol.DirectWindowTarget(protocol.UserClientID), 100)
		require.NoError(t, err)
		require.Equal(t, []string{"hello from a new DM"}, scrollbackBodies(entries))
		synctest.Wait()
		require.Equal(t, []protocol.Delivery{{
			Event:       sent,
			HistoryOnly: true,
		}}, collectSubscriptionDeliveries(sub))
	})
}

func TestSubscription_restores_a_model_self_DM_after_reattach(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	firstClient := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	first, err := subscribeTestClient(t.Context(), t, sess, firstClient, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)
	first.Activate()

	response, err := sess.Handle(ctx, firstClient, protocol.PrivMsg{
		Target: protocol.NickTarget("botty"),
		Body:   "note to self",
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	first.Unsubscribe()

	secondClient := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	second, err := subscribeTestClient(t.Context(), t, sess, secondClient, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)
	second.Activate()

	entries, err := second.Scrollback(ctx, protocol.DirectWindowTarget(botty.ID()), 100)
	require.NoError(t, err)
	require.Equal(t, []string{"note to self"}, scrollbackBodies(entries))
}

func TestSubscription_caps_a_channel_scrollback_read(t *testing.T) {
	sess, stored := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, stored, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)
	response, err := sess.Handle(ctx, client, protocol.Join{
		Channels: []domain.ChannelName{"#general"},
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	sub.Activate()

	records := make([]store.ChannelScrollbackRecord, protocol.MaxScrollbackEntries+1)
	for i := range records {
		records[i] = store.ChannelScrollbackRecord{
			InstanceID: botty.ID(),
			Channel:    "#general",
			Event: domain.Message{Source: domain.LegacyClientSource(

				"alice"), Target: "#general", Body: fmt.Sprintf("line %04d", i), At: fixedTime.Add(time.Duration(i) * time.Second)},
		}
	}
	_, err = stored.AppendChannelScrollback(ctx, records)
	require.NoError(t, err)

	entries, err := sub.Scrollback(ctx, protocol.ChannelWindowTarget("#general"), protocol.MaxScrollbackEntries+100)
	require.NoError(t, err)

	want := make([]string, protocol.MaxScrollbackEntries)
	for i := range want {
		want[i] = fmt.Sprintf("line %04d", i+1)
	}
	require.Equal(t, want, scrollbackBodies(entries))
}

func TestSubscription_caps_a_DM_scrollback_read(t *testing.T) {
	sess, stored := newTestSession(t)
	ctx := t.Context()

	botty := seedInstanceRow(t, stored, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{
		ReplayHistory: true,
	})
	require.NoError(t, err)
	sub.Activate()

	for i := range protocol.MaxScrollbackEntries + 1 {
		_, err := stored.AppendEvent(ctx, domain.ChannelName(botty.ID()), domain.Message{Source: domain.LegacyClientSource(

			userNick(t, sess)), Target: domain.ChannelName(botty.ID()), Body: fmt.Sprintf("line %04d", i), At: fixedTime.Add(time.Duration(i) * time.Second)})
		require.NoError(t, err)
	}

	entries, err := sub.Scrollback(
		ctx, protocol.DirectWindowTarget(protocol.UserClientID), protocol.MaxScrollbackEntries+100,
	)
	require.NoError(t, err)

	want := make([]string, protocol.MaxScrollbackEntries)
	for i := range want {
		want[i] = fmt.Sprintf("line %04d", i+1)
	}
	require.Equal(t, want, scrollbackBodies(entries))
}

func TestSubscription_Replies_is_actor_bound_and_capped(t *testing.T) {
	sess, backing := newTestSession(t)
	actor := seedInstanceRow(t, backing, instanceSpec{Nick: "botty", ModelID: "test/model"})
	other := seedInstanceRow(t, backing, instanceSpec{Nick: "other", ModelID: "test/model"})

	client := &subscribeFakeClient{id: protocol.ClientID(actor.ID())}
	sub, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	for i := range protocol.MaxScrollbackEntries + 1 {
		_, err := backing.AppendInstanceReply(t.Context(), actor.ID(), nil, domain.SystemNotice{
			Text: fmt.Sprintf("reply-%04d", i),
		})
		require.NoError(t, err)
	}
	_, err = backing.AppendInstanceReply(t.Context(), other.ID(), nil, domain.SystemNotice{Text: "private-other-reply"})
	require.NoError(t, err)

	got, err := sub.Replies(t.Context(), nil, protocol.MaxScrollbackEntries+100)
	require.NoError(t, err)

	want := make([]string, protocol.MaxScrollbackEntries)
	for i := range want {
		want[i] = fmt.Sprintf("reply-%04d", i+1)
	}
	gotBodies := make([]string, 0, len(got))
	for _, entry := range got {
		notice, ok := entry.Event.(domain.SystemNotice)
		require.True(t, ok)
		gotBodies = append(gotBodies, notice.Text)
	}
	require.Equal(t, want, gotBodies)

	sub.Unsubscribe()
	_, err = sub.Replies(t.Context(), nil, 1)
	require.ErrorIs(t, err, protocol.ErrSubscriptionClosed)
}

func TestSession_does_not_materialise_scrollback_for_a_live_only_subscription(t *testing.T) {
	sess, store := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	botty := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	client := &subscribeFakeClient{id: protocol.ClientID(botty.ID())}
	_, err := subscribeTestClient(t.Context(), t, sess, client, protocol.SubscribeOptions{})
	require.NoError(t, err)

	response, err := sess.Handle(ctx, client, protocol.Join{
		Channels: []domain.ChannelName{"#general"},
	})
	require.NoError(t, err)
	require.NoError(t, response.Err)
	_, err = userSendMessage(ctx, t, sess, "#general", "live only")
	require.NoError(t, err)

	stored, err := store.ChannelScrollback(ctx, botty.ID(), "#general", 100)
	require.NoError(t, err)
	require.Empty(t, stored)
}

func scrollbackBodies(entries []protocol.ScrollbackEntry) []string {
	bodies := make([]string, 0, len(entries))
	for _, entry := range entries {
		message, ok := protocol.FromChannelEvent(entry.Event)
		if ok {
			bodies = append(bodies, message.Body)
		}
	}

	return bodies
}

// subscribeFakeClient is the minimal [protocol.Client] satisfier
// used by the Subscribe contract tests. The session reads only the
// client's identity at subscribe time; the other interface methods
// are inert.
type subscribeFakeClient struct {
	id protocol.ClientID
}

type nonComparableClient struct {
	*subscribeFakeClient

	state []string
}

func (c *subscribeFakeClient) Identity() protocol.ClientID { return c.id }
func (c *subscribeFakeClient) Send(_ context.Context, _ protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}
func (c *subscribeFakeClient) Events() <-chan protocol.Delivery { return nil }
func (c *subscribeFakeClient) Caps() command.CapabilityHolder   { return subscribeFakeCaps{} }

type subscribeFakeCaps struct{}

func (subscribeFakeCaps) Has(_ command.Capability) bool { return false }

// TestSession_attach_hands_the_factory_a_snapshot pins what a
// subscriber receives. The attachment token decides who may
// subscribe as an identity; handing that subscriber the canonical
// actor would let it rename the client, move it between channels or
// rewrite its persona without going through a command. The factory
// is given a copy, and the nick it later needs comes from the
// subscription, which the server rewrites on NICK.
func TestSession_attach_hands_the_factory_a_snapshot(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#dev"),
	})
	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")

	id := protocol.ClientID(botty.ID())
	handed := sess.modelClientFactory.(*testModelClientFactory).attachedActor(id)
	require.NotNil(t, handed)

	handed.SetNick("intruder")
	handed.LeaveAllChannels()

	resolved, resolveErr := s.ResolveNick(ctx, "botty")
	subscriptionNick := sess.lookupClientHandle(id).Nick()

	type assertionSnapshot struct {
		ResolveError     error
		ResolvedNick     domain.Nick
		ActorInChannel   bool
		SubscriptionNick domain.Nick
	}

	require.Equal(t, assertionSnapshot{
		ResolvedNick:     "botty",
		ActorInChannel:   true,
		SubscriptionNick: "botty",
	}, assertionSnapshot{
		ResolveError:     resolveErr,
		ResolvedNick:     resolved.Nick(),
		ActorInChannel:   botty.InChannel("#dev"),
		SubscriptionNick: subscriptionNick,
	})
}
