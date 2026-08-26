package userclient_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/userclient"
)

// countingStore is the real store with a tally of the autojoin
// writes made through it, so a test can pin how many a sequence
// costs and not only what it left behind.
type countingStore struct {
	userclient.Store

	autojoinWrites int
}

type blockingAutojoinStore struct {
	userclient.Store

	started chan struct{}
	proceed chan struct{}
}

type blockingDMWindowStore struct {
	userclient.Store

	started chan struct{}
	proceed chan struct{}
}

type blockingDMWindowCloseStore struct {
	userclient.Store

	started chan struct{}
	proceed chan struct{}
}

type orderedUIStateStore struct {
	userclient.Store

	mu      sync.Mutex
	effects []string
}

func (s *blockingAutojoinStore) SetAutojoinChannels(
	ctx context.Context,
	channels []domain.ChannelName,
) error {
	close(s.started)
	<-s.proceed

	return s.Store.SetAutojoinChannels(ctx, channels)
}

func (s *blockingDMWindowStore) AddDMWindow(
	ctx context.Context,
	peer domain.InstanceID,
) error {
	close(s.started)
	<-s.proceed

	return s.Store.AddDMWindow(ctx, peer)
}

func (s *blockingDMWindowCloseStore) RemoveDMWindow(
	ctx context.Context,
	peer domain.InstanceID,
) error {
	close(s.started)
	<-s.proceed

	return s.Store.RemoveDMWindow(ctx, peer)
}

func (s *orderedUIStateStore) AddDMWindow(
	ctx context.Context,
	peer domain.InstanceID,
) error {
	s.mu.Lock()
	s.effects = append(s.effects, "open "+string(peer))
	s.mu.Unlock()

	return s.Store.AddDMWindow(ctx, peer)
}

func (s *orderedUIStateStore) SetLastWindow(
	ctx context.Context,
	window domain.Window,
) error {
	s.mu.Lock()
	s.effects = append(s.effects, "focus "+string(window.Name()))
	s.mu.Unlock()

	return s.Store.SetLastWindow(ctx, window)
}

func (s *orderedUIStateStore) Effects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.effects)
}

func (s *countingStore) SetAutojoinChannels(ctx context.Context, channels []domain.ChannelName) error {
	s.autojoinWrites++

	return s.Store.SetAutojoinChannels(ctx, channels)
}

// joinFailureStore rejects writes for one channel so the real
// session can return both successful JOIN events and an execution
// error from the same multi-target command.
type joinFailureStore struct {
	session.Store

	channel domain.ChannelName
	err     error
	cancel  context.CancelFunc
}

func (s *joinFailureStore) CommitChannelJoin(
	ctx context.Context,
	join storemod.ChannelJoin,
) (storemod.CommittedChannelEvent, error) {
	if domain.KeyForChannel(join.Window.Name()) == domain.KeyForChannel(s.channel) {
		if s.cancel != nil {
			s.cancel()
			return s.Store.CommitChannelJoin(ctx, join)
		}

		return storemod.CommittedChannelEvent{}, s.err
	}

	return s.Store.CommitChannelJoin(ctx, join)
}

// fixture is the common setup the user-client tests share: an
// in-memory store, a noop-API model manager, a session, and an
// attached user-client. `userStore` is the handle the user-client
// itself writes through; `store` is the same database, for the
// assertions and the seeding a test does directly.
type fixture struct {
	sess      *session.Session
	store     *storemod.SQLiteStore
	userStore *countingStore
	user      *userclient.UserClient
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	mgr := modelmanager.New(modelmanager.Config{
		Store:       s,
		APIClient:   &apitest.Fake{},
		BaseContext: t.Context,
	})
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

	userCredential := protocol.NewUserCredential()
	sess := session.New(t.Context(), s, mgr, nil,
		session.WithUserCredential(userCredential))
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	userStore := &countingStore{Store: s}

	user := userclient.New("testuser", sess, userStore,
		userclient.NewStoreReplyLog(s), userCredential)
	require.NoError(t, user.Attach(t.Context()))

	return &fixture{sess: sess, store: s, userStore: userStore, user: user}
}

func TestUserClient_reports_operator_capability(t *testing.T) {
	f := newFixture(t)

	require.True(t, f.user.Caps().Has(protocol.CapOperator))
	require.Equal(t, domain.Nick("testuser"), f.user.Nick())
	require.Equal(t, protocol.UserClientID, f.user.Identity())
}

// TestUserClient_capabilities_come_from_the_session pins where the
// answer is read from. The operator bit arrives with the `+o` the
// attach requests, so a client that has not attached holds nothing.
// That is the same rule a model-client answers under, and neither
// side can set it on its own.
func TestUserClient_capabilities_come_from_the_session(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	mgr := modelmanager.New(modelmanager.Config{
		Store:       s,
		APIClient:   &apitest.Fake{},
		BaseContext: t.Context,
	})
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

	userCredential := protocol.NewUserCredential()
	sess := session.New(t.Context(), s, mgr, nil,
		session.WithUserCredential(userCredential))
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, s,
		userclient.NewStoreReplyLog(s), userCredential)

	require.False(t, user.Caps().Has(protocol.CapOperator))

	require.NoError(t, user.Attach(t.Context()))

	require.True(t, user.Caps().Has(protocol.CapOperator))
}

