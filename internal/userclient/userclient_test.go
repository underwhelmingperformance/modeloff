package userclient_test

import (
	"context"
	"slices"
	"testing"
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

// fixture is the common setup the user-client tests share: an
// in-memory store, a noop-API model manager, a session, and an
// attached user-client.
type fixture struct {
	sess  *session.Session
	store *storemod.SQLiteStore
	user  *userclient.UserClient
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

	sess := session.New(t.Context, s, mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, s, userclient.NewStoreReplyLog(s))
	require.NoError(t, user.Attach(t.Context()))

	return &fixture{sess: sess, store: s, user: user}
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

	sess := session.New(t.Context, s, mgr, nil)
	t.Cleanup(func() { _ = sess.Shutdown(context.Background()) })

	user := userclient.New("testuser", sess, s, userclient.NewStoreReplyLog(s))

	require.False(t, user.Caps().Has(protocol.CapOperator))

	require.NoError(t, user.Attach(t.Context()))

	require.True(t, user.Caps().Has(protocol.CapOperator))
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
						Target: "#general",
						From:   "botty",
						Body:   body,
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
		Target: "#general",
		From:   "botty",
		Body:   "hello after you arrived",
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
			Target: "#locked",
			From:   "botty",
			Body:   body,
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

func TestUserClient_RecordReply_persists_to_issuer_reply_log(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	reply := domain.CommandError{
		Target: "#general",
		Err:    "whois: no such nick",
		At:     time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	}

	f.user.RecordReply(ctx, reply)

	replies, err := f.store.InstanceRepliesBefore(ctx, domain.InstanceID(protocol.UserClientID), nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{{ID: 1, Event: reply}}, replies)
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
			user := userclient.New("testuser", f.sess, store, userclient.NewStoreReplyLog(f.store))

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

	require.NoError(t, f.user.OpenDMWindow(ctx, "inst-botty"))
	require.NoError(t, f.user.OpenDMWindow(ctx, "inst-helper"))
	require.NoError(t, f.user.OpenDMWindow(ctx, "inst-botty"))

	open, err := f.user.DMWindows(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID{"inst-botty", "inst-helper"}, open)

	require.NoError(t, f.user.CloseDMWindow(ctx, "inst-botty"))
	require.NoError(t, f.user.CloseDMWindow(ctx, "inst-botty"))

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
}

func (*recordingStore) ListAutojoinChannels(context.Context) ([]domain.ChannelName, error) {
	return nil, nil
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
