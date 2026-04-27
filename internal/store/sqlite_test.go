package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
)

var testTime = time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)

// storeTestMembers builds a MemberList for tests by constructing a
// synthetic model Instance per nick and persisting it to the store.
// Persisting is required because `GetChannel` resolves stub member
// references against the `instances` table; a channel saved with
// unpersisted members would have those members dropped as dead
// references on the next load.
func storeTestMembers(t *testing.T, s *SQLiteStore, nicks ...domain.Nick) domain.MemberList {
	t.Helper()

	ml := domain.NewMemberList()
	for _, nick := range nicks {
		inst := domain.NewModelInstance(
			domain.InstanceID("inst-"+string(nick)),
			nick,
			"test/model",
			"",
			nil,
		)
		require.NoError(t, s.SaveInstance(t.Context(), inst))
		ml.Add(inst)
	}

	return ml
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

func normaliseChannels(channels []domain.Channel) []comparableChannel {
	out := make([]comparableChannel, len(channels))

	for i, ch := range channels {
		out[i] = comparableChannel{
			Name:       ch.Name,
			Kind:       ch.Kind,
			Topic:      ch.Topic,
			TopicSetBy: ch.TopicSetBy,
			TopicSetAt: ch.TopicSetAt,
			Members:    ch.Members.Slice(),
			Created:    ch.Created,
		}
	}

	return out
}

func requireChannelsEqual(t *testing.T, expected, actual []domain.Channel) {
	t.Helper()

	require.Equal(t, normaliseChannels(expected), normaliseChannels(actual))
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

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()

	db, err := sql.Open("sqlite3", SQLitePragmaDSN(":memory:"))
	require.NoError(t, err)

	s, err := NewSQLiteStore(t.Context(), db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

// TestNewSQLiteStore_sets_pragmas exercises the connection-time
// PRAGMAs against a file-backed database so the WAL switch is
// observable. The DSN configures `busy_timeout`, `journal_mode`, and
// `foreign_keys` on every connection the pool opens, so the
// assertions hold regardless of pool size — verified by sampling two
// distinct connections from the same `*sql.DB`. The shared
// `:memory:` test store reports `journal_mode = "memory"` instead
// because there is no on-disk file to journal.
func TestNewSQLiteStore_sets_pragmas(t *testing.T) {
	ctx := t.Context()

	db, err := sql.Open("sqlite3", SQLitePragmaDSN(filepath.Join(t.TempDir(), "pragmas.db")))
	require.NoError(t, err)

	s, err := NewSQLiteStore(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	type pragmas struct {
		JournalMode string
		BusyTimeout int
		ForeignKeys int
	}

	readPragmas := func(t *testing.T, conn *sql.Conn) pragmas {
		t.Helper()
		var p pragmas
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&p.JournalMode))
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&p.BusyTimeout))
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&p.ForeignKeys))
		return p
	}

	c1, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c1.Close() })

	c2, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c2.Close() })

	want := pragmas{JournalMode: "wal", BusyTimeout: 5000, ForeignKeys: 1}
	require.Equal(t, want, readPragmas(t, c1))
	require.Equal(t, want, readPragmas(t, c2))
}

// --- Channels ---

func TestSQLiteStore_ListChannelsEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListChannels(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SaveAndGetChannel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{
		Name:    "#general",
		Kind:    domain.KindChannel,
		Topic:   "General chat",
		Members: storeTestMembers(t, s, "alice", "bob"),
		Created: testTime,
	}

	require.NoError(t, s.SaveChannel(ctx, ch))

	got, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	requireChannelsEqual(t, []domain.Channel{ch}, []domain.Channel{got})
}

func TestSQLiteStore_SaveChannel_recordsSpan(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)
	ctx := t.Context()
	s := newTestStore(t).WithTracerProvider(provider)

	ch := domain.Channel{
		Name:    "#observability",
		Kind:    domain.KindChannel,
		Members: storeTestMembers(t, s, "alice"),
		Created: testTime,
	}

	require.NoError(t, s.SaveChannel(ctx, ch))

	span := oteltest.FindSpan(t, recorder, "store.sqlite.save_channel")
	require.Equal(t, "store.sqlite.save_channel", oteltest.AttrValue(span.Attributes(), observability.AttrOperation))
	require.Equal(t, "#observability", oteltest.AttrValue(span.Attributes(), observability.AttrChannel))
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
}