// TestUserClient_attach_registers_the_connection_record pins the row
// the rest of the system reads this client through, and the reason
// a database written before the row existed needs no migration: the
// row is created on demand by an upsert on the first attach.
//
// A channel record naming this client in its member list resolves
// that entry back through the row, so without it the entry would be
// dropped at load and every pointer comparison downstream would
// stop matching.
func TestUserClient_attach_registers_the_connection_record(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	ctx := t.Context()

	_, err := s.GetInstanceByID(ctx, domain.InstanceID(protocol.UserClientID))
	require.Error(t, err, "a database written before this row holds no row for it")

	mgr := modelmanager.New(modelmanager.Config{
		Store:       s,
		APIClient:   &apitest.Fake{},
		BaseContext: t.Context,
	})
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

	userCredential := protocol.NewUserCredential()
	sess := session.New(t.Context(), s, mgr, nil,
		session.WithUserCredential(userCredential))
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, s,
		userclient.NewStoreReplyLog(s), userCredential)
	require.NoError(t, user.Attach(ctx))

	row, err := s.GetInstanceByID(ctx, domain.InstanceID(protocol.UserClientID))
	require.NoError(t, err)
	require.Equal(t, user.ID(), row.ID())
	require.Equal(t, user.Nick(), row.Nick())

	require.NoError(t, user.Join(ctx, "#general"))

	w, err := user.Window(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, protocol.ChannelWindowTarget("#general"), w.Target())
}

func TestUserClient_attach_is_idempotent(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.user.Attach(t.Context()))
	require.NotNil(t, f.user.Subscription())
}

func TestUserClient_Join_routes_through_dispatcher(t *testing.T) {
	f := newFixture(t)

	require.NoError(t, f.user.Join(t.Context(), "#general"))

	channels := f.user.Channels()
	require.NotNil(t, channels)
	_, ok := channels.Get("#general")
	require.True(t, ok)
}

// TestUserClient_joining_marks_the_channel_read pins that arriving
// in a channel leaves nothing showing as unread. The read cursor is
// this client's state, so the client is what stamps it; the server
// does not touch it.
//
// Every way the user joins has to behave the same. `/join` builds
// its own [protocol.Join] in the chatcmd grammar and dispatches it
// through `Send`, never touching [userclient.UserClient.Join], so a
// cursor stamped only in `Join` would leave the app's primary join
// command showing a badge the moment you arrive. The unprefixed
// case covers `/join general`, where the dispatcher normalises the
// name and the cursor has to land on the same key the events did.
//
// The rejoin case is the one that matters most in use: a channel
// left and rejoined must not count the backlog it was away for.
func TestUserClient_joining_marks_the_channel_read(t *testing.T) {
	tests := []struct {
		name  string
		setUp func(t *testing.T, f *fixture)
		join  func(t *testing.T, f *fixture)
	}{
		{
			name:  "Join does not count its own arrival",
			setUp: func(*testing.T, *fixture) {},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Join(t.Context(), "#general"))
			},
		},
		{
			name:  "a JOIN dispatched through Send does not count its own arrival",
			setUp: func(*testing.T, *fixture) {},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				resp, err := f.user.Send(t.Context(), protocol.Join{Channels: []domain.ChannelName{"#general"}})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
			},
		},
		{
			name:  "a JOIN naming the channel unprefixed stamps the normalised name",
			setUp: func(*testing.T, *fixture) {},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				resp, err := f.user.Send(t.Context(), protocol.Join{Channels: []domain.ChannelName{"general"}})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
			},
		},
		{
			name: "a rejoin does not count the backlog",
			setUp: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Join(t.Context(), "#general"))
				require.NoError(t, f.user.Part(t.Context(), "#general", "brb"))

				for _, body := range []string{"first", "second", "third"} {
					_, err := f.store.AppendEvent(t.Context(), "#general", domain.Message{
						Source: domain.ClientSource("inst-botty", "botty"),
						Target: "#general", Body: body,
					})
					require.NoError(t, err)
				}
			},
			join: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Join(t.Context(), "#general"))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setUp(t, f)
			tt.join(t, f)

			unread, err := f.sess.UnreadCount(t.Context(), "#general")
			require.NoError(t, err)
			require.Equal(t, 0, unread)
		})
	}
}

// TestUserClient_a_message_after_joining_counts_as_unread pins the
// other half: the cursor stamped on arrival must not swallow what
// arrives afterwards. This is what puts the badge on an inactive
// channel in the sidebar.
func TestUserClient_a_message_after_joining_counts_as_unread(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	require.NoError(t, f.user.Join(ctx, "#general"))

	unread, err := f.sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, unread)

	_, err = f.store.AppendEvent(ctx, "#general", domain.Message{
		Source: domain.ClientSource("inst-botty", "botty"),
		Target: "#general", Body: "hello after you arrived",
	})
	require.NoError(t, err)

	unread, err = f.sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 1, unread)
}

