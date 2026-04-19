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
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
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

type comparableChannel struct {
	Name       domain.ChannelName
	Kind       domain.ChannelKind
	Topic      string
	TopicSetBy domain.Nick
	TopicSetAt time.Time
	Members    []domain.Member
	Created    time.Time
}

func normaliseChannel(ch domain.Channel) comparableChannel {
	return comparableChannel{
		Name:       ch.Name,
		Kind:       ch.Kind,
		Topic:      ch.Topic,
		TopicSetBy: ch.TopicSetBy,
		TopicSetAt: ch.TopicSetAt,
		Members:    ch.Members.Slice(),
		Created:    ch.Created,
	}
}

func loadGolden(t *testing.T, name string) string {
	t.Helper()

	root, err := os.OpenRoot("testdata")
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	f, err := root.Open(name)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(f)
	require.NoError(t, err)

	return string(data)
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

func drainEvent[T domain.SessionEvent](t *testing.T, sess *Session) T {
	t.Helper()

	select {
	case evt := <-sess.Events():
		got, ok := evt.(T)
		require.True(t, ok, "expected %T, got %T", *new(T), evt)
		return got
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %T", *new(T))
		return *new(T)
	}
}

// drainEventSkipping reads events from the session channel, skipping
// dispatch lifecycle events (DispatchStartedEvent, DispatchDoneEvent),
// until it finds one matching T.
func drainEventSkipping[T domain.SessionEvent](t *testing.T, sess *Session) T {
	t.Helper()

	for {
		select {
		case evt := <-sess.Events():
			if got, ok := evt.(T); ok {
				return got
			}

			switch evt.(type) {
			case domain.DispatchStartedEvent,
				domain.DispatchDoneEvent,
				domain.SystemNoticeEvent:
				continue
			default:
				t.Fatalf("expected %T, got %T", *new(T), evt)
				return *new(T)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %T", *new(T))
			return *new(T)
		}
	}
}

// drainDispatchEvents reads and discards dispatch lifecycle events
// until the channel is quiet.
func drainDispatchEvents(t *testing.T, sess *Session) {
	t.Helper()

	for {
		select {
		case evt := <-sess.Events():
			switch evt.(type) {
			case domain.DispatchStartedEvent, domain.DispatchDoneEvent:
				continue
			default:
				t.Fatalf("expected dispatch event, got %T", evt)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

// sessionEventMatcher pairs a human-readable name (used in
// timeout messages) with a predicate over [domain.SessionEvent].
// Build matchers via [matchEvent].
type sessionEventMatcher struct {
	name  string
	match func(domain.SessionEvent) bool
}

// matchEvent returns a [sessionEventMatcher] that accepts any event
// whose dynamic type is `T`.
func matchEvent[T domain.SessionEvent]() sessionEventMatcher {
	var zero T

	return sessionEventMatcher{
		name: reflect.TypeOf(zero).String(),
		match: func(evt domain.SessionEvent) bool {
			_, ok := evt.(T)
			return ok
		},
	}
}

// drainUntilMatched is the most general of the session-test drain
// helpers (`drainEvent`, `drainEventSkipping`, `drainNEvents`,
// `drainEvents`, `drainUntilMatched`) and the default choice when a
// test wants to decouple from the current implementation's exact
// event count.
//
// It reads from the session events channel until every member of
// `expected` has been matched at least once, returning two slices in
// arrival order: `matched` holds the events that satisfied a matcher;
// `extras` holds everything else seen along the way. Callers that
// want to assert no surprises can `require.Empty(t, extras)`; callers
// that only care about the wanted events can ignore it.
//
// Matchers are matched left-to-right, first-un-seen-wins: passing the
// same matcher twice (e.g. two `matchEvent[domain.SystemNoticeEvent]`)
// consumes two distinct events of that type.
//
// Picking between the helpers:
//   - `drainNEvents` is appropriate when the test is asserting on the
//     exact event count (e.g. pinning a no-extra-emit invariant). Most
//     setup-clearing call sites should prefer `drainUntilMatched`.
//   - `drainEventSkipping` is appropriate when the test expects only
//     dispatch-lifecycle noise between events of interest and wants
//     any other event type to fail loudly. `drainUntilMatched` is the
//     permissive counterpart that tolerates any unrelated event.
func drainUntilMatched(t *testing.T, sess *Session, expected ...sessionEventMatcher) (matched, extras []domain.SessionEvent) {
	t.Helper()

	matched = make([]domain.SessionEvent, 0, len(expected))
	extras = make([]domain.SessionEvent, 0)
	seen := make([]bool, len(expected))
	matchedCount := 0

	unmatchedNames := func() []string {
		names := make([]string, 0, len(expected))
		for i, matcher := range expected {
			if !seen[i] {
				names = append(names, matcher.name)
			}
		}
		return names
	}

	eventTypes := func(events []domain.SessionEvent) []string {
		types := make([]string, 0, len(events))
		for _, evt := range events {
			types = append(types, reflect.TypeOf(evt).String())
		}
		return types
	}

	for matchedCount < len(expected) {
		select {
		case evt := <-sess.Events():
			matchedIndex := -1

			for i, matcher := range expected {
				if seen[i] {
					continue
				}

				if matcher.match(evt) {
					matchedIndex = i
					break
				}
			}

			if matchedIndex >= 0 {
				seen[matchedIndex] = true
				matched = append(matched, evt)
				matchedCount++
				continue
			}

			extras = append(extras, evt)
		case <-time.After(time.Second):
			t.Fatalf(
				"timed out waiting for events %v; matched=%v extras=%v",
				unmatchedNames(),
				eventTypes(matched),
				eventTypes(extras),
			)
		}
	}

	return matched, extras
}

// testMembers builds a MemberList using canonical `*Instance`
// handles from the given session + store. The user is looked up via
// `sess.UserInstance()`; every model nick is resolved from the store
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

		if nick == sess.UserNick() {
			inst = sess.UserInstance()
		} else {
			var err error
			inst, err = s.ResolveNick(t.Context(), nick)
			if err != nil {
				inst = seedInstance(t, s, instanceSpec{Nick: nick, ModelID: "test/model"})
			}
		}

		ml.Add(inst)
		if nick == sess.UserNick() {
			ml.SetMode(inst, domain.ModeOp)
		} else {
			ml.SetMode(inst, domain.ModeVoice)
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

func requireChannelEqual(t *testing.T, expected, actual domain.Channel) {
	t.Helper()

	require.Equal(t, normaliseChannel(expected), normaliseChannel(actual))
}

func requireInstanceEqual(t *testing.T, expected, actual *domain.Instance) {
	t.Helper()

	require.Equal(t, normaliseInstance(expected), normaliseInstance(actual))
}

func newTestSession(t *testing.T) (*Session, *storemod.SQLiteStore) {
	t.Helper()

	return newTestSessionWithAPI(t, &fakeAPIClient{})
}

func newTestSessionWithAPI(t *testing.T, apiClient api.Client) (*Session, *storemod.SQLiteStore) {
	t.Helper()

	s := storetest.NewMemoryStore(t)

	sess := New(s, nil, apiClient, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	return sess, s
}

func TestSession_Join(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	evt := drainEvent[domain.JoinEvent](t, sess)
	require.Equal(t, domain.JoinEvent{
		Channel:  "#general",
		Instance: sess.UserInstance(),
		Created:  true,
		At:       fixedTime,
	}, evt)

	// Channel should be persisted.
	ch, err := sess.GetChannel(ctx, "#general")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "testuser"),
		Created: fixedTime,
	}, ch)

	// Last channel should be set.
	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#general"), last)
}

func TestSession_JoinExistingChannel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	existing := domain.Channel{
		Name:    "#existing",
		Kind:    domain.KindChannel,
		Topic:   "Already here",
		Members: testMembers(t, sess, s, "testuser"),
		Created: fixedTime.Add(-time.Hour),
	}
	saveTestChannel(t, sess, s, existing)

	require.NoError(t, sess.Join(ctx, "#existing"))

	// Channel should not be overwritten.
	ch, err := sess.GetChannel(ctx, "#existing")
	require.NoError(t, err)
	require.Equal(t, "Already here", ch.Topic)

	// No join event should be stored since the user was already a member.
	types := channelEventTypes(t, s, "#existing")
	require.Empty(t, types)
}

func TestSession_JoinAlreadyMember_no_duplicate_event(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))

	// Join again — should not emit a second join event.
	require.NoError(t, sess.Join(ctx, "#general"))

	// First join creates the channel, so we get join + mode_change.
	// Second join should add nothing.
	types := channelEventTypes(t, s, "#general")
	require.Equal(t, []string{"join", "mode_change"}, types)
}

func TestSession_JoinSwitchAndReturn_no_duplicate_event(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	require.NoError(t, sess.Join(ctx, "#random"))

	// Switch back to #general — no new join event.
	require.NoError(t, sess.Join(ctx, "#general"))

	types := channelEventTypes(t, s, "#general")
	require.Equal(t, []string{"join", "mode_change"}, types)

	// SetLastChannel should still be updated.
	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#general"), last)
}

func TestSession_JoinAutojoinChannels_populates_user_join_times(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "botty")
	seedChannelWithMembers(t, sess, s, "#random", "botty")
	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general", "#random"}))

	require.True(t, sess.UserJoinedAt("#general").IsZero())
	require.True(t, sess.UserJoinedAt("#random").IsZero())

	require.NoError(t, sess.JoinAutojoinChannels(ctx))

	// Drain join + mode events for each channel, skipping dispatch events.
	for range 2 {
		drainEventSkipping[domain.JoinEvent](t, sess)
		drainEventSkipping[domain.ModeChangeEvent](t, sess)
	}

	require.Equal(t, fixedTime, sess.UserJoinedAt("#general"))
	require.Equal(t, fixedTime, sess.UserJoinedAt("#random"))
}

func TestSession_JoinAutojoinChannels_empty_autojoin_is_noop(t *testing.T) {
	sess, _ := newTestSession(t)

	require.NoError(t, sess.JoinAutojoinChannels(t.Context()))
	requireChannels(t, sess.UserInstance().Channels())
}

func TestSession_JoinAutojoinChannels_emits_join_events(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#alpha", "botty")
	seedChannelWithMembers(t, sess, s, "#beta", "botty")
	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#alpha", "#beta"}))

	require.NoError(t, sess.JoinAutojoinChannels(ctx))

	joinA := drainEventSkipping[domain.JoinEvent](t, sess)
	require.Equal(t, domain.ChannelName("#alpha"), joinA.Channel)

	_ = drainEventSkipping[domain.ModeChangeEvent](t, sess)

	joinB := drainEventSkipping[domain.JoinEvent](t, sess)
	require.Equal(t, domain.ChannelName("#beta"), joinB.Channel)

	_ = drainEventSkipping[domain.ModeChangeEvent](t, sess)
}

func TestSession_Leave(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#leaving",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "testuser", "botty"),
		Created: fixedTime,
	}
	saveTestChannel(t, sess, s, ch)

	require.NoError(t, sess.Part(ctx, "#leaving", ""))
	evt := drainEvent[domain.PartEvent](t, sess)
	require.Equal(t, domain.PartEvent{
		Channel:  "#leaving",
		Instance: sess.UserInstance(),
		At:       fixedTime,
	}, evt)

	updated, err := sess.GetChannel(ctx, "#leaving")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:    "#leaving",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "botty"),
		Created: fixedTime,
	}, updated)
}

func TestSession_LeaveNonexistent(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, sess.Part(t.Context(), "#ghost", ""))
}

func TestSession_Part_carries_message(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#farewell",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "testuser"),
		Created: fixedTime,
	}
	saveTestChannel(t, sess, s, ch)

	require.NoError(t, sess.Part(ctx, "#farewell", "see ya later"))
	evt := drainEvent[domain.PartEvent](t, sess)
	require.Equal(t, domain.PartEvent{
		Channel:  "#farewell",
		Instance: sess.UserInstance(),
		Message:  "see ya later",
		At:       fixedTime,
	}, evt)
}

func TestSession_Connect_marks_session_active(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Connect(ctx))

	got, err := s.GetSessionActive(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.Equal(t, fixedTime, sess.ConnectedAt())

	select {
	case <-sess.Connected():
	default:
		t.Fatal("Connected() channel should be closed after Connect")
	}

	statusEvents := channelEventTypes(t, s, domain.StatusChannelName)
	require.Equal(t, []string{"system_notice"}, statusEvents,
		"clean connect should append exactly one Connected notice")
}

func TestSession_Connect_clears_unclean_user_membership(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, "stale"))
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedChannelWithMembers(t, sess, s, "#random", "testuser")

	require.NoError(t, sess.Connect(ctx))

	general, err := sess.GetChannel(ctx, "#general")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "botty"),
		Created: fixedTime,
	}, general)

	random, err := sess.GetChannel(ctx, "#random")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:    "#random",
		Kind:    domain.KindChannel,
		Members: domain.NewMemberList(),
		Created: fixedTime,
	}, random)

	statusEvents := channelEventTypes(t, s, domain.StatusChannelName)
	require.Equal(t, []string{"system_notice", "system_notice"}, statusEvents,
		"unclean connect should append a Connected notice and a Reconnected-after-unclean notice")
}

