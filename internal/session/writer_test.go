package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

// gatedStore delays every window load until `gate` is closed. Tests
// use it to park a command mid-flight at the point where it has
// decided what the channel looks like but has not yet written its
// mutation back, which is where an unserialised server loses one of
// two concurrent updates.
type gatedStore struct {
	Store

	gate <-chan struct{}
}

func (g *gatedStore) GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error) {
	<-g.gate

	return g.Store.GetWindow(ctx, name)
}

// bareClient is a [protocol.Client] with no dispatch machinery
// behind it: it carries an identity and an actor handle and routes
// `Send` straight at the dispatcher. Tests that want to issue
// commands as a model actor without an LLM turn attached use it.
type bareClient struct {
	sess     *Session
	id       protocol.ClientID
	instance *domain.Instance
	sub      protocol.Subscription
}

func (c *bareClient) Identity() protocol.ClientID { return c.id }

func (c *bareClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}

func (c *bareClient) Events() <-chan protocol.Delivery {
	if c.sub == nil {
		return nil
	}

	return c.sub.Events()
}

func (c *bareClient) Caps() command.CapabilityHolder { return bareCaps{} }

type bareCaps struct{}

func (bareCaps) Has(command.Capability) bool { return false }

// attachBareClient registers a [bareClient] for `inst` on `sess`.
func attachBareClient(t *testing.T, sess *Session, inst *domain.Instance) *bareClient {
	t.Helper()

	c := &bareClient{sess: sess, id: protocol.ClientID(inst.ID()), instance: inst}

	sub, err := sess.Subscribe(c, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)
	c.sub = sub

	return c
}

// newGatedTestSession builds a session whose window loads are held
// behind `gate`, mirroring [newTestSessionWithAPI] otherwise. The
// returned store is the underlying one, so test assertions read
// through it without tripping the gate.
func newGatedTestSession(t *testing.T, gate <-chan struct{}) (*Session, *storemod.SQLiteStore) {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	factory := newTestModelClientFactory(t, &fakeAPIClient{})

	sess := New(t.Context, &gatedStore{Store: s, gate: gate}, factory, nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s
}

// TestSession_concurrent_joins_do_not_lose_a_membership pins the
// single-writer guarantee. Two clients JOIN the same channel at the
// same time. Each JOIN reads the channel, adds its actor, and writes
// the result back; without server-side serialisation the two reads
// see the same state and the second write erases the first actor's
// membership.
//
// The gated store parks both commands inside their window load, so
// the test forces the interleaving and does not leave it to timing.
// Under a serial command loop the second JOIN cannot even reach the
// load until the first has committed.
func TestSession_concurrent_joins_do_not_lose_a_membership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		sess, s := newGatedTestSession(t, gate)
		ctx := t.Context()

		first := domain.NewModelInstance("inst-first", "first", "test/model", "", nil)
		second := domain.NewModelInstance("inst-second", "second", "test/model", "", nil)

		require.NoError(t, s.SaveInstance(ctx, first))
		require.NoError(t, s.SaveInstance(ctx, second))

		firstClient := attachBareClient(t, sess, first)
		secondClient := attachBareClient(t, sess, second)

		var (
			wg                  sync.WaitGroup
			firstErr, secondErr error
		)

		wg.Go(func() {
			resp, err := firstClient.Send(ctx, protocol.Join{Channel: "#room"})
			firstErr = errors.Join(err, resp.Err)
		})

		// Let the first JOIN reach the gated load before the second
		// is issued, so the two commands genuinely overlap.
		synctest.Wait()

		wg.Go(func() {
			resp, err := secondClient.Send(ctx, protocol.Join{Channel: "#room"})
			secondErr = errors.Join(err, resp.Err)
		})

		synctest.Wait()
		close(gate)
		wg.Wait()

		require.NoError(t, firstErr)
		require.NoError(t, secondErr)

		window, err := sess.loadChannelWindow(ctx, "#room")
		require.NoError(t, err)

		require.Equal(t, []domain.Nick{"first", "second"}, memberNicks(window))
	})
}