// TestUserClient_multi_target_join_stamps_only_the_channels_that_joined
// covers the read-cursor half of a multi-target JOIN (RFC 2812
// §3.2.1): a gate refusal on one channel must not withhold the
// cursor stamp the other, successful channel in the same command
// has already earned.
func TestUserClient_multi_target_join_stamps_only_the_channels_that_joined(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	locked := domain.NewChannelWindow("#locked", time.Now())
	locked.Modes = domain.ChannelModes{InviteOnly: true}
	require.NoError(t, f.store.SaveWindow(ctx, locked))

	for _, body := range []string{"first", "second"} {
		_, err := f.store.AppendEvent(ctx, "#locked", domain.Message{
			Source: domain.ClientSource("inst-botty", "botty"),
			Target: "#locked", Body: body,
		})
		require.NoError(t, err)
	}

	resp, err := f.user.Send(ctx, protocol.Join{Channels: []domain.ChannelName{"#general", "#locked"}})
	require.NoError(t, err)

	var refused domain.ChannelInviteOnlyError
	require.ErrorAs(t, resp.Err, &refused)
	require.Equal(t, domain.ChannelName("#locked"), refused.Channel)

	generalUnread, err := f.sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, generalUnread, "the channel that joined must have its cursor stamped")

	lockedUnread, err := f.sess.UnreadCount(ctx, "#locked")
	require.NoError(t, err)
	require.Equal(t, 2, lockedUnread, "the refused channel must not have its cursor stamped")
}

func TestUserClient_partial_join_updates_state_before_returning_an_execution_error(t *testing.T) {
	tests := []struct {
		name          string
		cancelCommand bool
	}{
		{name: "transaction fails"},
		{name: "transaction sees command cancellation", cancelCommand: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			backing := storetest.NewMemoryStore(t)
			bot := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
			require.NoError(t, backing.SaveInstance(ctx, bot))

			for _, channel := range []domain.ChannelName{"#general", "#locked"} {
				window := domain.NewChannelWindow(channel, time.Now())
				window.Members.Add(bot)
				if channel == "#locked" {
					window.Modes = domain.ChannelModes{InviteOnly: true}
				}
				require.NoError(t, backing.SaveWindow(ctx, window))
				for _, body := range []string{"first", "second"} {
					_, err := backing.AppendEvent(ctx, channel, domain.Message{
						Source: domain.ClientSource(bot.ID(), bot.Nick()),
						Target: channel, Body: body,
					})
					require.NoError(t, err)
				}
			}

			executionFailure := errors.New("partial join failure")
			failing := &joinFailureStore{
				Store: backing, channel: "#broken", err: executionFailure,
			}
			sendCtx := ctx
			if tc.cancelCommand {
				var cancel context.CancelFunc
				sendCtx, cancel = context.WithCancel(ctx)
				failing.cancel = cancel
				failing.err = nil
			}

			mgr := modelmanager.New(modelmanager.Config{
				Store: backing, APIClient: &apitest.Fake{}, BaseContext: t.Context,
			})
			t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

			credential := protocol.NewUserCredential()
			sess := session.New(ctx, failing, mgr, nil,
				session.WithUserCredential(credential))
			t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

			user := userclient.New("testuser", sess, backing,
				userclient.NewStoreReplyLog(backing), credential)
			require.NoError(t, user.Attach(ctx))

			response, executionErr := user.Send(sendCtx, protocol.Join{Channels: []domain.ChannelName{
				"#general", "#locked", "#broken",
			}})

			events := slices.Clone(response.Events)
			for i, event := range events {
				if refusal, ok := event.(domain.ChannelInviteOnlyError); ok {
					refusal.At = time.Time{}
					events[i] = refusal
				}
			}

			autojoin, err := backing.ListAutojoinChannels(ctx)
			require.NoError(t, err)
			generalUnread, err := sess.UnreadCount(ctx, "#general")
			require.NoError(t, err)
			lockedUnread, err := sess.UnreadCount(ctx, "#locked")
			require.NoError(t, err)
			brokenWindow, brokenWindowErr := user.Window(ctx, "#broken")
			inBrokenBeforeRetry := user.InChannel("#broken")
			_, storedWindowErr := backing.GetWindow(ctx, "#broken")
			storedUser, storedUserErr := backing.GetInstanceByID(ctx, user.ID())
			storedInstanceUnjoined := storedUserErr == nil && !storedUser.InChannel("#broken")
			failing.err = nil
			failing.cancel = nil
			retryResponse, retryErr := user.Send(ctx, protocol.Join{Channels: []domain.ChannelName{"#broken"}})

			expectedExecutionErr := executionFailure
			if tc.cancelCommand {
				expectedExecutionErr = context.Canceled
			}
			var joinExecutionErr protocol.JoinExecutionError
			require.ErrorAs(t, executionErr, &joinExecutionErr)
			require.ErrorIs(t, joinExecutionErr.Err, expectedExecutionErr)
			var responseErr domain.ChannelInviteOnlyError
			require.ErrorAs(t, response.Err, &responseErr)
			responseErr.At = time.Time{}

			type assertionSnapshot struct {
				Events                 []protocol.Event
				ResponseError          domain.ChannelInviteOnlyError
				ExecutionChannel       domain.ChannelName
				Autojoin               []domain.ChannelName
				GeneralUnread          int
				LockedUnread           int
				InGeneral              bool
				InLocked               bool
				InBrokenBeforeRetry    bool
				BrokenWindow           protocol.WindowTarget
				BrokenWindowOK         bool
				StoredWindowAbsent     bool
				StoredInstanceUnjoined bool
				RetryResponse          protocol.Response
				RetryOK                bool
			}

			require.Equal(t, assertionSnapshot{
				Events: []protocol.Event{
					domain.JoinedChannel{Channel: "#general"},
					domain.ChannelInviteOnlyError{Channel: "#locked"},
				},
				ResponseError:          domain.ChannelInviteOnlyError{Channel: "#locked"},
				ExecutionChannel:       "#broken",
				Autojoin:               []domain.ChannelName{"#general"},
				GeneralUnread:          0,
				LockedUnread:           2,
				InGeneral:              true,
				StoredWindowAbsent:     true,
				StoredInstanceUnjoined: true,
				RetryResponse: protocol.Response{Events: []protocol.Event{
					domain.JoinedChannel{Channel: "#broken"},
				}},
				RetryOK: true,
			}, assertionSnapshot{
				Events:              events,
				ResponseError:       responseErr,
				ExecutionChannel:    joinExecutionErr.Channel,
				Autojoin:            autojoin,
				GeneralUnread:       generalUnread,
				LockedUnread:        lockedUnread,
				InGeneral:           user.InChannel("#general"),
				InLocked:            user.InChannel("#locked"),
				InBrokenBeforeRetry: inBrokenBeforeRetry,
				BrokenWindow: func() protocol.WindowTarget {
					if brokenWindow == nil {
						return nil
					}
					return brokenWindow.Target()
				}(),
				BrokenWindowOK: brokenWindowErr == nil,
				StoredWindowAbsent: errors.Is(
					storedWindowErr, storemod.ErrNoSuchChannel,
				),
				StoredInstanceUnjoined: storedInstanceUnjoined,
				RetryResponse:          retryResponse,
				RetryOK:                retryErr == nil,
			})
		})
	}
}