func TestSQLiteStore_GetChannelNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetChannel(t.Context(), "#nonexistent")
	require.ErrorIs(t, err, ErrNoSuchChannel)
}

func TestSQLiteStore_ListChannels(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	channels := []domain.Channel{
		{Name: "#alpha", Kind: domain.KindChannel, Created: testTime},
		{Name: "#beta", Kind: domain.KindChannel, Created: testTime.Add(time.Hour)},
	}

	for _, ch := range channels {
		require.NoError(t, s.SaveChannel(ctx, ch))
	}

	got, err := s.ListChannels(ctx)
	require.NoError(t, err)
	requireChannelsEqual(t, channels, got)
}

func TestSQLiteStore_ListChannels_includes_dms(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	channels := []domain.Channel{
		{Name: "#alpha", Kind: domain.KindChannel, Created: testTime},
		{Name: "botty", Kind: domain.KindDM, Created: testTime.Add(time.Hour)},
	}

	for _, ch := range channels {
		require.NoError(t, s.SaveChannel(ctx, ch))
	}

	got, err := s.ListChannels(ctx)
	require.NoError(t, err)
	requireChannelsEqual(t, channels, got)
}

func TestSQLiteStore_SaveAndGetWindow_status(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	want := domain.NewStatusWindow(testTime)
	require.NoError(t, s.SaveWindow(ctx, want))

	got, err := s.GetWindow(ctx, domain.StatusChannelName)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestSQLiteStore_SaveAndGetWindow_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	want := domain.NewChannelWindow("#general", testTime)
	want.Topic = "welcome"
	want.TopicSetBy = "alice"
	want.TopicSetAt = testTime

	require.NoError(t, s.SaveWindow(ctx, want))

	got, err := s.GetWindow(ctx, "#general")
	require.NoError(t, err)
	cw, ok := got.(*domain.ChannelWindow)
	require.True(t, ok)
	require.Equal(t, want.Name(), cw.Name())
	require.Equal(t, want.Topic, cw.Topic)
	require.Equal(t, want.TopicSetBy, cw.TopicSetBy)
	require.Equal(t, want.TopicSetAt, cw.TopicSetAt)
}

func TestSQLiteStore_SaveAndGetWindow_dm(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	bot := domain.NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)
	require.NoError(t, s.SaveInstance(ctx, bot))

	want := domain.NewDMWindow(bot, testTime)
	require.NoError(t, s.SaveWindow(ctx, want))

	got, err := s.GetWindow(ctx, domain.ChannelName(bot.ID()))
	require.NoError(t, err)
	dm, ok := got.(*domain.DMWindow)
	require.True(t, ok)
	require.Equal(t, domain.ChannelName("id-1"), dm.Name())
	require.Same(t, bot, dm.Counterpart)
}

func TestSQLiteStore_ListWindows_mixed(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	bot := domain.NewModelInstance("id-1", "botty", "anthropic/claude-3-haiku", "", nil)
	require.NoError(t, s.SaveInstance(ctx, bot))

	require.NoError(t, s.SaveWindow(ctx, domain.NewStatusWindow(testTime)))
	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#general", testTime.Add(time.Hour))))
	require.NoError(t, s.SaveWindow(ctx, domain.NewDMWindow(bot, testTime.Add(2*time.Hour))))

	got, err := s.ListWindows(ctx)
	require.NoError(t, err)

	kinds := make([]domain.ChannelKind, 0, len(got))
	for _, w := range got {
		kinds = append(kinds, w.Kind())
	}
	require.ElementsMatch(t, []domain.ChannelKind{domain.KindStatus, domain.KindChannel, domain.KindDM}, kinds)
}

func TestSQLiteStore_DeleteWindow(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#general", testTime)))
	require.NoError(t, s.DeleteWindow(ctx, "#general"))

	_, err := s.GetWindow(ctx, "#general")
	require.ErrorIs(t, err, ErrNoSuchChannel)
}

func TestSQLiteStore_DeleteChannel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{Name: "#deleteme", Kind: domain.KindChannel, Created: testTime}
	require.NoError(t, s.SaveChannel(ctx, ch))
	require.NoError(t, s.DeleteChannel(ctx, "#deleteme"))

	_, err := s.GetChannel(ctx, "#deleteme")
	require.Error(t, err)
}

