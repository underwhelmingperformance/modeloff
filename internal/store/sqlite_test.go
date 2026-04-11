package store

import (
	"database/sql"
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

func storeTestMembers(nicks ...domain.Nick) domain.MemberList {
	ml := domain.NewMemberList()
	for _, nick := range nicks {
		ml.Add(nick)
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

func normaliseInstance(inst domain.Instance) comparableInstance {
	var channels []channelEntry

	if inst.Channels != nil {
		for pair := inst.Channels.Oldest(); pair != nil; pair = pair.Next() {
			channels = append(channels, channelEntry{Name: pair.Key, JoinedAt: pair.Value})
		}
	}

	return comparableInstance{
		Nick:     inst.Nick,
		ModelID:  inst.ModelID,
		Persona:  inst.Persona,
		Channels: channels,
	}
}

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)

	s, err := NewSQLiteStore(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
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
		Members: storeTestMembers("alice", "bob"),
		Created: testTime,
	}

	require.NoError(t, s.SaveChannel(ctx, ch))

	got, err := s.GetChannel(ctx, "#general")
	require.NoError(t, err)
	requireChannelsEqual(t, []domain.Channel{ch}, []domain.Channel{got})
}

func TestSQLiteStore_SaveChannel_recordsSpan(t *testing.T) {
	recorder := oteltest.InstallSpanRecorder(t)
	ctx := t.Context()
	s := newTestStore(t)

	ch := domain.Channel{
		Name:    "#observability",
		Kind:    domain.KindChannel,
		Members: storeTestMembers("alice"),
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
	require.Error(t, err)
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
	ch.Members = storeTestMembers("charlie")
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
		Members:    storeTestMembers("alice"),
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

	event := domain.ChannelJoin{
		Channel: "#general",
		Nick:    "alice",
		At:      testTime,
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

	events := []domain.ChannelEvent{
		domain.ChannelMessage{Channel: "#general", From: "alice", Body: "hello", At: testTime},
		domain.ChannelJoin{Channel: "#general", Nick: "bob", At: testTime},
		domain.ChannelPart{Channel: "#general", Nick: "bob", At: testTime},
		domain.ChannelTopicChange{Channel: "#general", Topic: "new", By: "alice", At: testTime},
		domain.ChannelModeChange{Channel: "#general", Nick: "bob", Mode: domain.ModeVoice, At: testTime},
		domain.ChannelModelInvited{Channel: "#general", Nick: "botty", ModelID: "test/model", At: testTime},
		domain.ChannelModelKicked{Channel: "#general", Nick: "botty", At: testTime},
		domain.ChannelNickChange{Channel: "#general", OldNick: "bob", NewNick: "robert", At: testTime},
	}

	for _, e := range events {
		_, err := s.AppendEvent(ctx, "#general", e)
		require.NoError(t, err)
	}

	got, err := s.EventsFrom(ctx, "#general", nil, 100)
	require.NoError(t, err)

	gotEvents := make([]domain.ChannelEvent, len(got))
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

	inst := domain.Instance{
		Nick:     "claude",
		ModelID:  "anthropic/claude-3-haiku",
		Persona:  "Helpful assistant",
		Channels: channels,
	}

	require.NoError(t, s.SaveInstance(ctx, inst))

	got, err := s.GetInstance(ctx, "claude")
	require.NoError(t, err)
	require.Equal(t, normaliseInstance(inst), normaliseInstance(got))
}

func TestSQLiteStore_GetInstanceNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetInstance(t.Context(), "ghost")
	require.Error(t, err)
}

func TestSQLiteStore_DeleteInstance(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.Instance{Nick: "temp", ModelID: "test/model"}
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.DeleteInstance(ctx, "temp"))

	_, err := s.GetInstance(ctx, "temp")
	require.Error(t, err)
}

func TestSQLiteStore_ListInstances(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	instances := []domain.Instance{
		{Nick: "a", ModelID: "model/a"},
		{Nick: "b", ModelID: "model/b"},
	}

	for _, inst := range instances {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}

	got, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Equal(t, instances, got)
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

	id, err := s.AppendEvent(ctx, ch, domain.ChannelJoin{
		Channel: ch, Nick: "testuser", At: testTime,
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
	id2, err := s.AppendEvent(ctx, "#general", domain.ChannelMessage{
		Channel: "#general", From: "alice", Body: "hello", At: testTime,
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
	eventID, err := s.AppendEvent(ctx, "#general", domain.ChannelJoin{
		Channel: "#general", Nick: "alice", At: testTime,
	})
	require.NoError(t, err)
	require.NoError(t, s.SaveInstance(ctx, domain.Instance{
		Nick: "botty", ModelID: "test/model",
	}))
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

func TestSQLiteStore_PendingQuit_round_trip(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	pq := domain.PendingQuit{
		Nick:     "testuser",
		Message:  "see ya",
		At:       time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
		Channels: []domain.ChannelName{"#general", "#random"},
	}

	require.NoError(t, s.SavePendingQuit(ctx, pq))

	got, err := s.GetPendingQuit(ctx)
	require.NoError(t, err)
	require.Equal(t, &pq, got)
}

func TestSQLiteStore_PendingQuit_get_when_none(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetPendingQuit(t.Context())
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSQLiteStore_PendingQuit_clear(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	pq := domain.PendingQuit{
		Nick:     "testuser",
		At:       time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
		Channels: []domain.ChannelName{"#general"},
	}

	require.NoError(t, s.SavePendingQuit(ctx, pq))
	require.NoError(t, s.ClearPendingQuit(ctx))

	got, err := s.GetPendingQuit(ctx)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSQLiteStore_PendingQuit_overwrite(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()

	pq1 := domain.PendingQuit{
		Nick:     "testuser",
		Message:  "first",
		At:       time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC),
		Channels: []domain.ChannelName{"#general"},
	}

	pq2 := domain.PendingQuit{
		Nick:     "testuser",
		Message:  "second",
		At:       time.Date(2025, 6, 15, 13, 0, 0, 0, time.UTC),
		Channels: []domain.ChannelName{"#random"},
	}

	require.NoError(t, s.SavePendingQuit(ctx, pq1))
	require.NoError(t, s.SavePendingQuit(ctx, pq2))

	got, err := s.GetPendingQuit(ctx)
	require.NoError(t, err)
	require.Equal(t, &pq2, got)
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

// --- Helpers ---

func appendTestEvents(t *testing.T, s *SQLiteStore, ch domain.ChannelName, n int) []int64 {
	t.Helper()

	ids := make([]int64, n)

	for i := range n {
		event := domain.ChannelMessage{
			Channel: ch,
			From:    "alice",
			Body:    "message",
			At:      testTime.Add(time.Duration(i) * time.Second),
		}

		id, err := s.AppendEvent(t.Context(), ch, event)
		require.NoError(t, err)

		ids[i] = id
	}

	return ids
}
