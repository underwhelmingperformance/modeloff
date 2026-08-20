package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

var fixedTime = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

// testChannels builds an ordered map of channel names to a fixed
// join time for use in test instance construction.
func testChannels(names ...domain.ChannelName) *orderedmap.OrderedMap[domain.ChannelName, time.Time] {
	m := orderedmap.New[domain.ChannelName, time.Time]()
	for _, n := range names {
		m.Set(n, fixedTime)
	}

	return m
}

// requireChannels asserts that the given ordered map contains exactly
// the expected channel names, in order.
func requireChannels(t *testing.T, channels *orderedmap.OrderedMap[domain.ChannelName, time.Time], expected ...domain.ChannelName) {
	t.Helper()

	var got []domain.ChannelName
	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		got = append(got, pair.Key)
	}

	require.Equal(t, []domain.ChannelName(expected), got)
}

type channelEntry struct {
	Name     domain.ChannelName
	JoinedAt time.Time
}

type comparableInstance struct {
	Nick     domain.Nick
	ModelID  domain.ModelID
	Persona  string
	Channels []channelEntry
}

func normaliseInstance(inst *domain.Instance) comparableInstance {
	if inst == nil {
		return comparableInstance{}
	}

	var channels []channelEntry

	if ch := inst.Channels(); ch != nil {
		for pair := ch.Oldest(); pair != nil; pair = pair.Next() {
			channels = append(channels, channelEntry{Name: pair.Key, JoinedAt: pair.Value})
		}
	}

	return comparableInstance{
		Nick:     inst.Nick(),
		ModelID:  inst.ModelID,
		Persona:  inst.Persona(),
		Channels: channels,
	}
}

// userClient returns the registered user-client handle as a
// [protocol.Client]. The session-test fixture attaches a thin
// user-client at construction time (see [newTestSessionWithAPI]),
// and tests reach for it via this accessor instead of holding a
// reference through the fixture struct.
func userClient(t testing.TB, sess *Session) protocol.Client {
	t.Helper()

	c := sess.LookupClient(protocol.UserClientID)
	require.NotNil(t, c, "user-client must be attached for this test")

	return c
}

// userInstance returns the user's `*domain.Instance` from the
// registered user-client subscription. Panics if no user-client
// is attached — every test path through [newTestSessionWithAPI]
// attaches one.
func userInstance(t testing.TB, sess *Session) *domain.Instance {
	t.Helper()

	inst := sess.userInstance()
	require.NotNil(t, inst, "user-client must be attached for this test")

	return inst
}

// userNick is shorthand for `userInstance(t, sess).Nick()`.
func userNick(t testing.TB, sess *Session) domain.Nick {
	t.Helper()

	return userInstance(t, sess).Nick()
}

// userJoinedAt returns the time the user joined `ch`, or the zero
// time when the user is not in the channel.
func userJoinedAt(t testing.TB, sess *Session, ch domain.ChannelName) time.Time {
	t.Helper()

	channels := userInstance(t, sess).Channels()
	if channels == nil {
		return time.Time{}
	}

	at, ok := channels.Get(ch)
	if !ok {
		return time.Time{}
	}

	return at
}

// joinAs is shorthand for the error half of [Session.joinAs], for
// tests that do not care which spelling of the name the join landed
// on.
func joinAs(ctx context.Context, sess *Session, actor *domain.Instance, ch domain.ChannelName, key string) error {
	_, err := sess.joinAs(ctx, actor, clientJoin, ch, key)

	return err
}

// userJoin is shorthand for `joinAs(ctx, sess, userInstance(...))`.
func userJoin(ctx context.Context, t testing.TB, sess *Session, ch domain.ChannelName) error {
	t.Helper()

	return joinAs(ctx, sess, userInstance(t, sess), ch, "")
}

// userPart is shorthand for `sess.partAs(ctx, userInstance(...))`.
func userPart(ctx context.Context, t testing.TB, sess *Session, ch domain.ChannelName, message string) error {
	t.Helper()

	return sess.partAs(ctx, userInstance(t, sess), ch, message)
}

// userSendMessage is shorthand for `sess.sendMessageAs(ctx, userInstance(...))`.
func userSendMessage(ctx context.Context, t testing.TB, sess *Session, ch domain.ChannelName, body string) (domain.Message, error) {
	t.Helper()

	return sess.sendMessageAs(ctx, userInstance(t, sess), ch, body)
}

// userSetTopic is shorthand for `sess.setTopicAs(ctx, userInstance(...))`.
func userSetTopic(ctx context.Context, t testing.TB, sess *Session, ch domain.ChannelName, topic string) error {
	t.Helper()

	return sess.setTopicAs(ctx, userInstance(t, sess), ch, topic)
}

// userChangeNick is shorthand for `sess.changeNickAs(ctx, userInstance(...))`.
func userChangeNick(ctx context.Context, t testing.TB, sess *Session, newNick domain.Nick) error {
	t.Helper()

	return sess.changeNickAs(ctx, userInstance(t, sess), newNick)
}

// userPoke runs a manual poke pass over every channel — the same
// operation the `/poke` command triggers through the session.
func userPoke(ctx context.Context, t testing.TB, sess *Session) error {
	t.Helper()

	return sess.PokeNow(ctx)
}

// userJoinAutojoinChannels mirrors the user-client side autojoin
// loop: read the autojoin list and JOIN each entry via the
// user-actor.
func userJoinAutojoinChannels(ctx context.Context, t testing.TB, sess *Session) error {
	t.Helper()

	channels, err := sess.store.ListAutojoinChannels(ctx)
	if err != nil {
		return err
	}

	for _, ch := range channels {
		if err := userJoin(ctx, t, sess, ch); err != nil {
			return err
		}
	}

	return nil
}

// nextEvent reads the next event from the user-client
// subscription's protocol bus.
func nextEvent(t testing.TB, sess *Session) (domain.Event, bool) {
	t.Helper()

	delivery, ok := <-userClient(t, sess).Events()
	return delivery.Event, ok
}

// collectEmittedEvents returns every event currently queued on
// the user-client subscription's protocol bus, in arrival order.
// The drain is non-blocking and returns whatever is in the buffer
// at call time. Tests with in-goroutine producers (the synchronous
// actor methods — join, part, topic, nick, mode) call this directly
// after the action; tests with goroutine producers (model dispatch)
// `synctest.Wait()` first, then call.
//
// Use this to assert structurally on the full event slice. Any
// future emission added between the test's action and its check
// fails the test until the expected slice updates, which is the
// right pressure.
func collectEmittedEvents(t testing.TB, sess *Session) []domain.Event {
	t.Helper()

	uc := userClient(t, sess)

	var events []domain.Event

	for {
		select {
		case delivery := <-uc.Events():
			events = append(events, delivery.Event)
		default:
			return events
		}
	}
}

// bootstrapModeChange returns the UserModeChange every test
// session emits at attach time: the server promotes the user-
// client to +o via a wire MODE response. Tests prepend it to
// their expected event slice when asserting on the full stream
// from session start.
//
// `at` is the time recorded on the bootstrap event. Tests capture
// it with `time.Now()` before calling [newTestSession] so the
// comparison is symmetric with how the session itself stamped the
// event — `time.Now()` at attach. Inside a [synctest.Test] bubble
// both reads return the same bubble clock value as long as no
// goroutine yields between them, which is the case for the
// synchronous attach path.
func bootstrapModeChange(t testing.TB, sess *Session, at time.Time) domain.UserModeChange {
	t.Helper()

	user := userInstance(t, sess)
	return domain.UserModeChange{
		Nick:       user.Nick(),
		InstanceID: user.ID(),
		Flag:       domain.ModeOperator,
		Add:        true,
		At:         at,
		Instance:   user,
	}
}

// testMembers builds a MemberList using canonical `*Instance`
// handles from the given session + store. The user is looked up via
// `userInstance(t, sess)`; every model nick is resolved from the store
// via `ResolveNick`. If a model nick is not yet seeded, a placeholder
// instance is created under the conventional `inst-<nick>` id so
// tests can express channel membership without pre-seeding every
// referenced instance. The `seedInstance` helper picks the same id
// and updates fields in place, so the canonical pointer is stable
// whether the test seeds before or after calling `testMembers`.
func testMembers(t *testing.T, sess *Session, s *storemod.SQLiteStore, nicks ...domain.Nick) domain.MemberList {
	t.Helper()

	ml := domain.NewMemberList()
	for _, nick := range nicks {
		var inst *domain.Instance

		if nick == userNick(t, sess) {
			inst = userInstance(t, sess)
		} else {
			var err error
			inst, err = s.ResolveNick(t.Context(), nick)
			if err != nil {
				inst = seedInstance(t, sess, s, instanceSpec{Nick: nick, ModelID: "test/model"})
			}
		}

		ml.Add(inst)
		if nick == userNick(t, sess) {
			ml.SetModes(inst, domain.MemberModes{Operator: true})
		}
	}
	return ml
}

// testMemberID returns the synthetic InstanceID used for a nick in
// tests. The human "testuser" is always keyed with the empty
// InstanceID to match the production invariant; every other nick
// gets a stable "inst-<nick>" id.
func testMemberID(nick domain.Nick) domain.InstanceID {
	if nick == "testuser" {
		return ""
	}

	return domain.InstanceID("inst-" + string(nick))
}

func requireChannelEqual(t *testing.T, expected, actual *domain.ChannelWindow) {
	t.Helper()

	require.Equal(t, expected, actual)
}

// newTestChannelWindow constructs a `*domain.ChannelWindow` for use
// in test fixtures and assertions. The returned window has its
// `Members` field set to the supplied list (or an empty member
// list when none is given) so callers don't have to reach for the
// constructor's default and overwrite afterward.
func newTestChannelWindow(name domain.ChannelName, created time.Time, members domain.MemberList) *domain.ChannelWindow {
	cw := domain.NewChannelWindow(name, created)
	if members.Len() > 0 {
		cw.Members = members
	}

	return cw
}

func requireInstanceEqual(t *testing.T, expected, actual *domain.Instance) {
	t.Helper()

	require.Equal(t, normaliseInstance(expected), normaliseInstance(actual))
}

func newTestSession(t *testing.T) (*Session, *storemod.SQLiteStore) {
	t.Helper()

	return newTestSessionWithAPI(t, &fakeAPIClient{})
}

// unpaceFlood turns off the server's RFC 1459 §8.10 pacing for a
// test that deliberately floods the session to reach some other
// bound, such as the send-queue allowance. Those tests send
// thousands of commands from one client, which the pacing would
// spread over hours of the test's clock, and the pacing is not what
// they are checking. Every other test runs on the production
// setting, so ordinary command sequences stay under it.
func unpaceFlood(sess *Session) {
	sess.flood = floodPolicy{}
}

// addModelViaWire issues a [protocol.AddModel] through the
// user-client (which holds `+o` from bootstrap) so tests exercise
// the same dispatcher path the chatcmd `/add-model` and the
// model-tool call take.
func addModelViaWire(ctx context.Context, t testing.TB, sess *Session, ch domain.ChannelName, model domain.ModelID, persona string) error {
	t.Helper()

	resp, err := userClient(t, sess).Send(ctx, protocol.AddModel{
		Channel: ch,
		Model:   model,
		Persona: persona,
	})
	if err != nil {
		return err
	}

	return resp.Err
}

// userQuitViaWire issues a [protocol.Quit] through the user-client,
// matching what the chat-screen does when the user types `/quit`.
// Returns the dispatcher error (transport + `Response.Err`) so
// callers can assert on it directly.
func userQuitViaWire(ctx context.Context, t testing.TB, sess *Session, message string) error {
	t.Helper()

	resp, err := userClient(t, sess).Send(ctx, protocol.Quit{Reason: message})
	if err != nil {
		return err
	}

	return resp.Err
}

// kickViaWire issues a [protocol.Kick] through the user-client,
// matching what the chat-screen does when the user types
// `/kick`. The dispatcher surfaces `UnknownNickError` as
// `Response.Err`; callers that want to assert the failure shape
// can branch on the typed value via `errors.As`.
func kickViaWire(ctx context.Context, t testing.TB, sess *Session, ch domain.ChannelName, nick domain.Nick) error {
	t.Helper()

	resp, err := userClient(t, sess).Send(ctx, protocol.Kick{Channel: ch, Nick: nick})
	if err != nil {
		return err
	}

	return resp.Err
}

// modelQuitViaWire issues a [protocol.Quit] through the named
// model-client. The model-actor branch of `handleQuit` broadcasts
// QUIT to peers and reaps the subscription, matching the
// model-tool path.
func modelQuitViaWire(ctx context.Context, t testing.TB, sess *Session, actor *domain.Instance, message string) error {
	t.Helper()

	client := attachModelClient(t, sess, actor)
	require.NotNil(t, client, "model client must exist for quit test")

	resp, err := client.Send(ctx, protocol.Quit{Reason: message})
	if err != nil {
		return err
	}

	return resp.Err
}

func newTestSessionWithAPI(t *testing.T, apiClient api.Client) (*Session, *storemod.SQLiteStore) {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	factory := newTestModelClientFactory(t, apiClient)

	sess := New(t.Context, s, factory, nil)
	// The factory registered its cleanup first, so it runs last:
	// `Shutdown` closes the gate every pump exits on, and the
	// factory's `detachAll` then joins every dispatch goroutine.
	// That join is what keeps a goroutine from outliving the test,
	// so this shutdown does not need a context that outlives it
	// either.
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	// Attach before swapping the clock: the bootstrap `+o` MODE
	// the user-client subscription emits is stamped with `s.now()`
	// at attach time. Tests that capture `time.Now()` before
	// constructing the session compare against that wall-clock
	// stamp, so the attach must run while `s.now` is still the
	// default `time.Now`.
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s
}

// attachTestUserClient registers a thin user-client with the
// session under `nick` and grants it `+o` via
// [protocol.SubscribeOptions.InitialModes], matching what the
// production-side `userclient.New(...).Attach(...)` does. The
// session-test fixture lives in this package and cannot import
// `internal/userclient` (the dependency is one-way), so it
// satisfies [protocol.Client] inline here.
func attachTestUserClient(t *testing.T, sess *Session, nick domain.Nick) {
	t.Helper()

	inst := domain.NewUserInstance(nick)
	tc := &testUserClient{sess: sess, instance: inst}
	sub, err := sess.Subscribe(tc, protocol.SubscribeOptions{
		Instance:     inst,
		InitialModes: []domain.Mode{domain.ModeOperator},
		EchoMessage:  true,
	})
	require.NoError(t, err)
	tc.sub = sub
}

// testUserClient is the session-test fixture's minimal
// [protocol.Client] for the user-actor side of the bus. Production
// uses `internal/userclient`; this file mirrors it for the
// in-package tests that cannot import that package.
type testUserClient struct {
	sess     *Session
	instance *domain.Instance
	sub      protocol.Subscription
}