func TestSQLiteStore_SaveChannelOverwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{Name: "#evolving", Kind: domain.KindChannel, Created: testTime}
	require.NoError(t, s.SaveChannel(ctx, ch))

	ch.Topic = "Updated topic"
	ch.Members = storeTestMembers(t, s, "charlie")
	require.NoError(t, s.SaveChannel(ctx, ch))

	got, err := s.GetChannel(ctx, "#evolving")
	require.NoError(t, err)
	requireChannelsEqual(t, []domain.Channel{ch}, []domain.Channel{got})
}

func TestSQLiteStore_SaveAndGetChannelWithTopicMetadata(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{
		Name:       "#dev",
		Kind:       domain.KindChannel,
		Topic:      "Go development",
		TopicSetBy: "alice",
		TopicSetAt: testTime,
		Members:    storeTestMembers(t, s, "alice"),
		Created:    testTime,
	}

	require.NoError(t, s.SaveChannel(ctx, ch))

	got, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)
	requireChannelsEqual(t, []domain.Channel{ch}, []domain.Channel{got})
}

// --- Event log ---

func TestSQLiteStore_AppendAndReadEvent(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	event := domain.Join{
		Target: "#general",
		Nick:   "alice",
		At:     testTime,
	}

	id, err := s.AppendEvent(ctx, "#general", event)
	require.NoError(t, err)
	require.Greater(t, id, int64(0))

	got, err := s.EventsBefore(ctx, "#general", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{
		{ID: id, Event: event},
	}, got)
}

func TestSQLiteStore_EventsBefore_nil_returns_latest(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	got, err := s.EventsBefore(ctx, "#general", nil, 3)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}

	require.Equal(t, []int64{ids[2], ids[3], ids[4]}, gotIDs)
}

func TestSQLiteStore_EventsBefore_with_cursor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	got, err := s.EventsBefore(ctx, "#general", &ids[3], 2)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}

	require.Equal(t, []int64{ids[1], ids[2]}, gotIDs)
}

func TestSQLiteStore_EventsFrom_nil_returns_earliest(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	got, err := s.EventsFrom(ctx, "#general", nil, 3)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}

	require.Equal(t, []int64{ids[0], ids[1], ids[2]}, gotIDs)
}

func TestSQLiteStore_EventsFrom_with_cursor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	got, err := s.EventsFrom(ctx, "#general", &ids[2], 2)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}

	require.Equal(t, []int64{ids[2], ids[3]}, gotIDs)
}

func TestSQLiteStore_Events_fewer_than_requested(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 2)

	got, err := s.EventsBefore(ctx, "#general", nil, 10)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}

	require.Equal(t, []int64{ids[0], ids[1]}, gotIDs)
}

func TestSQLiteStore_Events_empty_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	got, err := s.EventsBefore(ctx, "#empty", nil, 10)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_Events_isolated_by_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	alphaIDs := appendTestEvents(t, s, "#alpha", 3)
	betaIDs := appendTestEvents(t, s, "#beta", 2)

	gotA, err := s.EventsBefore(ctx, "#alpha", nil, 10)
	require.NoError(t, err)

	gotAIDs := make([]int64, len(gotA))
	for i, e := range gotA {
		gotAIDs[i] = e.ID
	}

	require.Equal(t, []int64{alphaIDs[0], alphaIDs[1], alphaIDs[2]}, gotAIDs)

	gotB, err := s.EventsBefore(ctx, "#beta", nil, 10)
	require.NoError(t, err)

	gotBIDs := make([]int64, len(gotB))
	for i, e := range gotB {
		gotBIDs[i] = e.ID
	}

	require.Equal(t, []int64{betaIDs[0], betaIDs[1]}, gotBIDs)
}