func TestUserClient_Drain_waits_for_cancelled_command_bookkeeping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		backing := storetest.NewMemoryStore(t)
		mgr := modelmanager.New(modelmanager.Config{
			Store: backing, APIClient: &apitest.Fake{}, BaseContext: t.Context,
		})
		t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

		credential := protocol.NewUserCredential()
		sess := session.New(ctx, backing, mgr, nil,
			session.WithUserCredential(credential))
		t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

		clientStore := &blockingAutojoinStore{
			Store:   backing,
			started: make(chan struct{}),
			proceed: make(chan struct{}),
		}
		user := userclient.New("testuser", sess, clientStore,
			userclient.NewStoreReplyLog(backing), credential)
		require.NoError(t, user.Attach(ctx))

		commandCtx, cancelCommand := context.WithCancel(ctx)
		defer cancelCommand()

		type commandResult struct {
			Response protocol.Response
			Error    error
		}
		commandDone := make(chan commandResult, 1)
		go func() {
			resp, err := user.Send(commandCtx, protocol.Join{
				Channels: []domain.ChannelName{"#general"},
			})
			commandDone <- commandResult{Response: resp, Error: err}
		}()

		<-clientStore.started
		cancelCommand()

		drainDone := make(chan error, 1)
		go func() { drainDone <- user.Drain(ctx) }()
		synctest.Wait()

		select {
		case err := <-drainDone:
			t.Fatalf("drain returned before autojoin was stored: %v", err)
		default:
		}

		close(clientStore.proceed)
		result := <-commandDone
		drainErr := <-drainDone
		autojoin, err := backing.ListAutojoinChannels(ctx)
		require.NoError(t, err)
		closedResponse, closedErr := user.Send(ctx, protocol.Join{
			Channels: []domain.ChannelName{"#other"},
		})

		type assertionSnapshot struct {
			CommandResponse protocol.Response
			CommandError    error
			DrainError      error
			Autojoin        []domain.ChannelName
			ClosedResponse  protocol.Response
			ClosedError     error
		}

		require.Equal(t, assertionSnapshot{
			CommandResponse: protocol.Response{Events: []protocol.Event{
				domain.JoinedChannel{Channel: "#general"},
			}},
			Autojoin:    []domain.ChannelName{"#general"},
			ClosedError: protocol.ErrSubscriptionClosed,
		}, assertionSnapshot{
			CommandResponse: result.Response,
			CommandError:    result.Error,
			DrainError:      drainErr,
			Autojoin:        autojoin,
			ClosedResponse:  closedResponse,
			ClosedError:     closedErr,
		})
	})
}