// TestSession_concurrent_nick_changes_cannot_both_claim_a_nick pins
// the same guarantee for the nick space. Two clients rename to the
// same nick at once; exactly one takes it and the other is refused
// with [domain.NickInUseError], because the check and the claim run
// as one step on the command loop.
func TestSession_concurrent_nick_changes_cannot_both_claim_a_nick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		sess, s := newGatedTestSession(t, gate)
		ctx := t.Context()

		first := domain.NewModelInstance("inst-first", "first", "test/model", "", nil)
		second := domain.NewModelInstance("inst-second", "second", "test/model", "", nil)

		require.NoError(t, s.SaveInstance(ctx, first))
		require.NoError(t, s.SaveInstance(ctx, second))

		firstClient := attachBareClient(t, sess, first)
		secondClient := attachBareClient(t, sess, second)

		// NICK does not load a window, so nothing here waits on the
		// gate; closing it up front keeps the fixture honest.
		close(gate)

		var (
			wg                  sync.WaitGroup
			firstErr, secondErr error
		)

		wg.Go(func() {
			resp, err := firstClient.Send(ctx, protocol.Nick{New: "shared"})
			firstErr = errors.Join(err, resp.Err)
		})

		wg.Go(func() {
			resp, err := secondClient.Send(ctx, protocol.Nick{New: "shared"})
			secondErr = errors.Join(err, resp.Err)
		})

		wg.Wait()

		switch {
		case firstErr == nil && secondErr == nil:
			t.Fatal("both renames claimed the nick")
		case firstErr != nil && secondErr != nil:
			t.Fatalf("both renames were refused: %v / %v", firstErr, secondErr)
		}

		winner, loser := first, second
		refusal := secondErr

		if secondErr == nil {
			winner, loser = second, first
			refusal = firstErr
		}

		require.Equal(t, domain.Nick("shared"), winner.Nick())
		require.NotEqual(t, domain.Nick("shared"), loser.Nick())

		var inUse domain.NickInUseError
		require.ErrorAs(t, refusal, &inUse)
		require.Equal(t, domain.NickInUseError{Nick: "shared", At: fixedTime}, inUse)
	})
}

// TestSession_add_model_refuses_a_nick_taken_after_preparation pins
// the ADDMODEL half of the same rule. The nick a model is given is
// chosen off the command loop, against a snapshot of the nick space
// that a rename can invalidate before the instance is registered.
// The claim on the loop is what decides, so a nick taken in the
// meantime refuses the add and no duplicate is minted.
func TestSession_add_model_refuses_a_nick_taken_after_preparation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		// The fixture factory hands out a fixed nick; give it to an
		// instance that already exists so the claim collides.
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "fakenick",
			ModelID:  "test/model",
			Channels: testChannels("#dev"),
		})

		err := addModelViaWire(ctx, t, sess, "#dev", "test/model", "")

		var inUse domain.NickInUseError
		require.ErrorAs(t, err, &inUse)
		require.Equal(t, domain.NickInUseError{Nick: "fakenick", At: fixedTime}, inUse)
	})
}

// TestSession_a_silent_consumer_does_not_stall_the_command_loop pins
// that the server keeps serving while one client stops reading. A
// model that is mid-turn is not draining its events channel and is
// itself waiting on the loop for a command; if the loop blocked
// handing that model a delivery, the two would wait on each other
// and every other client would wait behind them.
//
// The silent client here stands in for that model: it is subscribed,
// it is in the channel, and it never reads. The traffic sent past it
// is several times its channel's capacity, and a third client's
// command still completes afterwards.
func TestSession_a_silent_consumer_does_not_stall_the_command_loop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		unpaceFlood(sess)

		ctx := t.Context()

		silent := domain.NewModelInstance("inst-silent", "silent", "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, silent))
		attachBareClient(t, sess, silent)

		talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "", nil)
		require.NoError(t, s.SaveInstance(ctx, talker))
		talkerClient := attachBareClient(t, sess, talker)

		require.NoError(t, userJoin(ctx, t, sess, "#busy"))

		resp, err := talkerClient.Send(ctx, protocol.Join{Channel: "#busy"})
		require.NoError(t, errors.Join(err, resp.Err))

		for i := range eventBufSize * 3 {
			resp, err := talkerClient.Send(ctx, protocol.PrivMsg{
				Target: "#busy",
				Body:   fmt.Sprintf("message %d", i),
			})
			require.NoError(t, errors.Join(err, resp.Err))
		}

		// A third client's command still gets through, so the loop was
		// never held up by the client that stopped reading.
		require.NoError(t, userSetTopic(ctx, t, sess, "#busy", "still serving"))

		window, err := sess.loadChannelWindow(ctx, "#busy")
		require.NoError(t, err)
		require.Equal(t, "still serving", window.Topic)
	})
}