func (c *testUserClient) Identity() protocol.ClientID { return protocol.UserClientID }
func (c *testUserClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}
func (c *testUserClient) Events() <-chan protocol.Delivery {
	if c.sub == nil {
		return nil
	}
	return c.sub.Events()
}
func (c *testUserClient) Caps() command.CapabilityHolder { return testUserCaps{} }

type testUserCaps struct{}

func (testUserCaps) Has(c command.Capability) bool { return c == protocol.CapOperator }

func TestSession_Join(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		synctest.Wait()

		user := userInstance(t, sess)
		members := domain.NewMemberList()
		members.Add(user)
		members.SetModes(user, domain.MemberModes{Operator: true})

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user,
				Created:  true,
				At:       fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: members,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#general", fixedTime, testMembers(t, sess, s, "testuser")), ch)

		last, err := s.GetLastChannel(ctx)
		require.NoError(t, err)
		require.Equal(t, domain.ChannelName(""), last)
	})
}

func TestSession_JoinExistingChannel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	existing := newTestChannelWindow("#existing", fixedTime.Add(-time.Hour), testMembers(t, sess, s, "testuser"))
	existing.Topic = "Already here"
	saveTestChannel(t, sess, s, existing)

	require.NoError(t, userJoin(ctx, t, sess, "#existing"))

	// Channel should not be overwritten.
	ch, err := sess.loadChannelWindow(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, "Already here", ch.Topic)

	// No join event should be stored since the user was already a member.
	types := channelEventTypes(t, s, "#existing")
	require.Empty(t, types)
}

func TestSession_JoinAlreadyMember_no_duplicate_event(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	// Join again — should not emit a second join event.
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	// First join creates the channel, so we get a join event.
	// Second join should add nothing.
	types := channelEventTypes(t, s, "#general")
	require.Equal(t, []string{"join"}, types)
}

func TestSession_JoinSwitchAndReturn_no_duplicate_event(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#general"))
	require.NoError(t, userJoin(ctx, t, sess, "#random"))

	// Switch back to #general — no new join event.
	require.NoError(t, userJoin(ctx, t, sess, "#general"))

	types := channelEventTypes(t, s, "#general")
	require.Equal(t, []string{"join"}, types)
}

func TestSession_JoinAutojoinChannels_populates_user_join_times(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "botty")
		seedChannelWithMembers(t, sess, s, "#random", "botty")
		require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general", "#random"}))

		require.True(t, userJoinedAt(t, sess, "#general").IsZero())
		require.True(t, userJoinedAt(t, sess, "#random").IsZero())

		require.NoError(t, userJoinAutojoinChannels(ctx, t, sess))
		synctest.Wait()

		user := userInstance(t, sess)
		members := func(ch domain.ChannelName) domain.MemberList {
			cw, err := sess.loadChannelWindow(ctx, ch)
			require.NoError(t, err)
			return cw.Members
		}

		var expected []domain.Event
		expected = append(expected, bootstrapModeChange(t, sess, bootAt))
		for _, ch := range []domain.ChannelName{"#general", "#random"} {
			expected = append(expected,
				domain.Join{
					Target:   ch,
					Nick:     "testuser",
					Instance: user,
					At:       fixedTime,
				},
				domain.NamesReplyEvent{
					Channel: ch,
					Members: members(ch),
					At:      fixedTime,
				},
				domain.NamesEnd{
					Channel: ch,
					At:      fixedTime,
				},
			)
		}

		require.Equal(t, expected, collectEmittedEvents(t, sess))

		require.Equal(t, fixedTime, userJoinedAt(t, sess, "#general"))
		require.Equal(t, fixedTime, userJoinedAt(t, sess, "#random"))
	})
}

func TestSession_JoinAutojoinChannels_empty_autojoin_is_noop(t *testing.T) {
	sess, _ := newTestSession(t)

	require.NoError(t, userJoinAutojoinChannels(t.Context(), t, sess))
	requireChannels(t, userInstance(t, sess).Channels())
}

func TestSession_JoinAutojoinChannels_emits_join_events(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#alpha", "botty")
		seedChannelWithMembers(t, sess, s, "#beta", "botty")
		require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#alpha", "#beta"}))

		require.NoError(t, userJoinAutojoinChannels(ctx, t, sess))
		synctest.Wait()

		user := userInstance(t, sess)
		members := func(ch domain.ChannelName) domain.MemberList {
			cw, err := sess.loadChannelWindow(ctx, ch)
			require.NoError(t, err)
			return cw.Members
		}

		var expected []domain.Event
		expected = append(expected, bootstrapModeChange(t, sess, bootAt))
		for _, ch := range []domain.ChannelName{"#alpha", "#beta"} {
			expected = append(expected,
				domain.Join{
					Target:   ch,
					Nick:     "testuser",
					Instance: user,
					At:       fixedTime,
				},
				domain.NamesReplyEvent{
					Channel: ch,
					Members: members(ch),
					At:      fixedTime,
				},
				domain.NamesEnd{
					Channel: ch,
					At:      fixedTime,
				},
			)
		}

		require.Equal(t, expected, collectEmittedEvents(t, sess))
	})
}

func TestSession_Leave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		saveTestChannel(t, sess, s, newTestChannelWindow("#leaving", fixedTime, testMembers(t, sess, s, "testuser", "botty")))

		require.NoError(t, userPart(ctx, t, sess, "#leaving", ""))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Part{
				Target:   "#leaving",
				Nick:     "testuser",
				Instance: user,
				At:       fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		updated, err := sess.loadChannelWindow(ctx, "#leaving")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#leaving", fixedTime, testMembers(t, sess, s, "botty")), updated)
	})
}

func TestSession_LeaveNonexistent(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, userPart(t.Context(), t, sess, "#ghost", ""))
}

func TestSession_Part_carries_message(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		saveTestChannel(t, sess, s, newTestChannelWindow("#farewell", fixedTime, testMembers(t, sess, s, "testuser")))

		require.NoError(t, userPart(ctx, t, sess, "#farewell", "see ya later"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Part{
				Target:   "#farewell",
				Nick:     "testuser",
				Instance: userInstance(t, sess),
				Message:  "see ya later",
				At:       fixedTime,
			},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_Connect_marks_session_active(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Connect(ctx))
		synctest.Wait()

		got, err := s.GetSessionActive(ctx)
		require.NoError(t, err)
		require.NotEmpty(t, got)
		require.Equal(t, fixedTime, sess.ConnectedAt())

		select {
		case <-sess.Connected():
		default:
			t.Fatal("Connected() channel should be closed after Connect")
		}

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       userNick(t, sess),
				At:         fixedTime,
			},
		}, collectEmittedEvents(t, sess))
		require.Empty(t, channelEventTypes(t, s, domain.StatusChannelName),
			"session must not persist server-narrated events on &modeloff")
	})
}

func TestSession_Connect_clears_unclean_user_membership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, s.SetSessionActive(ctx, "stale"))
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedChannelWithMembers(t, sess, s, "#random", "testuser")

		require.NoError(t, sess.Connect(ctx))
		synctest.Wait()

		general, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#general", fixedTime, testMembers(t, sess, s, "botty")), general)

		random, err := sess.loadChannelWindow(ctx, "#random")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#random", fixedTime, domain.NewMemberList()), random)

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       userNick(t, sess),
				At:         fixedTime,
			},
			domain.Reconnected{At: fixedTime},
		}, collectEmittedEvents(t, sess))
		require.Empty(t, channelEventTypes(t, s, domain.StatusChannelName),
			"session must not persist server-narrated events on &modeloff")
	})
}

func TestSession_Connect_then_JoinAutojoin_stamps_UserJoinedAt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		// Simulate the original bug's preconditions: stale membership left
		// over from a prior session, plus a non-empty session_active marker.
		require.NoError(t, s.SetSessionActive(ctx, "stale"))
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedChannelWithMembers(t, sess, s, "#random", "testuser")
		require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general", "#random"}))

		require.NoError(t, sess.Connect(ctx))
		require.NoError(t, userJoinAutojoinChannels(ctx, t, sess))
		synctest.Wait()

		require.Equal(t, fixedTime, userJoinedAt(t, sess, "#general"))
		require.Equal(t, fixedTime, userJoinedAt(t, sess, "#random"))

		user := userInstance(t, sess)
		members := func(ch domain.ChannelName) domain.MemberList {
			cw, err := sess.loadChannelWindow(ctx, ch)
			require.NoError(t, err)
			return cw.Members
		}

		expected := []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       userNick(t, sess),
				At:         fixedTime,
			},
			domain.Reconnected{At: fixedTime},
		}
		for _, ch := range []domain.ChannelName{"#general", "#random"} {
			expected = append(expected,
				domain.Join{
					Target:   ch,
					Nick:     "testuser",
					Instance: user,
					At:       fixedTime,
				},
				domain.NamesReplyEvent{
					Channel: ch,
					Members: members(ch),
					At:      fixedTime,
				},
				domain.NamesEnd{
					Channel: ch,
					At:      fixedTime,
				},
			)
		}

		require.Equal(t, expected, collectEmittedEvents(t, sess))
	})
}

func TestSession_Connect_Quit_Reconnect_omits_status_channel_from_autojoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := storetest.NewMemoryStore(t)

		bootAt1 := time.Now()
		sess1 := New(t.Context, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
		t.Cleanup(func() { _ = sess1.Shutdown(t.Context()) })
		attachTestUserClient(t, sess1, "testuser")
		sess1.now = func() time.Time { return fixedTime }
		ctx := t.Context()

		require.NoError(t, sess1.Connect(ctx))
		require.NoError(t, userJoin(ctx, t, sess1, "#general"))
		// Snapshot the channel membership before Quit clears the user
		// from it; the NamesReplyEvent emitted at join time carries
		// the live MemberList by reference.
		general1, err := sess1.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		generalMembers := general1.Members

		require.NoError(t, userQuitViaWire(ctx, t, sess1, "bye"))
		synctest.Wait()

		user1 := userInstance(t, sess1)
		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess1, bootAt1),
			domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       userNick(t, sess1),
				At:         fixedTime,
			},
			domain.Join{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user1,
				Created:  true,
				At:       fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: generalMembers,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess1))

		autojoin, err := s.ListAutojoinChannels(ctx)
		require.NoError(t, err)
		require.Equal(t, []domain.ChannelName{"#general"}, autojoin)

		// Starting a fresh session over the same store must not replay the
		// status channel into the autojoin loop.
		bootAt2 := time.Now()
		sess2 := New(t.Context, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
		t.Cleanup(func() { _ = sess2.Shutdown(t.Context()) })
		attachTestUserClient(t, sess2, "testuser")
		sess2.now = func() time.Time { return fixedTime }
		require.NoError(t, sess2.Connect(ctx))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess2, bootAt2),
			domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       userNick(t, sess2),
				At:         fixedTime,
			},
		}, collectEmittedEvents(t, sess2))

		autojoin, err = s.ListAutojoinChannels(ctx)
		require.NoError(t, err)
		require.Equal(t, []domain.ChannelName{"#general"}, autojoin)
	})
}

func TestSession_Connect_unclean_recovery_emits_welcome_and_reconnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		s := storetest.NewMemoryStore(t)
		sess := New(t.Context, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }
		ctx := t.Context()

		require.NoError(t, s.SetSessionActive(ctx, "stale"))

		require.NoError(t, sess.Connect(ctx))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       userNick(t, sess),
				At:         fixedTime,
			},
			domain.Reconnected{At: fixedTime},
		}, collectEmittedEvents(t, sess))
		require.Empty(t, channelEventTypes(t, s, domain.StatusChannelName),
			"session must not persist server-narrated events on &modeloff")
	})
}

// TestSession_user_snapshot_race_free hammers joinAs, partAs, and
// UserJoinedAt from concurrent goroutines. Run under -race it catches
// any regression that reintroduces direct mutation of the shared
// OrderedMap.
func TestSession_user_snapshot_race_free(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	// Drain emitted events so the mutators don't block on a full buffer.
	drainCtx, cancelDrain := context.WithCancel(ctx)
	t.Cleanup(cancelDrain)

	go func() {
		for {
			select {
			case <-userClient(t, sess).Events():
			case <-drainCtx.Done():
				return
			}
		}
	}()

	const iters = 200
	channels := []domain.ChannelName{"#alpha", "#beta", "#gamma", "#delta"}

	var wg sync.WaitGroup

	wg.Go(func() {
		for i := range iters {
			ch := channels[i%len(channels)]
			_ = userJoin(ctx, t, sess, ch)
			_ = userPart(ctx, t, sess, ch, "")
		}
	})

	wg.Go(func() {
		for i := range iters {
			ch := channels[i%len(channels)]
			_ = userJoinedAt(t, sess, ch)
			_ = userNick(t, sess)
		}
	})

	wg.Wait()

	// Final state: whichever of Join/Part ran last wins, but the
	// invariant we care about is "no torn read, no panic".
	// UserJoinedAt on any known channel returns either zero time or
	// fixedTime, never garbage.
	for _, ch := range channels {
		got := userJoinedAt(t, sess, ch)
		if !got.IsZero() {
			require.Equal(t, fixedTime, got, "UserJoinedAt must return a coherent snapshot value")
		}
	}
}

func TestSession_Connect_is_idempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		recorder, provider := oteltest.NewSpanRecorder(t)
		sess, s := newTestSession(t)
		sess.WithTracerProvider(provider)
		ctx := t.Context()

		require.NoError(t, sess.Connect(ctx))
		require.NoError(t, sess.Connect(ctx))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Welcome{
				ServerName: domain.StatusServerName,
				Nick:       userNick(t, sess),
				At:         fixedTime,
			},
		}, collectEmittedEvents(t, sess))
		require.Empty(t, channelEventTypes(t, s, domain.StatusChannelName),
			"session must not persist server-narrated events on &modeloff")

		select {
		case <-sess.Connected():
		default:
			t.Fatal("Connected() channel should be closed after Connect")
		}

		// The no-op second call records no span: it short-circuits before
		// the span-bracketing runner so session.connect counts reflect
		// real attempts only.
		var connectSpans int
		for _, span := range recorder.Ended() {
			if span.Name() == "session.connect" {
				connectSpans++
			}
		}
		require.Equal(t, 1, connectSpans)
	})
}

func TestSession_Quit_appends_channel_quit_events_and_saves_autojoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		require.NoError(t, userJoin(ctx, t, sess, "#random"))

		general, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		generalMembers := general.Members
		random, err := sess.loadChannelWindow(ctx, "#random")
		require.NoError(t, err)
		randomMembers := random.Members

		require.NoError(t, userQuitViaWire(ctx, t, sess, "goodnight"))
		synctest.Wait()

		user := userInstance(t, sess)
		expected := []domain.Event{bootstrapModeChange(t, sess, bootAt)}
		for _, entry := range []struct {
			ch      domain.ChannelName
			members domain.MemberList
		}{
			{"#general", generalMembers},
			{"#random", randomMembers},
		} {
			expected = append(expected,
				domain.Join{
					Target:   entry.ch,
					Nick:     "testuser",
					Instance: user,
					Created:  true,
					At:       fixedTime,
				},
				domain.NamesReplyEvent{
					Channel: entry.ch,
					Members: entry.members,
					At:      fixedTime,
				},
				domain.NamesEnd{
					Channel: entry.ch,
					At:      fixedTime,
				},
			)
		}
		require.Equal(t, expected, collectEmittedEvents(t, sess))

		autojoin, err := s.ListAutojoinChannels(ctx)
		require.NoError(t, err)
		require.Equal(t, []domain.ChannelName{"#general", "#random"}, autojoin)

		for _, ch := range []domain.ChannelName{"#general", "#random"} {
			require.Equal(t, []string{"join", "quit"}, channelEventTypes(t, s, ch))
		}
	})
}