func TestUserClient_Drain_waits_for_autojoin_restore_bookkeeping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		backing := storetest.NewMemoryStore(t)
		require.NoError(t, backing.SetAutojoinChannels(ctx,
			[]domain.ChannelName{"#general"}))

		mgr := modelmanager.New(modelmanager.Config{
			Store: backing, APIClient: &apitest.Fake{}, BaseContext: t.Context,
		})
		t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

		credential := protocol.NewUserCredential()
		sess := session.New(ctx, backing, mgr, nil,
			session.WithUserCredential(credential))
		t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

		clientStore := &blockingAutojoinStore{
			Store:   backing,
			started: make(chan struct{}),
			proceed: make(chan struct{}),
		}
		user := userclient.New("testuser", sess, clientStore,
			userclient.NewStoreReplyLog(backing), credential)
		require.NoError(t, user.Attach(ctx))

		restoreDone := make(chan error, 1)
		go func() { restoreDone <- user.JoinAutojoinChannels(ctx) }()
		<-clientStore.started

		drainDone := make(chan error, 1)
		go func() { drainDone <- user.Drain(ctx) }()
		synctest.Wait()

		var earlyDrain *error
		select {
		case err := <-drainDone:
			earlyDrain = &err
		default:
		}

		close(clientStore.proceed)
		restoreErr := <-restoreDone
		var drainErr error
		if earlyDrain != nil {
			drainErr = *earlyDrain
		} else {
			drainErr = <-drainDone
		}
		autojoin, err := backing.ListAutojoinChannels(ctx)
		require.NoError(t, err)

		type assertionSnapshot struct {
			DrainWaited  bool
			RestoreError error
			DrainError   error
			Autojoin     []domain.ChannelName
		}

		require.Equal(t, assertionSnapshot{
			DrainWaited: true,
			Autojoin:    []domain.ChannelName{"#general"},
		}, assertionSnapshot{
			DrainWaited:  earlyDrain == nil,
			RestoreError: restoreErr,
			DrainError:   drainErr,
			Autojoin:     autojoin,
		})
	})
}

func TestUserClient_Drain_waits_for_dm_window_bookkeeping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		backing := storetest.NewMemoryStore(t)
		require.NoError(t, backing.SaveInstance(ctx, domain.NewModelInstance(
			"inst-botty", "botty", "test/model", "", nil,
		)))
		mgr := modelmanager.New(modelmanager.Config{
			Store: backing, APIClient: &apitest.Fake{}, BaseContext: t.Context,
		})
		t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

		credential := protocol.NewUserCredential()
		sess := session.New(ctx, backing, mgr, nil,
			session.WithUserCredential(credential))
		t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

		clientStore := &blockingDMWindowStore{
			Store: backing, started: make(chan struct{}), proceed: make(chan struct{}),
		}
		user := userclient.New("testuser", sess, clientStore,
			userclient.NewStoreReplyLog(backing), credential)
		require.NoError(t, user.Attach(ctx))

		write := user.RecordDMWindowOpen("inst-botty")
		<-clientStore.started

		drainDone := make(chan error, 1)
		go func() { drainDone <- user.Drain(ctx) }()
		synctest.Wait()

		var earlyDrain *error
		select {
		case err := <-drainDone:
			earlyDrain = &err
		default:
		}

		close(clientStore.proceed)
		writeErr := write.Wait()
		var drainErr error
		if earlyDrain != nil {
			drainErr = *earlyDrain
		} else {
			drainErr = <-drainDone
		}
		open, err := backing.ListDMWindows(ctx)
		require.NoError(t, err)
		closedErr := user.RecordDMWindowOpen("inst-helper").Wait()

		type assertionSnapshot struct {
			DrainWaited bool
			WriteError  error
			DrainError  error
			Open        []domain.InstanceID
			ClosedError error
		}

		require.Equal(t, assertionSnapshot{
			DrainWaited: true,
			Open:        []domain.InstanceID{"inst-botty"},
			ClosedError: protocol.ErrSubscriptionClosed,
		}, assertionSnapshot{
			DrainWaited: earlyDrain == nil,
			WriteError:  writeErr,
			DrainError:  drainErr,
			Open:        open,
			ClosedError: closedErr,
		})
	})
}

func TestUserClient_UI_state_writes_finish_in_acceptance_order(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	require.NoError(t, backing.SaveInstance(t.Context(), domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "", nil,
	)))
	require.NoError(t, backing.AddDMWindow(t.Context(), "inst-botty"))
	clientStore := &blockingDMWindowCloseStore{
		Store: backing, started: make(chan struct{}), proceed: make(chan struct{}),
	}
	user := userclient.New("testuser", nil, clientStore, nil, nil)

	closeWrite := user.RecordDMWindowClosed("inst-botty")
	<-clientStore.started
	openWrite := user.RecordDMWindowOpen("inst-botty")

	close(clientStore.proceed)
	closeErr := closeWrite.Wait()
	openErr := openWrite.Wait()
	open, err := backing.ListDMWindows(t.Context())
	require.NoError(t, err)

	type assertionSnapshot struct {
		CloseError error
		OpenError  error
		Open       []domain.InstanceID
	}

	require.Equal(t, assertionSnapshot{
		Open: []domain.InstanceID{"inst-botty"},
	}, assertionSnapshot{
		CloseError: closeErr,
		OpenError:  openErr,
		Open:       open,
	})
}

func TestUserClient_UI_state_writes_begin_in_acceptance_order(t *testing.T) {
	backing := storetest.NewMemoryStore(t)
	require.NoError(t, backing.SaveInstance(t.Context(), domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "", nil,
	)))
	clientStore := &orderedUIStateStore{Store: backing}
	user := userclient.New("testuser", nil, clientStore, nil, nil)

	openWrite := user.RecordDMWindowOpen("inst-botty")
	focusWrite := user.RecordLastWindow(domain.NewStatusWindow(time.Time{}))

	openWriteErr := openWrite.Wait()
	focusWriteErr := focusWrite.Wait()
	open, openReadErr := backing.ListDMWindows(t.Context())
	last, lastErr := backing.GetLastWindow(t.Context())

	type assertionSnapshot struct {
		OpenWriteError  error
		FocusWriteError error
		OpenReadError   error
		Open            []domain.InstanceID
		LastReadError   error
		Last            domain.Window
		Effects         []string
	}

	require.Equal(t, assertionSnapshot{
		Open:    []domain.InstanceID{"inst-botty"},
		Last:    domain.WindowKey(domain.StatusChannelName),
		Effects: []string{"open inst-botty", "focus " + string(domain.StatusChannelName)},
	}, assertionSnapshot{
		OpenWriteError:  openWriteErr,
		FocusWriteError: focusWriteErr,
		OpenReadError:   openReadErr,
		Open:            open,
		LastReadError:   lastErr,
		Last:            last,
		Effects:         clientStore.Effects(),
	})
}