func TestSQLiteStore_Events_type_discriminator_round_trip(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	events := []domain.PersistableEvent{
		domain.Message{Target: "#general", From: "alice", Body: "hello", At: testTime},
		domain.Join{Target: "#general", Nick: "bob", At: testTime},
		domain.Part{Target: "#general", Nick: "bob", At: testTime},
		domain.TopicChange{Target: "#general", Topic: "new", By: "alice", At: testTime},
		domain.ModeChange{Target: "#general", Nick: "bob", Mode: domain.ModeVoice, By: "ChanServ", At: testTime},
		domain.ModelInvited{Target: "#general", Nick: "botty", By: "alice", At: testTime},
		domain.ModelKicked{Target: "#general", Nick: "botty", By: "alice", At: testTime},
		domain.NickChange{Target: "#general", OldNick: "bob", NewNick: "robert", At: testTime},
	}

	for _, e := range events {
		_, err := s.AppendEvent(ctx, "#general", e)
		require.NoError(t, err)
	}

	got, err := s.EventsFrom(ctx, "#general", nil, 100)
	require.NoError(t, err)

	gotEvents := make([]domain.PersistableEvent, len(got))
	for i, se := range got {
		gotEvents[i] = se.Event
	}

	require.Equal(t, events, gotEvents)
}

// --- Model instances ---

func TestSQLiteStore_ListInstancesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListInstances(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SaveAndGetInstance(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	channels := orderedmap.New[domain.ChannelName, time.Time]()
	channels.Set("#general", testTime)
	channels.Set("#dev", testTime)

	inst := domain.NewModelInstance(
		"inst-claude",
		"claude",
		"anthropic/claude-3-haiku",
		"Helpful assistant",
		channels,
	)

	require.NoError(t, s.SaveInstance(ctx, inst))

	byID, err := s.GetInstanceByID(ctx, "inst-claude")
	require.NoError(t, err)
	require.Equal(t, normaliseInstance(inst), normaliseInstance(byID))

	// The store returns the canonical pointer — the saved handle
	// itself — so later callers observe the same pointer identity.
	require.Same(t, inst, byID)

	viaNick, err := s.ResolveNick(ctx, "claude")
	require.NoError(t, err)
	require.Same(t, inst, viaNick)
}

func TestSQLiteStore_ResolveNick_not_found(t *testing.T) {
	s := newTestStore(t)

	_, err := s.ResolveNick(t.Context(), "ghost")
	require.ErrorIs(t, err, ErrNoSuchNick)
}

func TestSQLiteStore_GetInstanceByIDNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetInstanceByID(t.Context(), "inst-ghost")
	require.Error(t, err)
}

func TestSQLiteStore_DeleteInstanceByID(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.NewModelInstance("inst-temp", "temp", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-temp"))

	_, err := s.GetInstanceByID(ctx, "inst-temp")
	require.Error(t, err)
}

func TestSQLiteStore_registry_canonical_pointer_across_reloads(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.NewModelInstance("inst-keep", "keep", "test/model", "original", nil)
	require.NoError(t, s.SaveInstance(ctx, inst))

	// A subsequent GetInstanceByID returns the same handle.
	got, err := s.GetInstanceByID(ctx, "inst-keep")
	require.NoError(t, err)
	require.Same(t, inst, got)

	// The session is authoritative for the live state of every
	// registered instance; the store row is a save-time snapshot.
	// A second SaveInstance from a shadow handle updates the row
	// but does not touch the cached handle — reloading returns the
	// original pointer with its original state.
	shadow := domain.NewModelInstance("inst-keep", "renamed", "test/model", "updated", nil)
	require.NoError(t, s.SaveInstance(ctx, shadow))

	refreshed, err := s.GetInstanceByID(ctx, "inst-keep")
	require.NoError(t, err)
	require.Same(t, inst, refreshed)
	require.Equal(t, domain.Nick("keep"), refreshed.Nick())
	require.Equal(t, "original", refreshed.Persona())
}

func TestSQLiteStore_GetChannel_drops_dead_member_references(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	members := storeTestMembers(t, s, "alice", "bob")

	ch := domain.Channel{
		Name:    "#dev",
		Kind:    domain.KindChannel,
		Members: members,
		Created: testTime,
	}
	require.NoError(t, s.SaveChannel(ctx, ch))

	// Delete bob's backing instance. The channel membership record
	// still references inst-bob but no instance row remains.
	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-bob"))

	got, err := s.GetChannel(ctx, "#dev")
	require.NoError(t, err)

	// The surviving member is alice; bob's stub is dropped. Compare
	// the nick snapshots so the assertion doesn't depend on pointer
	// identity of the canonical handles.
	gotNicks := make([]domain.Nick, 0, got.Members.Len())
	for _, m := range got.Members.All() {
		gotNicks = append(gotNicks, m.Nick)
	}

	require.Equal(t, []domain.Nick{"alice"}, gotNicks)
}

func TestSQLiteStore_ListInstances(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	instances := []*domain.Instance{
		domain.NewModelInstance("inst-a", "a", "model/a", "", nil),
		domain.NewModelInstance("inst-b", "b", "model/b", "", nil),
	}

	for _, inst := range instances {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}

	got, err := s.ListInstances(ctx)
	require.NoError(t, err)
	// Normalise through the snapshot helper so the comparison
	// operates on display fields rather than pointer internals.
	wantNorm := make([]comparableInstance, len(instances))
	for i, inst := range instances {
		wantNorm[i] = normaliseInstance(inst)
	}

	gotNorm := make([]comparableInstance, len(got))
	for i, inst := range got {
		gotNorm[i] = normaliseInstance(inst)
	}

	require.Equal(t, wantNorm, gotNorm)

	// Store guarantees canonical pointer identity across calls; the
	// second invocation returns the same handles.
	got2, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Equal(t, len(got), len(got2))
	for i := range got {
		require.Same(t, got[i], got2[i])
	}
}

// --- Last channel state ---

func TestSQLiteStore_GetLastChannelEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetLastChannel(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SetAndGetLastChannel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastChannel(ctx, "#general"))

	got, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#general"), got)
}