func TestSession_Connect_then_JoinAutojoin_stamps_UserJoinedAt(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	// Simulate the original bug's preconditions: stale membership left
	// over from a prior session, plus a non-empty session_active marker.
	require.NoError(t, s.SetSessionActive(ctx, "stale"))
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedChannelWithMembers(t, sess, s, "#random", "testuser")
	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general", "#random"}))

	require.NoError(t, sess.Connect(ctx))

	// Connect on an unclean session emits: status-channel JoinEvent,
	// status-channel ModeChangeEvent, "Connected to modeloff"
	// SystemNoticeEvent, and "Reconnected after unclean shutdown"
	// SystemNoticeEvent.
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.SystemNoticeEvent](),
		matchEvent[domain.SystemNoticeEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.JoinAutojoinChannels(ctx))

	require.Equal(t, fixedTime, sess.UserJoinedAt("#general"))
	require.Equal(t, fixedTime, sess.UserJoinedAt("#random"))

	// Each channel should have produced a fresh JoinEvent + ModeChangeEvent.
	for range 2 {
		_ = drainEventSkipping[domain.JoinEvent](t, sess)
		_ = drainEventSkipping[domain.ModeChangeEvent](t, sess)
	}
}

func TestSession_FocusChannel_emits_event_and_persists_last_channel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.FocusChannel(ctx, "#general"))

	evt := drainEvent[domain.FocusChannelEvent](t, sess)
	require.Equal(t, domain.ChannelName("#general"), evt.Channel)
	require.Equal(t, fixedTime, evt.At)

	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#general"), last)
}

func TestSession_FocusChannel_nonmember_is_noop(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.FocusChannel(ctx, "#nope"))

	select {
	case evt := <-sess.Events():
		t.Fatalf("expected no event, got %T", evt)
	default:
	}
}

func TestSession_Connect_Quit_Reconnect_omits_status_channel_from_autojoin(t *testing.T) {
	s := storetest.NewMemoryStore(t)

	sess1 := New(s, nil, &fakeAPIClient{}, "testuser", "", "")
	sess1.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	require.NoError(t, sess1.Connect(ctx))
	_, extras := drainUntilMatched(t, sess1,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.SystemNoticeEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess1.Join(ctx, "#general"))
	_, extras = drainUntilMatched(t, sess1,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess1.Quit(ctx, "bye"))

	autojoin, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#general"}, autojoin)

	// Starting a fresh session over the same store must not replay the
	// status channel into the autojoin loop.
	sess2 := New(s, nil, &fakeAPIClient{}, "testuser", "", "")
	sess2.now = func() time.Time { return fixedTime }
	require.NoError(t, sess2.Connect(ctx))

	autojoin, err = s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#general"}, autojoin)
}

func TestSession_Connect_unclean_recovery_emits_status_notices(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, &fakeAPIClient{}, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, "stale"))

	require.NoError(t, sess.Connect(ctx))

	// Persisted status-channel event log: Connected notice then
	// Reconnected-after-unclean notice, in order.
	require.Equal(t, []string{"system_notice", "system_notice"},
		channelEventTypes(t, s, domain.StatusChannelName))

	events, err := s.EventsBefore(ctx, domain.StatusChannelName, nil, 10)
	require.NoError(t, err)

	type storedNotice struct {
		Channel domain.ChannelName
		Text    string
	}
	got := make([]storedNotice, 0, len(events))
	for _, e := range events {
		notice, ok := e.Event.(domain.ChannelSystemNotice)
		require.True(t, ok, "expected ChannelSystemNotice, got %T", e.Event)
		got = append(got, storedNotice{Channel: notice.Channel, Text: notice.Text})
	}
	require.Equal(t, []storedNotice{
		{Channel: domain.StatusChannelName, Text: "Connected to modeloff"},
		{Channel: domain.StatusChannelName, Text: "Reconnected after unclean shutdown"},
	}, got)
}

// TestSession_user_snapshot_race_free hammers JoinAs, PartAs, and
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
			case <-sess.Events():
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
			_ = sess.Join(ctx, string(ch))
			_ = sess.Part(ctx, ch, "")
		}
	})

	wg.Go(func() {
		for i := range iters {
			ch := channels[i%len(channels)]
			_ = sess.UserJoinedAt(ch)
			_ = sess.UserNick()
		}
	})

	wg.Wait()

	// Final state: whichever of Join/Part ran last wins, but the
	// invariant we care about is "no torn read, no panic".
	// UserJoinedAt on any known channel returns either zero time or
	// fixedTime, never garbage.
	for _, ch := range channels {
		got := sess.UserJoinedAt(ch)
		if !got.IsZero() {
			require.Equal(t, fixedTime, got, "UserJoinedAt must return a coherent snapshot value")
		}
	}
}

func TestSession_Connect_is_idempotent(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Connect(ctx))
	require.NoError(t, sess.Connect(ctx))

	// Second Connect is a no-op: no duplicate "Connected" notice, no
	// panic from close-of-closed-channel.
	require.Equal(t, []string{"system_notice"},
		channelEventTypes(t, s, domain.StatusChannelName))

	select {
	case <-sess.Connected():
	default:
		t.Fatal("Connected() channel should be closed after Connect")
	}

	// The no-op second call records no span: it short-circuits before
	// startSpan so session.connect counts reflect real attempts only.
	var connectSpans int
	for _, span := range recorder.Ended() {
		if span.Name() == "session.connect" {
			connectSpans++
		}
	}
	require.Equal(t, 1, connectSpans)
}

func TestSession_FocusChannel_status_channel_is_valid(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Connect(ctx))
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.SystemNoticeEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.FocusChannel(ctx, domain.StatusChannelName))

	evt := drainEvent[domain.FocusChannelEvent](t, sess)
	require.Equal(t, domain.FocusChannelEvent{
		Channel: domain.StatusChannelName,
		At:      fixedTime,
	}, evt)

	last, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.StatusChannelName, last)
}

func TestSession_Quit_appends_channel_quit_events_and_saves_autojoin(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.Join(ctx, "#random"))
	_, extras = drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.Quit(ctx, "goodnight"))

	autojoin, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#general", "#random"}, autojoin)

	for _, ch := range []domain.ChannelName{"#general", "#random"} {
		require.Equal(t, []string{"join", "mode_change", "quit"}, channelEventTypes(t, s, ch))
	}
}

func TestSession_Quit_removes_user_from_channel_members(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.Quit(ctx, ""))

	ch, err := sess.GetChannel(ctx, "#general")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Members: domain.NewMemberList(),
		Created: fixedTime,
	}, ch)
}

func TestSession_Quit_clears_in_memory_channels(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.Quit(ctx, ""))

	require.Equal(t, 0, sess.UserInstance().Channels().Len())
}

// TestSession_user_state_triple_stays_consistent verifies that the
// three sources of user-membership state stay aligned through a
// full command sequence (join, part, rejoin, nick change). The
// invariant being pinned: userModes, user.Channels(), and the
// stripped persisted Channel.Members agree at every step.
func TestSession_user_state_triple_stays_consistent(t *testing.T) {
	type userSnapshot struct {
		Channels   []domain.ChannelName
		Mode       domain.NickMode
		OnDiskUser bool
	}

	snapshot := func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ch domain.ChannelName) userSnapshot {
		t.Helper()

		var channels []domain.ChannelName
		for pair := sess.UserInstance().Channels().Oldest(); pair != nil; pair = pair.Next() {
			channels = append(channels, pair.Key)
		}

		stored, err := s.GetChannel(t.Context(), ch)
		onDisk := err == nil && stored.Members.HasInstance(sess.UserInstance())

		return userSnapshot{
			Channels:   channels,
			Mode:       sess.userModeFor(t.Context(), ch),
			OnDiskUser: onDisk,
		}
	}

	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.Equal(t, userSnapshot{
		Channels:   []domain.ChannelName{"#general"},
		Mode:       domain.ModeOp,
		OnDiskUser: false,
	}, snapshot(t, sess, s, "#general"))

	require.NoError(t, sess.Part(ctx, "#general", ""))
	_, extras = drainUntilMatched(t, sess,
		matchEvent[domain.PartEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.Equal(t, userSnapshot{
		Channels:   nil,
		Mode:       domain.ModeNone,
		OnDiskUser: false,
	}, snapshot(t, sess, s, "#general"))

	require.NoError(t, sess.Join(ctx, "#general"))
	_, extras = drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.Equal(t, userSnapshot{
		Channels:   []domain.ChannelName{"#general"},
		Mode:       domain.ModeOp,
		OnDiskUser: false,
	}, snapshot(t, sess, s, "#general"))

	require.NoError(t, sess.ChangeNick(ctx, "renamed"))
	_, extras = drainUntilMatched(t, sess, matchEvent[domain.NickChangeEvent]())
	require.Empty(t, extras)

	require.Equal(t, userSnapshot{
		Channels:   []domain.ChannelName{"#general"},
		Mode:       domain.ModeOp,
		OnDiskUser: false,
	}, snapshot(t, sess, s, "#general"))
}

func TestSession_Quit_clears_session_active_marker(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, fixedTime.Format(time.RFC3339Nano)))

	require.NoError(t, sess.Quit(ctx, ""))

	got, err := s.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSession_Quit_no_channels_is_noop_but_clears_marker(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, fixedTime.Format(time.RFC3339Nano)))

	require.NoError(t, sess.Quit(ctx, "bye"))

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
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			calls.Add(1)
			return protocol.Reply("bye"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	sess.UserInstance().MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
		m.Set("#general", fixedTime)
	})

	require.NoError(t, sess.Quit(ctx, "bye"))

	require.Equal(t, int32(0), calls.Load(),
		"Quit must not dispatch to models; models see the quit next time they are dispatched against")
}

func TestSession_AddModel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "testuser"),
		Created: fixedTime,
	}
	saveTestChannel(t, sess, s, ch)

	require.NoError(t, sess.AddModel(ctx, "#dev", "anthropic/claude-3-haiku", ""))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.NotNil(t, evt.Instance)
	require.NotEmpty(t, evt.Instance.ID())
	require.Equal(t, domain.ModelInvitedEvent{
		Channel:  "#dev",
		Instance: evt.Instance,
		By:       "testuser",
		At:       fixedTime,
	}, evt)
	requireInstanceEqual(t, domain.NewModelInstance(
		evt.Instance.ID(), "fakenick", "anthropic/claude-3-haiku", "", testChannels("#dev"),
	), evt.Instance)

	// Instance should be persisted.
	inst, err := s.ResolveNick(ctx, "fakenick")
	require.NoError(t, err)
	requireInstanceEqual(t, domain.NewModelInstance(
		evt.Instance.ID(), "fakenick", "anthropic/claude-3-haiku", "", testChannels("#dev"),
	), inst)

	// Channel should have new member.
	updated, err := sess.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, []domain.Member{
		{Instance: sess.UserInstance(), Nick: "testuser", Mode: domain.ModeOp},
		{Instance: evt.Instance, Nick: "fakenick", Mode: domain.ModeVoice},
	}, updated.Members.Slice())
}

func TestSession_Kick(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#dev", "#random"),
	})
	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "testuser", "botty"),
		Created: fixedTime,
	}
	saveTestChannel(t, sess, s, ch)

	require.NoError(t, sess.Kick(ctx, "#dev", "botty"))
	evt := drainEvent[domain.ModelKickedEvent](t, sess)
	require.Equal(t, domain.ModelKickedEvent{
		Channel:  "#dev",
		Instance: botty,
		By:       "testuser",
		At:       fixedTime,
	}, evt)

	// Channel should no longer have the kicked member.
	updated, err := sess.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, testMembers(t, sess, s, "testuser").Slice(), updated.Members.Slice())

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels(), "#random")
}

func TestSession_mutationOperations_recordSpans(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, &fakeAPIClient{}, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	require.NoError(t, sess.Join(ctx, "#general"))

	seedChannelWithMembers(t, sess, s, "#leave", "testuser")
	require.NoError(t, sess.Part(ctx, "#leave", ""))

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	channel, err := sess.GetChannel(ctx, "#general")
	require.NoError(t, err)
	channel.Members.Add(botty)
	saveTestChannel(t, sess, s, channel)
	require.NoError(t, sess.Kick(ctx, "#general", "botty"))

	require.NoError(t, sess.SetTopic(ctx, "#general", "observability"))
	require.NoError(t, sess.ChangeNick(ctx, "renamed"))

	dmBot := seedInstance(t, s, instanceSpec{
		Nick:     "dm-bot",
		ModelID:  "test/dm-model",
		Channels: testChannels(),
	})
	_, _, err = sess.OpenDM(ctx, dmBot)
	require.NoError(t, err)

	require.NoError(t, sess.Reset(ctx))

	// Background goroutines (Kick / Reset dispatch) end their spans
	// asynchronously, so poll until the full expected set is present
	// rather than snapshotting once.
	expected := []string{
		"session.change_nick",
		"session.dispatch_background",
		"session.join",
		"session.kick",
		"session.open_dm",
		"session.part",
		"session.reset",
		"session.set_topic",
		"store.sqlite.append_event",
		"store.sqlite.events_before",
		"store.sqlite.get_channel",
		"store.sqlite.get_instance_by_id",
		"store.sqlite.list_channels",
		"store.sqlite.migrate_v2",
		"store.sqlite.reset",
		"store.sqlite.resolve_nick",
		"store.sqlite.save_channel",
		"store.sqlite.save_instance",
		"store.sqlite.set_autojoin_channels",
		"store.sqlite.set_last_channel",
	}

	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		ended := make(map[string]sdktrace.ReadOnlySpan)
		for _, span := range recorder.Ended() {
			ended[span.Name()] = span
		}
		assert.ElementsMatch(collect, expected, slices.Collect(maps.Keys(ended)))
	}, 2*time.Second, 10*time.Millisecond)
}