func TestUserClient_JoinAutojoinChannels_emits_aggregate_span(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	f := newFixture(t)
	ctx := t.Context()

	require.NoError(t, f.store.SetAutojoinChannels(ctx,
		[]domain.ChannelName{"#alpha", "#beta"}))

	require.NoError(t, f.user.JoinAutojoinChannels(ctx))

	span := oteltest.FindSpan(t, recorder, "userclient.autojoin")
	require.Equal(t, "2",
		oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinCount))
	require.Equal(t, "0",
		oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinFailed))
	require.Equal(t, `["#alpha","#beta"]`,
		oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinChannels))
}

func TestUserClient_JoinAutojoinChannels_counts_only_unconfirmed_targets(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previousProvider) })

	ctx := t.Context()
	backing := storetest.NewMemoryStore(t)
	locked := domain.NewChannelWindow("#locked", time.Now())
	locked.Modes = domain.ChannelModes{InviteOnly: true}
	require.NoError(t, backing.SaveWindow(ctx, locked))
	require.NoError(t, backing.SetAutojoinChannels(ctx,
		[]domain.ChannelName{"#general", "#locked", "#broken"}))

	executionFailure := errors.New("disk full")
	failing := &joinFailureStore{
		Store: backing, channel: "#broken", err: executionFailure,
	}
	mgr := modelmanager.New(modelmanager.Config{
		Store: backing, APIClient: &apitest.Fake{}, BaseContext: t.Context,
	})
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })

	credential := protocol.NewUserCredential()
	sess := session.New(ctx, failing, mgr, nil,
		session.WithUserCredential(credential))
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })
	user := userclient.New("testuser", sess, backing,
		userclient.NewStoreReplyLog(backing), credential)
	require.NoError(t, user.Attach(ctx))

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if len(groups) == 0 && attr.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return attr
		},
	})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	require.NoError(t, user.JoinAutojoinChannels(ctx))

	var autojoinLogs []map[string]any
	decoder := json.NewDecoder(&logs)
	for {
		var record map[string]any
		err := decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if record["msg"] == "autojoin channel" || record["msg"] == "autojoin channels" {
			autojoinLogs = append(autojoinLogs, record)
		}
	}

	autojoin, err := backing.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	span := oteltest.FindSpan(t, recorder, "userclient.autojoin")

	type assertionSnapshot struct {
		Autojoin  []domain.ChannelName
		InGeneral bool
		InLocked  bool
		InBroken  bool
		Count     string
		Failed    string
		Channels  string
		Logs      []map[string]any
	}

	require.Equal(t, assertionSnapshot{
		Autojoin:  []domain.ChannelName{"#general"},
		InGeneral: true,
		Count:     "3",
		Failed:    "2",
		Channels:  `["#broken","#general","#locked"]`,
		Logs: []map[string]any{
			{
				"component": "userclient",
				"error":     "cannot join #locked: invite-only channel",
				"level":     "ERROR",
				"msg":       "autojoin channel",
			},
			{
				"component":            "userclient",
				"requested_channels":   []any{"#broken", "#general", "#locked"},
				"confirmed_channels":   []any{"#general"},
				"unconfirmed_channels": []any{"#broken", "#locked"},
				"error":                "join #broken: disk full",
				"level":                "ERROR",
				"msg":                  "autojoin channels",
			},
		},
	}, assertionSnapshot{
		Autojoin:  autojoin,
		InGeneral: user.InChannel("#general"),
		InLocked:  user.InChannel("#locked"),
		InBroken:  user.InChannel("#broken"),
		Count:     oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinCount),
		Failed:    oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinFailed),
		Channels:  oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinChannels),
		Logs:      autojoinLogs,
	})
}