func TestSQLiteStore_SetLastChannelOverwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastChannel(ctx, "#first"))
	require.NoError(t, s.SetLastChannel(ctx, "#second"))

	got, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#second"), got)
}

func TestSQLiteStore_GetLastReadEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetLastRead(t.Context(), "#general")
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}

func seedChannelWithEvent(t *testing.T, s *SQLiteStore, ch domain.ChannelName) int64 {
	t.Helper()
	ctx := t.Context()

	require.NoError(t, s.SaveChannel(ctx, domain.Channel{Name: ch, Created: testTime}))

	id, err := s.AppendEvent(ctx, ch, domain.Join{
		Target: ch, Nick: "testuser", At: testTime,
	})
	require.NoError(t, err)

	return id
}

func TestSQLiteStore_SetAndGetLastRead(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	eventID := seedChannelWithEvent(t, s, "#general")
	require.NoError(t, s.SetLastRead(ctx, "#general", eventID))

	got, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, eventID, got)
}

func TestSQLiteStore_SetLastRead_independent_per_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	id1 := seedChannelWithEvent(t, s, "#general")
	id2 := seedChannelWithEvent(t, s, "#random")

	require.NoError(t, s.SetLastRead(ctx, "#general", id1))
	require.NoError(t, s.SetLastRead(ctx, "#random", id2))

	g, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, id1, g)

	r, err := s.GetLastRead(ctx, "#random")
	require.NoError(t, err)
	require.Equal(t, id2, r)
}

func TestSQLiteStore_SetLastRead_overwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	seedChannelWithEvent(t, s, "#general")
	// Append a second event to get a different ID.
	id2, err := s.AppendEvent(ctx, "#general", domain.Message{
		Target: "#general", From: "alice", Body: "hello", At: testTime,
	})
	require.NoError(t, err)

	require.NoError(t, s.SetLastRead(ctx, "#general", 1))
	require.NoError(t, s.SetLastRead(ctx, "#general", id2))

	got, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, id2, got)
}

// --- Reset ---

func TestSQLiteStore_Reset(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveChannel(ctx, domain.Channel{
		Name: "#general", Kind: domain.KindChannel, Created: testTime,
	}))
	eventID, err := s.AppendEvent(ctx, "#general", domain.Join{
		Target: "#general", Nick: "alice", At: testTime,
	})
	require.NoError(t, err)
	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
	))
	require.NoError(t, s.SetLastChannel(ctx, "#general"))
	require.NoError(t, s.SetLastRead(ctx, "#general", eventID))

	require.NoError(t, s.Reset(ctx))

	channels, err := s.ListChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, channels)

	events, err := s.EventsBefore(ctx, "#general", nil, 10)
	require.NoError(t, err)
	require.Empty(t, events)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Empty(t, instances)

	lastCh, err := s.GetLastChannel(ctx)
	require.NoError(t, err)
	require.Empty(t, lastCh)

	lastRead, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Empty(t, lastRead)
}