func TestSession_Quit_removes_user_from_channel_members(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		general, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		generalMembers := general.Members

		require.NoError(t, userQuitViaWire(ctx, t, sess, ""))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user,
				Created:  true,
				At:       fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: generalMembers,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		ch, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		requireChannelEqual(t, newTestChannelWindow("#general", fixedTime, domain.NewMemberList()), ch)
	})
}

func TestSession_Quit_clears_in_memory_channels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		general, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		generalMembers := general.Members

		require.NoError(t, userQuitViaWire(ctx, t, sess, ""))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user,
				Created:  true,
				At:       fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: generalMembers,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
				At:      fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		remaining := []domain.ChannelName{}
		for pair := userInstance(t, sess).Channels().Oldest(); pair != nil; pair = pair.Next() {
			remaining = append(remaining, pair.Key)
		}

		require.Equal(t, []domain.ChannelName{}, remaining,
			"quit must clear the user's channel list")
	})
}

// TestSession_user_state_triple_stays_consistent verifies that the
// three sources of user-membership state stay aligned through a
// full command sequence (join, part, rejoin, nick change). The
// invariant being pinned: userModes, user.Channels(), and the
// stripped persisted Channel.Members agree at every step.
func TestSession_user_state_triple_stays_consistent(t *testing.T) {
	type userSnapshot struct {
		Channels   []domain.ChannelName
		Modes      domain.MemberModes
		OnDiskUser bool
	}

	snapshot := func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ch domain.ChannelName) userSnapshot {
		t.Helper()

		var channels []domain.ChannelName
		for pair := userInstance(t, sess).Channels().Oldest(); pair != nil; pair = pair.Next() {
			channels = append(channels, pair.Key)
		}

		w, err := s.GetWindow(t.Context(), ch)
		var onDisk bool
		if err == nil {
			if cw, ok := w.(*domain.ChannelWindow); ok {
				onDisk = cw.Members.HasInstance(userInstance(t, sess))
			}
		}

		return userSnapshot{
			Channels:   channels,
			Modes:      sess.userModesFor(t.Context(), ch),
			OnDiskUser: onDisk,
		}
	}

	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		require.Equal(t, userSnapshot{
			Channels:   []domain.ChannelName{"#general"},
			Modes:      domain.MemberModes{Operator: true},
			OnDiskUser: false,
		}, snapshot(t, sess, s, "#general"))

		general1, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		generalMembers1 := general1.Members

		require.NoError(t, userPart(ctx, t, sess, "#general", ""))
		require.Equal(t, userSnapshot{
			Channels:   nil,
			Modes:      domain.MemberModes{},
			OnDiskUser: false,
		}, snapshot(t, sess, s, "#general"))

		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		require.Equal(t, userSnapshot{
			Channels:   []domain.ChannelName{"#general"},
			Modes:      domain.MemberModes{Operator: true},
			OnDiskUser: false,
		}, snapshot(t, sess, s, "#general"),
			"the user parted #general while sole occupant, so the channel was "+
				"destroyed (RFC 2811 §2). The rejoin recreates the channel, and "+
				"the creating user gets +o per RFC 2811 §4.3")

		general2, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		generalMembers2 := general2.Members

		require.NoError(t, userChangeNick(ctx, t, sess, "renamed"))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			domain.UserModeChange{
				Nick:       "testuser",
				InstanceID: user.ID(),
				Flag:       domain.ModeOperator,
				Add:        true,
				At:         bootAt,
				Instance:   user,
			},
			domain.Join{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user,
				Created:  true,
				At:       fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: generalMembers1,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
				At:      fixedTime,
			},
			domain.Part{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user,
				At:       fixedTime,
			},
			domain.Join{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user,
				Created:  true,
				At:       fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: generalMembers2,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
				At:      fixedTime,
			},
			domain.NickChange{
				InstanceID: user.ID(),
				OldNick:    "testuser",
				NewNick:    "renamed",
				Instance:   user,
				At:         fixedTime,
			},
		}, collectEmittedEvents(t, sess),
			"the part destroyed the channel (RFC 2811 §2); the rejoin recreates "+
				"it with Created:true and the user gets +o as the new creator")

		require.Equal(t, userSnapshot{
			Channels:   []domain.ChannelName{"#general"},
			Modes:      domain.MemberModes{Operator: true},
			OnDiskUser: false,
		}, snapshot(t, sess, s, "#general"))
	})
}

func TestSession_Quit_clears_session_active_marker(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, fixedTime.Format(time.RFC3339Nano)))

	require.NoError(t, userQuitViaWire(ctx, t, sess, ""))

	got, err := s.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSession_Quit_no_channels_is_noop_but_clears_marker(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, fixedTime.Format(time.RFC3339Nano)))

	require.NoError(t, userQuitViaWire(ctx, t, sess, "bye"))

	autojoin, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, autojoin)

	got, err := s.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSession_Quit_does_not_dispatch_to_models(t *testing.T) {
	var calls atomic.Int32

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
			calls.Add(1)
			return msgToolCalls(t, domain.ChannelName(events[0].Target), "bye"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	userInstance(t, sess).JoinChannel("#general", fixedTime)

	require.NoError(t, userQuitViaWire(ctx, t, sess, "bye"))

	require.Equal(t, int32(0), calls.Load(),
		"Quit must not dispatch to models; models see the quit next time they are dispatched against")
}

func TestSession_AddModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")))

		require.NoError(t, addModelViaWire(ctx, t, sess, "#dev", "anthropic/claude-3-haiku", ""))
		synctest.Wait()

		inst, err := s.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)
		require.NotEmpty(t, inst.ID())

		updated, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)

		// The model's own JOIN is delivered but raises no dispatch
		// turn — it has nothing to say about its own arrival.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "fakenick",
				InstanceID: inst.ID(),
				At:         fixedTime,
				Instance:   inst,
			},
		}, collectEmittedEvents(t, sess))

		requireInstanceEqual(t, domain.NewModelInstance(
			inst.ID(), "fakenick", "anthropic/claude-3-haiku", "", testChannels("#dev"),
		), inst)

		require.Equal(t, []domain.Member{
			{Instance: userInstance(t, sess), Nick: "testuser", Modes: domain.MemberModes{Operator: true}},
			{Instance: inst, Nick: "fakenick", Modes: domain.MemberModes{}},
		}, slices.Collect(updated.Members.All()))
	})
}

func TestSession_Kick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#dev", "#random"),
		})
		saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser", "botty")))

		require.NoError(t, kickViaWire(ctx, t, sess, "#dev", "botty"))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Kicked{
				Target:     "#dev",
				Nick:       "botty",
				InstanceID: botty.ID(),
				By:         "testuser",
				At:         fixedTime,
				Instance:   botty,
			},
		}, collectEmittedEvents(t, sess))

		updated, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Equal(t, slices.Collect(testMembers(t, sess, s, "testuser").All()), slices.Collect(updated.Members.All()))

		inst, err := s.ResolveNick(ctx, "botty")
		require.NoError(t, err)
		requireChannels(t, inst.Channels(), "#random")
	})
}

func TestSession_mutationOperations_recordSpans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder, provider := oteltest.NewSpanRecorder(t)
		s := storetest.NewMemoryStore(t).WithTracerProvider(provider)
		sess := New(t.Context, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil).WithTracerProvider(provider)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#general"))

		seedChannelWithMembers(t, sess, s, "#leave", "testuser")
		require.NoError(t, userPart(ctx, t, sess, "#leave", ""))

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		channel, err := sess.loadChannelWindow(ctx, "#general")
		require.NoError(t, err)
		channel.Members.Add(botty)
		saveTestChannel(t, sess, s, channel)
		require.NoError(t, kickViaWire(ctx, t, sess, "#general", "botty"))

		require.NoError(t, userSetTopic(ctx, t, sess, "#general", "observability"))
		require.NoError(t, userChangeNick(ctx, t, sess, "renamed"))

		expected := []string{
			"session.change_nick",
			"session.handle",
			"session.join",
			"session.kick",
			"session.part",
			"session.set_topic",
			"session.set_user_mode",
			"store.sqlite.append_event",
			"store.sqlite.delete_window",
			"store.sqlite.events_before",
			"store.sqlite.get_instance_by_id",
			"store.sqlite.get_window",
			"store.sqlite.instance_replies_before",
			"store.sqlite.resolve_nick",
			"store.sqlite.save_instance",
			"store.sqlite.save_window",
			"store.sqlite.set_autojoin_channels",
		}

		synctest.Wait()

		ended := make(map[string]sdktrace.ReadOnlySpan)
		for _, span := range recorder.Ended() {
			ended[span.Name()] = span
		}
		require.ElementsMatch(t, expected, slices.Collect(maps.Keys(ended)))
	})
}

func TestSession_spans_carry_AttrInstanceID(t *testing.T) {
	tests := []struct {
		name       string
		spanName   string
		act        func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context)
		wantInstID domain.InstanceID
	}{
		{
			name:     "change_nick for user carries empty id",
			spanName: "session.change_nick",
			act: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				require.NoError(t, userJoin(ctx, t, sess, "#general"))
				require.NoError(t, userChangeNick(ctx, t, sess, "renamed"))
			},
			wantInstID: "",
		},
		{
			name:     "change_nick for model carries model's id",
			spanName: "session.change_nick",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				botty := seedInstance(t, sess, s, instanceSpec{
					Nick:    "botty",
					ModelID: "test/model",
				})
				require.NoError(t, sess.changeNickAs(ctx, botty, "botty2"))
			},
			wantInstID: testMemberID("botty"),
		},
		{
			name:     "join for user carries empty id",
			spanName: "session.join",
			act: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				require.NoError(t, userJoin(ctx, t, sess, "#general"))
			},
			wantInstID: "",
		},
		{
			name:     "join for model carries model's id",
			spanName: "session.join",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				seedChannelWithMembers(t, sess, s, "#dev", "testuser")
				botty := seedInstance(t, sess, s, instanceSpec{
					Nick:    "botty",
					ModelID: "test/model",
				})
				require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
			},
			wantInstID: testMemberID("botty"),
		},
		{
			name:     "kick carries target's id",
			spanName: "session.kick",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				seedInstance(t, sess, s, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#dev"),
				})
				seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
				require.NoError(t, kickViaWire(ctx, t, sess, "#dev", "botty"))
			},
			wantInstID: testMemberID("botty"),
		},
		{
			name:     "part for model carries model's id",
			spanName: "session.part",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				botty := seedInstance(t, sess, s, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#dev"),
				})
				seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
				require.NoError(t, sess.partAs(ctx, botty, "#dev", ""))
			},
			wantInstID: testMemberID("botty"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder, provider := oteltest.NewSpanRecorder(t)
			sess, s := newTestSession(t)
			sess.WithTracerProvider(provider)

			tt.act(t, sess, s, t.Context())

			span := oteltest.FindSpan(t, recorder, tt.spanName)
			require.Equal(t,
				string(tt.wantInstID),
				oteltest.AttrValue(span.Attributes(), observability.AttrInstanceID),
			)
		})
	}
}

// TestSession_dispatch_to_instance_span_carries_instance_id pins the
// dispatched instance's id onto the per-instance turn span. It sits
// outside the table above because driving a turn means waking the
// model's own dispatch goroutine, which needs a synctest bubble to
// settle.
func TestSession_dispatch_to_instance_span_carries_instance_id(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder, provider := oteltest.NewSpanRecorder(t)
		sess, s := newTestSession(t)
		sess.WithTracerProvider(provider)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hi")

		span := oteltest.FindSpan(t, recorder, "modelclient.dispatch_to_instance")
		require.Equal(t,
			string(testMemberID("botty")),
			oteltest.AttrValue(span.Attributes(), observability.AttrInstanceID),
		)
	})
}

func TestSession_Dispatch_api_failure_records_dispatch_error_kind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder, provider := oteltest.NewSpanRecorder(t)
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{}, fmt.Errorf("upstream boom")
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		sess.WithTracerProvider(provider)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hi")

		span := oteltest.FindSpan(t, recorder, "modelclient.dispatch_turn")
		require.Equal(t, observability.ResultError, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
		require.Equal(t, observability.ErrorKindDispatch, oteltest.AttrValue(span.Attributes(), observability.AttrErrorKind))
	})
}

// TestSession_AutojoinChannels_drives_per_channel_joins pins the
// shape the user-client autojoin loop relies on: walking the
// stored autojoin list and JOINing each entry as the user-actor
// leaves the user instance carrying every channel in membership.
// The aggregate observability span is the user-client's concern
// and is covered alongside the user-client implementation.
func TestSession_AutojoinChannels_drives_per_channel_joins(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#alpha", "#beta"}))
	require.NoError(t, userJoinAutojoinChannels(ctx, t, sess))

	var joined []domain.ChannelName
	for pair := userInstance(t, sess).Channels().Oldest(); pair != nil; pair = pair.Next() {
		joined = append(joined, pair.Key)
	}
	require.Equal(t, []domain.ChannelName{"#alpha", "#beta"}, joined)
}

func TestSession_dispatchToInstance_recordsPassReasonAndToolTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder, provider := oteltest.NewSpanRecorder(t)
		dataStore := storetest.NewMemoryStore(t)
		memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
		fake := &fakeAPIClient{
			sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{
					PendingToolCalls: []api.PendingToolCall{{
						ID:   "call-1",
						Name: "write_memory",
						Args: mustRawJSON(t, `{"key":"topic","content":"observability"}`),
					}},
				}, nil
			},
			continueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
				return api.CompletionResult{}, nil
			},
		}
		sess := New(t.Context, dataStore, newTestModelClientFactoryWith(t, fake, memStore), nil).WithTracerProvider(provider)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }
		ctx := t.Context()

		seedInstance(t, sess, dataStore, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, dataStore, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hi")

		span := oteltest.FindSpan(t, recorder, "modelclient.dispatch_to_instance")
		require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
		require.Equal(t, observability.PassReasonModelPass, oteltest.AttrValue(span.Attributes(), observability.AttrPassReason))
		require.Equal(t, "1", oteltest.AttrValue(span.Attributes(), observability.AttrToolTurnCount))
	})
}

func TestSession_modelDispatchTurn_recordsSpan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		recorder, provider := oteltest.NewSpanRecorder(t)
		sess, s := newTestSession(t)
		sess.WithTracerProvider(provider)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		attachModelClient(t, sess, botty)

		sess.Emit(ctx, domain.PokeEvent{Channel: "#general", At: fixedTime})
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.PokeEvent{Channel: "#general", At: fixedTime},
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		span := oteltest.FindSpan(t, recorder, "modelclient.dispatch_turn")
		require.Equal(t, "#general", oteltest.AttrValue(span.Attributes(), observability.AttrChannel))
		require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
	})
}