// TestUserClient_membership_commands_rewrite_the_autojoin_list pins
// where the list is written from. It is this client's own record of
// what to rejoin next time, so the client rewrites it after each
// command of its own that changes which channels it is in, and the
// session never touches it.
//
// QUIT is the case that does not rewrite it. Ending a connection
// does not say the channels should stay behind, and the list is
// what brings them back on the next one.
func TestUserClient_membership_commands_rewrite_the_autojoin_list(t *testing.T) {
	tests := []struct {
		name string
		act  func(t *testing.T, f *fixture)
		want []domain.ChannelName
	}{
		{
			name: "a JOIN adds the channel",
			act:  func(*testing.T, *fixture) {},
			want: []domain.ChannelName{"#dev", "#general"},
		},
		{
			name: "a PART drops the channel",
			act: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Part(t.Context(), "#general", "bye"))
			},
			want: []domain.ChannelName{"#dev"},
		},
		{
			name: "a KICK of this client's own nick drops the channel",
			act: func(t *testing.T, f *fixture) {
				t.Helper()

				resp, err := f.user.Send(t.Context(), protocol.Kick{Channel: "#general", Nick: f.user.Nick()})
				require.NoError(t, err)
				require.NoError(t, resp.Err)
			},
			want: []domain.ChannelName{"#dev"},
		},
		{
			name: "a QUIT leaves the list standing",
			act: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Quit(t.Context(), "bye"))
			},
			want: []domain.ChannelName{"#dev", "#general"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()

			require.NoError(t, f.user.Join(ctx, "#general"))
			require.NoError(t, f.user.Join(ctx, "#dev"))

			tc.act(t, f)

			got, err := f.store.ListAutojoinChannels(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestUserClient_ObserveKick_excludes_the_channel_before_membership_changes(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	require.NoError(t, f.user.Join(ctx, "#general"))
	require.NoError(t, f.user.Join(ctx, "#dev"))

	persist := f.user.ObserveKick(domain.Kicked{
		Target: "#GENERAL", Subject: "oldnick", SubjectIsSelf: true,
	})
	require.NotNil(t, persist)
	persist(ctx)

	require.True(t, f.user.InChannel("#general"),
		"the observed event can arrive before the session updates membership")

	got, err := f.store.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev"}, got)
}

func TestUserClient_stale_kick_write_does_not_remove_a_later_rejoin(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	require.NoError(t, f.user.Join(ctx, "#general"))
	require.NoError(t, f.user.Join(ctx, "#dev"))

	persist := f.user.ObserveKick(domain.Kicked{
		Target: "#general", Subject: "oldnick", SubjectIsSelf: true,
	})
	require.NotNil(t, persist)

	resp, err := f.user.Send(ctx, protocol.Kick{Channel: "#general", Nick: f.user.Nick()})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	require.NoError(t, f.user.Join(ctx, "#general"))

	persist(ctx)

	got, err := f.store.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev", "#general"}, got)
}

// TestUserClient_autojoin_list_omits_the_status_window pins the
// filter on the way out. `&modeloff` is a legal channel name under
// RFC 2812 §1.3, so a JOIN naming it is admitted, but it is the
// client's own view of the server and lives as long as the session.
// Rejoining it on the next start would put a second window on the
// sidebar next to the one the chat-screen builds for itself.
func TestUserClient_autojoin_list_omits_the_status_window(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	require.NoError(t, f.user.Join(ctx, "#general"))
	require.NoError(t, f.user.Join(ctx, domain.StatusChannelName))

	got, err := f.store.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#general"}, got)
}

// TestUserClient_Quit_ends_the_session_active_marker covers the
// crash marker's other half. The session clears it once the user's
// QUIT teardown is durable. A run that ends without a QUIT leaves it
// in place.
func TestUserClient_Quit_ends_the_session_active_marker(t *testing.T) {
	tests := []struct {
		name string
		act  func(t *testing.T, f *fixture)
		want string
	}{
		{
			name: "a QUIT clears it",
			act: func(t *testing.T, f *fixture) {
				t.Helper()

				require.NoError(t, f.user.Quit(t.Context(), "bye"))
			},
			want: "",
		},
		{
			name: "a run that never quits leaves it",
			act:  func(*testing.T, *fixture) {},
			want: "open",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := t.Context()

			require.NoError(t, f.store.SetSessionActive(ctx, "open"))
			require.NoError(t, f.user.Join(ctx, "#general"))

			tc.act(t, f)

			active, err := f.store.GetSessionActive(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.want, active)
		})
	}
}

func TestUserClient_RecordReply_persists_to_issuer_reply_log(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	reply := domain.CommandError{
		Target: "#general",
		Err:    "whois: no such nick",
		At:     time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	}

	f.user.RecordReply(ctx, protocol.ChannelWindowTarget("#dev"), reply)

	replies, err := f.store.InstanceRepliesBefore(ctx, domain.InstanceID(protocol.UserClientID), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []storemod.InstanceReplyRecord{{
		ID: 1, Window: protocol.ChannelWindowTarget("#dev"), Event: reply,
	}}, replies)
}

// TestUserClient_MarkRead_reads_the_window_it_marks pins where the
// read cursor comes from. A channel's newest event is under the
// channel's own key. A DM's is not: the two directions are logged
// under their recipients, so a cursor taken from the window's key
// alone stops at the last line the user sent and leaves everything
// the counterpart has said since counted as unread.
func TestUserClient_MarkRead_reads_the_window_it_marks(t *testing.T) {
	const peer = domain.InstanceID("inst-botty")

	tests := []struct {
		name   string
		window domain.ChannelName
		want   int64
	}{
		{name: "a channel reads its own log", window: "#general", want: 11},
		{name: "a DM reads the thread", window: domain.ChannelName(peer), want: 22},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)

			store := &recordingStore{
				channelHead: 11,
				threadHead:  22,
			}
			user := userclient.New("testuser", f.sess, store,
				userclient.NewStoreReplyLog(f.store), nil)

			require.NoError(t, user.MarkRead(t.Context(), tc.window))

			require.Equal(t, []lastRead{{channel: tc.window, eventID: tc.want}}, store.stamped)
		})
	}
}