func TestSession_SendMessageAs_status_channel_records_validation_error_kind(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	sess, _ := newTestSession(t)

	err := sess.SendMessageAs(t.Context(), sess.UserInstance(), domain.StatusChannelName, "hello")
	require.Error(t, err)

	span := oteltest.FindSpan(t, recorder, "session.send_message")
	require.Equal(t, observability.ResultError, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
	require.Equal(t, observability.ErrorKindValidation, oteltest.AttrValue(span.Attributes(), observability.AttrErrorKind))
}

func TestSession_OpenDM_status_channel_records_validation_error_kind(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	sess, _ := newTestSession(t)

	ghost := domain.NewModelInstance("ghost-id", domain.Nick(domain.StatusChannelName), "test/model", "", nil)

	_, _, err := sess.OpenDM(t.Context(), ghost)
	require.Error(t, err)

	span := oteltest.FindSpan(t, recorder, "session.open_dm")
	require.Equal(t, observability.ResultError, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
	require.Equal(t, observability.ErrorKindValidation, oteltest.AttrValue(span.Attributes(), observability.AttrErrorKind))
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
				require.NoError(t, sess.Join(ctx, "#general"))
				require.NoError(t, sess.ChangeNick(ctx, "renamed"))
			},
			wantInstID: "",
		},
		{
			name:     "change_nick for model carries model's id",
			spanName: "session.change_nick",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				botty := seedInstance(t, s, instanceSpec{
					Nick:    "botty",
					ModelID: "test/model",
				})
				require.NoError(t, sess.ChangeNickAs(ctx, botty, "botty2"))
			},
			wantInstID: testMemberID("botty"),
		},
		{
			name:     "join for user carries empty id",
			spanName: "session.join",
			act: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				require.NoError(t, sess.Join(ctx, "#general"))
			},
			wantInstID: "",
		},
		{
			name:     "join for model carries model's id",
			spanName: "session.join",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				seedChannelWithMembers(t, sess, s, "#dev", "testuser")
				botty := seedInstance(t, s, instanceSpec{
					Nick:    "botty",
					ModelID: "test/model",
				})
				require.NoError(t, sess.JoinAs(ctx, botty, "#dev"))
			},
			wantInstID: testMemberID("botty"),
		},
		{
			name:     "kick carries target's id",
			spanName: "session.kick",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				seedInstance(t, s, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#dev"),
				})
				seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
				require.NoError(t, sess.Kick(ctx, "#dev", "botty"))
			},
			wantInstID: testMemberID("botty"),
		},
		{
			name:     "part for model carries model's id",
			spanName: "session.part",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				botty := seedInstance(t, s, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#dev"),
				})
				seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
				require.NoError(t, sess.PartAs(ctx, botty, "#dev", ""))
			},
			wantInstID: testMemberID("botty"),
		},
		{
			name:     "dispatch_to_instance carries the dispatched instance id",
			spanName: "session.dispatch_to_instance",
			act: func(t *testing.T, sess *Session, s *storemod.SQLiteStore, ctx context.Context) {
				t.Helper()
				seedInstance(t, s, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#general"),
				})
				seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
				_, ircMsg := seedUserMessage(t, s, "#general", "hi")
				_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
				require.NoError(t, err)
			},
			wantInstID: testMemberID("botty"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := oteltest.InstallSpanRecorder(t)
			sess, s := newTestSession(t)

			tt.act(t, sess, s, t.Context())

			span := oteltest.FindSpan(t, recorder, tt.spanName)
			require.Equal(t,
				string(tt.wantInstID),
				oteltest.AttrValue(span.Attributes(), observability.AttrInstanceID),
			)
		})
	}
}

func TestSession_DispatchToChannel_api_failure_records_dispatch_error_kind(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{}, fmt.Errorf("upstream boom")
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	_, ircMsg := seedUserMessage(t, s, "#general", "hi")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.Error(t, err)

	span := oteltest.FindSpan(t, recorder, "session.dispatch_to_channel")
	require.Equal(t, observability.ResultError, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
	require.Equal(t, observability.ErrorKindDispatch, oteltest.AttrValue(span.Attributes(), observability.AttrErrorKind))
}

func TestSession_JoinAutojoinChannels_records_aggregate_span(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#alpha", "#beta"}))

	require.NoError(t, sess.JoinAutojoinChannels(ctx))

	span := oteltest.FindSpan(t, recorder, "session.autojoin")
	require.Equal(t, "2", oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinCount))
	require.Equal(t, "0", oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinFailed))
	require.Equal(t, `["#alpha","#beta"]`, oteltest.AttrValue(span.Attributes(), observability.AttrAutojoinChannels))
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
}

func TestSession_dispatchToInstance_recordsPassReasonAndToolTurns(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	dataStore := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				PendingToolCalls: []api.PendingToolCall{{
					ID:   "call-1",
					Name: "write_memory",
					Args: mustRawJSON(t, `{"key":"topic","content":"observability"}`),
				}},
			}, nil
		},
		continueWithToolResultsFn: func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
			return api.CompletionResult{
				Response: protocol.ModelResponse{
					Kind:   protocol.ResponseSilence,
					Reason: "nothing to say",
				},
			}, nil
		},
	}
	sess := New(dataStore, memStore, fake, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	botty := seedInstance(t, dataStore, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, dataStore, "#general", "testuser", "botty")
	channel, err := dataStore.GetChannel(ctx, "#general")
	require.NoError(t, err)

	replies, err := sess.dispatchToInstance(ctx, channel, botty, "#general", nil, nil)
	require.NoError(t, err)
	require.Empty(t, replies)

	span := oteltest.FindSpan(t, recorder, "session.dispatch_to_instance")
	require.Equal(t, observability.ResultPass, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
	require.Equal(t, observability.PassReasonModelPass, oteltest.AttrValue(span.Attributes(), observability.AttrPassReason))
	require.Equal(t, "0", oteltest.AttrValue(span.Attributes(), observability.AttrRetryCount))
	require.Equal(t, "1", oteltest.AttrValue(span.Attributes(), observability.AttrToolTurnCount))
}

func TestSession_dispatchInBackground_recordsSpan(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")
	sess.dispatchInBackground(ctx, "#general", nil)

	drainEvent[domain.DispatchDoneEvent](t, sess)

	span := oteltest.FindSpan(t, recorder, "session.dispatch_background")
	require.Equal(t, "#general", oteltest.AttrValue(span.Attributes(), observability.AttrChannel))
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
}

func TestSession_SendMessage(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello world"))
	evt := drainEvent[domain.MessageEvent](t, sess)
	require.Equal(t, domain.MessageEvent{
		Event: domain.ChannelMessage{
			Channel: "#general",
			From:    "testuser",
			Body:    "hello world",
			At:      fixedTime,
		},
	}, evt)

	// Message should be persisted as a ChannelMessage event.
	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)

	// No instances, so dispatch completes immediately.
	events := drainEvents(t, sess, 1)
	require.Equal(t, []domain.SessionEvent{
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)
}

func TestSession_SendMessage_emits_dispatch_events(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("got it"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello"))

	// Drain the MessageEvent first, then the dispatch events.
	drainEvent[domain.MessageEvent](t, sess)
	events := drainEvents(t, sess, 1)

	require.Equal(t, []domain.SessionEvent{
		domain.DispatchStartedEvent{Channel: "#general", Nicks: []domain.Nick{"botty"}},
		domain.ModelReplyEvent{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "got it",
				At:         fixedTime,
			},
			At: fixedTime,
		},
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)
}

func TestSession_JoinEvent_triggers_dispatch(t *testing.T) {
	var receivedEvents []protocol.IRCMessage

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			receivedEvents = events
			return protocol.Reply("welcome"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	// Seed a channel with a model already present so join dispatch
	// has someone to notify. The user is NOT yet a member.
	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "botty")

	// Join an existing channel — the reactive dispatch should fire.
	require.NoError(t, sess.Join(ctx, "#general"))

	// Five events are expected: the synchronous JoinEvent, the
	// synchronous ModeChangeEvent emitted by emitJoinProtocol, and the
	// async DispatchStartedEvent / ModelReplyEvent / DispatchDoneEvent
	// from the dispatch goroutine triggered by the JoinEvent. Drain a
	// fixed count so we cannot return early when DispatchDoneEvent
	// arrives before emitJoinProtocol has finished emitting
	// ModeChangeEvent — that race is the bug this test pinned.
	events := drainNEvents(t, sess, 5)

	// JoinEvent is always first — it is emitted synchronously before
	// the dispatch goroutine starts. The remaining events include both
	// synchronous protocol events (ModeChangeEvent) and async dispatch
	// events. Because the dispatch goroutine races with the caller's
	// emitJoinProtocol call, ModeChangeEvent and DispatchStartedEvent
	// can appear in either order. Assert the full set and the relative
	// ordering within the dispatch lifecycle.
	require.IsType(t, domain.JoinEvent{}, events[0])
	require.Equal(t,
		domain.JoinEvent{Channel: "#general", Instance: sess.UserInstance(), At: fixedTime},
		events[0],
	)

	wantMode := domain.ModeChangeEvent{
		Channel:  "#general",
		Instance: sess.UserInstance(),
		Mode:     domain.ModeOp, Actor: "ChanServ", At: fixedTime,
	}
	wantStarted := domain.DispatchStartedEvent{
		Channel: "#general", Nicks: []domain.Nick{"botty"},
	}
	wantReply := domain.ModelReplyEvent{
		Channel:  "#general",
		Instance: botty,
		Event: domain.ChannelMessage{
			Channel:    "#general",
			From:       "botty",
			InstanceID: testMemberID("botty"),
			Body:       "welcome",
			At:         fixedTime,
		},
		At: fixedTime,
	}
	wantDone := domain.DispatchDoneEvent{Channel: "#general"}

	rest := events[1:]
	require.ElementsMatch(t,
		[]domain.SessionEvent{wantMode, wantStarted, wantReply, wantDone},
		rest,
	)

	// Dispatch lifecycle ordering: Started before Reply before Done.
	idxOf := func(target domain.SessionEvent) int {
		for i, e := range rest {
			if reflect.DeepEqual(target, e) {
				return i
			}
		}

		t.Fatalf("event %T not found", target)
		return -1
	}

	require.Less(t, idxOf(wantStarted), idxOf(wantReply), "DispatchStartedEvent must precede ModelReplyEvent")
	require.Less(t, idxOf(wantReply), idxOf(wantDone), "ModelReplyEvent must precede DispatchDoneEvent")

	// The trigger event sent to the model should be a JOIN message.
	require.Equal(t, []protocol.IRCMessage{{
		Kind:   protocol.KindJoin,
		From:   "testuser",
		Target: "#general",
		At:     fixedTime,
	}}, receivedEvents)
}

func TestSession_model_reply_does_not_retrigger_dispatch(t *testing.T) {
	var dispatchCount int

	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			dispatchCount++
			return protocol.Reply("got it"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	require.NoError(t, sess.SendMessage(ctx, "#general", "hello"))

	// Drain the MessageEvent and all dispatch events.
	drainEvent[domain.MessageEvent](t, sess)
	drainEvents(t, sess, 1)

	// Only one dispatch should have occurred — the ModelReplyEvent
	// emitted by the dispatch goroutine must not trigger another
	// dispatch.
	require.Equal(t, 1, dispatchCount)
}

func TestDispatchToInstance_excludes_own_events(t *testing.T) {
	const bottyID = "inst-botty"
	const helperID = "inst-helper"

	eventsByModel := make(map[domain.ModelID][]protocol.IRCMessage)

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, selfID domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			// Simulate what buildMessages does: exclude self-events.
			for _, e := range events {
				if selfID != "" && e.InstanceID == selfID {
					continue
				}

				eventsByModel[modelID] = append(eventsByModel[modelID], e)
			}

			return protocol.Reply("hello"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedInstance(t, s, instanceSpec{
		InstanceID: bottyID,
		Nick:       "botty",
		ModelID:    "test/model-a",
		Channels:   testChannels("#general"),
	})
	seedInstance(t, s, instanceSpec{
		InstanceID: helperID,
		Nick:       "helper",
		ModelID:    "test/model-b",
		Channels:   testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty", "helper")

	// Trigger events include a message from botty itself.
	triggerEvents := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#general", Body: "hi", At: fixedTime},
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: bottyID, Target: "#general", Body: "my own msg", At: fixedTime},
	}

	_, err := sess.DispatchToChannel(ctx, "#general", triggerEvents)
	require.NoError(t, err)

	// botty should only see alice's message, not its own.
	require.Equal(t, []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#general", Body: "hi", At: fixedTime},
	}, eventsByModel["test/model-a"])

	// helper should see alice's message, botty's original message, and
	// botty's reply (appended by the dispatch loop).
	require.Equal(t, []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#general", Body: "hi", At: fixedTime},
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: bottyID, Target: "#general", Body: "my own msg", At: fixedTime},
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: bottyID, Target: "#general", Body: "hello", At: fixedTime},
	}, eventsByModel["test/model-b"])
}