func TestSQLiteStore_SessionActive_empty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetSessionActive(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SessionActive_round_trip(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, "2026-04-16T09:00:00Z"))

	got, err := s.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Equal(t, "2026-04-16T09:00:00Z", got)
}

func TestSQLiteStore_SessionActive_clear(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, "2026-04-16T09:00:00Z"))
	require.NoError(t, s.ClearSessionActive(ctx))

	got, err := s.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_NewSQLiteStore_purges_legacy_pending_quit(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	// Pre-create the schema and seed a stale pending_quit row.
	_, err = db.Exec(`CREATE TABLE state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO state (key, value) VALUES ('pending_quit', 'stale-blob')`)
	require.NoError(t, err)

	s, err := NewSQLiteStore(t.Context(), db)
	require.NoError(t, err)

	var got string
	err = db.QueryRow(`SELECT value FROM state WHERE key = 'pending_quit'`).Scan(&got)
	require.ErrorIs(t, err, sql.ErrNoRows, "legacy pending_quit row should be purged on store open")

	// Other state untouched.
	require.NoError(t, s.SetSessionActive(t.Context(), "ok"))
	v, err := s.GetSessionActive(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ok", v)
}

func TestSQLiteStore_NewSQLiteStore_drops_legacy_instance_tables(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	// Seed the v1 nick-keyed shape of `instances` and `memories`
	// plus one row in each so we can confirm that the drop happens
	// rather than an in-place column rename or a failed CREATE.
	_, err = db.Exec(`CREATE TABLE instances (
	    nick TEXT PRIMARY KEY,
	    data TEXT NOT NULL
	)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO instances (nick, data) VALUES ('legacy', '{}')`)
	require.NoError(t, err)

	_, err = db.Exec(`CREATE TABLE memories (
	    nick    TEXT NOT NULL,
	    key     TEXT NOT NULL,
	    content TEXT NOT NULL,
	    PRIMARY KEY (nick, key)
	)`)
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO memories (nick, key, content) VALUES ('legacy', 'k', 'v')`)
	require.NoError(t, err)

	_, err = NewSQLiteStore(t.Context(), db)
	require.NoError(t, err)

	// The legacy row must be gone; schema v2 is identified by the
	// presence of the `instance_id` column on `instances` and on
	// `memories`.
	assertColumnPresent(t, db, "instances", "instance_id")
	assertColumnPresent(t, db, "memories", "instance_id")

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM instances`).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count, "legacy instances rows should be dropped")

	err = db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count, "legacy memories rows should be dropped")
}

func TestSQLiteStore_NewSQLiteStore_preserves_v2_instances(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.SetMaxOpenConns(1)

	// Open once to create the v2 schema and seed a row.
	s, err := NewSQLiteStore(t.Context(), db)
	require.NoError(t, err)

	seed := domain.NewModelInstance("inst-keep", "keep", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(t.Context(), seed))

	// Reopen: the migration detector must see the v2 shape and leave
	// data alone.
	_, err = NewSQLiteStore(t.Context(), db)
	require.NoError(t, err)

	got, err := s.GetInstanceByID(t.Context(), "inst-keep")
	require.NoError(t, err)
	require.Equal(t, normaliseInstance(seed), normaliseInstance(got))
}

// assertColumnPresent checks that a given column exists on a table.
func assertColumnPresent(t *testing.T, db *sql.DB, table, column string) {
	t.Helper()

	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rows.Close() })

	var found bool
	for rows.Next() {
		var (
			cid     int
			name    string
			colType string
			notNull int
			dflt    sql.NullString
			pk      int
		)

		require.NoError(t, rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk))
		if name == column {
			found = true
		}
	}

	require.NoError(t, rows.Err())
	require.True(t, found, "table %q is missing column %q", table, column)
}

func TestSQLiteStore_SessionActive_overwrite(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	require.NoError(t, s.SetSessionActive(ctx, "first"))
	require.NoError(t, s.SetSessionActive(ctx, "second"))

	got, err := s.GetSessionActive(ctx)
	require.NoError(t, err)
	require.Equal(t, "second", got)
}