func TestSession_SendMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		persisted, err := userSendMessage(ctx, t, sess, "#general", "hello world")
		require.NoError(t, err)
		require.Equal(t, domain.Message{
			Target: "#general",
			From:   "testuser",
			Body:   "hello world",
			At:     fixedTime,
		}, persisted)

		synctest.Wait()

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		}, msgs)

		// The user-client holds echo-message, so its own line returns
		// on the bus; a channel without models produces no dispatch
		// lifecycle.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_SendMessage_emits_dispatch_events(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "got it"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		_, err := userSendMessage(ctx, t, sess, "#general", "hello")
		require.NoError(t, err)
		synctest.Wait()

		// The user-client holds echo-message, so its own outgoing line
		// returns on the bus; botty's dispatch goroutine triggers on it
		// and emits its reply Message on the wire.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{
				Target: "#general",
				From:   "testuser",
				Body:   "hello",
				At:     fixedTime,
			},
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.Message{
				Target:     "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "got it",
				At:         fixedTime,
			},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_JoinEvent_triggers_dispatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()

		var receivedEvents []protocol.IRCMessage

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				receivedEvents = events
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "welcome"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		// Seed a channel with a model already present so join dispatch
		// has someone to notify. The user is NOT yet a member.
		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "botty")

		// Join an existing channel — the reactive dispatch should fire.
		require.NoError(t, userJoin(ctx, t, sess, "#general"))
		synctest.Wait()

		userInst := userInstance(t, sess)
		events := collectEmittedEvents(t, sess)

		// The NamesReply carries the channel's MemberList at join
		// time. Extracting the exact MemberList for an equality match
		// would couple to its internals, so confirm one is present
		// addressing the right channel and time, then assert the rest.
		// Its RPL_ENDOFNAMES terminator is filtered the same way.
		var sawNames, sawNamesEnd bool
		rest := make([]domain.Event, 0, len(events))
		for _, e := range events {
			if n, ok := e.(domain.NamesReplyEvent); ok {
				require.Equal(t, domain.ChannelName("#general"), n.Channel)
				require.Equal(t, fixedTime, n.At)
				sawNames = true

				continue
			}

			if n, ok := e.(domain.NamesEnd); ok {
				require.Equal(t, domain.NamesEnd{Channel: "#general", At: fixedTime}, n)
				sawNamesEnd = true

				continue
			}

			rest = append(rest, e)
		}
		require.True(t, sawNames, "expected a NamesReplyEvent in the join burst")
		require.True(t, sawNamesEnd, "expected a NamesEnd terminator in the join burst")

		wantStarted := domain.ModelDispatchStarted{Instance: botty, At: fixedTime}
		wantReply := domain.Message{
			Target:     "#general",
			From:       "botty",
			InstanceID: testMemberID("botty"),
			Body:       "welcome",
			At:         fixedTime,
		}
		wantDone := domain.ModelDispatchDone{Instance: botty, At: fixedTime}

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#general",
				Nick:       "testuser",
				InstanceID: userInst.ID(),
				Instance:   userInst,
				At:         fixedTime,
			},
			wantStarted,
			wantReply,
			wantDone,
		}, rest)

		// Dispatch lifecycle ordering: Started before reply Message before Done.
		idxOf := func(target domain.Event) int {
			for i, e := range rest {
				if reflect.DeepEqual(target, e) {
					return i
				}
			}

			t.Fatalf("event %T not found", target)
			return -1
		}

		require.Less(t, idxOf(wantStarted), idxOf(wantReply), "ModelDispatchStarted must precede reply Message")
		require.Less(t, idxOf(wantReply), idxOf(wantDone), "reply Message must precede ModelDispatchDone")

		// The trigger event sent to the model should be a JOIN message.
		require.Equal(t, []protocol.IRCMessage{{
			Kind:   protocol.KindJoin,
			From:   "testuser",
			Target: "#general",
			At:     fixedTime,
		}}, receivedEvents)
	})
}

func TestSession_model_reply_does_not_retrigger_dispatch(t *testing.T) {
	var dispatchCount int

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
			dispatchCount++
			return msgToolCalls(t, domain.ChannelName(events[0].Target), "got it"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, err := userSendMessage(ctx, t, sess, "#general", "hello")
	require.NoError(t, err)

	// The user's own outgoing message is not echoed on the
	// events channel; drain the dispatch lifecycle only.
	drainEvents(t, sess, 1)

	// Only one dispatch should have occurred — the ModelReplyEvent
	// emitted by the dispatch goroutine must not trigger another
	// dispatch.
	require.Equal(t, 1, dispatchCount)
}

// TestDispatchToInstance_excludes_own_events pins the echo gate's
// self-suppression rule: a model's outbound `domain.Message` is
// fanned to every channel member except the originating client
// (RFC 2812 §3.3.1). The test stands up two model bots and drives
// a user PRIVMSG into the channel; botty replies and helper passes.
// The asserted shape pins both sides of the gate: botty is
// dispatched with the user's message and never with its own reply,
// and helper is dispatched with both.
func TestDispatchToInstance_excludes_own_events(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const bottyID = "inst-botty"
		const helperID = "inst-helper"

		recorder := newDispatchRecorder()

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				recorder.record(modelID, events)

				if modelID == "test/model-a" && triggeredBy(events, "testuser") {
					return msgToolCalls(t, domain.ChannelName(events[0].Target), "hello"), nil
				}

				return api.CompletionResult{}, nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			InstanceID: bottyID,
			Nick:       "botty",
			ModelID:    "test/model-a",
			Channels:   testChannels("#general"),
		})
		seedInstance(t, sess, s, instanceSpec{
			InstanceID: helperID,
			Nick:       "helper",
			ModelID:    "test/model-b",
			Channels:   testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess), "botty", "helper")

		_, err := userSendMessage(ctx, t, sess, "#general", "hi")
		require.NoError(t, err)

		synctest.Wait()

		userTrigger := protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			From:   string(userNick(t, sess)),
			Target: "#general",
			Body:   "hi",
			At:     fixedTime,
		}
		bottyTrigger := protocol.IRCMessage{
			Kind:       protocol.KindPrivMsg,
			From:       "botty",
			InstanceID: bottyID,
			Target:     "#general",
			Body:       "hello",
			At:         fixedTime,
		}

		require.Equal(t, map[domain.ModelID][]protocol.IRCMessage{
			"test/model-a": {userTrigger},
			"test/model-b": {userTrigger, bottyTrigger},
		}, recorder.byModel(),
			"botty (model-a) is dispatched with the user's trigger and never "+
				"receives its own reply; helper (model-b) receives both")
	})
}

func TestDispatchToInstances_model_does_not_reply_to_self(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const bottyID = "inst-botty"
		const helperID = "inst-helper"

		recorder := newDispatchRecorder()

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				recorder.record(modelID, events)

				if modelID == "test/model-a" && triggeredBy(events, "testuser") {
					return msgToolCalls(t, domain.ChannelName(events[0].Target), "first reply"), nil
				}

				return api.CompletionResult{}, nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess), "botty", "helper")
		seedInstance(t, sess, s, instanceSpec{
			InstanceID: bottyID,
			Nick:       "botty",
			ModelID:    "test/model-a",
			Channels:   testChannels("#general"),
		})
		seedInstance(t, sess, s, instanceSpec{
			InstanceID: helperID,
			Nick:       "helper",
			ModelID:    "test/model-b",
			Channels:   testChannels("#general"),
		})

		_, err := userSendMessage(ctx, t, sess, "#general", "hello everyone")
		require.NoError(t, err)

		synctest.Wait()

		userTrigger := protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			From:   string(userNick(t, sess)),
			Target: "#general",
			Body:   "hello everyone",
			At:     fixedTime,
		}
		bottyTrigger := protocol.IRCMessage{
			Kind:       protocol.KindPrivMsg,
			From:       "botty",
			InstanceID: bottyID,
			Target:     "#general",
			Body:       "first reply",
			At:         fixedTime,
		}

		require.Equal(t, map[domain.ModelID][]protocol.IRCMessage{
			"test/model-a": {userTrigger},
			"test/model-b": {userTrigger, bottyTrigger},
		}, recorder.byModel(),
			"botty (model-a) sees only the user's message — the echo gate hides "+
				"its own reply. helper (model-b) sees the user's message and "+
				"botty's reply.")
	})
}

func TestSession_Dispatch_broadcasts_to_channel_instances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "got it"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "got it", At: fixedTime},
		}, msgs)
	})
}

func TestSession_Dispatch_does_not_broadcast_when_no_model_instances(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "should not appear"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		}, msgs)
	})
}

func TestSession_Dispatch_pass_response_does_not_store_model_message(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{}, nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		}, msgs)
	})
}

func TestSession_Dispatch_reply_response_stores_model_message(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "hello back"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "hello back", At: fixedTime},
		}, msgs)
	})
}

func TestSession_Dispatch_broadcasts_only_to_members_of_that_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), fmt.Sprintf("reply from %s", modelID)), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedChannelWithMembers(t, sess, s, "#random", "testuser", "otherbot")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model-a",
			Channels: testChannels("#general"),
		})
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "otherbot",
			ModelID:  "test/model-b",
			Channels: testChannels("#random"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		generalMsgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "reply from test/model-a", At: fixedTime},
		}, generalMsgs)

		randomMsgs := channelMessages(t, s, "#random")
		require.Empty(t, randomMsgs)
	})
}

func TestSession_Dispatch_reply_is_not_rebroadcast_to_its_sender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "reply once"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "reply once", At: fixedTime},
		}, msgs)
	})
}

func TestSession_Dispatch_multiple_instances_each_reply_once(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Reply only to the user's message; peer-bot triggers pass.
		// Otherwise the bus would loop replies between bots
		// indefinitely (each reply fans to the other, which then
		// replies, etc.) — production controls that with the "lurk"
		// prompt and model judgement; the test pins the one-reply-
		// per-bot shape directly by stubbing.
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				if !triggeredBy(events, "testuser") {
					return api.CompletionResult{}, nil
				}

				return msgToolCalls(t, domain.ChannelName(events[0].Target), fmt.Sprintf("reply from %s", modelID)), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess), "bot-a", "bot-b")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "bot-a",
			ModelID:  "test/model-a",
			Channels: testChannels("#general"),
		})
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "bot-b",
			ModelID:  "test/model-b",
			Channels: testChannels("#general"),
		})

		_, err := userSendMessage(ctx, t, sess, "#general", "hello world")
		require.NoError(t, err)

		synctest.Wait()

		msgs := channelMessages(t, s, "#general")

		require.Equal(t, domain.Message{
			Target: "#general", From: "testuser", Body: "hello world", At: fixedTime,
		}, msgs[0])
		require.ElementsMatch(t, []domain.Message{
			{Target: "#general", From: "bot-a", InstanceID: testMemberID("bot-a"), Body: "reply from test/model-a", At: fixedTime},
			{Target: "#general", From: "bot-b", InstanceID: testMemberID("bot-b"), Body: "reply from test/model-b", At: fixedTime},
		}, msgs[1:])
	})
}

func TestSession_Dispatch_ignores_empty_reply_body(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "   "), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		}, msgs)
	})
}

// TestSession_Dispatch_api_error_does_not_stop_the_other_instance
// pins the isolation each model-client's own dispatch goroutine
// gives it: bot-a's upstream failure is reported to the operator as
// a `ModelUnavailableError` and bot-b's turn runs regardless.
func TestSession_Dispatch_api_error_does_not_stop_the_other_instance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				if modelID == "test/model-a" {
					return api.CompletionResult{}, fmt.Errorf("network timeout")
				}

				return msgToolCalls(t, domain.ChannelName(events[0].Target), "reply from bot-b"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "bot-a", "bot-b")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "bot-a",
			ModelID:  "test/model-a",
			Channels: testChannels("#general"),
		})
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "bot-b",
			ModelID:  "test/model-b",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello world")

		require.Contains(t, collectEmittedEvents(t, sess), domain.Event(domain.ModelUnavailableError{
			Channel: "#general",
			Nick:    "bot-a",
			At:      fixedTime,
		}))

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
			{Target: "#general", From: "bot-b", InstanceID: testMemberID("bot-b"), Body: "reply from bot-b", At: fixedTime},
		}, msgs)
	})
}

func TestSession_Poke_api_error_emits_error_event(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
			if modelID == "test/model-a" {
				return api.CompletionResult{}, fmt.Errorf("rate limited")
			}

			return msgToolCalls(t, domain.ChannelName(events[0].Target), "still here"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "bot-a")
	seedInstance(t, sess, s, instanceSpec{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#random", "testuser", "bot-b")
	seedInstance(t, sess, s, instanceSpec{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: testChannels("#random"),
	})

	require.NoError(t, userPoke(ctx, t, sess))
	events := drainEvents(t, sess, 2)

	var failure *domain.ModelUnavailableError
	var hasReply bool

	for _, evt := range events {
		switch e := evt.(type) {
		case domain.ModelUnavailableError:
			ev := e
			failure = &ev
		case domain.Message:
			if e.From == "bot-b" {
				hasReply = true
			}
		}
	}

	require.NotNil(t, failure,
		"dispatch failure should emit a ModelUnavailableError on the bus")
	require.Equal(t, domain.ModelUnavailableError{
		Channel: "#general", Nick: "bot-a", At: fixedTime,
	}, *failure)
	require.True(t, hasReply, "successful model dispatch should emit its reply Message on the wire")

	msgs := channelMessages(t, s, "#random")
	require.Equal(t, []domain.Message{
		{Target: "#random", From: "bot-b", InstanceID: testMemberID("bot-b"), Body: "still here", At: fixedTime},
	}, msgs)
}

func TestSession_SetTopic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#dev"))

		require.NoError(t, userSetTopic(ctx, t, sess, "#dev", "Development Chat"))
		synctest.Wait()

		user := userInstance(t, sess)

		require.Equal(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target: "#dev", Nick: "testuser", Created: true,
				At: fixedTime, Instance: user,
			},
			domain.NamesReplyEvent{
				Channel: "#dev",
				Members: testMembers(t, sess, s, "testuser"),
				At:      fixedTime,
			},
			domain.NamesEnd{Channel: "#dev", At: fixedTime},
			domain.TopicChange{
				Target: "#dev", Topic: "Development Chat", By: "testuser",
				At: fixedTime, ByInstance: user,
			},
		}, collectEmittedEvents(t, sess),
			"the user is a member of #dev, so its own topic change comes back over the bus")

		require.Equal(t, []string{"join", "topic_change"}, channelEventTypes(t, s, "#dev"),
			"the topic change is broadcast and persisted to the channel")

		updated, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Equal(t, "Development Chat", updated.Topic)
		require.Equal(t, domain.Nick("testuser"), updated.TopicSetBy)
		require.Equal(t, fixedTime, updated.TopicSetAt)
	})
}