func TestDispatchToInstances_model_does_not_reply_to_self(t *testing.T) {
	const bottyID = "inst-botty"
	const helperID = "inst-helper"

	receivedByModel := make(map[domain.ModelID][]protocol.IRCMessage)

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, selfID domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			for _, e := range events {
				if selfID != "" && e.InstanceID == selfID {
					continue
				}

				receivedByModel[modelID] = append(receivedByModel[modelID], e)
			}

			if modelID == "test/model-a" {
				return protocol.Reply("first reply"), nil
			}

			return protocol.Reply("second reply"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty", "helper")
	seedInstance(t, s, instanceSpec{
		InstanceID: bottyID,
		Nick:       "botty",
		ModelID:    "test/model-a",
		Channels:   testChannels("#general"),
	})
	seedInstance(t, s, instanceSpec{
		InstanceID: helperID,
		Nick:       "helper",
		ModelID:    "test/model-b",
		Channels:   testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello everyone")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	// model-a (botty) should only see the user's message, not its own reply.
	require.Equal(t, []protocol.IRCMessage{ircMsg}, receivedByModel["test/model-a"])

	// model-b (helper) should see the user's message plus botty's reply,
	// but not its own.
	require.Equal(t, []protocol.IRCMessage{
		ircMsg,
		{Kind: protocol.KindPrivMsg, From: "botty", InstanceID: bottyID, Target: "#general", Body: "first reply", At: fixedTime},
	}, receivedByModel["test/model-b"])
}

func TestSession_DispatchToChannel_broadcasts_to_channel_instances(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("got it"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "got it", At: fixedTime},
	}, msgs)
}

func TestSession_DispatchToChannel_does_not_broadcast_when_no_model_instances(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("should not appear"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)
}

func TestSession_DispatchToChannel_pass_response_does_not_store_model_message(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{
				Kind:   protocol.ResponseSilence,
				Reason: "nothing to add",
			}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)
}

func TestSession_DispatchToChannel_reply_response_stores_model_message(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("hello back"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "hello back", At: fixedTime},
	}, msgs)
}

func TestSession_DispatchToChannel_broadcasts_only_to_members_of_that_channel(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply(fmt.Sprintf("reply from %s", modelID)), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedChannelWithMembers(t, sess, s, "#random", "testuser", "otherbot")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, instanceSpec{
		Nick:     "otherbot",
		ModelID:  "test/model-b",
		Channels: testChannels("#random"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	generalMsgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "reply from test/model-a", At: fixedTime},
	}, generalMsgs)

	randomMsgs := channelMessages(t, s, "#random")
	require.Empty(t, randomMsgs)
}

func TestSession_DispatchToChannel_reply_is_not_rebroadcast_in_same_dispatch(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("reply once"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "reply once", At: fixedTime},
	}, msgs)
}

func TestSession_DispatchToChannel_multiple_instances_each_reply_once(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply(fmt.Sprintf("reply from %s", modelID)), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "bot-a", "bot-b")
	seedInstance(t, s, instanceSpec{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, instanceSpec{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")

	require.Equal(t, domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime,
	}, msgs[0])
	require.ElementsMatch(t, []domain.ChannelMessage{
		{Channel: "#general", From: "bot-a", InstanceID: testMemberID("bot-a"), Body: "reply from test/model-a", At: fixedTime},
		{Channel: "#general", From: "bot-b", InstanceID: testMemberID("bot-b"), Body: "reply from test/model-b", At: fixedTime},
	}, msgs[1:])
}

func TestSession_DispatchToChannel_ignores_empty_reply_body(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("   "), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
	}, msgs)
}

func TestSession_DispatchToChannel_api_error_continues_to_next_instance(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if modelID == "test/model-a" {
				return protocol.ModelResponse{}, fmt.Errorf("network timeout")
			}

			return protocol.Reply("reply from bot-b"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "bot-a", "bot-b")
	seedInstance(t, s, instanceSpec{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedInstance(t, s, instanceSpec{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.Error(t, err, "should surface the API error")
	require.ErrorContains(t, err, "network timeout")

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "bot-b", InstanceID: testMemberID("bot-b"), Body: "reply from bot-b", At: fixedTime},
	}, msgs)
}

func TestSession_Poke_api_error_emits_error_event(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if modelID == "test/model-a" {
				return protocol.ModelResponse{}, fmt.Errorf("rate limited")
			}

			return protocol.Reply("still here"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "bot-a")
	seedInstance(t, s, instanceSpec{
		Nick:     "bot-a",
		ModelID:  "test/model-a",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#random", "testuser", "bot-b")
	seedInstance(t, s, instanceSpec{
		Nick:     "bot-b",
		ModelID:  "test/model-b",
		Channels: testChannels("#random"),
	})

	require.NoError(t, sess.Poke(ctx))
	events := drainEvents(t, sess, 2)

	var hasStatusNotice bool
	var hasReply bool

	for _, evt := range events {
		switch e := evt.(type) {
		case domain.SystemNoticeEvent:
			if e.Channel == domain.StatusChannelName {
				hasStatusNotice = true
			}
		case domain.ModelReplyEvent:
			hasReply = true
		}
	}

	require.True(t, hasStatusNotice, "dispatch failure should append a notice to the status channel")
	require.True(t, hasReply, "should emit a ModelReplyEvent for the successful channel")

	msgs := channelMessages(t, s, "#random")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#random", From: "bot-b", InstanceID: testMemberID("bot-b"), Body: "still here", At: fixedTime},
	}, msgs)
}

func TestSession_SetTopic(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel, Created: fixedTime}
	saveTestChannel(t, sess, s, ch)

	require.NoError(t, sess.SetTopic(ctx, "#dev", "Development Chat"))
	evt := drainEvent[domain.TopicChangeEvent](t, sess)
	require.Equal(t, domain.TopicChangeEvent{
		Channel: "#dev",
		Topic:   "Development Chat",
		By:      "testuser",
		At:      fixedTime,
	}, evt)

	// Channel topic and metadata should be updated.
	updated, err := sess.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	requireChannelEqual(t, domain.Channel{
		Name:       "#dev",
		Kind:       domain.KindChannel,
		Topic:      "Development Chat",
		TopicSetBy: "testuser",
		TopicSetAt: fixedTime,
		Created:    fixedTime,
	}, updated)
}

func TestSession_ChangeNick(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, &fakeAPIClient{}, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	// Join a channel so the nick change emits per-channel events.
	// Creating a channel emits JoinEvent, ModeChangeEvent, and a
	// DispatchDoneEvent from reactive dispatch (no models → immediate
	// done). The ModeChangeEvent and DispatchDoneEvent race.
	require.NoError(t, sess.Join(t.Context(), "#general"))
	_, extras := drainUntilMatched(t, sess,
		matchEvent[domain.JoinEvent](),
		matchEvent[domain.ModeChangeEvent](),
		matchEvent[domain.DispatchDoneEvent](),
	)
	require.Empty(t, extras)

	require.NoError(t, sess.ChangeNick(t.Context(), "newname"))
	evt := drainEvent[domain.NickChangeEvent](t, sess)
	require.Equal(t, domain.NickChangeEvent{
		Channel:  "#general",
		Instance: sess.UserInstance(),
		OldNick:  "testuser",
		NewNick:  "newname",
		At:       fixedTime,
	}, evt)

	require.Equal(t, domain.Nick("newname"), sess.UserNick())
}

func TestSession_Whois(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	inst := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "A test bot",
		Channels: testChannels("#dev"),
	})

	got, err := sess.Whois(ctx, "botty")
	require.NoError(t, err)
	require.Same(t, inst, got)
}

func TestSession_WhoisNotFound(t *testing.T) {
	sess, _ := newTestSession(t)

	_, err := sess.Whois(t.Context(), "ghost")
	require.Error(t, err)
}

func TestSession_AddModelNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, sess.AddModel(t.Context(), "#ghost", "anthropic/claude-3-haiku", ""))
}

func TestSession_InviteAs_existing_instance_to_nonexistent_channel_does_not_corrupt(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	require.Error(t, sess.InviteAs(ctx, sess.UserInstance(), "botty", "#ghost"))

	// Instance should not have the phantom channel in its set.
	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels(), "#general")
}

func TestSession_AddModelGenerateNickError(t *testing.T) {
	fake := &fakeAPIClient{
		generateNickFn: func(_ context.Context, _, _ domain.ModelID) (domain.Nick, error) {
			return "", fmt.Errorf("API unavailable")
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "testuser"),
		Created: fixedTime,
	}
	saveTestChannel(t, sess, s, ch)

	require.Error(t, sess.AddModel(ctx, "#dev", "anthropic/claude-3-haiku", ""))
}

func TestSession_AddModel_persists_persona(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	require.NoError(t, sess.AddModel(ctx, "#general", "anthropic/claude-3-haiku", "Helpful assistant"))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, "Helpful assistant", evt.Instance.Persona())

	inst, err := s.ResolveNick(ctx, "fakenick")
	require.NoError(t, err)
	require.Equal(t, "Helpful assistant", inst.Persona())
}

func TestSession_InviteAs_reuses_existing_instance(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser")
	seedChannelWithMembers(t, sess, s, "#random", "testuser")

	require.NoError(t, sess.InviteAs(ctx, sess.UserInstance(), "botty", "#random"))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, domain.ModelInvitedEvent{
		Channel:  "#random",
		Instance: botty,
		By:       "testuser",
		At:       fixedTime,
	}, evt)
	requireInstanceEqual(t, domain.NewModelInstance(
		testMemberID("botty"), "botty", "test/model", "Helpful assistant",
		testChannels("#general", "#random"),
	), evt.Instance)

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireInstanceEqual(t, domain.NewModelInstance(
		testMemberID("botty"), "botty", "test/model", "Helpful assistant",
		testChannels("#general", "#random"),
	), inst)

	channel, err := sess.GetChannel(ctx, "#random")
	require.NoError(t, err)
	require.Equal(t, testMembers(t, sess, s, "testuser", "botty").Slice(), channel.Members.Slice())
}

func TestSession_InviteAs_existing_instance_is_idempotent(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	require.NoError(t, sess.InviteAs(ctx, sess.UserInstance(), "botty", "#general"))

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels(), "#general")

	channel, err := sess.GetChannel(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, testMembers(t, sess, s, "testuser", "botty").Slice(), channel.Members.Slice())
}

func TestSession_InviteAs_existing_instance_preserves_persona(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Existing persona",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser")
	seedChannelWithMembers(t, sess, s, "#random", "testuser")

	require.NoError(t, sess.InviteAs(ctx, sess.UserInstance(), "botty", "#random"))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, "Existing persona", evt.Instance.Persona())

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	require.Equal(t, "Existing persona", inst.Persona())
}

func TestSession_AddModel_creates_new_instance_per_invocation(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")
	seedChannelWithMembers(t, sess, s, "#random", "testuser")

	require.NoError(t, sess.AddModel(ctx, "#general", "test/model", "Helpful assistant"))
	evt1 := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.NotEmpty(t, evt1.Instance.ID())
	drainEvent[domain.ModeChangeEvent](t, sess)
	drainEvent[domain.DispatchStartedEvent](t, sess)
	drainEvent[domain.DispatchDoneEvent](t, sess)

	require.NoError(t, sess.AddModel(ctx, "#random", "test/model", ""))
	evt2 := drainEvent[domain.ModelInvitedEvent](t, sess)

	// Each invocation produces a fresh `*Instance` with its own id;
	// `AddModel` no longer conflates backing model with instance
	// identity.
	require.NotEqual(t, evt1.Instance.ID(), evt2.Instance.ID())
	require.NotSame(t, evt1.Instance, evt2.Instance)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)

	ids := make([]domain.InstanceID, len(instances))
	for i, inst := range instances {
		ids[i] = inst.ID()
	}

	require.ElementsMatch(t, []domain.InstanceID{evt1.Instance.ID(), evt2.Instance.ID()}, ids)
}