func TestSQLiteStore_Reset_empty_store(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.Reset(t.Context()))
}

// --- Personas ---

func TestSQLiteStore_ListPersonasEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListPersonas(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SaveAndGetPersona(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	p := domain.Persona{
		ID:          "grumpy-sysadmin",
		Description: "A grumpy sysadmin who has seen it all.",
		Origin:      domain.PersonaGenerated,
	}

	require.NoError(t, s.SavePersona(ctx, p))

	got, err := s.GetPersona(ctx, "grumpy-sysadmin")
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestSQLiteStore_GetPersonaNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetPersona(t.Context(), "ghost")
	require.Error(t, err)
}

func TestSQLiteStore_SavePersona_upsert(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	original := domain.Persona{
		ID:          "the-optimist",
		Description: "Always looks on the bright side.",
		Origin:      domain.PersonaGenerated,
	}

	require.NoError(t, s.SavePersona(ctx, original))

	updated := domain.Persona{
		ID:          "the-optimist",
		Description: "Relentlessly positive.",
		Origin:      domain.PersonaUser,
	}

	require.NoError(t, s.SavePersona(ctx, updated))

	got, err := s.GetPersona(ctx, "the-optimist")
	require.NoError(t, err)
	require.Equal(t, updated, got)
}

func TestSQLiteStore_ListPersonas_ordered(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	personas := []domain.Persona{
		{ID: "alpha", Description: "First", Origin: domain.PersonaUser},
		{ID: "beta", Description: "Second", Origin: domain.PersonaGenerated},
		{ID: "gamma", Description: "Third", Origin: domain.PersonaGenerated},
	}

	for _, p := range personas {
		require.NoError(t, s.SavePersona(ctx, p))
	}

	got, err := s.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, personas, got)
}

func TestSQLiteStore_DeletePersonasByOrigin(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	personas := []domain.Persona{
		{ID: "gen-one", Description: "Generated one", Origin: domain.PersonaGenerated},
		{ID: "gen-two", Description: "Generated two", Origin: domain.PersonaGenerated},
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
	}

	for _, p := range personas {
		require.NoError(t, s.SavePersona(ctx, p))
	}

	require.NoError(t, s.DeletePersonasByOrigin(ctx, domain.PersonaGenerated))

	got, err := s.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
	}, got)
}

func TestSQLiteStore_ReplaceGeneratedPersonas(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	initial := []domain.Persona{
		{ID: "gen-one", Description: "Generated one", Origin: domain.PersonaGenerated},
		{ID: "gen-two", Description: "Generated two", Origin: domain.PersonaGenerated},
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
	}

	for _, p := range initial {
		require.NoError(t, s.SavePersona(ctx, p))
	}

	replacements := []domain.Persona{
		{ID: "new-a", Description: "New A", Origin: domain.PersonaGenerated},
		{ID: "new-b", Description: "New B", Origin: domain.PersonaGenerated},
		{ID: "new-c", Description: "New C", Origin: domain.PersonaGenerated},
	}

	require.NoError(t, s.ReplaceGeneratedPersonas(ctx, replacements))

	got, err := s.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
		{ID: "new-a", Description: "New A", Origin: domain.PersonaGenerated},
		{ID: "new-b", Description: "New B", Origin: domain.PersonaGenerated},
		{ID: "new-c", Description: "New C", Origin: domain.PersonaGenerated},
	}, got)
}

func TestSQLiteStore_DeletePersonasByOrigin_noop_when_none(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.DeletePersonasByOrigin(t.Context(), domain.PersonaGenerated))
}

