package session

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

// failingLoadStore fails every window load with `err` once armed.
// Tests use it to stand in for a store that is briefly unavailable.
type failingLoadStore struct {
	Store

	failing atomic.Bool
	err     error
}

func (f *failingLoadStore) GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error) {
	if f.failing.Load() {
		return nil, f.err
	}

	return f.Store.GetWindow(ctx, name)
}

// countingLoadStore records how many window loads reach the store.
type countingLoadStore struct {
	Store

	loads atomic.Int64
}

func (c *countingLoadStore) GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error) {
	c.loads.Add(1)

	return c.Store.GetWindow(ctx, name)
}

// newSessionWithStore builds a session over `wrapped`, otherwise
// matching [newTestSessionWithAPI].
func newSessionWithStore(t *testing.T, wrapped Store) *Session {
	t.Helper()

	sess := New(t.Context, wrapped, newTestModelClientFactory(t, &fakeAPIClient{}))
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess
}

// TestJoinAs_load_failure_leaves_the_channel_intact pins that a JOIN
// creates a channel only when the channel genuinely does not exist.
// Treating any load failure as "absent" would have the join write a
// blank record over a live channel, taking its topic, modes and
// invitation list with it.
func TestJoinAs_load_failure_leaves_the_channel_intact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backing := storetest.NewMemoryStore(t)
		ctx := t.Context()

		established := domain.NewChannelWindow("#dev", fixedTime)
		established.Topic = "shipping the thing"
		established.TopicSetBy = "testuser"
		established.TopicSetAt = fixedTime
		established.Modes = domain.ChannelModes{TopicLock: true, InviteOnly: true}
		established.InvitedNicks.Add("botty")
		require.NoError(t, backing.SaveWindow(ctx, established))

		// The session starts with cold live state, so the join has to
		// consult the store — which is where the failure lands.
		failing := &failingLoadStore{Store: backing, err: fmt.Errorf("database is locked")}
		sess := newSessionWithStore(t, failing)

		failing.failing.Store(true)

		require.Error(t, userJoin(ctx, t, sess, "#dev"))

		failing.failing.Store(false)

		reloaded, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		require.Equal(t, "shipping the thing", reloaded.Topic)
		require.Equal(t, domain.Nick("testuser"), reloaded.TopicSetBy)
		require.Equal(t, domain.ChannelModes{TopicLock: true, InviteOnly: true}, reloaded.Modes)
		require.True(t, reloaded.InvitedNicks.Contains("botty"))
	})
}

// TestLoadChannelWindow_serves_repeat_reads_from_live_state pins
// that the session answers channel questions from memory. The store
// is consulted once, to fill the record; after that every read —
// the send gates, the fan-out's `+a` check, a model assembling its
// prompt — is answered without touching the database.
func TestLoadChannelWindow_serves_repeat_reads_from_live_state(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counting := &countingLoadStore{Store: storetest.NewMemoryStore(t)}
		sess := newSessionWithStore(t, counting)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		before := counting.loads.Load()

		for range 5 {
			_, err := sess.loadChannelWindow(ctx, "#dev")
			require.NoError(t, err)

			_, err = sess.GetWindow(ctx, "#dev")
			require.NoError(t, err)

			modes, ok := sess.channelModes(ctx, "#dev")
			require.True(t, ok)
			require.Equal(t, domain.ChannelModes{}, modes)
		}

		require.Equal(t, before, counting.loads.Load(),
			"live channel state answers reads without a store round-trip")
	})
}

// TestLoadChannelWindow_hands_each_reader_its_own_copy pins the
// isolation live state depends on: a reader that mutates what it
// was given has not edited the record the next command will read.
func TestLoadChannelWindow_hands_each_reader_its_own_copy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#dev"),
		})
		seedChannelWithMembers(t, sess, s, "#dev", "testuser", botty.Nick())

		scribbled, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		scribbled.Topic = "scribbled over"
		scribbled.Members.RemoveInstance(botty)
		scribbled.InvitedNicks.Add("gatecrasher")

		fresh, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		require.Equal(t, "", fresh.Topic)
		require.True(t, fresh.Members.HasInstance(botty))
		require.False(t, fresh.InvitedNicks.Contains("gatecrasher"))
	})
}

// lockAssertingDeleteStore checks, from inside the store delete,
// that the session still holds its channel-state lock.
type lockAssertingDeleteStore struct {
	Store

	t    *testing.T
	sess *Session
}

func (l *lockAssertingDeleteStore) DeleteWindow(ctx context.Context, name domain.ChannelName) error {
	l.t.Helper()

	require.False(l.t, l.sess.channels.mu.TryLock(),
		"destroyChannel must hold channels.mu across the store delete")

	return l.Store.DeleteWindow(ctx, name)
}

// TestDestroyChannel_holds_the_lock_across_the_store_delete pins
// that destroying a channel is one step as far as any reader is
// concerned. A read that lands while the row is still being deleted
// must not fill live state from that row: the channel would come
// back with the topic, modes and invitation list RFC 2811 §2 says
// die with it, and a rejoin would find it established.
//
// The property is asserted where it holds — inside the delete —
// so the test does not depend on winning a scheduling race to
// detect a lock that was released too early.
func TestDestroyChannel_holds_the_lock_across_the_store_delete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asserting := &lockAssertingDeleteStore{Store: storetest.NewMemoryStore(t), t: t}
		sess := newSessionWithStore(t, asserting)
		asserting.sess = sess
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#brief"))
		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#brief", "here and gone"))

		require.NoError(t, userPart(ctx, t, sess, "#brief", "bye"))

		_, err := sess.loadChannelWindow(ctx, "#brief")
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel)

		_, live := sess.channelModes(ctx, "#brief")
		require.False(t, live)
	})
}

// TestCommitChannel_destroying_a_channel_clears_live_state pins that
// the last occupant leaving takes the live record with it, so a
// later read reports the channel as gone.
func TestCommitChannel_destroying_a_channel_clears_live_state(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#brief"))
		require.NoError(t, userPart(ctx, t, sess, "#brief", "bye"))

		_, err := sess.loadChannelWindow(ctx, "#brief")
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel)

		_, ok := sess.channelModes(ctx, "#brief")
		require.False(t, ok)
	})
}