// TestUserClient_DM_window_set_round_trips pins the client-owned
// record of which DM windows are open. A channel comes back on its
// own through autojoin; a DM window is not a membership the server
// holds, so this set is what the chat-screen reopens at bootstrap.
// Both writes are idempotent, and closing a window leaves the others
// alone.
func TestUserClient_DM_window_set_round_trips(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	for id, nick := range map[domain.InstanceID]domain.Nick{"inst-botty": "botty", "inst-helper": "helper"} {
		require.NoError(t, f.store.SaveInstance(ctx, domain.NewModelInstance(id, nick, "test/model", "", nil)))
	}

	require.NoError(t, f.user.RecordDMWindowOpen("inst-botty").Wait())
	require.NoError(t, f.user.RecordDMWindowOpen("inst-helper").Wait())
	require.NoError(t, f.user.RecordDMWindowOpen("inst-botty").Wait())

	open, err := f.user.DMWindows(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID{"inst-botty", "inst-helper"}, open)

	require.NoError(t, f.user.RecordDMWindowClosed("inst-botty").Wait())
	require.NoError(t, f.user.RecordDMWindowClosed("inst-botty").Wait())

	open, err = f.user.DMWindows(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID{"inst-helper"}, open)
}

// lastRead is one cursor write, whichever of
// [userclient.Store.SetLastRead] or [userclient.Store.SetDMLastRead]
// made it. For a DM call, channel holds the peer InstanceID converted
// to a [domain.ChannelName], matching the window it was asked to
// mark.
type lastRead struct {
	channel domain.ChannelName
	eventID int64
}

// recordingStore answers each read with a single event carrying a
// distinct id, so a test can tell which read a cursor came from, and
// records the cursors written to it.
type recordingStore struct {
	channelHead int64
	threadHead  int64
	stamped     []lastRead
	dmWindows   []domain.InstanceID
	autojoin    []domain.ChannelName
}

func (*recordingStore) ListAutojoinChannels(context.Context) ([]domain.ChannelName, error) {
	return nil, nil
}

func (s *recordingStore) SetAutojoinChannels(_ context.Context, channels []domain.ChannelName) error {
	s.autojoin = slices.Clone(channels)

	return nil
}

func (*recordingStore) SaveInstance(context.Context, *domain.Instance) error {
	return nil
}

func (*recordingStore) GetInstanceByID(context.Context, domain.InstanceID) (*domain.Instance, error) {
	return domain.NewUserInstance("testuser"), nil
}

func (s *recordingStore) ClearSessionActive(context.Context) error {
	return nil
}

func (s *recordingStore) EventsBefore(_ context.Context, _ domain.ChannelName, _ *int64, _ int) ([]domain.StoredEvent, error) {
	return []domain.StoredEvent{{ID: s.channelHead, Event: domain.Message{}}}, nil
}

func (s *recordingStore) DMEventsBefore(_ context.Context, _, _ domain.InstanceID, _ *int64, _ int) ([]domain.StoredEvent, error) {
	return []domain.StoredEvent{{ID: s.threadHead, Event: domain.Message{}}}, nil
}

func (s *recordingStore) SetLastRead(_ context.Context, ch domain.ChannelName, eventID int64) error {
	s.stamped = append(s.stamped, lastRead{channel: ch, eventID: eventID})

	return nil
}

func (s *recordingStore) SetDMLastRead(_ context.Context, peer domain.InstanceID, eventID int64) error {
	s.stamped = append(s.stamped, lastRead{channel: domain.ChannelName(peer), eventID: eventID})

	return nil
}

func (s *recordingStore) ListDMWindows(context.Context) ([]domain.InstanceID, error) {
	return slices.Clone(s.dmWindows), nil
}

func (s *recordingStore) AddDMWindow(_ context.Context, peer domain.InstanceID) error {
	if slices.Contains(s.dmWindows, peer) {
		return nil
	}

	s.dmWindows = append(s.dmWindows, peer)

	return nil
}

func (s *recordingStore) RemoveDMWindow(_ context.Context, peer domain.InstanceID) error {
	s.dmWindows = slices.DeleteFunc(s.dmWindows, func(open domain.InstanceID) bool { return open == peer })

	return nil
}

func (*recordingStore) SetLastWindow(context.Context, domain.Window) error {
	return nil
}

func (*recordingStore) ClearLastWindow(context.Context) error {
	return nil
}

// TestUserClient_autojoin_restore_writes_the_list_once covers the
// restore reading and rewriting the same list. It joins in chunks of
// [protocol.MaxJoinTargets], and a write between two chunks would
// leave the stored list holding only the channels joined so far: a
// process that ended there would come back to a list missing the
// tail it never reached. The write happens once, after the last
// chunk, and carries every channel.
func TestUserClient_autojoin_restore_writes_the_list_once(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	want := make([]domain.ChannelName, 0, protocol.MaxJoinTargets+3)
	for i := range cap(want) {
		want = append(want, domain.ChannelName(fmt.Sprintf("#chan%02d", i)))
	}

	require.NoError(t, f.store.SetAutojoinChannels(ctx, want))

	f.userStore.autojoinWrites = 0

	require.NoError(t, f.user.JoinAutojoinChannels(ctx))

	require.Equal(t, 1, f.userStore.autojoinWrites,
		"the restore spans more than one chunk and writes the list once, at the end")

	got, err := f.store.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, want, got)
}