// TestSession_SetTopic_requires_membership covers RFC 2812 §3.2.4:
// TOPIC is refused for a client that is not on the channel, even on
// a `-t` channel and even for a server operator. The `+o` override
// waives channel-op status, which is a privilege among the members;
// it does not make a non-member a member.
func TestSession_SetTopic_requires_membership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		saveTestChannel(t, sess, s, domain.NewChannelWindow("#dev", fixedTime))

		err := userSetTopic(ctx, t, sess, "#dev", "Development Chat")
		require.Equal(t, domain.NotOnChannelError{Channel: "#dev", Command: "TOPIC", At: fixedTime}, err)

		unchanged, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Equal(t, "", unchanged.Topic)

		require.Empty(t, channelEventTypes(t, s, "#dev"))
	})
}

func TestSession_ChangeNick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		s := storetest.NewMemoryStore(t)
		sess := New(t.Context, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		// Join a channel so the nick change emits per-channel events.
		require.NoError(t, userJoin(t.Context(), t, sess, "#general"))
		general, err := sess.loadChannelWindow(t.Context(), "#general")
		require.NoError(t, err)
		generalMembers := general.Members

		require.NoError(t, userChangeNick(t.Context(), t, sess, "newname"))
		synctest.Wait()

		user := userInstance(t, sess)
		require.ElementsMatch(t, []domain.Event{
			domain.UserModeChange{
				Nick:       "testuser",
				InstanceID: user.ID(),
				Flag:       domain.ModeOperator,
				Add:        true,
				At:         bootAt,
				Instance:   user,
			},
			domain.Join{
				Target:   "#general",
				Nick:     "testuser",
				Instance: user,
				Created:  true,
				At:       fixedTime,
			},
			domain.NamesReplyEvent{
				Channel: "#general",
				Members: generalMembers,
				At:      fixedTime,
			},
			domain.NamesEnd{
				Channel: "#general",
				At:      fixedTime,
			},
			domain.NickChange{
				InstanceID: user.ID(),
				OldNick:    "testuser",
				NewNick:    "newname",
				Instance:   user,
				At:         fixedTime,
			},
		}, collectEmittedEvents(t, sess))

		require.Equal(t, domain.Nick("newname"), userNick(t, sess))
	})
}

func TestSession_ChangeNickAs_collisions(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, sess *Session, store *storemod.SQLiteStore) (actor *domain.Instance, target domain.Nick)
		wantError bool
	}{
		{
			name: "model collides with another model",
			setup: func(t *testing.T, sess *Session, store *storemod.SQLiteStore) (*domain.Instance, domain.Nick) {
				_ = seedInstance(t, sess, store, instanceSpec{Nick: "alice", ModelID: "test/model"})
				bob := seedInstance(t, sess, store, instanceSpec{Nick: "bob", ModelID: "test/model"})

				return bob, "alice"
			},
			wantError: true,
		},
		{
			name: "model collides with user",
			setup: func(t *testing.T, sess *Session, store *storemod.SQLiteStore) (*domain.Instance, domain.Nick) {
				bob := seedInstance(t, sess, store, instanceSpec{Nick: "bob", ModelID: "test/model"})

				return bob, userNick(t, sess)
			},
			wantError: true,
		},
		{
			name: "user collides with model",
			setup: func(t *testing.T, sess *Session, store *storemod.SQLiteStore) (*domain.Instance, domain.Nick) {
				_ = seedInstance(t, sess, store, instanceSpec{Nick: "alice", ModelID: "test/model"})

				return userInstance(t, sess), "alice"
			},
			wantError: true,
		},
		{
			name: "rename to same nick is a no-op",
			setup: func(t *testing.T, sess *Session, store *storemod.SQLiteStore) (*domain.Instance, domain.Nick) {
				bob := seedInstance(t, sess, store, instanceSpec{Nick: "bob", ModelID: "test/model"})

				return bob, "bob"
			},
			wantError: false,
		},
		{
			name: "fresh nick is accepted",
			setup: func(t *testing.T, sess *Session, store *storemod.SQLiteStore) (*domain.Instance, domain.Nick) {
				bob := seedInstance(t, sess, store, instanceSpec{Nick: "bob", ModelID: "test/model"})

				return bob, "carol"
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess, store := newTestSession(t)
			actor, target := tt.setup(t, sess, store)

			err := sess.changeNickAs(t.Context(), actor, target)

			if tt.wantError {
				var nickInUse domain.NickInUseError
				require.ErrorAs(t, err, &nickInUse)
				require.Equal(t, target, nickInUse.Nick)
				return
			}

			require.NoError(t, err)
			require.Equal(t, target, actor.Nick())
		})
	}
}

func TestSession_ResolveNick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	inst := seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "A test bot",
		Channels: testChannels("#dev"),
	})

	got, err := sess.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	require.Same(t, inst, got)
}

func TestSession_ResolveNickNotFound(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.ResolveNick(t.Context(), "ghost")
	require.Error(t, err)
}

func TestSession_AddModelNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, addModelViaWire(t.Context(), t, sess, "#ghost", "anthropic/claude-3-haiku", ""))
}

func TestSession_InviteAs_existing_instance_to_nonexistent_channel_does_not_corrupt(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, err := sess.inviteAs(ctx, userInstance(t, sess), "botty", "#ghost")
	require.Error(t, err)

	// Instance should not have the phantom channel in its set.
	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels(), "#general")
}

func TestSession_AddModel_persists_persona(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		require.NoError(t, addModelViaWire(ctx, t, sess, "#general", "anthropic/claude-3-haiku", "Helpful assistant"))
		synctest.Wait()

		inst, err := s.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)

		// The model's own JOIN is delivered but raises no dispatch
		// turn — it has nothing to say about its own arrival.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#general",
				Nick:       "fakenick",
				InstanceID: inst.ID(),
				At:         fixedTime,
				Instance:   inst,
			},
		}, collectEmittedEvents(t, sess))

		require.Equal(t, "Helpful assistant", inst.Persona())
	})
}

func TestSession_InviteAs_reuses_existing_instance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser")
		seedChannelWithMembers(t, sess, s, "#random", "testuser")

		event, err := sess.inviteAs(ctx, userInstance(t, sess), "botty", "#random")
		require.NoError(t, err)
		require.Equal(t, domain.Invited{
			Target:       "#random",
			Nick:         "botty",
			InstanceID:   botty.ID(),
			By:           "testuser",
			ByInstanceID: "",
			At:           fixedTime,
			Instance:     botty,
		}, event)
		synctest.Wait()

		// INVITE delivery is scoped to inviter + invitee
		// (RFC 2812 §3.2.7); the user-client bus carries only
		// botty's dispatch lifecycle.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		requireInstanceEqual(t, domain.NewModelInstance(
			testMemberID("botty"), "botty", "test/model", "Helpful assistant",
			testChannels("#general"),
		), botty)

		inst, err := s.ResolveNick(ctx, "botty")
		require.NoError(t, err)
		requireInstanceEqual(t, domain.NewModelInstance(
			testMemberID("botty"), "botty", "test/model", "Helpful assistant",
			testChannels("#general"),
		), inst)

		channel, err := sess.loadChannelWindow(ctx, "#random")
		require.NoError(t, err)
		require.False(t, channel.Members.HasInstance(botty),
			"INVITE does not mutate membership; the invited model joins via "+
				"its own dispatch turn")
	})
}

func TestSession_InviteAs_existing_member_returns_443(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	// Inviting a nick that is already on the channel refuses with
	// RFC 2812 numeric 443 (ERR_USERONCHANNEL) and leaves the
	// channel membership untouched.
	event, err := sess.inviteAs(ctx, userInstance(t, sess), "botty", "#general")
	require.Nil(t, event)
	require.Equal(t, domain.UserOnChannelError{Nick: "botty", Channel: "#general", At: fixedTime}, err)

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels(), "#general")

	channel, err := sess.loadChannelWindow(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, slices.Collect(testMembers(t, sess, s, "testuser", "botty").All()), slices.Collect(channel.Members.All()))
}

func TestSession_InviteAs_existing_instance_preserves_persona(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Persona:  "Existing persona",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser")
		seedChannelWithMembers(t, sess, s, "#random", "testuser")

		event, err := sess.inviteAs(ctx, userInstance(t, sess), "botty", "#random")
		require.NoError(t, err)
		require.Equal(t, domain.Invited{
			Target:       "#random",
			Nick:         "botty",
			InstanceID:   botty.ID(),
			By:           "testuser",
			ByInstanceID: "",
			At:           fixedTime,
			Instance:     botty,
		}, event)
		synctest.Wait()

		// INVITE delivery is scoped to inviter + invitee
		// (RFC 2812 §3.2.7); the user-client bus carries only
		// botty's dispatch lifecycle.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		require.Equal(t, "Existing persona", botty.Persona())

		inst, err := s.ResolveNick(ctx, "botty")
		require.NoError(t, err)
		require.Equal(t, "Existing persona", inst.Persona())
	})
}

func TestSession_KickNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, kickViaWire(t.Context(), t, sess, "#ghost", "botty"))
}

func TestSession_KickNonMember(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		saveTestChannel(t, sess, s, newTestChannelWindow("#dev", fixedTime, testMembers(t, sess, s, "testuser")))

		// Kicking an unresolved nick surfaces UnknownNickError
		// from the dispatcher and leaves the channel state and
		// the events bus untouched — no KickedEvent, no
		// membership mutation, no instance-channels mutation.
		require.ErrorAs(t, kickViaWire(ctx, t, sess, "#dev", "nobody"), &domain.UnknownNickError{})
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
		}, collectEmittedEvents(t, sess))

		updated, err := sess.loadChannelWindow(ctx, "#dev")
		require.NoError(t, err)
		require.Equal(t, slices.Collect(testMembers(t, sess, s, "testuser").All()), slices.Collect(updated.Members.All()))
	})
}

func TestSession_SetTopicNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, userSetTopic(t.Context(), t, sess, "#ghost", "topic"))
}

func TestSession_Dispatch_includes_memory_in_prompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
		require.NoError(t, memStore.Write(t.Context(), testMemberID("botty"), memory.Entry{
			Key:     "mood",
			Content: "curious",
		}))

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, system string, history []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				// The persona is the app's own statement of who this
				// instance is, so it is in the system prompt. A memory
				// is text the instance stored, so it rides in the
				// transcript as a server reply the model reads as
				// data.
				memoryLine := protocol.IRCMessage{
					Kind:   protocol.KindServerReply,
					Target: "#general",
					Body:   "your stored memories: [mood=curious]",
				}

				if strings.Contains(system, "Your persona: Helpful assistant") &&
					slices.Contains(history, memoryLine) {
					return msgToolCalls(t, domain.ChannelName(events[0].Target), "memory and persona received"), nil
				}

				return api.CompletionResult{}, nil
			},
		}
		s := storetest.NewMemoryStore(t)
		sess := New(t.Context, s, newTestModelClientFactoryWith(t, fake, memStore), nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Persona:  "Helpful assistant",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(t.Context(), t, sess, "#general", "hello world")

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello world", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "memory and persona received", At: fixedTime},
		}, msgs)
	})
}

func TestSession_Poke_emits_dispatch_events(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "poke received"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		require.NoError(t, userPoke(ctx, t, sess))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.PokeEvent{Channel: "#general", At: fixedTime},
			domain.ModelDispatchStarted{Instance: botty, At: fixedTime},
			domain.Message{
				Target:     "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "poke received",
				At:         fixedTime,
			},
			domain.ModelDispatchDone{Instance: botty, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "poke received", At: fixedTime},
		}, msgs)
	})
}

// TestSession_DM_routing_survives_counterpart_rename verifies
// that a message addressed into a DM after the counterpart has
// renamed reaches the renamed instance. DMs are addressed by
// the counterpart's `InstanceID`, which is stable across nick
// changes; the sidebar's `DisplayName` reads the live nick from
// the canonical `*Instance` so it follows the rename too.
func TestSession_DM_routing_survives_counterpart_rename(t *testing.T) {
	delivered := make(chan domain.Nick, 1)
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, trigger []protocol.IRCMessage) (api.CompletionResult, error) {
			if len(trigger) == 0 {
				return api.CompletionResult{}, nil
			}

			delivered <- domain.Nick(trigger[0].Target)
			return api.CompletionResult{}, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	dm := domain.NewDMWindow(botty, fixedTime)

	require.NoError(t, sess.changeNickAs(ctx, botty, "foobar"))

	_, err := sess.sendMessageAs(ctx, userInstance(t, sess), dm.Name(), "hi")
	require.NoError(t, err)

	require.Equal(t, domain.Nick(dm.Name()), <-delivered)
	require.Equal(t, "foobar", dm.DisplayName())
}

// TestSession_Dispatch_dm_only_targets_that_instance pins where a
// DM's two directions are logged and who they reach. Each direction
// is logged under its recipient: the user's line under botty's id,
// botty's answer under the user's (empty) id. The conversation is the
// union of the two, which is what `DMEventsBefore` reads back and
// what either party sees as one thread.
//
// The reply addresses the sender by nick, the way a client answers a
// PRIVMSG. A model has no other name for the person it is talking to:
// the trigger's target is the model's own id, which names the
// conversation and not a client the model may address.
func TestSession_Dispatch_dm_only_targets_that_instance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].From), "dm reply"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:    "botty",
			ModelID: "test/model-a",
		})
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "otherbot",
			ModelID:  "test/model-b",
			Channels: testChannels("#general"),
		})

		target := domain.ChannelName(botty.ID())

		dispatchUserMessage(ctx, t, sess, target, "hello in dm")

		require.Equal(t, []domain.Message{
			{Target: target, From: "testuser", Body: "hello in dm", At: fixedTime},
			{Target: "", From: "botty", InstanceID: testMemberID("botty"), Body: "dm reply", At: fixedTime},
		}, dmThreadMessages(t, s, "", botty.ID()))

		require.Empty(t, channelMessages(t, s, "#general"))
	})
}

// dmThreadMessages reads the messages of the DM thread between
// `self` and `peer`, in chronological order and in both directions.
func dmThreadMessages(t *testing.T, s *storemod.SQLiteStore, self, peer domain.InstanceID) []domain.Message {
	t.Helper()

	events, err := s.DMEventsBefore(t.Context(), self, peer, nil, 1000)
	require.NoError(t, err)

	var msgs []domain.Message

	for _, se := range events {
		if cm, ok := se.Event.(domain.Message); ok {
			msgs = append(msgs, cm)
		}
	}

	return msgs
}

// markReadViaStore stamps the read cursor for `ch` at the newest
// event, the way [userclient.UserClient.MarkRead] does. Where the
// cursor sits is client state; the session only reads it back
// through [Session.UnreadCount].
func markReadViaStore(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName) {
	t.Helper()

	events, err := s.EventsBefore(t.Context(), ch, nil, 1)
	require.NoError(t, err)
	require.NotEmpty(t, events, "no event to mark read in %s", ch)

	require.NoError(t, s.SetLastRead(t.Context(), ch, events[0].ID))
}