func TestSession_KickNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, sess.Kick(t.Context(), "#ghost", "botty"))
}

func TestSession_KickNonMember(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	original := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, "testuser"),
		Created: fixedTime,
	}
	saveTestChannel(t, sess, s, original)

	// Kicking an unresolved nick must be a no-op: no
	// ModelKickedEvent emission (the empty-id fallback would
	// otherwise point the UI at the human user), no stored
	// membership mutation, no instance-channels mutation.
	require.NoError(t, sess.Kick(ctx, "#dev", "nobody"))

	select {
	case evt := <-sess.Events():
		t.Fatalf("unexpected event for unknown-nick kick: %T %+v", evt, evt)
	case <-time.After(50 * time.Millisecond):
	}

	updated, err := sess.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, testMembers(t, sess, s, "testuser").Slice(), updated.Members.Slice())
}

func TestSession_SetTopicNonexistentChannel(t *testing.T) {
	sess, _ := newTestSession(t)

	require.Error(t, sess.SetTopic(t.Context(), "#ghost", "topic"))
}

func TestSession_DispatchToChannel_includes_memory_in_prompt(t *testing.T) {
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	require.NoError(t, memStore.Write(t.Context(), testMemberID("botty"), memory.Entry{
		Key:     "mood",
		Content: "curious",
	}))

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, system string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if strings.Contains(system, "Your persona: Helpful assistant") &&
				strings.Contains(system, "[mood=curious]") {
				return protocol.Reply("memory and persona received"), nil
			}

			return protocol.ModelResponse{Kind: protocol.ResponseSilence}, nil
		},
	}
	s := storetest.NewMemoryStore(t)
	sess := New(s, memStore, fake, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Persona:  "Helpful assistant",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello world")

	_, err := sess.DispatchToChannel(t.Context(), "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello world", At: fixedTime},
		{Channel: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "memory and persona received", At: fixedTime},
	}, msgs)
}

func TestBuildSystemPrompt(t *testing.T) {
	sess, s := newTestSession(t)
	botty := seedInstance(t, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
		Persona: "grumpy sysadmin",
	})
	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Topic:   "go stuff",
		Members: testMembers(t, sess, s, "testuser", "botty"),
	}

	prompt := buildSystemPrompt(ch, botty, nil)

	require.Equal(t, loadGolden(t, "system_prompt.golden.txt"), prompt)
}

func TestBuildSystemPrompt_with_memories(t *testing.T) {
	ch := domain.Channel{
		Name: "#dev",
		Kind: domain.KindChannel,
	}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	memories := []memory.Entry{
		{Key: "mood", Content: "curious"},
		{Key: "goal", Content: "learn go"},
	}

	prompt := buildSystemPrompt(ch, inst, memories)

	require.Equal(t, loadGolden(t, "system_prompt_with_memories.golden.txt"), prompt)
}

func TestSession_Poke_emits_dispatch_events(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("poke received"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	require.NoError(t, sess.Poke(ctx))

	// PokeEvent is emitted via emit(), then the reactive dispatch
	// runs in the background.
	pokeEvt := drainEvent[domain.PokeEvent](t, sess)
	require.Equal(t, domain.PokeEvent{Channel: "#general", At: fixedTime}, pokeEvt)

	events := drainEvents(t, sess, 1)

	require.Equal(t, []domain.SessionEvent{
		domain.DispatchStartedEvent{Channel: "#general", Nicks: []domain.Nick{"botty"}},
		domain.ModelReplyEvent{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "poke received",
				At:         fixedTime,
			},
			At: fixedTime,
		},
		domain.DispatchDoneEvent{Channel: "#general"},
	}, events)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "botty", InstanceID: testMemberID("botty"), Body: "poke received", At: fixedTime},
	}, msgs)
}

func TestSession_OpenDM_creates_dm_channel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	ch, created, err := sess.OpenDM(ctx, botty)
	require.NoError(t, err)
	require.True(t, created)
	dmMembers := domain.NewMemberList()
	dmMembers.Add(sess.UserInstance())
	dmMembers.Add(botty)

	requireChannelEqual(t, domain.Channel{
		Name:    "botty",
		Kind:    domain.KindDM,
		Members: dmMembers,
		Created: fixedTime,
	}, ch)

	got, err := sess.GetChannel(ctx, "botty")
	require.NoError(t, err)
	requireChannelEqual(t, ch, got)

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels(), "botty")
}

func TestSession_OpenDM_reuses_existing_dm_channel(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("botty"),
	})
	existing := domain.Channel{
		Name:    "botty",
		Kind:    domain.KindDM,
		Members: testMembers(t, sess, s, "testuser", "botty"),
		Created: fixedTime.Add(-time.Hour),
	}
	saveTestChannel(t, sess, s, existing)

	ch, created, err := sess.OpenDM(ctx, botty)
	require.NoError(t, err)
	require.False(t, created)
	requireChannelEqual(t, existing, ch)

	inst, err := s.ResolveNick(ctx, "botty")
	require.NoError(t, err)
	requireChannels(t, inst.Channels(), "botty")
}

func TestSession_DispatchToChannel_dm_only_targets_that_instance(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("dm reply"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model-a",
	})
	seedInstance(t, s, instanceSpec{
		Nick:     "otherbot",
		ModelID:  "test/model-b",
		Channels: testChannels("#general"),
	})

	_, _, err := sess.OpenDM(ctx, botty)
	require.NoError(t, err)

	_, ircMsg := seedUserMessage(t, s, "botty", "hello in dm")

	_, err = sess.DispatchToChannel(ctx, "botty", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	msgs := channelMessages(t, s, "botty")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "botty", From: "testuser", Body: "hello in dm", At: fixedTime},
		{Channel: "botty", From: "botty", InstanceID: testMemberID("botty"), Body: "dm reply", At: fixedTime},
	}, msgs)
}

func TestSession_MarkRead_and_UnreadCount(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "first", At: fixedTime,
	})
	require.NoError(t, err)
	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "second", At: fixedTime,
	})
	require.NoError(t, err)

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 2, count)

	require.NoError(t, sess.MarkRead(ctx, "#general"))

	count, err = sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestSession_UnreadCount_after_new_messages(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "first", At: fixedTime,
	})
	require.NoError(t, err)

	require.NoError(t, sess.MarkRead(ctx, "#general"))

	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "second", At: fixedTime,
	})
	require.NoError(t, err)
	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "third", At: fixedTime,
	})
	require.NoError(t, err)

	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestSession_Join_marks_channel_as_read(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")
	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "old", At: fixedTime,
	})
	require.NoError(t, err)

	require.NoError(t, sess.Join(ctx, "#general"))

	// The user is already a member, so no JoinEvent is appended.
	// MarkRead clears the unread count to zero.
	count, err := sess.UnreadCount(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, 0, count)
}

func TestSession_SetAPIKey(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	initial := &fakeAPIClient{}
	replacement := &fakeAPIClient{}
	sess := New(s, nil, initial, "testuser", "", "")
	sess.SetAPIFactory(func(apiKey, _ string) (api.Client, error) {
		require.Equal(t, "test-key", apiKey)
		return replacement, nil
	})

	require.NoError(t, sess.SetAPIKey(t.Context(), "test-key", ""))
	require.Equal(t, "test-key", sess.apiKey)
	require.Same(t, replacement, sess.api)
}

func TestSession_SetAPIKey_factory_failure_keeps_existing_client(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	initial := &fakeAPIClient{}
	sess := New(s, nil, initial, "testuser", "", "")
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return nil, fmt.Errorf("boom")
	})

	err := sess.SetAPIKey(t.Context(), "test-key", "")
	require.Error(t, err)
	require.Same(t, initial, sess.api)
	require.Equal(t, "", sess.apiKey)
}

func TestSession_SetBaseURL(t *testing.T) {
	s := storetest.NewMemoryStore(t)

	var factoryBaseURL string
	factoryCalls := 0
	newClient := &fakeAPIClient{}

	sess := New(s, nil, &fakeAPIClient{}, "testuser", "test-key", "")
	sess.SetAPIFactory(func(_, baseURL string) (api.Client, error) {
		factoryCalls++
		factoryBaseURL = baseURL
		return newClient, nil
	})

	require.NoError(t, sess.SetBaseURL(t.Context(), "https://custom.example.com"))
	require.Equal(t, 1, factoryCalls)
	require.Equal(t, "https://custom.example.com", factoryBaseURL)
}

func TestSession_runtimeConfigOperations_recordSpans(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, &fakeAPIClient{}, "testuser", "test-key", "")
	sess.SetAPIFactory(func(_, _ string) (api.Client, error) {
		return &fakeAPIClient{}, nil
	})

	require.NoError(t, sess.SetAPIKey(t.Context(), "next-key", "https://openrouter.ai/api/v1"))
	sess.SetSmallModel(t.Context(), "anthropic/claude-haiku-4.5")
	require.NoError(t, sess.SetBaseURL(t.Context(), "https://custom.example.com"))

	apiKeySpan := oteltest.FindSpan(t, recorder, "session.set_api_key")
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(apiKeySpan.Attributes(), observability.AttrResult))

	smallModelSpan := oteltest.FindSpan(t, recorder, "session.set_small_model")
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(smallModelSpan.Attributes(), observability.AttrResult))
	require.Equal(t, "anthropic/claude-haiku-4.5", oteltest.AttrValue(smallModelSpan.Attributes(), observability.AttrModelID))

	baseURLSpan := oteltest.FindSpan(t, recorder, "session.set_base_url")
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(baseURLSpan.Attributes(), observability.AttrResult))
}

func TestSession_DispatchToChannel_filters_history_before_join(t *testing.T) {
	beforeJoin := fixedTime.Add(-10 * time.Minute)
	afterJoin := fixedTime.Add(10 * time.Minute)

	var receivedHistory []protocol.IRCMessage

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			receivedHistory = history
			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "pass"}, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	// Append a message event from before the model joined.
	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general",
		From:    "testuser",
		Body:    "old message",
		At:      beforeJoin,
	})
	require.NoError(t, err)

	// Append a message event from after the model joined.
	_, err = s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general",
		From:    "testuser",
		Body:    "new message",
		At:      afterJoin,
	})
	require.NoError(t, err)

	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general")})

	newEvent := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		From:   "testuser",
		Target: "#general",
		Body:   "ping",
	}
	_, err = sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{newEvent})
	require.NoError(t, err)

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
}

func TestSession_DispatchToChannel_forwards_replies_to_subsequent_models(t *testing.T) {
	// Track the events each model receives.
	eventsByModel := map[domain.ModelID][]protocol.IRCMessage{}

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			eventsByModel[modelID] = append([]protocol.IRCMessage{}, events...)

			if modelID == "test/alpha" {
				return protocol.Reply("alpha says hi"), nil
			}

			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "pass"}, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "alpha", "beta")
	seedInstance(t, s, instanceSpec{
		Nick:     "alpha",
		ModelID:  "test/alpha",
		Channels: testChannels("#general")})
	seedInstance(t, s, instanceSpec{
		Nick:     "beta",
		ModelID:  "test/beta",
		Channels: testChannels("#general")})

	userEvent := protocol.IRCMessage{
		Kind:   protocol.KindPrivMsg,
		From:   "testuser",
		Target: "#general",
		Body:   "hello everyone",
	}

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{userEvent})
	require.NoError(t, err)

	// Alpha should see only the user's message.
	require.Equal(t, []protocol.IRCMessage{userEvent}, eventsByModel["test/alpha"])

	// Beta should see the user's message AND alpha's reply.
	require.Equal(t, []protocol.IRCMessage{
		userEvent,
		{
			Kind:       protocol.KindPrivMsg,
			From:       "alpha",
			InstanceID: testMemberID("alpha"),
			Target:     "#general",
			Body:       "alpha says hi",
			At:         fixedTime,
		},
	}, eventsByModel["test/beta"])
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
	sendEventsFn              func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error)
	sendEventsFullFn          func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
	continueWithToolResultsFn func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error)
	generateNickFn            func(context.Context, domain.ModelID, domain.ModelID) (domain.Nick, error)
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
	if f.sendEventsFullFn != nil {
		return f.sendEventsFullFn(ctx, modelID, selfInstanceID, system, history, events)
	}

	if f.sendEventsFn != nil {
		response, err := f.sendEventsFn(ctx, modelID, selfInstanceID, system, history, events)
		return api.CompletionResult{Response: response}, err
	}

	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "fake"},
	}, nil
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

	return api.CompletionResult{
		Response: protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "fake"},
	}, nil
}

func (f *fakeAPIClient) GenerateNick(ctx context.Context, nickModel domain.ModelID, modelID domain.ModelID) (api.NicknameResult, error) {
	if f.generateNickFn != nil {
		nick, err := f.generateNickFn(ctx, nickModel, modelID)
		return api.NicknameResult{Nick: nick}, err
	}

	return api.NicknameResult{Nick: "fakenick"}, nil
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

func TestSession_Reset(t *testing.T) {
	s := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	sess := New(s, memStore, &fakeAPIClient{}, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	_, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "testuser", Body: "hello", At: fixedTime,
	})
	require.NoError(t, err)
	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "mood", Content: "happy"}))

	require.NoError(t, sess.Reset(ctx))

	channels, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Empty(t, instances)

	msgs := channelMessages(t, s, "#general")
	require.Empty(t, msgs)

	memories, err := memStore.Read(ctx, testMemberID("botty"))
	require.NoError(t, err)
	require.Empty(t, memories)
}