// TestSession_add_model_rolls_back_when_the_join_is_refused pins
// the unwind for the half of ADDMODEL that can fail after the
// instance already exists. Registration and the JOIN are separate
// trips through the command loop, so a JOIN the channel refuses —
// here an `+i` gate the new nick was never invited past — lands
// with the nick already claimed and the model-client already
// attached. Left in place that is a nick nobody can reuse and a
// dispatch goroutine nobody will ever stop.
func TestSession_add_model_rolls_back_when_the_join_is_refused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		// `admitModelAs` seeds the invitation that clears `+i`, so
		// close the channel to the limit instead: `+l 1` is already
		// met by the user.
		setChannelModes(t, sess, "#dev", domain.ChannelModes{InviteOnly: true, UserLimit: 1})

		err := addModelViaWire(ctx, t, sess, "#dev", "test/model", "")

		var full domain.ChannelFullError
		require.ErrorAs(t, err, &full)
		require.Equal(t, domain.ChannelFullError{Channel: "#dev", At: fixedTime}, full)

		synctest.Wait()

		// The nick is free again, so a later claim is not refused.
		require.NoError(t, sess.requireNickFree(ctx, "fakenick"))

		_, resolveErr := sess.ResolveNick(ctx, "fakenick")
		require.ErrorIs(t, resolveErr, storemod.ErrNoSuchNick)

		instances, err := s.ListInstances(ctx)
		require.NoError(t, err)
		require.Empty(t, instances, "the registered instance is deleted when its join is refused")

		factory, ok := sess.modelClientFactory.(*testModelClientFactory)
		require.True(t, ok)
		require.Empty(t, factory.attached(), "no model-client survives a refused add")

		window, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Equal(t, []domain.Nick{userNick(t, sess)}, memberNicks(window))
	})
}

// TestSession_reaping_a_subscription_mid_send_is_clean pins the
// teardown path for a client with a backlog. Reaping releases the
// send queue and closes `done` while the pump is parked handing a
// delivery over, so both arms of its select come ready at once and
// it may complete the send against a queue that is already gone.
// The pump has no recover, so getting that wrong takes the process
// with it.
//
// The consumer here drains slowly and unpredictably against a
// backlog several times its channel's capacity, and the reap lands
// while deliveries are still moving.
func TestSession_reaping_a_subscription_mid_send_is_clean(t *testing.T) {
	sess, s := newTestSession(t)
	unpaceFlood(sess)

	ctx := t.Context()

	talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, talker))
	talkerClient := attachBareClient(t, sess, talker)

	require.NoError(t, userJoin(ctx, t, sess, "#busy"))

	resp, err := talkerClient.Send(ctx, protocol.Join{Channel: "#busy"})
	require.NoError(t, errors.Join(err, resp.Err))

	// Which arm of the pump's select wins when both come ready is a
	// coin flip, so the scenario is run over a series of
	// subscriptions and any one of them is enough to catch it.
	for round := range 24 {
		id := domain.InstanceID(fmt.Sprintf("inst-reader-%d", round))

		reader := domain.NewModelInstance(id, domain.Nick(id), "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, reader))

		readerClient := attachBareClient(t, sess, reader)

		// Build a backlog with nobody reading, so the pump ends up
		// parked handing a delivery to a full channel.
		for i := range eventBufSize * 2 {
			resp, err := talkerClient.Send(ctx, protocol.PrivMsg{
				Target: "#busy",
				Body:   fmt.Sprintf("round %d message %d", round, i),
			})
			require.NoError(t, errors.Join(err, resp.Err))
		}

		// Freeing a slot and reaping at the same moment is what
		// leaves both of the pump's arms ready at once.
		var wg sync.WaitGroup

		wg.Go(func() {
			for {
				select {
				case <-readerClient.sub.Done():
					return
				case <-readerClient.Events():
				}
			}
		})

		readerClient.sub.Unsubscribe()

		wg.Wait()
	}

	// The session is still serving after the reaps.
	require.NoError(t, userSetTopic(ctx, t, sess, "#busy", "still serving"))
}

// TestSession_commands_after_shutdown_are_refused pins the command
// loop's lifecycle. Once the session has shut down there is no
// writer left to run a command, so a client that keeps sending is
// told so; it does not block on a queue nothing drains.
func TestSession_commands_after_shutdown_are_refused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))
		require.NoError(t, sess.Shutdown(ctx))

		synctest.Wait()

		_, err := userClient(t, sess).Send(ctx, protocol.Join{Channel: "#late"})
		require.ErrorIs(t, err, ErrSessionClosed)

		_, err = sess.loadChannelWindow(ctx, "#late")
		require.ErrorIs(t, err, storemod.ErrNoSuchChannel)
	})
}

// memberNicks returns the window's member nicks in member-list order.
func memberNicks(window *domain.ChannelWindow) []domain.Nick {
	var nicks []domain.Nick
	for m := range window.Members.All() {
		nicks = append(nicks, m.Nick)
	}

	return nicks
}