// markDMReadViaStore stamps the DM read cursor with peer at the
// newest event of the thread, the way
// [userclient.UserClient.MarkRead] does for a DM window.
func markDMReadViaStore(t *testing.T, s *storemod.SQLiteStore, peer domain.InstanceID) {
	t.Helper()

	events, err := s.DMEventsBefore(t.Context(), "", peer, nil, 1)
	require.NoError(t, err)
	require.NotEmpty(t, events, "no event to mark read in DM with %s", peer)

	require.NoError(t, s.SetDMLastRead(t.Context(), peer, events[0].ID))
}

// TestSession_UnreadCount_counts_both_directions_of_a_DM pins the
// badge for a DM window with no cursor recorded yet. Each direction
// is logged under its recipient, so counting the window's own key
// would count the lines the user sent and none of the ones it has
// not read: a model that answers three times would show nothing new.
// TestSession_MarkRead_and_UnreadCount_for_a_DM exercises the cursor
// half.
func TestSession_UnreadCount_counts_both_directions_of_a_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	window := domain.ChannelName(botty.ID())

	appendDM := func(from domain.Nick, id domain.InstanceID, target domain.ChannelName, body string) {
		t.Helper()

		_, err := s.AppendEvent(ctx, target, domain.Message{
			Target:     target,
			From:       from,
			InstanceID: id,
			Body:       body,
			At:         fixedTime,
		})
		require.NoError(t, err)
	}

	appendDM("testuser", "", window, "are you there?")
	appendDM("botty", botty.ID(), "", "i am")
	appendDM("botty", botty.ID(), "", "and again")

	count, err := sess.UnreadCount(ctx, window)
	require.NoError(t, err)
	require.Equal(t, 3, count)
}

func TestSession_MarkRead_and_UnreadCount(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	_, err := s.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general", From: "testuser", Body: "first", At: fixedTime,
	})
	require.NoError(t, err)
	_, err = s.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general", From: "testuser", Body: "second", At: fixedTime,
	})
	require.NoError(t, err)

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 2, count)

	markReadViaStore(t, s, "#general")

	count, err = sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

// TestSession_MarkRead_and_UnreadCount_for_a_DM is the DM
// counterpart to TestSession_MarkRead_and_UnreadCount, pinning the
// cursor half of the badge that last_read.channel's foreign key to
// channels(name) used to block: a DM window is never a row in
// channels, so the cursor for a DM had nowhere to be recorded.
func TestSession_MarkRead_and_UnreadCount_for_a_DM(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
	window := domain.ChannelName(botty.ID())

	appendDM := func(from domain.Nick, id domain.InstanceID, target domain.ChannelName, body string) {
		t.Helper()

		_, err := s.AppendEvent(ctx, target, domain.Message{
			Target:     target,
			From:       from,
			InstanceID: id,
			Body:       body,
			At:         fixedTime,
		})
		require.NoError(t, err)
	}

	appendDM("testuser", "", window, "are you there?")
	appendDM("botty", botty.ID(), "", "i am")

	count, err := sess.UnreadCount(ctx, window)
	require.NoError(t, err)
	require.Equal(t, 2, count)

	markDMReadViaStore(t, s, botty.ID())

	count, err = sess.UnreadCount(ctx, window)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	appendDM("botty", botty.ID(), "", "still there?")

	count, err = sess.UnreadCount(ctx, window)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	markDMReadViaStore(t, s, botty.ID())

	count, err = sess.UnreadCount(ctx, window)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestSession_UnreadCount_after_new_messages(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	_, err := s.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general", From: "testuser", Body: "first", At: fixedTime,
	})
	require.NoError(t, err)

	markReadViaStore(t, s, "#general")

	_, err = s.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general", From: "testuser", Body: "second", At: fixedTime,
	})
	require.NoError(t, err)
	_, err = s.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general", From: "testuser", Body: "third", At: fixedTime,
	})
	require.NoError(t, err)

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestSession_Dispatch_filters_history_before_join(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		beforeJoin := fixedTime.Add(-10 * time.Minute)
		afterJoin := fixedTime.Add(10 * time.Minute)

		var receivedHistory []protocol.IRCMessage

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				receivedHistory = history
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		// Both events are in the channel log before botty connects:
		// the attach is the single point at which it reads the log,
		// so what it reads there is what this test is about.
		_, err := s.AppendEvent(ctx, "#general", domain.Message{
			Target: "#general",
			From:   "testuser",
			Body:   "old message",
			At:     beforeJoin,
		})
		require.NoError(t, err)

		_, err = s.AppendEvent(ctx, "#general", domain.Message{
			Target: "#general",
			From:   "testuser",
			Body:   "new message",
			At:     afterJoin,
		})
		require.NoError(t, err)

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general")})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "ping")

		// The model should only see the message from after it joined, not the
		// one from before.
		require.Equal(t, []protocol.IRCMessage{
			{
				Kind:   protocol.KindPrivMsg,
				From:   "testuser",
				Target: "#general",
				Body:   "new message",
				At:     afterJoin,
			},
		}, receivedHistory)
	})
}

// TestSession_Dispatch_forwards_replies_to_subsequent_models pins
// cross-model fan-out: when alpha replies, that reply reaches beta's
// dispatch loop as a trigger of its own. Alpha's Send goes through
// `sendMessageAs`, fans out, and beta's dispatch goroutine picks the
// new event off its subscription.
func TestSession_Dispatch_forwards_replies_to_subsequent_models(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder := newDispatchRecorder()

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				recorder.record(modelID, events)

				if modelID == "test/alpha" {
					return msgToolCalls(t, domain.ChannelName(events[0].Target), "alpha says hi"), nil
				}

				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", userNick(t, sess), "alpha", "beta")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "alpha",
			ModelID:  "test/alpha",
			Channels: testChannels("#general")})
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "beta",
			ModelID:  "test/beta",
			Channels: testChannels("#general")})

		_, err := userSendMessage(ctx, t, sess, "#general", "hello everyone")
		require.NoError(t, err)

		synctest.Wait()

		userTrigger := protocol.IRCMessage{
			Kind:   protocol.KindPrivMsg,
			From:   string(userNick(t, sess)),
			Target: "#general",
			Body:   "hello everyone",
			At:     fixedTime,
		}
		alphaTrigger := protocol.IRCMessage{
			Kind:       protocol.KindPrivMsg,
			From:       "alpha",
			InstanceID: testMemberID("alpha"),
			Target:     "#general",
			Body:       "alpha says hi",
			At:         fixedTime,
		}

		require.Equal(t, map[domain.ModelID][]protocol.IRCMessage{
			"test/alpha": {userTrigger},
			"test/beta":  {userTrigger, alphaTrigger},
		}, recorder.byModel(),
			"alpha sees only the user's message (echo gate hides its own reply); "+
				"beta sees the user's message and alpha's reply")
	})
}

// --- Log capture ---

// logBuffer is a thread-safe bytes.Buffer that captures slog JSON
// output and allows searching for records by message.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lb *logBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	return lb.buf.Write(p)
}

// find returns the first JSON log record whose "msg" field equals the
// given message, or nil if not found.
func (lb *logBuffer) find(msg string) map[string]any {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	for line := range bytes.SplitSeq(lb.buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			continue
		}

		if record["msg"] == msg {
			return record
		}
	}

	return nil
}

// --- Fake API client ---

type fakeAPIClient struct {
	listModelsFn              func(context.Context) ([]api.ModelInfo, error)
	sendEventsFn              func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
	continueWithToolResultsFn func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error)
	generateNickFn            func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error)
	generatePersonasFn        func(context.Context, domain.ModelID) ([]domain.Persona, error)
}

func (f *fakeAPIClient) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.listModelsFn != nil {
		return f.listModelsFn(ctx)
	}

	return nil, nil
}

func (f *fakeAPIClient) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	if f.sendEventsFn != nil {
		return f.sendEventsFn(ctx, modelID, selfInstanceID, system, history, events)
	}

	return api.CompletionResult{}, nil
}

func (f *fakeAPIClient) ContinueWithToolResults(
	ctx context.Context,
	conv *api.Conversation,
	results []api.ToolResult,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	if f.continueWithToolResultsFn != nil {
		return f.continueWithToolResultsFn(ctx, conv, results)
	}

	return api.CompletionResult{}, nil
}

func (f *fakeAPIClient) GenerateNick(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (api.NicknameResult, error) {
	if f.generateNickFn != nil {
		nick, err := f.generateNickFn(ctx, smallModel, persona, exclude)
		return api.NicknameResult{Nick: nick}, err
	}

	// Default fake returns "fakenick" on the first try and a numbered
	// variant on each retry so AddModel test paths that invoke
	// `GenerateNick` multiple times (or hit a collision) produce
	// distinct nicks without each test wiring its own counter.
	nick := domain.Nick("fakenick")
	if len(exclude) > 0 {
		nick = domain.Nick(fmt.Sprintf("fakenick%d", len(exclude)))
	}

	return api.NicknameResult{Nick: nick}, nil
}

func (f *fakeAPIClient) GeneratePersonas(ctx context.Context, smallModel domain.ModelID) ([]domain.Persona, error) {
	if f.generatePersonasFn != nil {
		return f.generatePersonasFn(ctx, smallModel)
	}

	return nil, nil
}

type failingMemoryStore struct {
	writeErr  error
	deleteErr error
}

func (f *failingMemoryStore) Read(_ context.Context, _ domain.InstanceID) ([]memory.Entry, error) {
	return nil, nil
}

func (f *failingMemoryStore) Write(_ context.Context, _ domain.InstanceID, _ memory.Entry) error {
	return f.writeErr
}

func (f *failingMemoryStore) Delete(_ context.Context, _ domain.InstanceID, _ string) error {
	return f.deleteErr
}

func (f *failingMemoryStore) Reset(_ context.Context) error {
	return nil
}

func newTestSessionWithMemory(t *testing.T, apiClient api.Client) (*Session, *storemod.SQLiteStore, *memory.StoreAdapter) {
	t.Helper()

	s := storetest.NewMemoryStore(t)

	m := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	sess := New(t.Context, s, newTestModelClientFactoryWith(t, apiClient, m), nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s, m
}

func mustRawJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()

	return json.RawMessage(raw)
}

func mustToolResultContent(t *testing.T, payload modelclient.ToolResultPayload) string {
	t.Helper()

	data, err := json.Marshal(payload)
	require.NoError(t, err)

	return string(data)
}

func TestSession_Dispatch_write_memory_then_reply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var continueResults []api.ToolResult
		turn := 0
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_1", Name: "write_memory", Args: mustRawJSON(t, `{"key":"mood","content":"happy"}`)},
					},
				}, nil
			},
			continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
				defer func() { turn++ }()
				if turn == 0 {
					continueResults = results
					return msgToolCalls(t, "#general", "noted!"), nil
				}
				return api.CompletionResult{}, nil
			},
		}

		sess, s, memStore := newTestSessionWithMemory(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []api.ToolResult{
			{ToolCallID: "call_1", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: true, Summary: `stored memory "mood"`})},
		}, continueResults)

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "noted!", At: fixedTime},
		}, channelMessages(t, s, "#general"))

		memories, err := memStore.Read(ctx, testMemberID("botty"))
		require.NoError(t, err)
		require.Equal(t, []memory.Entry{{Key: "mood", Content: "happy"}}, memories)
	})
}

func TestSession_Dispatch_delete_memory_then_pass(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var continueResults []api.ToolResult
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_1", Name: "delete_memory", Args: mustRawJSON(t, `{"key":"old_stuff"}`)},
					},
				}, nil
			},
			continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
				continueResults = results
				return api.CompletionResult{}, nil
			},
		}

		sess, s, memStore := newTestSessionWithMemory(t, fake)
		ctx := t.Context()

		require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "old_stuff", Content: "remove me"}))

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []api.ToolResult{
			{ToolCallID: "call_1", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: true, Summary: `deleted memory "old_stuff"`})},
		}, continueResults)

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
		}, channelMessages(t, s, "#general"))

		memories, err := memStore.Read(ctx, testMemberID("botty"))
		require.NoError(t, err)
		require.Empty(t, memories)
	})
}

func TestSession_Dispatch_memory_write_error_returns_error_to_model(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var continueResults []api.ToolResult
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_1", Name: "write_memory", Args: mustRawJSON(t, `{"key":"mood","content":"happy"}`)},
					},
				}, nil
			},
			continueWithToolResultsFn: continueOnceWith(&continueResults, msgToolCalls(t, "#general", "ok anyway")),
		}

		s := storetest.NewMemoryStore(t)
		memStore := &failingMemoryStore{writeErr: fmt.Errorf("disk full")}
		sess := New(t.Context, s, newTestModelClientFactoryWith(t, fake, memStore), nil)
		t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
		attachTestUserClient(t, sess, "testuser")
		sess.now = func() time.Time { return fixedTime }
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []api.ToolResult{
			{ToolCallID: "call_1", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: false, Error: "disk full"})},
		}, continueResults)

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "ok anyway", At: fixedTime},
		}, channelMessages(t, s, "#general"))
	})
}

func TestSession_Dispatch_multiple_memory_calls_in_one_response(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var continueResults []api.ToolResult
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_1", Name: "write_memory", Args: mustRawJSON(t, `{"key":"mood","content":"happy"}`)},
						{ID: "call_2", Name: "write_memory", Args: mustRawJSON(t, `{"key":"topic","content":"go programming"}`)},
					},
				}, nil
			},
			continueWithToolResultsFn: continueOnceWith(&continueResults, msgToolCalls(t, "#general", "stored both")),
		}

		sess, s, memStore := newTestSessionWithMemory(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []api.ToolResult{
			{ToolCallID: "call_1", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: true, Summary: `stored memory "mood"`})},
			{ToolCallID: "call_2", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: true, Summary: `stored memory "topic"`})},
		}, continueResults)

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "stored both", At: fixedTime},
		}, channelMessages(t, s, "#general"))

		memories, err := memStore.Read(ctx, testMemberID("botty"))
		require.NoError(t, err)
		require.Equal(t, []memory.Entry{
			{Key: "mood", Content: "happy"},
			{Key: "topic", Content: "go programming"},
		}, memories)
	})
}

func TestSession_Dispatch_search_memory_then_reply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var continueResults []api.ToolResult
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_1", Name: "search_memory", Args: mustRawJSON(t, `{"query":"favourite colour","limit":5}`)},
					},
				}, nil
			},
			continueWithToolResultsFn: continueOnceWith(&continueResults, msgToolCalls(t, "#general", "your favourite colour is blue")),
		}

		sess, s, memStore := newTestSessionWithMemory(t, fake)
		ctx := t.Context()

		require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "colour", Content: "blue"}))

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "what is my favourite colour?")

		require.Equal(t, []api.ToolResult{
			{ToolCallID: "call_1", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: false, Error: "unknown tool \"search_memory\""})},
		}, continueResults)

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "what is my favourite colour?", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "your favourite colour is blue", At: fixedTime},
		}, channelMessages(t, s, "#general"))
	})
}