func TestSession_Reset_nil_memory_store(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser")

	require.NoError(t, sess.Reset(ctx))

	channels, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)
}

func TestBuildSystemPrompt_instructs_single_line_messages(t *testing.T) {
	ch := domain.Channel{Name: "#dev", Kind: domain.KindChannel}
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	prompt := buildSystemPrompt(ch, inst, nil)

	require.Contains(t, prompt, "Each message must be a single line with no newline characters.")
}

func TestSession_DispatchToChannel_retries_on_multiline_reply(t *testing.T) {
	attempts := make([]string, 0, 2)
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if len(attempts) == 0 {
				attempts = append(attempts, "multiline")
				return protocol.Reply("line one\nline two"), nil
			}

			attempts = append(attempts, "clean")
			return protocol.Reply("clean reply"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []string{"multiline", "clean"}, attempts)
	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "clean reply",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_drops_reply_after_max_retries(t *testing.T) {
	calls := 0
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			calls++
			return protocol.Reply("always\nmultiline"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, 3, calls)
	require.Empty(t, replies)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello", At: fixedTime},
	}, msgs)
}

func TestSession_DispatchToChannel_accepts_single_line_reply(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("no newlines here"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "no newlines here",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func newTestSessionWithMemory(t *testing.T, apiClient api.Client) (*Session, *storemod.SQLiteStore, *memory.StoreAdapter) {
	t.Helper()

	s := storetest.NewMemoryStore(t)

	m := memory.NewStoreAdapter(storetest.NewMemoryStore(t))
	sess := New(s, m, apiClient, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	return sess, s, m
}

func mustRawJSON(t *testing.T, raw string) json.RawMessage {
	t.Helper()

	return json.RawMessage(raw)
}

func mustToolResultContent(t *testing.T, payload ToolResultPayload) string {
	t.Helper()

	data, err := json.Marshal(payload)
	require.NoError(t, err)

	return string(data)
}

func TestSession_DispatchToChannel_write_memory_then_reply(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Name: "write_memory", Args: mustRawJSON(t, `{"key":"mood","content":"happy"}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("noted!"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: mustToolResultContent(t, ToolResultPayload{OK: true, Summary: `stored memory "mood"`})},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "noted!",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)

	memories, err := memStore.Read(ctx, testMemberID("botty"))
	require.NoError(t, err)
	require.Equal(t, []memory.Entry{{Key: "mood", Content: "happy"}}, memories)
}

func TestSession_DispatchToChannel_delete_memory_then_pass(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Name: "delete_memory", Args: mustRawJSON(t, `{"key":"old_stuff"}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "nothing to say"},
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "old_stuff", Content: "remove me"}))

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: mustToolResultContent(t, ToolResultPayload{OK: true, Summary: `deleted memory "old_stuff"`})},
	}, continueResults)

	memories, err := memStore.Read(ctx, testMemberID("botty"))
	require.NoError(t, err)
	require.Empty(t, memories)
}

func TestSession_DispatchToChannel_memory_write_error_returns_error_to_model(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Name: "write_memory", Args: mustRawJSON(t, `{"key":"mood","content":"happy"}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("ok anyway"),
			}, nil
		},
	}

	s := storetest.NewMemoryStore(t)
	memStore := &failingMemoryStore{writeErr: fmt.Errorf("disk full")}
	sess := New(s, memStore, fake, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: mustToolResultContent(t, ToolResultPayload{OK: false, Error: "disk full"})},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "ok anyway",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_multiple_memory_calls_in_one_response(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Name: "write_memory", Args: mustRawJSON(t, `{"key":"mood","content":"happy"}`)},
					{ID: "call_2", Name: "write_memory", Args: mustRawJSON(t, `{"key":"topic","content":"go programming"}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("stored both"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: mustToolResultContent(t, ToolResultPayload{OK: true, Summary: `stored memory "mood"`})},
		{ToolCallID: "call_2", Content: mustToolResultContent(t, ToolResultPayload{OK: true, Summary: `stored memory "topic"`})},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "stored both",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)

	memories, err := memStore.Read(ctx, testMemberID("botty"))
	require.NoError(t, err)
	require.Equal(t, []memory.Entry{
		{Key: "mood", Content: "happy"},
		{Key: "topic", Content: "go programming"},
	}, memories)
}

func TestSession_DispatchToChannel_search_memory_then_reply(t *testing.T) {
	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Name: "search_memory", Args: mustRawJSON(t, `{"query":"favourite colour","limit":5}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("your favourite colour is blue"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithMemory(t, fake)
	ctx := t.Context()

	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "colour", Content: "blue"}))

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "what is my favourite colour?")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_1", Content: mustToolResultContent(t, ToolResultPayload{OK: false, Error: "unknown tool \"search_memory\""})},
	}, continueResults)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "your favourite colour is blue",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
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

	m := memory.NewIndexedStoreFromDB(backing, chromem.NewDB(), embeddingFunc)
	sess := New(s, m, apiClient, "testuser", "", "")
	sess.now = func() time.Time { return fixedTime }

	return sess, s, m
}

func TestSession_DispatchToChannel_search_memory_with_vector_store(t *testing.T) {
	// Three topics in 3 dimensions. Querying "cats" produces [1,0,0],
	// giving each entry a distinct cosine similarity:
	//   "cats are great"    → [1,0,0] → 1.0
	//   "no keyword match"  → uniform  → 1/√3 ≈ 0.577
	//   "dogs are loyal"    → [0,1,0] → 0.0
	embSrv := newEmbeddingServer(t, 3, map[string]int{
		"cats": 0,
		"dogs": 1,
		"fish": 2,
	})

	uniformSim := float32(1.0 / math.Sqrt(3))

	var continueResults []api.ToolResult
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_1", Name: "search_memory", Args: mustRawJSON(t, `{"query":"cats","limit":3}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			continueResults = results
			return api.CompletionResult{
				Response: protocol.Reply("your favourite is cats"),
			}, nil
		},
	}

	sess, s, memStore := newTestSessionWithIndexedMemory(t, fake, embSrv.URL)
	ctx := t.Context()

	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "fav_pet", Content: "cats are great"}))
	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "hobby", Content: "no keyword match here"}))
	require.NoError(t, memStore.Write(ctx, testMemberID("botty"), memory.Entry{Key: "other_pet", Content: "dogs are loyal"}))

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "what is my favourite pet?")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	// Unmarshal the JSON content so we can assert the full search
	// results slice, then assert the full tool results wrapper too.
	var payload ToolResultPayload
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

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "your favourite is cats",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_write_then_search_memory_with_vector_store(t *testing.T) {
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
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{
				Conversation: &api.Conversation{},
				PendingToolCalls: []api.PendingToolCall{
					{ID: "call_write_cats", Name: "write_memory", Args: mustRawJSON(t, `{"key":"pet_cats","content":"cats are wonderful"}`)},
					{ID: "call_write_dogs", Name: "write_memory", Args: mustRawJSON(t, `{"key":"pet_dogs","content":"dogs are loyal"}`)},
				},
			}, nil
		},
		continueWithToolResultsFn: func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
			// First continuation receives the write results;
			// second receives the search results.
			if writeResults == nil {
				writeResults = results
				return api.CompletionResult{
					Conversation: &api.Conversation{},
					PendingToolCalls: []api.PendingToolCall{
						{ID: "call_search", Name: "search_memory", Args: mustRawJSON(t, `{"query":"cats","limit":5}`)},
					},
				}, nil
			}

			searchResults = results
			return api.CompletionResult{
				Response: protocol.Reply("noted"),
			}, nil
		},
	}

	sess, s, _ := newTestSessionWithIndexedMemory(t, fake, embSrv.URL)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []api.ToolResult{
		{ToolCallID: "call_write_cats", Content: mustToolResultContent(t, ToolResultPayload{OK: true, Summary: `stored memory "pet_cats"`})},
		{ToolCallID: "call_write_dogs", Content: mustToolResultContent(t, ToolResultPayload{OK: true, Summary: `stored memory "pet_dogs"`})},
	}, writeResults)

	var searchPayload ToolResultPayload
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

func TestSession_DispatchToChannel_memory_loop_respects_max_turns(t *testing.T) {
	// The model never calls reply/pass — just keeps writing memories
	// forever. The loop should stop after maxToolLoopTurns continue
	// calls and return no replies.
	var writtenKeys []string
	fake := &fakeAPIClient{
		sendEventsFullFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
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
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)

	// 1 initial SendEvents + maxToolLoopTurns continues = maxToolLoopTurns
	// tool result deliveries.
	require.Equal(t, []string{
		"call_init",
		"call_k1",
		"call_k2",
		"call_k3",
		"call_k4",
	}, writtenKeys)
}

func TestSession_DispatchToChannel_encodes_structured_reply_formatting(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			fg := uint8(4)
			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Messages: []protocol.ReplyPart{{
					Kind: protocol.ReplyMessage,
					Spans: []protocol.ReplySpan{
						{Text: "hello "},
						{Text: "world", Style: &protocol.ReplyStyle{Bold: true, FG: &fg}},
					},
				}},
			}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "hello \x02\x0304world\x0f",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_retries_on_invalid_structured_formatting(t *testing.T) {
	attempts := make([]string, 0, 2)
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			if len(attempts) == 0 {
				attempts = append(attempts, "invalid")
				return protocol.ModelResponse{
					Kind: protocol.ResponseReply,
					Messages: []protocol.ReplyPart{{
						Kind: protocol.ReplyMessage,
						Spans: []protocol.ReplySpan{
							{Text: "", Style: &protocol.ReplyStyle{Bold: true}},
						},
					}},
				}, nil
			}

			attempts = append(attempts, "clean")
			return protocol.Reply("clean reply"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	botty := seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Equal(t, []string{"invalid", "clean"}, attempts)
	require.Equal(t, []domain.ModelReplyEvent{
		{
			Channel:  "#general",
			Instance: botty,
			Event: domain.ChannelMessage{
				Channel:    "#general",
				From:       "botty",
				InstanceID: testMemberID("botty"),
				Body:       "clean reply",
				At:         fixedTime,
			},
			At: fixedTime,
		},
	}, replies)
}

func TestSession_DispatchToChannel_format_retry_exhaustion(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	calls := 0
	fake := &fakeAPIClient{
		sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
			calls++
			return protocol.ModelResponse{
				Kind: protocol.ResponseReply,
				Messages: []protocol.ReplyPart{{
					Kind: protocol.ReplyMessage,
					Spans: []protocol.ReplySpan{
						{Text: "", Style: &protocol.ReplyStyle{Bold: true}},
					},
				}},
			}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)

	require.Equal(t, 3, calls)
	require.Empty(t, replies)

	msgs := channelMessages(t, s, "#general")
	require.Equal(t, []domain.ChannelMessage{
		{Channel: "#general", From: "testuser", Body: "hello", At: fixedTime},
	}, msgs)

	span := oteltest.FindSpan(t, recorder, "session.dispatch_to_instance")
	require.Equal(t, observability.PassReasonFormatRetryExhausted, oteltest.AttrValue(span.Attributes(), observability.AttrPassReason))
}

func TestSession_DispatchToChannel_newline_retry_exhaustion(t *testing.T) {
	// Responses with embedded newlines also fail format validation,
	// so the format reason takes precedence when both apply. To
	// exercise the newline-specific exhaustion mapping, verify
	// passReasonForResponse separately and ensure the dispatch still
	// produces the correct observability attributes when newlines
	// are the only issue.

	t.Run("passReasonForResponse maps newline silence", func(t *testing.T) {
		resp := protocol.ModelResponse{
			Kind:   protocol.ResponseSilence,
			Reason: silenceReasonNewlineRetries,
		}
		require.Equal(t, observability.PassReasonNewlineRetryExhausted, passReasonForResponse(resp))
	})

	t.Run("dispatch with multiline body exhausts retries", func(t *testing.T) {
		recorder := oteltest.InstallSpanRecorder(t)
		calls := 0
		fake := &fakeAPIClient{
			sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (protocol.ModelResponse, error) {
				calls++
				return protocol.Reply("always\nmultiline"), nil
			},
		}
		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
		seedInstance(t, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#general"),
		})

		_, ircMsg := seedUserMessage(t, s, "#general", "hello")

		replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
		require.NoError(t, err)

		require.Equal(t, 3, calls)
		require.Empty(t, replies)

		msgs := channelMessages(t, s, "#general")
		require.Equal(t, []domain.ChannelMessage{
			{Channel: "#general", From: "testuser", Body: "hello", At: fixedTime},
		}, msgs)

		// A body with newlines also fails format validation, so
		// format takes precedence in the retry reason.
		span := oteltest.FindSpan(t, recorder, "session.dispatch_to_instance")
		require.Equal(t, observability.PassReasonFormatRetryExhausted, oteltest.AttrValue(span.Attributes(), observability.AttrPassReason))
	})
}

