package store

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/set"
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
			Members: ch.Members.Slice(),
			Created: ch.Created,
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
	require.Equal(t, 3, len(got))
	require.Equal(t, ids[2], got[0].ID)
	require.Equal(t, ids[4], got[2].ID)
}

func TestSQLiteStore_EventsBefore_with_cursor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	got, err := s.EventsBefore(ctx, "#general", &ids[3], 2)
	require.NoError(t, err)
	require.Equal(t, 2, len(got))
	require.Equal(t, ids[1], got[0].ID)
	require.Equal(t, ids[2], got[1].ID)
}

func TestSQLiteStore_EventsFrom_nil_returns_earliest(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	got, err := s.EventsFrom(ctx, "#general", nil, 3)
	require.NoError(t, err)
	require.Equal(t, 3, len(got))
	require.Equal(t, ids[0], got[0].ID)
	require.Equal(t, ids[2], got[2].ID)
}

func TestSQLiteStore_EventsFrom_with_cursor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	got, err := s.EventsFrom(ctx, "#general", &ids[2], 2)
	require.NoError(t, err)
	require.Equal(t, 2, len(got))
	require.Equal(t, ids[2], got[0].ID)
	require.Equal(t, ids[3], got[1].ID)
}

func TestSQLiteStore_Events_fewer_than_requested(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	appendTestEvents(t, s, "#general", 2)

	got, err := s.EventsBefore(ctx, "#general", nil, 10)
	require.NoError(t, err)
	require.Equal(t, 2, len(got))
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

	appendTestEvents(t, s, "#alpha", 3)
	appendTestEvents(t, s, "#beta", 2)

	gotA, err := s.EventsBefore(ctx, "#alpha", nil, 10)
	require.NoError(t, err)
	require.Equal(t, 3, len(gotA))

	gotB, err := s.EventsBefore(ctx, "#beta", nil, 10)
	require.NoError(t, err)
	require.Equal(t, 2, len(gotB))
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

	inst := domain.ModelInstance{
		Nick:     "claude",
		ModelID:  "anthropic/claude-3-haiku",
		Persona:  "Helpful assistant",
		Channels: set.NewOrdered[domain.ChannelName]("#general", "#dev"),
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

	inst := domain.ModelInstance{Nick: "temp", ModelID: "test/model"}
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.DeleteInstance(ctx, "temp"))

	_, err := s.GetInstance(ctx, "temp")
	require.Error(t, err)
}

func TestSQLiteStore_ListInstances(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	instances := []domain.ModelInstance{
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
	require.NoError(t, s.SaveInstance(ctx, domain.ModelInstance{
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

func TestSQLiteStore_Reset_empty_store(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.Reset(t.Context()))
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