// newEmbeddingServer returns an httptest server that responds to
// OpenAI-compatible embedding requests. The topics map assigns each
// keyword a dimension in the embedding vector; matching keywords get a
// unit vector in that dimension, non-matching text gets a uniform
// spread.
func newEmbeddingServer(t *testing.T, dims int, topics map[string]int) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)

		var req struct {
			Input string `json:"input"`
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		vec := make([]float32, dims)

		matched := false
		for keyword, dim := range topics {
			if strings.Contains(req.Input, keyword) {
				vec[dim] = 1.0
				matched = true

				break
			}
		}

		if !matched {
			val := float32(1.0 / math.Sqrt(float64(dims)))
			for i := range vec {
				vec[i] = val
			}
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": vec},
			},
		}))
	}))
	t.Cleanup(srv.Close)

	return srv
}

func newTestSessionWithIndexedMemory(
	t *testing.T,
	apiClient api.Client,
	embeddingURL string,
) (*Session, *storemod.SQLiteStore, *memory.IndexedStore) {
	t.Helper()

	s := storetest.NewMemoryStore(t)

	backing := memory.NewStoreAdapter(storetest.NewMemoryStore(t))

	normalized := true
	embeddingFunc := chromem.NewEmbeddingFuncOpenAICompat(
		embeddingURL, "test-key", "test-model", &normalized,
	)

	m := memory.NewIndexedStoreFromDB(t.Context(), backing, chromem.NewDB(), embeddingFunc)
	sess := New(t.Context, s, newTestModelClientFactoryWith(t, apiClient, m), nil)
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }

	return sess, s, m
}

func TestSession_Dispatch_search_memory_with_vector_store(t *testing.T) {
	// Three topics in 3 dimensions. Querying "cats" produces [1,0,0],
	// giving each entry a distinct cosine similarity:
	//   "cats are great"    → [1,0,0] → 1.0
	//   "no keyword match"  → uniform  → 1/√3 ≈ 0.577
	//   "dogs are loyal"    → [0,1,0] → 0.0
	//
	embSrv := newEmbeddingServer(t, 3, map[string]int{
		"cats": 0,
		"dogs": 1,
		"fish": 2,
	})

	uniformSim := float32(1.0 / math.Sqrt(3))

	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Name: "search_memory", Args: mustRawJSON(t, `{"query":"cats","limit":3}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: continueOnceWith(&continueResults, msgToolCalls(t, "#general", "your favourite is cats")),
	}

	sess, s, memStore := newTestSessionWithIndexedMemory(t, fake, embSrv.URL)
	ctx := t.Context()

	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "fav_pet", Content: "cats are great"}))
	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "hobby", Content: "no keyword match here"}))
	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "other_pet", Content: "dogs are loyal"}))

	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	dispatchUserMessageAwaitingTurns(ctx, t, sess, "#general", "what is my favourite pet?", 1)

	// Unmarshal the JSON content so we can assert the full search
	// results slice, then assert the full tool results wrapper too.
	var payload modelclient.ToolResultPayload
	require.NoError(t, json.Unmarshal([]byte(continueResults[0].Content), &payload))
	require.True(t, payload.OK)
	require.Equal(t, "found 3 matching memories", payload.Summary)

	data, err := json.Marshal(payload.Data)
	require.NoError(t, err)

	var searchResults []memory.SearchResult
	require.NoError(t, json.Unmarshal(data, &searchResults))

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: continueResults[0].Content},
	}, continueResults)

	require.Equal(t, []memory.SearchResult{
		{Entry: memory.Entry{Key: "fav_pet", Content: "cats are great"}, Similarity: 1.0},
		{Entry: memory.Entry{Key: "hobby", Content: "no keyword match here"}, Similarity: uniformSim},
		{Entry: memory.Entry{Key: "other_pet", Content: "dogs are loyal"}, Similarity: 0},
	}, searchResults)

	require.Equal(t, []domain.Message{
		{Target: "#general", From: "testuser", Body: "what is my favourite pet?", At: fixedTime},
		{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "your favourite is cats", At: fixedTime},
	}, channelMessages(t, s, "#general"))
}

func TestSession_Dispatch_write_then_search_memory_with_vector_store(t *testing.T) {
	// Two topics in 2 dimensions. After writing two entries, a search
	// for "cats" returns both with distinct scores:
	//   "cats are wonderful" → [1,0] → 1.0
	//   "dogs are loyal"     → [0,1] → 0.0
	embSrv := newEmbeddingServer(t, 2, map[string]int{
		"cats": 0,
		"dogs": 1,
	})

	var writeResults, searchResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_write_cats", Name: "write_memory", Args: mustRawJSON(t, `{"key":"pet_cats","content":"cats are wonderful"}`)},
					{ID: "call_write_dogs", Name: "write_memory", Args: mustRawJSON(t, `{"key":"pet_dogs","content":"dogs are loyal"}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func() func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
			turn := 0
			return func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
				defer func() { turn++ }()
				switch turn {
				case 0:
					writeResults = results
					return api.CompletionResult{
						Conversation: &api.Conversation{},
						PendingToolCalls: []api.PendingToolCall{
							{ID: "call_search", Name: "search_memory", Args: mustRawJSON(t, `{"query":"cats","limit":5}`)},
						},
					}, nil
				case 1:
					searchResults = results
					return msgToolCalls(t, "#general", "noted"), nil
				default:
					return api.CompletionResult{}, nil
				}
			}
		}(),
	}

	sess, s, _ := newTestSessionWithIndexedMemory(t, fake, embSrv.URL)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	dispatchUserMessageAwaitingTurns(ctx, t, sess, "#general", "hello", 1)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_write_cats", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: true, Summary: `stored memory "pet_cats"`})},
		{ToolCallID: "call_write_dogs", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: true, Summary: `stored memory "pet_dogs"`})},
	}, writeResults)

	var searchPayload modelclient.ToolResultPayload
	require.NoError(t, json.Unmarshal([]byte(searchResults[0].Content), &searchPayload))
	require.True(t, searchPayload.OK)

	data, err := json.Marshal(searchPayload.Data)
	require.NoError(t, err)

	var parsed []memory.SearchResult
	require.NoError(t, json.Unmarshal(data, &parsed))

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_search", Content: searchResults[0].Content},
	}, searchResults)

	require.Equal(t, []memory.SearchResult{
		{Entry: memory.Entry{Key: "pet_cats", Content: "cats are wonderful"}, Similarity: 1.0},
		{Entry: memory.Entry{Key: "pet_dogs", Content: "dogs are loyal"}, Similarity: 0},
	}, parsed)
}

func TestSession_Dispatch_memory_loop_respects_max_turns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The model never calls reply/pass — just keeps writing memories
		// forever. The loop should stop after maxToolLoopTurns continue
		// calls and return no replies.
		var writtenKeys []string
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_init", Name: "write_memory", Args: mustRawJSON(t, `{"key":"k0","content":"v0"}`)},
					},
				}, nil
			},
			continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
				for _, r := range results {
					writtenKeys = append(writtenKeys, r.ToolCallID)
				}

				// Return another memory write — the loop should eventually stop.
				nextKey := fmt.Sprintf("k%d", len(writtenKeys))
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_" + nextKey, Name: "write_memory", Args: mustRawJSON(t, fmt.Sprintf(`{"key":"%s","content":"val"}`, nextKey))},
					},
				}, nil
			},
		}

		sess, s, _ := newTestSessionWithMemory(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		// 1 initial SendEvents + maxToolLoopTurns continues = maxToolLoopTurns
		// tool result deliveries.
		require.Equal(t, []string{
			"call_init",
			"call_k1",
			"call_k2",
			"call_k3",
			"call_k4",
		}, writtenKeys)
	})
}

func TestSession_Dispatch_encodes_msg_tool_spans(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				fg := uint8(4)
				return msgSpansToolCall(t, domain.ChannelName(events[0].Target), []protocol.ReplySpan{
					{Text: "hello "},
					{Text: "world", Style: &protocol.ReplyStyle{Bold: true, FG: &fg}},
				}), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "hello \x02\x0304world\x0f", At: fixedTime},
		}, channelMessages(t, s, "#general"))
	})
}

func TestSession_Dispatch_msg_tool_error_lets_model_retry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var rejected []api.ToolResult
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgSpansToolCall(t, domain.ChannelName(events[0].Target), []protocol.ReplySpan{
					{Text: "", Style: &protocol.ReplyStyle{Bold: true}},
				}), nil
			},
			continueWithToolResultsFn: continueOnceWith(&rejected, msgToolCalls(t, "#general", "clean reply")),
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []api.ToolResult{
			{ToolCallID: "call_msg_spans_0", Content: mustToolResultContent(t, modelclient.ToolResultPayload{OK: false, Error: "span 0 is empty"})},
		}, rejected)
		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "clean reply", At: fixedTime},
		}, channelMessages(t, s, "#general"))
	})
}

func TestSession_Dispatch_repeated_msg_tool_errors_drop_after_max_turns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		recorder, provider := oteltest.NewSpanRecorder(t)
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgSpansToolCall(t, domain.ChannelName(events[0].Target), []protocol.ReplySpan{
					{Text: "", Style: &protocol.ReplyStyle{Bold: true}},
				}), nil
			},
			continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, _ []api.ToolResult) (api.CompletionResult, error) {
				return msgSpansToolCall(t, "#general", []protocol.ReplySpan{
					{Text: "", Style: &protocol.ReplyStyle{Bold: true}},
				}), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		sess.WithTracerProvider(provider)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
		}, channelMessages(t, s, "#general"))

		span := oteltest.FindSpan(t, recorder, "modelclient.dispatch_to_instance")
		require.Equal(t, observability.PassReasonToolLoopExhausted, oteltest.AttrValue(span.Attributes(), observability.AttrPassReason))
	})
}

func TestSession_Dispatch_me_tool_sends_action(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				return meToolCall(t, "waves"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
			{Target: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "waves", Action: true, At: fixedTime},
		}, channelMessages(t, s, "#general"))
	})
}

func TestSession_Dispatch_msg_tool_rejects_newline_body(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				return msgToolCalls(t, domain.ChannelName(events[0].Target), "always\nmultiline"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		dispatchUserMessage(ctx, t, sess, "#general", "hello")

		require.Equal(t, []domain.Message{
			{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
		}, channelMessages(t, s, "#general"))
	})
}

// seedChannelWithMembers commits a channel with the given members.
// Each member nick must either be the session user or must have been
// previously seeded via `seedInstance` so that its canonical handle
// exists in the store. If the user is listed, the session's in-memory
// `user.Channels()` and recorded user mode are updated to match.
//
// The commit goes through the session so the seeded channel lands in
// live state as well as the store — the session answers every
// channel question from live state, so a fixture that wrote only to
// the store would be seeding a record nothing reads. The user is an
// ephemeral actor stripped on the way to disk, which the session's
// own commit path handles.
func seedChannelWithMembers(t *testing.T, sess *Session, s *storemod.SQLiteStore, name domain.ChannelName, members ...domain.Nick) {
	t.Helper()

	cw := newTestChannelWindow(name, fixedTime, testMembers(t, sess, s, members...))

	registerUserMembership(t, sess, name, members)

	require.NoError(t, sess.persistChannelWindow(t.Context(), cw))
}

// saveTestChannel commits a pre-built window fixture. For
// `*domain.ChannelWindow` it splits ephemeral user membership
// from the on-disk form: if the channel's member list lists the
// session user, the session's `user.Channels()` + recorded user
// mode are updated to match, and the commit goes through the
// session so the channel lands in live state as well as the store.
// Tests construct channel windows with the user as a member for
// readability; the store never sees the user. DM and status
// windows are persisted as-is — the session holds no live state
// for them.
func saveTestChannel(t *testing.T, sess *Session, s *storemod.SQLiteStore, w domain.Window) {
	t.Helper()

	cw, ok := w.(*domain.ChannelWindow)
	if !ok {
		require.NoError(t, s.SaveWindow(t.Context(), w))
		return
	}

	if m, found := cw.Members.GetByInstance(userInstance(t, sess)); found {
		userInstance(t, sess).JoinChannel(cw.Name(), fixedTime)

		modes := m.Modes
		if modes == (domain.MemberModes{}) {
			modes = domain.MemberModes{Operator: true}
		}

		sess.setUserModes(t.Context(), cw.Name(), modes)
	}

	require.NoError(t, sess.persistChannelWindow(t.Context(), cw))
}

// registerUserMembership updates the session's in-memory user state
// when a test seeds a channel that lists the user as a member. It
// adds the channel to `user.Channels()` and records the
// conventional operator privilege that `joinAs` would have set on a
// real join. Tests that want different privileges can override via
// the internal `setUserModes` helper.
func registerUserMembership(t *testing.T, sess *Session, name domain.ChannelName, members []domain.Nick) {
	userNick := userNick(t, sess)
	for _, m := range members {
		if m != userNick {
			continue
		}

		userInstance(t, sess).JoinChannel(name, fixedTime)
		sess.setUserModes(t.Context(), name, domain.MemberModes{Operator: true})
		return
	}
}

// seedInstance is the legacy helper that matches the old test
// vocabulary. It accepts an `instanceSpec` and returns the canonical
// handle. If spec.InstanceID is
// empty, the conventional `inst-<nick>` id is used instead. If the
// store already has a canonical handle for the resolved id (a
// previous seedInstance, or an auto-seed from testMembers), its
// fields are updated in place and that canonical pointer is
// returned — so a test can refer to the instance before or after
// seeding and get the same pointer either way.
// seedInstance writes a model-instance row to the store and
// attaches a `*modelclient.ModelClient` for it via
// [attachModelClient]. Pairing the store write with the attach
// call keeps the test fixture's invariant — a seeded instance is
// a registered subscriber with its dispatch goroutine running —
// aligned with the production lifecycle.
func seedInstance(t *testing.T, sess *Session, s *storemod.SQLiteStore, spec instanceSpec) *domain.Instance {
	t.Helper()

	id := spec.InstanceID
	if id == "" {
		id = testMemberID(spec.Nick)
	}

	ctx := t.Context()

	if existing, err := s.GetInstanceByID(ctx, id); err == nil && existing != nil {
		existing.SetNick(spec.Nick)
		existing.SetPersona(spec.Persona)
		if spec.ModelID != "" {
			existing.ModelID = spec.ModelID
		}
		existing.LeaveAllChannels()
		if spec.Channels != nil {
			for pair := spec.Channels.Oldest(); pair != nil; pair = pair.Next() {
				existing.JoinChannel(pair.Key, pair.Value)
			}
		}
		require.NoError(t, s.SaveInstance(ctx, existing))
		attachModelClient(t, sess, existing)
		return existing
	}

	inst := domain.NewModelInstance(id, spec.Nick, spec.ModelID, spec.Persona, spec.Channels)
	require.NoError(t, s.SaveInstance(ctx, inst))
	attachModelClient(t, sess, inst)

	return inst
}