// seedChannelWithMembers persists a channel with the given members.
// Each member nick must either be the session user or must have been
// previously seeded via `seedInstance` so that its canonical handle
// exists in the store. If the user is listed, the session's in-memory
// `user.Channels()` and recorded user mode are updated to match, and
// the user is stripped from the member list before persistence — the
// user is an ephemeral actor that is never written to disk.
func seedChannelWithMembers(t *testing.T, sess *Session, s *storemod.SQLiteStore, name domain.ChannelName, members ...domain.Nick) {
	t.Helper()

	ch := domain.Channel{
		Name:    name,
		Kind:    domain.KindChannel,
		Members: testMembers(t, sess, s, members...),
		Created: fixedTime,
	}

	registerUserMembership(t, sess, name, members)

	ch.Members = cloneMembersWithout(ch.Members, sess.UserInstance())
	require.NoError(t, s.SaveChannel(t.Context(), ch))
}

// saveTestChannel persists a pre-built `domain.Channel` literal in
// the test-style, splitting ephemeral user membership from the on-
// disk form. If the channel's Members lists the session user, the
// session's `user.Channels()` + recorded user mode are updated to
// match, and the user is stripped from `ch.Members` before
// `s.SaveChannel` is called. Tests construct channel literals with
// the user as a member for readability; the store never sees the
// user.
func saveTestChannel(t *testing.T, sess *Session, s *storemod.SQLiteStore, ch domain.Channel) {
	t.Helper()

	user := sess.UserInstance()
	if m, ok := ch.Members.GetByInstance(user); ok {
		sess.UserInstance().MutateChannels(func(mm *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
			if _, exists := mm.Get(ch.Name); !exists {
				mm.Set(ch.Name, fixedTime)
			}
		})

		mode := m.Mode
		if mode == domain.ModeNone && ch.Kind == domain.KindChannel {
			mode = domain.ModeOp
		}

		if mode != domain.ModeNone {
			sess.setUserMode(t.Context(), ch.Name, mode)
		}

		ch.Members = cloneMembersWithout(ch.Members, user)
	}

	require.NoError(t, s.SaveChannel(t.Context(), ch))
}

// registerUserMembership updates the session's in-memory user state
// when a test seeds a channel that lists the user as a member. It
// adds the channel to `user.Channels()` and records the
// conventional ModeOp that `JoinAs` would have set on a real join.
// Tests that want a different mode can override via the internal
// `setUserMode` helper.
func registerUserMembership(t *testing.T, sess *Session, name domain.ChannelName, members []domain.Nick) {
	userNick := sess.UserNick()
	for _, m := range members {
		if m != userNick {
			continue
		}

		sess.UserInstance().MutateChannels(func(mm *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
			if _, ok := mm.Get(name); !ok {
				mm.Set(name, fixedTime)
			}
		})
		sess.setUserMode(t.Context(), name, domain.ModeOp)
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
func seedInstance(t *testing.T, s *storemod.SQLiteStore, spec instanceSpec) *domain.Instance {
	t.Helper()

	id := spec.InstanceID
	if id == "" {
		id = testMemberID(spec.Nick)
	}

	ctx := t.Context()

	if existing, err := s.GetInstanceByID(ctx, id); err == nil && existing != nil {
		// If the caller specified a ModelID that differs from the
		// existing handle (typical when `testMembers` auto-seeded a
		// placeholder under "test/model" first), drop and recreate so
		// the handle carries the correct backing model. Tests that
		// need the canonical-pointer invariant must seed before
		// referencing.
		if spec.ModelID == "" || spec.ModelID == existing.ModelID {
			existing.SetNick(spec.Nick)
			existing.SetPersona(spec.Persona)
			existing.MutateChannels(func(m *orderedmap.OrderedMap[domain.ChannelName, time.Time]) {
				for pair := m.Oldest(); pair != nil; {
					next := pair.Next()
					m.Delete(pair.Key)
					pair = next
				}
				if spec.Channels != nil {
					for pair := spec.Channels.Oldest(); pair != nil; pair = pair.Next() {
						m.Set(pair.Key, pair.Value)
					}
				}
			})
			require.NoError(t, s.SaveInstance(ctx, existing))
			return existing
		}

		require.NoError(t, s.DeleteInstanceByID(ctx, id))
	}

	inst := domain.NewModelInstance(id, spec.Nick, spec.ModelID, spec.Persona, spec.Channels)
	require.NoError(t, s.SaveInstance(ctx, inst))

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

// seedUserMessage appends a user message as a ChannelMessage event and
// returns the event and its protocol representation. Unlike
// sess.SendMessage, this does not trigger background dispatch.
func seedUserMessage(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName, body string) (domain.ChannelMessage, protocol.IRCMessage) {
	t.Helper()

	cm := domain.ChannelMessage{
		Channel: ch,
		From:    "testuser",
		Body:    body,
		At:      fixedTime,
	}

	_, err := s.AppendEvent(t.Context(), ch, cm)
	require.NoError(t, err)

	ircMsg, _ := protocol.FromChannelEvent(cm)

	return cm, ircMsg
}

// channelMessages extracts ChannelMessage events from stored events.
func channelMessages(t *testing.T, s *storemod.SQLiteStore, ch domain.ChannelName) []domain.ChannelMessage {
	t.Helper()

	events, err := s.EventsBefore(t.Context(), ch, nil, 1000)
	require.NoError(t, err)

	var msgs []domain.ChannelMessage

	for _, se := range events {
		if cm, ok := se.Event.(domain.ChannelMessage); ok {
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
		types[i] = domain.ChannelEventType(se.Event)
	}

	return types
}

func TestSession_DispatchToChannel_content_filtered_returns_silence(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{}, api.ErrContentFiltered
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)
}

func TestSession_DispatchToChannel_model_refused_returns_silence(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{}, &api.ErrModelRefused{Reason: "I cannot help with that"}
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	replies, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.NoError(t, err)
	require.Empty(t, replies)
}

func TestSession_DispatchToChannel_truncated_returns_error(t *testing.T) {
	fake := &fakeAPIClient{
		sendEventsFullFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
			return api.CompletionResult{}, api.ErrResponseTruncated
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#general", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#general"),
	})

	_, ircMsg := seedUserMessage(t, s, "#general", "hello")

	_, err := sess.DispatchToChannel(ctx, "#general", []protocol.IRCMessage{ircMsg})
	require.ErrorIs(t, err, api.ErrResponseTruncated)
}

// drainNEvents reads exactly n events from the session events channel
// and returns them in arrival order.
//
// Use this only when the test is asserting on the exact event count
// or arrival ordering. For setup-style drains, prefer
// `drainUntilMatched` so tests describe the semantic events they are
// clearing without over-coupling to the full stream shape.
//
// `drainEvents` is marker-based — it stops at the Nth
// `DispatchDoneEvent` — which is the right shape for tests that just
// need to wait for dispatch to finish, but unsafe for tests that
// combine a synchronous emit from the caller with asynchronous
// events from a dispatch goroutine triggered by an earlier emit:
// the dispatch goroutine can race ahead and emit `DispatchDoneEvent`
// before the caller has emitted its post-trigger synchronous events
// (e.g. `emitJoinProtocol`'s `ModeChangeEvent` after a
// `JoinEvent`-triggered dispatch). The marker fires, drain returns,
// and the synchronous event is queued but unobserved. A count-based
// drain blocks until the expected total arrives.
//
// Callers that want only the side-effect of clearing the channel may
// discard the return value with `_ = drainNEvents(...)`.
func drainNEvents(t *testing.T, sess *Session, n int) []domain.SessionEvent {
	t.Helper()

	events := make([]domain.SessionEvent, 0, n)

	for range n {
		select {
		case evt := <-sess.Events():
			events = append(events, evt)
		case <-time.After(time.Second):
			t.Fatalf("timed out draining events at %d/%d", len(events), n)
		}
	}

	return events
}

// drainEvents reads from the session events channel until n
// DispatchDoneEvent values have been received, and returns all
// events in order.
func drainEvents(t *testing.T, sess *Session, doneCount int) []domain.SessionEvent {
	t.Helper()

	var events []domain.SessionEvent
	done := 0

	for evt := range sess.Events() {
		events = append(events, evt)
		if _, ok := evt.(domain.DispatchDoneEvent); ok {
			done++
			if done >= doneCount {
				return events
			}
		}
	}

	t.Fatal("events channel closed before receiving all DispatchDoneEvents")

	return nil
}

func testPersonas() []domain.Persona {
	return []domain.Persona{
		{ID: "grumpy-sysadmin", Description: "Runs FreeBSD on everything.", Origin: domain.PersonaGenerated},
		{ID: "lurker-larry", Description: "Only corrects RFC citations.", Origin: domain.PersonaGenerated},
		{ID: "retro-gamer", Description: "Speedruns Doom on a toaster.", Origin: domain.PersonaGenerated},
	}
}

func TestSession_EnsurePersonas_lazy_generation(t *testing.T) {
	calls := 0
	fake := &fakeAPIClient{
		generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
			calls++
			return testPersonas(), nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	require.NoError(t, sess.EnsurePersonas(ctx))
	require.Equal(t, 1, calls)

	got, err := s.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, testPersonas(), got)

	// Second call should not generate again — pool is already populated.
	require.NoError(t, sess.EnsurePersonas(ctx))
	require.Equal(t, 1, calls)
}

func TestSession_Invite_without_persona_assigns_from_pool(t *testing.T) {
	fake := &fakeAPIClient{
		generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
			return testPersonas(), nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	require.NoError(t, sess.AddModel(ctx, "#dev", "anthropic/claude-3-haiku", ""))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)

	// Should have been assigned a persona description from the pool.
	require.NotEmpty(t, evt.Instance.Persona())

	descriptions := make(map[string]bool)
	for _, p := range testPersonas() {
		descriptions[p.Description] = true
	}

	require.True(t, descriptions[evt.Instance.Persona()],
		"assigned persona %q not in pool", evt.Instance.Persona())
}

func TestSession_Invite_with_explicit_persona_skips_pool(t *testing.T) {
	fake := &fakeAPIClient{
		generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
			t.Fatal("GeneratePersonas should not be called when persona is explicit")
			return nil, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	require.NoError(t, sess.AddModel(ctx, "#dev", "anthropic/claude-3-haiku", "Custom persona"))
	evt := drainEvent[domain.ModelInvitedEvent](t, sess)
	require.Equal(t, "Custom persona", evt.Instance.Persona())
}

func TestSession_RandomPersona(t *testing.T) {
	sess, s := newTestSessionWithAPI(t, &fakeAPIClient{})
	ctx := t.Context()

	for _, p := range testPersonas() {
		require.NoError(t, s.SavePersona(ctx, p))
	}

	got, err := sess.RandomPersona(ctx)
	require.NoError(t, err)

	ids := make(map[string]bool)
	for _, p := range testPersonas() {
		ids[p.ID] = true
	}

	require.True(t, ids[got.ID], "random persona %q not in pool", got.ID)
}

func TestSession_RandomPersona_empty_pool(t *testing.T) {
	sess, _ := newTestSessionWithAPI(t, &fakeAPIClient{})

	_, err := sess.RandomPersona(t.Context())
	require.EqualError(t, err, "no personas available")
}

func TestSession_RegeneratePersonas_preserves_user_defined(t *testing.T) {
	fake := &fakeAPIClient{
		generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
			return []domain.Persona{
				{ID: "new-gen", Description: "Freshly generated.", Origin: domain.PersonaGenerated},
			}, nil
		},
	}

	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	// Seed a user-defined persona and a generated one.
	require.NoError(t, s.SavePersona(ctx, domain.Persona{
		ID: "my-persona", Description: "User defined.", Origin: domain.PersonaUser,
	}))
	require.NoError(t, s.SavePersona(ctx, domain.Persona{
		ID: "old-gen", Description: "Old generated.", Origin: domain.PersonaGenerated,
	}))

	got, err := sess.RegeneratePersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "new-gen", Description: "Freshly generated.", Origin: domain.PersonaGenerated},
	}, got)

	// Store should have the user persona plus the new generated one.
	all, err := s.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "my-persona", Description: "User defined.", Origin: domain.PersonaUser},
		{ID: "new-gen", Description: "Freshly generated.", Origin: domain.PersonaGenerated},
	}, all)
}

func TestSession_SetPersona(t *testing.T) {
	sess, s := newTestSessionWithAPI(t, &fakeAPIClient{})
	ctx := t.Context()

	require.NoError(t, sess.SetPersona(ctx, "custom-bot", "A friendly helper."))

	got, err := s.GetPersona(ctx, "custom-bot")
	require.NoError(t, err)
	require.Equal(t, domain.Persona{
		ID:          "custom-bot",
		Description: "A friendly helper.",
		Origin:      domain.PersonaUser,
	}, got)
}