func TestSQLiteStore_Reset_includes_personas(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SavePersona(ctx, domain.Persona{
		ID: "test", Description: "Test persona", Origin: domain.PersonaUser,
	}))

	require.NoError(t, s.Reset(ctx))

	got, err := s.ListPersonas(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

// --- Autojoin ---

func TestSQLiteStore_ListAutojoinChannels_empty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListAutojoinChannels(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SetAndListAutojoinChannels(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general", "#dev"}))

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev", "#general"}, got)
}

func TestSQLiteStore_SetAutojoinChannels_replaces(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#old"}))
	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#new-a", "#new-b"}))

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#new-a", "#new-b"}, got)
}

func TestSQLiteStore_SetAutojoinChannels_empty(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general"}))
	require.NoError(t, s.SetAutojoinChannels(ctx, nil))

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SetAutojoinChannels_duplicates_ignored(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general", "#general", "#dev"}))

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#dev", "#general"}, got)
}

func TestSQLiteStore_Reset_includes_autojoin(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general"}))
	require.NoError(t, s.Reset(ctx))

	got, err := s.ListAutojoinChannels(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_Reset_rollback_on_partial_failure(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveChannel(ctx, domain.Channel{
		Name: "#general", Kind: domain.KindChannel, Created: testTime,
	}))
	eventID, err := s.AppendEvent(ctx, "#general", domain.Join{
		Target: "#general", Nick: "alice", At: testTime,
	})
	require.NoError(t, err)
	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
	))
	require.NoError(t, s.SetLastChannel(ctx, "#general"))
	require.NoError(t, s.SetLastRead(ctx, "#general", eventID))
	require.NoError(t, s.SavePersona(ctx, domain.Persona{
		ID:          "grumpy-sysadmin",
		Description: "A grumpy sysadmin who has seen it all.",
		Origin:      domain.PersonaGenerated,
	}))
	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general"}))

	// `memories` is one of the tables Reset deletes from; dropping it
	// after seeding the others guarantees the fifth DELETE in Reset
	// fails with "no such table", which must roll back the prior four
	// DELETEs in the same transaction.
	before := snapshotPersistentTables(t, s.db)

	_, err = s.db.ExecContext(ctx, `DROP TABLE memories`)
	require.NoError(t, err)

	require.Error(t, s.Reset(ctx))

	// Re-run the production `schema` constant so the snapshot helper
	// can read `memories` back to the same empty shape it had
	// pre-Reset, without changing what we are asserting on. Re-using
	// the production schema (rather than restating the table inline)
	// means the test cannot silently drift from the real definition;
	// the `IF NOT EXISTS` clauses make the re-exec a no-op for the
	// other seven tables.
	_, err = s.db.ExecContext(ctx, schema)
	require.NoError(t, err)

	after := snapshotPersistentTables(t, s.db)
	require.Equal(t, before, after)
}

// --- Helpers ---

func appendTestEvents(t *testing.T, s *SQLiteStore, ch domain.ChannelName, n int) []int64 {
	t.Helper()

	ids := make([]int64, n)

	for i := range n {
		event := domain.Message{
			Target: ch,
			From:   "alice",
			Body:   "message",
			At:     testTime.Add(time.Duration(i) * time.Second),
		}

		id, err := s.AppendEvent(t.Context(), ch, event)
		require.NoError(t, err)

		ids[i] = id
	}

	return ids
}

// snapshotPersistentTables returns a deterministic dump of every row
// in every table that `Reset` deletes from. The result is keyed by
// table name, and rows within a table are sorted lexicographically by
// their stringified column values so the comparison is stable across
// SQLite's row order.
func snapshotPersistentTables(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()

	queries := map[string]string{
		"last_read": `SELECT * FROM last_read`,
		"channels":  `SELECT * FROM channels`,
		"events":    `SELECT * FROM events`,
		"instances": `SELECT * FROM instances`,
		"memories":  `SELECT * FROM memories`,
		"personas":  `SELECT * FROM personas`,
		"state":     `SELECT * FROM state`,
		"autojoin":  `SELECT * FROM autojoin`,
	}

	out := make(map[string][]string, len(queries))

	for table, query := range queries {
		out[table] = dumpTable(t, db, query)
	}

	return out
}

func dumpTable(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()

	rows, err := db.QueryContext(t.Context(), query)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	require.NoError(t, err)

	var dump []string

	for rows.Next() {
		raw := make([]any, len(cols))
		ptrs := make([]any, len(cols))

		for i := range raw {
			ptrs[i] = &raw[i]
		}

		require.NoError(t, rows.Scan(ptrs...))

		parts := make([]string, len(cols))
		for i, name := range cols {
			parts[i] = name + "=" + stringify(raw[i])
		}

		dump = append(dump, strings.Join(parts, "|"))
	}

	require.NoError(t, rows.Err())

	sort.Strings(dump)

	return dump
}

// stringify assumes every column carries TEXT or INTEGER data; if the
// schema gains a typed time/blob column, extend the switch.
func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}