// instanceSpec bundles the fields a test cares about when describing
// a model instance to seed. Replaces the inlined `domain.Instance{…}`
// struct literals that were possible when Instance's fields were
// exported.
type instanceSpec struct {
	InstanceID domain.InstanceID
	Nick       domain.Nick
	ModelID    domain.ModelID
	Persona    string
	Channels   *orderedmap.OrderedMap[domain.ChannelName, time.Time]
}

// channelMessages extracts Message events from stored events.
func channelMessages(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName) []domain.Message {
	t.Helper()

	events, err := s.EventsBefore(t.Context(), ch, nil, 1000)
	require.NoError(t, err)

	var msgs []domain.Message

	for _, se := range events {
		if cm, ok := se.Event.(domain.Message); ok {
			msgs = append(msgs, cm)
		}
	}

	return msgs
}

func channelEventTypes(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName) []string {
	t.Helper()

	events, err := s.EventsBefore(t.Context(), ch, nil, 1000)
	require.NoError(t, err)

	types := make([]string, len(events))

	for i, se := range events {
		types[i] = domain.EventType(se.Event)
	}

	return types
}

// TestSession_Dispatch_upstream_outcome covers the three upstream
// endings a turn can have. Refusal and content filtering are silence
// the model chose, so nothing reaches the channel and no diagnostic
// is raised; a truncated response is a failure, and the operator is
// told through `ModelUnavailableError`.
func TestSession_Dispatch_upstream_outcome(t *testing.T) {
	tests := []struct {
		name          string
		upstream      error
		wantOperError bool
	}{
		{name: "content filtered is silence", upstream: api.ErrContentFiltered},
		{name: "model refusal is silence", upstream: &api.ErrModelRefused{Reason: "I cannot help with that"}},
		{name: "truncated response is a failure", upstream: api.ErrResponseTruncated, wantOperError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fake := &fakeAPIClient{
					sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
						return api.CompletionResult{}, tc.upstream
					},
				}

				sess, s := newTestSessionWithAPI(t, fake)
				ctx := t.Context()

				seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
				seedInstance(t, sess, s, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#general"),
				})

				dispatchUserMessage(ctx, t, sess, "#general", "hello")

				failure := domain.Event(domain.ModelUnavailableError{Channel: "#general", Nick: "botty", At: fixedTime})
				if tc.wantOperError {
					require.Contains(t, collectEmittedEvents(t, sess), failure)
				} else {
					require.NotContains(t, collectEmittedEvents(t, sess), failure)
				}

				require.Equal(t, []domain.Message{
					{Target: "#general", From: "testuser", Body: "hello", At: fixedTime},
				}, channelMessages(t, s, "#general"))
			})
		})
	}
}

// drainEvents reads from both event buses until n ModelDispatchDone
// values have been received, and returns all events in order.
func drainEvents(t *testing.T, sess *Session, doneCount int) []domain.Event {
	t.Helper()

	var events []domain.Event
	done := 0

	for {
		evt, ok := nextEvent(t, sess)
		if !ok {
			t.Fatal("events channels closed before receiving all ModelDispatchDones")
			return nil
		}

		events = append(events, evt)
		if _, ok := evt.(domain.ModelDispatchDone); ok {
			done++
			if done >= doneCount {
				return events
			}
		}
	}
}

func TestSession_Invite_with_explicit_persona_skips_pool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		fake := &fakeAPIClient{
			generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
				t.Fatal("GeneratePersonas should not be called when persona is explicit")
				return nil, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#dev", "testuser")

		require.NoError(t, addModelViaWire(ctx, t, sess, "#dev", "anthropic/claude-3-haiku", "Custom persona"))
		synctest.Wait()

		inst, err := s.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)

		// The model's own JOIN is delivered but raises no dispatch
		// turn — it has nothing to say about its own arrival.
		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Join{
				Target:     "#dev",
				Nick:       "fakenick",
				InstanceID: inst.ID(),
				At:         fixedTime,
				Instance:   inst,
			},
		}, collectEmittedEvents(t, sess))

		require.Equal(t, "Custom persona", inst.Persona())
	})
}

func TestDispatchToInstance_logs_dispatch_attributes(t *testing.T) {
	tests := []struct {
		name string
		fake *fakeAPIClient
		want map[string]any
	}{
		{
			name: "model replies via msg tool",
			fake: &fakeAPIClient{
				sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
					return msgToolCalls(t, domain.ChannelName(events[0].Target), "I have thoughts"), nil
				},
			},
			want: map[string]any{
				"component":       "modelclient",
				"channel":         "#dev",
				"nick":            "botty",
				"model_id":        "test/model-a",
				"trigger_count":   float64(1),
				"trigger_summary": "PRIVMSG from testuser",
				"tool_turns":      float64(1),
				"pass_reason":     "model_pass",
				"msg":             "dispatch to instance",
				"level":           "INFO",
			},
		},
		{
			name: "model passes by emitting no tool calls",
			fake: &fakeAPIClient{
				sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
					return api.CompletionResult{}, nil
				},
			},
			want: map[string]any{
				"component":       "modelclient",
				"channel":         "#dev",
				"nick":            "botty",
				"model_id":        "test/model-a",
				"trigger_count":   float64(1),
				"trigger_summary": "PRIVMSG from testuser",
				"tool_turns":      float64(0),
				"pass_reason":     "model_pass",
				"msg":             "dispatch to instance",
				"level":           "INFO",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var buf logBuffer

				handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
				slog.SetDefault(slog.New(handler))
				t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

				sess, s := newTestSessionWithAPI(t, tc.fake)
				ctx := t.Context()

				seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
				seedInstance(t, sess, s, instanceSpec{
					InstanceID: "inst-botty",
					Nick:       "botty",
					ModelID:    "test/model-a",
					Channels:   testChannels("#dev"),
				})

				dispatchUserMessage(ctx, t, sess, "#dev", "hi there")

				record := buf.find("dispatch to instance")
				require.NotNil(t, record, "expected 'dispatch to instance' log entry")
				delete(record, "time")
				require.Equal(t, tc.want, record)
			})
		})
	}
}

func TestSendMessageAs_model_triggers_dispatch_to_other_models(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		dispatched := make(map[domain.ModelID][]protocol.IRCMessage)

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				dispatched[modelID] = append(dispatched[modelID], events...)
				return api.CompletionResult{}, nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		alpha := seedInstance(t, sess, s, instanceSpec{
			InstanceID: "inst-alpha",
			Nick:       "alpha",
			ModelID:    "test/model-a",
			Channels:   testChannels("#general"),
		})
		beta := seedInstance(t, sess, s, instanceSpec{
			InstanceID: "inst-beta",
			Nick:       "beta",
			ModelID:    "test/model-b",
			Channels:   testChannels("#general"),
		})
		seedChannelWithMembers(t, sess, s, "#general", "testuser", "alpha", "beta")

		_, err := sess.sendMessageAs(ctx, alpha, "#general", "hello from alpha")
		require.NoError(t, err)
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			domain.Message{
				Target:     "#general",
				From:       "alpha",
				InstanceID: alpha.ID(),
				Body:       "hello from alpha",
				At:         fixedTime,
			},
			domain.ModelDispatchStarted{Instance: beta, At: fixedTime},
			domain.ModelDispatchDone{Instance: beta, At: fixedTime},
		}, collectEmittedEvents(t, sess))

		wantMsg := protocol.IRCMessage{
			Kind:       protocol.KindPrivMsg,
			From:       "alpha",
			InstanceID: "inst-alpha",
			Target:     "#general",
			Body:       "hello from alpha",
			At:         fixedTime,
		}

		require.Equal(t, map[domain.ModelID][]protocol.IRCMessage{
			"test/model-b": {wantMsg},
		}, dispatched)
	})
}

// TestAddModel_own_join_is_filed_but_not_dispatched pins the two
// halves of AGENTS.md's self-join guard: the newly-added model's own
// JOIN raises no dispatch turn — a model has nothing to say about its
// own arrival, and a turn nobody asked for is a paid API call for
// nothing — but the event still reaches the model's transcript, so a
// later turn's history shows it joined.
func TestAddModel_own_join_is_filed_but_not_dispatched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		dispatched := make(map[domain.ModelID][]protocol.IRCMessage)
		var lastHistory []protocol.IRCMessage

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				dispatched[modelID] = append(dispatched[modelID], events...)
				lastHistory = history
				return api.CompletionResult{}, nil
			},
			generateNickFn: func(_ context.Context, _ domain.ModelID, _ string, _ []domain.Nick) (domain.Nick, error) {
				return "botty", nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#dev", "testuser")

		require.NoError(t, addModelViaWire(ctx, t, sess, "#dev", "test/model", ""))
		synctest.Wait()

		bot, err := s.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)

		joinEvent := domain.Join{
			Target:     "#dev",
			Nick:       "fakenick",
			InstanceID: bot.ID(),
			At:         fixedTime,
			Instance:   bot,
		}

		require.ElementsMatch(t, []domain.Event{
			bootstrapModeChange(t, sess, bootAt),
			joinEvent,
		}, collectEmittedEvents(t, sess))
		require.Empty(t, dispatched)

		// A message from someone else does trigger a turn; its
		// history shows the earlier JOIN was filed, not dropped.
		_, err = userSendMessage(ctx, t, sess, "#dev", "hey botty")
		require.NoError(t, err)
		synctest.Wait()

		require.Contains(t, lastHistory, protocol.IRCMessage{
			Kind:       protocol.KindJoin,
			From:       "fakenick",
			InstanceID: bot.ID(),
			Target:     "#dev",
			At:         fixedTime,
		})
	})
}

// failingAppendStore wraps a [Store] and forces AppendEvent
// to return errFailedAppend for any channel listed in failChannels.
// All other methods pass through to the embedded interface
// unchanged.
type failingAppendStore struct {
	Store

	failChannels    map[domain.ChannelName]struct{}
	errFailedAppend error
}

func (f *failingAppendStore) AppendEvent(ctx context.Context, ch domain.ChannelName, event domain.ChannelActivity) (int64, error) {
	if _, ok := f.failChannels[ch]; ok {
		return 0, f.errFailedAppend
	}

	return f.Store.AppendEvent(ctx, ch, event)
}

// TestSession_appendEvent_persistence_failure_is_silent pins the
// contract for store-side append failures: they increment the
// `persistence_failures` counter and log via slog, but do not
// surface a chat-window notice. The IRC protocol has no numeric
// for "your server's database is broken"; the operator-facing
// signal is metrics and logs. The check covers both a regular
// channel and the chat-screen-owned `&modeloff` to confirm the
// behaviour is uniform.
func TestSession_appendEvent_persistence_failure_is_silent(t *testing.T) {
	cases := []struct {
		name    string
		channel domain.ChannelName
		event   domain.ChannelActivity
	}{
		{
			name:    "regular channel",
			channel: "#general",
			event: domain.Message{
				Target: "#general", From: "testuser", Body: "hello", At: fixedTime,
			},
		},
		{
			name:    "status channel",
			channel: domain.StatusChannelName,
			event: domain.Message{
				Target: domain.StatusChannelName, From: "testuser", Body: "boot notice", At: fixedTime,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				bootAt := time.Now()
				store := &failingAppendStore{
					Store:           storetest.NewMemoryStore(t),
					failChannels:    map[domain.ChannelName]struct{}{tc.channel: {}},
					errFailedAppend: fmt.Errorf("disk full"),
				}

				sess := New(t.Context, store, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
				t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })
				attachTestUserClient(t, sess, "testuser")
				sess.now = func() time.Time { return fixedTime }

				sess.appendEvent(t.Context(), tc.channel, tc.event)
				synctest.Wait()

				require.ElementsMatch(t, []domain.Event{
					bootstrapModeChange(t, sess, bootAt),
				}, collectEmittedEvents(t, sess))
			})
		})
	}
}

// TestSession_Shutdown_waits_for_dispatch_drain pins the
// [Session.Shutdown] contract: it does not cancel anything itself,
// it joins the dispatch goroutines that the caller's cancellation
// eventually wakes. The test spawns a session with a cancellable
// supplier ctx, registers a model-client to ensure at least one
// dispatch goroutine is running, cancels the supplier ctx, and
// asserts that `Shutdown` returns nil under a generous bound.
func TestSession_Shutdown_waits_for_dispatch_drain(t *testing.T) {
	supplyCtx, cancelSupply := context.WithCancel(t.Context())
	t.Cleanup(cancelSupply)

	s := storetest.NewMemoryStore(t)
	sess := New(func() context.Context { return supplyCtx }, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }

	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	cancelSupply()

	require.NoError(t, sess.Shutdown(t.Context()),
		"Shutdown must drain after the supplier ctx is cancelled")
}

// TestSession_Shutdown_returns_deadline_err_when_drain_exceeds_bound
// pins the deadline arm: if the caller's ctx expires before the
// dispatch goroutines exit, `Shutdown` returns `ctx.Err()`. We
// keep the supplier ctx alive (so the dispatch goroutine remains
// blocked on its events channel forever) and pass a ctx with an
// already-elapsed deadline to `Shutdown`.
func TestSession_Shutdown_returns_deadline_err_when_drain_exceeds_bound(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	sess := New(t.Context, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, sess.Shutdown(expired), context.Canceled,
		"Shutdown must surface the caller ctx error when drain exceeds the bound")
}

// TestSession_Subscribe_refused_after_shutdown pins the shutdown
// gate: a fresh subscription attempt after [Session.Shutdown] has
// begun must be refused. Refusal at the registration point is what
// keeps the modelclient-side dispatch goroutine off the bus once
// the session has stopped accepting new work.
func TestSession_Subscribe_refused_after_shutdown(t *testing.T) {
	supplyCtx, cancelSupply := context.WithCancel(t.Context())
	t.Cleanup(cancelSupply)

	s := storetest.NewMemoryStore(t)
	sess := New(func() context.Context { return supplyCtx }, s, newTestModelClientFactory(t, &fakeAPIClient{}), nil)
	attachTestUserClient(t, sess, "testuser")
	sess.now = func() time.Time { return fixedTime }

	cancelSupply()
	require.NoError(t, sess.Shutdown(t.Context()))

	inst := domain.NewModelInstance("late-inst", "latebot", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(t.Context(), inst))

	stub := &shutdownGateStubClient{id: protocol.ClientID(inst.ID())}
	_, err := sess.Subscribe(stub, protocol.SubscribeOptions{Instance: inst})
	require.Error(t, err, "Subscribe must refuse new registration after Shutdown")
}

// shutdownGateStubClient is the smallest possible [protocol.Client]
// the shutdown-gate test uses to drive [Session.Subscribe]. The
// session never reads `Events` or `Send` on it — the registration
// refusal short-circuits before either runs.
type shutdownGateStubClient struct {
	id protocol.ClientID
}

func (c *shutdownGateStubClient) Identity() protocol.ClientID { return c.id }
func (c *shutdownGateStubClient) Send(context.Context, protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}
func (c *shutdownGateStubClient) Events() <-chan protocol.Delivery { return nil }
func (c *shutdownGateStubClient) Caps() command.CapabilityHolder   { return nil }