func TestSession_ResetPersonas_removes_user_keeps_generated(t *testing.T) {
	sess, s := newTestSessionWithAPI(t, &fakeAPIClient{})
	ctx := t.Context()

	require.NoError(t, s.SavePersona(ctx, domain.Persona{
		ID: "my-persona", Description: "User defined.", Origin: domain.PersonaUser,
	}))
	require.NoError(t, s.SavePersona(ctx, domain.Persona{
		ID: "gen-persona", Description: "Generated.", Origin: domain.PersonaGenerated,
	}))

	removed, err := sess.ResetPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	got, err := s.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "gen-persona", Description: "Generated.", Origin: domain.PersonaGenerated},
	}, got)
}

func TestDispatchToInstance_logs_dispatch_attributes(t *testing.T) {
	var buf logBuffer

	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.Reply("I have thoughts"), nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		InstanceID: "inst-botty",
		Nick:       "botty",
		ModelID:    "test/model-a",
		Channels:   testChannels("#dev"),
	})

	triggerEvents := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#dev", Body: "hi there", At: fixedTime},
		{Kind: protocol.KindJoin, From: "bob", Target: "#dev", At: fixedTime},
	}

	_, err := sess.DispatchToChannel(ctx, "#dev", triggerEvents)
	require.NoError(t, err)

	record := buf.find("dispatch to instance")
	require.NotNil(t, record, "expected 'dispatch to instance' log entry")

	require.Equal(t, "session", record["component"])
	require.Equal(t, "#dev", record["channel"])
	require.Equal(t, "botty", record["nick"])
	require.Equal(t, "test/model-a", record["model_id"])
	require.Equal(t, float64(2), record["trigger_count"])
	require.Equal(t, "PRIVMSG from alice; JOIN from bob", record["trigger_summary"])
	require.Equal(t, "reply", record["result"])
	require.Equal(t, "I have thoughts", record["reply_preview"])
}

func TestDispatchToInstance_logs_pass_reason(t *testing.T) {
	var buf logBuffer

	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, _ []protocol.IRCMessage) (protocol.ModelResponse, error) {
			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "nothing to say"}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", "testuser", "botty")
	seedInstance(t, s, instanceSpec{
		InstanceID: "inst-botty",
		Nick:       "botty",
		ModelID:    "test/model-a",
		Channels:   testChannels("#dev"),
	})

	triggerEvents := []protocol.IRCMessage{
		{Kind: protocol.KindPrivMsg, From: "alice", Target: "#dev", Body: "anyone?", At: fixedTime},
	}

	_, err := sess.DispatchToChannel(ctx, "#dev", triggerEvents)
	require.NoError(t, err)

	record := buf.find("dispatch to instance")
	require.NotNil(t, record, "expected 'dispatch to instance' log entry")

	require.Equal(t, "pass", record["result"])
	require.Equal(t, "nothing to say", record["reply_preview"])
}

func TestSendMessageAs_model_triggers_dispatch_to_other_models(t *testing.T) {
	dispatched := make(map[domain.ModelID][]protocol.IRCMessage)

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			dispatched[modelID] = append(dispatched[modelID], events...)
			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "ok"}, nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	alpha := seedInstance(t, s, instanceSpec{
		InstanceID: "inst-alpha",
		Nick:       "alpha",
		ModelID:    "test/model-a",
		Channels:   testChannels("#general"),
	})
	seedInstance(t, s, instanceSpec{
		InstanceID: "inst-beta",
		Nick:       "beta",
		ModelID:    "test/model-b",
		Channels:   testChannels("#general"),
	})
	seedChannelWithMembers(t, sess, s, "#general", "testuser", "alpha", "beta")

	require.NoError(t, sess.SendMessageAs(ctx, alpha, "#general", "hello from alpha"))

	// Wait for async dispatch to complete.
	drainEvent[domain.MessageEvent](t, sess)
	drainEvent[domain.DispatchStartedEvent](t, sess)
	drainEvent[domain.DispatchDoneEvent](t, sess)

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
}

func TestAddModel_dispatches_invite_notification_to_model(t *testing.T) {
	dispatched := make(map[domain.ModelID][]protocol.IRCMessage)

	fake := &fakeAPIClient{
		sendEventsFn: func(_ context.Context, modelID domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (protocol.ModelResponse, error) {
			dispatched[modelID] = append(dispatched[modelID], events...)
			return protocol.ModelResponse{Kind: protocol.ResponseSilence, Reason: "ok"}, nil
		},
		generateNickFn: func(_ context.Context, _ domain.ModelID, _ domain.ModelID) (domain.Nick, error) {
			return "botty", nil
		},
	}
	sess, s := newTestSessionWithAPI(t, fake)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	require.NoError(t, sess.AddModel(ctx, "#dev", "test/model", ""))

	drainEvent[domain.ModelInvitedEvent](t, sess)
	drainEvent[domain.ModeChangeEvent](t, sess)
	drainEvent[domain.DispatchStartedEvent](t, sess)
	drainEvent[domain.DispatchDoneEvent](t, sess)

	require.Equal(t, map[domain.ModelID][]protocol.IRCMessage{
		"test/model": {
			{
				Kind:   protocol.KindInvite,
				From:   "testuser",
				Target: "#dev",
				At:     fixedTime,
			},
		},
	}, dispatched)
}

// listModelsCountingClient records the number of `ListModels` calls so
// short-circuit tests can assert the upstream is not re-hit after a
// known failure.
type listModelsCountingClient struct {
	fakeAPIClient

	calls atomic.Int32
	err   error
	infos []api.ModelInfo
}

func (c *listModelsCountingClient) ListModels(context.Context) ([]api.ModelInfo, error) {
	c.calls.Add(1)

	if c.err != nil {
		return nil, c.err
	}

	return c.infos, nil
}

func TestSession_AddModel_short_circuits_after_ListModels_failure(t *testing.T) {
	logs := installSessionLogCapture(t)

	upstreamErr := fmt.Errorf("upstream unreachable")
	client := &listModelsCountingClient{err: upstreamErr}

	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, client, "testuser", "test-key", "")
	sess.now = func() time.Time { return fixedTime }

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	_, err := sess.ListModels(t.Context())
	require.ErrorIs(t, err, upstreamErr)
	require.Equal(t, listModelsFailed, listModelsState(sess.listModelsState.Load()))

	addErr := sess.AddModel(t.Context(), "#dev", "anthropic/claude-3-haiku", "")
	require.ErrorIs(t, addErr, ErrModelListUnavailable)

	require.Equal(t, int32(1), client.calls.Load(),
		"AddModel must short-circuit on the cached failed state and not re-hit ListModels")

	transition := logs.find("model list state transitioned")
	require.NotNil(t, transition, "expected transition log record")
	require.Equal(t, "WARN", transition["level"])
	require.Equal(t, "session", transition["component"])
	require.Equal(t, "none", transition["from"])
	require.Equal(t, "failed", transition["to"])
	require.Equal(t, upstreamErr.Error(), transition["error"])

	shortCircuit := logs.find("add-model short-circuited: model list unavailable")
	require.NotNil(t, shortCircuit, "expected short-circuit log record")
	require.Equal(t, "INFO", shortCircuit["level"])
	require.Equal(t, "session", shortCircuit["component"])
	require.Equal(t, "anthropic/claude-3-haiku", shortCircuit["model_id"])
}

// installSessionLogCapture redirects `slog.Default()` to a JSON
// `logBuffer` for the duration of the test, restoring a discard
// handler on cleanup. Mirrors the inline pattern used by
// `TestDispatchToInstance_logs_dispatch_attributes` so log assertions
// can read structured fields out of the captured records.
func installSessionLogCapture(t *testing.T) *logBuffer {
	t.Helper()

	buf := &logBuffer{}
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	return buf
}

func TestClassifyEnsureModelError(t *testing.T) {
	tests := map[string]struct {
		err  error
		want string
	}{
		"ErrModelListUnavailable": {
			err:  ErrModelListUnavailable,
			want: observability.ErrorKindClientState,
		},
		"wrapped ErrModelListUnavailable": {
			err:  fmt.Errorf("wrap: %w", ErrModelListUnavailable),
			want: observability.ErrorKindClientState,
		},
		"ErrNoAPIKey": {
			err:  ErrNoAPIKey,
			want: observability.ErrorKindClientState,
		},
		"UnsupportedModelError": {
			err:  domain.UnsupportedModelError{ModelID: "foo"},
			want: observability.ErrorKindValidation,
		},
		"wrapped UnsupportedModelError": {
			err:  fmt.Errorf("wrap: %w", domain.UnsupportedModelError{ModelID: "foo"}),
			want: observability.ErrorKindValidation,
		},
		"upstream wrapped as list models": {
			err:  fmt.Errorf("list models: %w", fmt.Errorf("transport")),
			want: observability.ErrorKindDispatch,
		},
		"unrelated error": {
			err:  fmt.Errorf("anything else"),
			want: observability.ErrorKindDispatch,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyEnsureModelError(tc.err))
		})
	}
}

func TestSession_AddModel_lazy_loads_when_state_none(t *testing.T) {
	client := &listModelsCountingClient{infos: []api.ModelInfo{{ID: "anthropic/claude-3-haiku"}}}

	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, client, "testuser", "test-key", "")
	sess.now = func() time.Time { return fixedTime }

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	require.Equal(t, listModelsNone, listModelsState(sess.listModelsState.Load()))
	require.NoError(t, sess.AddModel(t.Context(), "#dev", "anthropic/claude-3-haiku", ""))
	require.Equal(t, listModelsOK, listModelsState(sess.listModelsState.Load()))
	require.Equal(t, int32(1), client.calls.Load())
}

func TestSession_AddModel_returns_unsupported_when_model_missing_from_cache(t *testing.T) {
	client := &listModelsCountingClient{infos: []api.ModelInfo{{ID: "openai/gpt-5"}}}

	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, client, "testuser", "test-key", "")
	sess.now = func() time.Time { return fixedTime }

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	_, err := sess.ListModels(t.Context())
	require.NoError(t, err)
	require.Equal(t, listModelsOK, listModelsState(sess.listModelsState.Load()))

	addErr := sess.AddModel(t.Context(), "#dev", "anthropic/claude-3-haiku", "")
	var unsupported domain.UnsupportedModelError
	require.ErrorAs(t, addErr, &unsupported)
	require.Equal(t, domain.ModelID("anthropic/claude-3-haiku"), unsupported.ModelID)
}

func TestSession_AddModel_short_circuits_when_lazy_load_fails(t *testing.T) {
	upstreamErr := fmt.Errorf("upstream unreachable")
	client := &listModelsCountingClient{err: upstreamErr}

	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, client, "testuser", "test-key", "")
	sess.now = func() time.Time { return fixedTime }

	seedChannelWithMembers(t, sess, s, "#dev", "testuser")

	first := sess.AddModel(t.Context(), "#dev", "anthropic/claude-3-haiku", "")
	require.ErrorIs(t, first, upstreamErr,
		"first AddModel should surface the underlying upstream error from the lazy load")
	require.Equal(t, listModelsFailed, listModelsState(sess.listModelsState.Load()))

	second := sess.AddModel(t.Context(), "#dev", "anthropic/claude-3-haiku", "")
	require.ErrorIs(t, second, ErrModelListUnavailable)
	require.Equal(t, int32(1), client.calls.Load(),
		"second AddModel must short-circuit and not re-hit ListModels")
}

func TestSession_SetAPIKey_resets_listModelsState(t *testing.T) {
	client := &listModelsCountingClient{err: fmt.Errorf("upstream unreachable")}

	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, client, "testuser", "initial-key", "")
	sess.now = func() time.Time { return fixedTime }
	sess.SetAPIFactory(func(string, string) (api.Client, error) {
		return &fakeAPIClient{}, nil
	})

	_, err := sess.ListModels(t.Context())
	require.Error(t, err)
	require.Equal(t, listModelsFailed, listModelsState(sess.listModelsState.Load()))

	require.NoError(t, sess.SetAPIKey(t.Context(), "next-key", ""))
	require.Equal(t, listModelsNone, listModelsState(sess.listModelsState.Load()))
	require.False(t, sess.supportedModelsReady)
	require.Nil(t, sess.supportedModels)
}

func TestSession_Reset_clears_listModelsState(t *testing.T) {
	client := &listModelsCountingClient{err: fmt.Errorf("upstream unreachable")}

	s := storetest.NewMemoryStore(t)
	sess := New(s, nil, client, "testuser", "test-key", "")
	sess.now = func() time.Time { return fixedTime }

	_, err := sess.ListModels(t.Context())
	require.Error(t, err)
	require.Equal(t, listModelsFailed, listModelsState(sess.listModelsState.Load()))

	require.NoError(t, sess.Reset(t.Context()))
	require.Equal(t, listModelsNone, listModelsState(sess.listModelsState.Load()))
	require.False(t, sess.supportedModelsReady)
	require.Nil(t, sess.supportedModels)
}
