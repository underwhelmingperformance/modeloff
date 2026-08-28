package store

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestSQLiteStore_pruneEvents_trims_channel_to_headroom pins the
// per-channel half of retention: a channel carrying more than
// eventRetentionHeadroom events is trimmed down to exactly that many,
// keeping the newest ones.
func TestSQLiteStore_pruneEvents_trims_channel_to_headroom(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#dev", testTime)))

	extra := 137
	ids := appendTestEvents(t, s, "#dev", eventRetentionHeadroom+extra)

	require.NoError(t, s.pruneEvents(ctx))

	count, err := s.CountEventsFrom(ctx, "#dev", nil)
	require.NoError(t, err)
	require.Equal(t, eventRetentionHeadroom, count)

	got, err := s.EventsBefore(ctx, "#dev", nil, eventRetentionHeadroom)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, ids[extra:], gotIDs, "the newest eventRetentionHeadroom events survive")
}

func TestSQLiteStore_pruneEvents_trims_each_actor_scrollback(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		channel = domain.ChannelName("#dev")
		actor   = domain.InstanceID("inst-botty")
		extra   = 37
	)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow(channel, testTime)))
	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(actor, "botty", "test/model", "", nil)))

	records := make([]ChannelScrollbackRecord, 0, eventRetentionHeadroom+extra)
	for i := range eventRetentionHeadroom + extra {
		records = append(records, ChannelScrollbackRecord{
			InstanceID: actor,
			Channel:    channel,
			Event: domain.Message{Source: domain.LegacyClientSource(

				"alice"), Target: channel, Body: string(rune(i)), At: testTime.Add(time.Duration(i) * time.Second)},
		})
	}

	ids, err := s.AppendChannelScrollback(ctx, records)
	require.NoError(t, err)
	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.ChannelScrollback(ctx, actor, channel, eventRetentionHeadroom+extra)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, event := range got {
		gotIDs[i] = event.ID
	}
	require.Equal(t, ids[extra:], gotIDs)
}

func TestSQLiteStore_pruneEvents_trims_each_private_reply_window(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		actor = domain.InstanceID("inst-botty")
		extra = 37
	)

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(actor, "botty", "test/model", "", nil)))

	globalIDs := make([]int64, eventRetentionHeadroom+extra)
	channelIDs := make([]int64, eventRetentionHeadroom+extra)
	for i := range globalIDs {
		globalID, err := s.AppendInstanceReply(ctx, actor, nil, domain.SystemNotice{
			Text: "reply",
			At:   testTime.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
		globalIDs[i] = globalID

		channelID, err := s.AppendInstanceReply(ctx, actor,
			protocol.ChannelWindowTarget("#dev"), domain.SystemNotice{
				Target: "#dev", Text: "channel reply",
				At: testTime.Add(time.Duration(i) * time.Second),
			})
		require.NoError(t, err)
		channelIDs[i] = channelID
	}

	require.NoError(t, s.pruneEvents(ctx))

	global, err := s.InstanceRepliesForWindowBefore(ctx, actor, nil, nil, len(globalIDs))
	require.NoError(t, err)
	channel, err := s.InstanceRepliesForWindowBefore(ctx, actor,
		protocol.ChannelWindowTarget("#dev"), nil, len(channelIDs))
	require.NoError(t, err)

	globalGot := make([]int64, len(global))
	for i, event := range global {
		globalGot[i] = event.ID
	}
	channelGot := make([]int64, len(channel))
	for i, event := range channel {
		channelGot[i] = event.ID
	}
	require.Equal(t, globalIDs[extra:], globalGot)
	require.Equal(t, channelIDs[extra:], channelGot)
}

func TestSQLiteStore_pruneEvents_removes_model_turns_outside_actor_visibility(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	live := domain.NewModelInstance("inst-live", "live", "test/model", "", nil)
	live.JoinChannel("#dev", testTime)
	departed := domain.NewModelInstance("inst-departed", "departed", "test/model", "", nil)
	invited := domain.NewModelInstance("inst-invited", "invited", "test/model", "", nil)
	peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
	for _, inst := range []*domain.Instance{live, departed, invited, peer} {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}

	window := domain.NewChannelWindow("#dev", testTime)
	window.Members.Add(live)
	window.Invitations.Add(invited.ID())
	require.NoError(t, s.SaveWindow(ctx, window))

	type turnFixture struct {
		actor  domain.InstanceID
		window protocol.WindowTarget
	}
	fixtures := []turnFixture{
		{actor: live.ID(), window: protocol.ChannelWindowTarget("#dev")},
		{actor: departed.ID(), window: protocol.ChannelWindowTarget("#dev")},
		{actor: invited.ID(), window: protocol.ChannelWindowTarget("#dev")},
		{actor: live.ID(), window: protocol.ChannelWindowTarget("#gone")},
		{actor: live.ID(), window: protocol.DirectWindowTarget(peer.ID())},
		{actor: live.ID(), window: protocol.DirectWindowTarget("inst-gone")},
		{actor: live.ID(), window: protocol.DirectWindowTarget("")},
		{actor: "inst-gone", window: protocol.DirectWindowTarget(peer.ID())},
	}
	for _, fixture := range fixtures {
		_, err := s.BeginModelTurn(ctx, ModelTurn{
			InstanceID: fixture.actor,
			Window:     fixture.window,
			ModelID:    "test/model",
			StartedAt:  testTime,
		}, ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime})
		require.NoError(t, err)
	}

	require.NoError(t, s.pruneEvents(ctx))

	got := dumpTable(t, s.db, `
		SELECT instance_id, window_kind, window_key
		FROM model_turns
		ORDER BY instance_id, window_kind, window_key
	`)
	require.Equal(t, []string{
		"instance_id=inst-invited|window_kind=1|window_key=#dev",
		"instance_id=inst-live|window_kind=1|window_key=#dev",
		"instance_id=inst-live|window_kind=2|window_key=",
		"instance_id=inst-live|window_kind=2|window_key=inst-peer",
	}, got)

	entries := dumpTable(t, s.db, `SELECT count(*) FROM model_turn_entries`)
	require.Equal(t, []string{"count(*)=4"}, entries)
}

func TestSQLiteStore_BeginModelTurn_bounds_live_actor_journal(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	actor := domain.NewModelInstance("inst-live", "live", "test/model", "", nil)
	actor.JoinChannel("#dev", testTime)
	actor.JoinChannel("#ops", testTime)
	require.NoError(t, s.SaveInstance(ctx, actor))

	for _, channel := range []domain.ChannelName{"#dev", "#ops"} {
		window := domain.NewChannelWindow(channel, testTime)
		window.Members.Add(actor)
		require.NoError(t, s.SaveWindow(ctx, window))
	}

	const extra = 3
	turnIDs := make([]ModelTurnID, 0, modelTurnRetentionHeadroom+extra)
	for i := range modelTurnRetentionHeadroom + extra {
		channel := domain.ChannelName("#dev")
		if i%2 != 0 {
			channel = "#ops"
		}
		turnID, err := s.BeginModelTurn(ctx, ModelTurn{
			InstanceID: actor.ID(),
			Window:     protocol.ChannelWindowTarget(channel),
			ModelID:    actor.ModelID,
			StartedAt:  testTime.Add(time.Duration(i) * time.Second),
		}, ModelTurnEntry{
			Kind: ModelTurnInput,
			Data: fmt.Appendf(nil, `{"turn":%d}`, i),
			At:   testTime.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
		turnIDs = append(turnIDs, turnID)
	}

	rows, err := queryRows(ctx, s.db, `SELECT id FROM model_turns ORDER BY id`, nil,
		scalarColumn[ModelTurnID]())
	require.NoError(t, err)
	require.Equal(t, turnIDs[extra:], rows)

	entries := dumpTable(t, s.db, `
		SELECT turn_id, kind, data
		FROM model_turn_entries
		ORDER BY turn_id
	`)
	wantEntries := make([]string, 0, modelTurnRetentionHeadroom)
	for i, turnID := range turnIDs[extra:] {
		wantEntries = append(wantEntries, fmt.Sprintf(
			"turn_id=%d|kind=input|data={\"turn\":%d}", turnID, i+extra,
		))
	}
	sort.Strings(wantEntries)
	require.Equal(t, wantEntries, entries)
}

func TestSQLiteStore_modelTurnRetention_counts_UTF8_bytes(t *testing.T) {
	type trimFunc func(context.Context, *SQLiteStore, domain.InstanceID) error

	tests := []struct {
		name string
		trim trimFunc
	}{
		{
			name: "live admission",
			trim: func(ctx context.Context, s *SQLiteStore, actor domain.InstanceID) error {
				return trimModelTurns(ctx, s.db, actor, 500, 85)
			},
		},
		{
			name: "startup retention",
			trim: func(ctx context.Context, s *SQLiteStore, _ domain.InstanceID) error {
				_, err := s.pruneModelTurnsTo(ctx, 500, 85)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			s := newTestStore(t)
			const actor = domain.InstanceID("inst-live")

			payload := []byte(`"éééééééééééééééééééé"`)

			for i := range 3 {
				_, err := s.BeginModelTurn(ctx, ModelTurn{
					InstanceID: actor,
					Window:     protocol.DirectWindowTarget("inst-peer"),
					ModelID:    "test/model",
					StartedAt:  testTime.Add(time.Duration(i) * time.Second),
				}, ModelTurnEntry{
					Kind: ModelTurnInput,
					Data: payload,
					At:   testTime.Add(time.Duration(i) * time.Second),
				})
				require.NoError(t, err)
			}

			require.NoError(t, test.trim(ctx, s, actor))

			turns, err := queryRows(ctx, s.db, `SELECT id FROM model_turns ORDER BY id`, nil,
				scalarColumn[ModelTurnID]())
			require.NoError(t, err)
			entries := dumpTable(t, s.db, `
				SELECT turn_id, data FROM model_turn_entries ORDER BY turn_id
			`)
			type assertionSnapshot struct {
				Turns   []ModelTurnID
				Entries []string
			}

			require.Equal(t, assertionSnapshot{
				Turns: []ModelTurnID{2, 3},
				Entries: []string{
					`turn_id=2|data="éééééééééééééééééééé"`,
					`turn_id=3|data="éééééééééééééééééééé"`,
				},
			}, assertionSnapshot{
				Turns: turns, Entries: entries,
			})
		})
	}
}

func TestSQLiteStore_DeleteInstanceByID_removes_private_replies(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const actor = domain.InstanceID("inst-gone")

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(actor, "gone", "test/model", "", nil)))
	_, err := s.AppendInstanceReply(ctx, actor, nil, domain.SystemNotice{Text: "before quit", At: testTime})
	require.NoError(t, err)

	require.NoError(t, s.DeleteInstanceByID(ctx, actor))

	got, err := s.InstanceRepliesBefore(ctx, actor, nil, 10)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_DeleteInstanceByID_removes_replies_from_its_DM_peers(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, peer))

	_, err := s.AppendInstanceReply(ctx, "", protocol.DirectWindowTarget(peer.ID()),
		domain.SystemNotice{Text: "private DM reply", At: testTime})
	require.NoError(t, err)
	require.NoError(t, s.DeleteInstanceByID(ctx, peer.ID()))

	got, err := s.InstanceRepliesForWindowBefore(
		ctx, "", protocol.DirectWindowTarget(peer.ID()), nil, 10,
	)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_DeleteInstanceByID_preserves_user_private_replies(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	user := domain.NewUserInstance("alice")
	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	replies := []domain.IssuerReply{
		domain.SystemNotice{Text: "first", At: testTime},
		domain.SystemNotice{Text: "second", At: testTime.Add(time.Second)},
	}

	for _, inst := range []*domain.Instance{user, botty} {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}
	records := make([]InstanceReplyRecord, 0, len(replies))
	for _, reply := range replies {
		id, err := s.AppendInstanceReply(ctx, user.ID(), protocol.ChannelWindowTarget("#dev"), reply)
		require.NoError(t, err)
		records = append(records, InstanceReplyRecord{
			ID: id, Window: protocol.ChannelWindowTarget("#dev"), Event: reply,
		})
	}
	turnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: botty.ID(),
		Window:     protocol.DirectWindowTarget(user.ID()),
		ModelID:    botty.ModelID,
		StartedAt:  testTime,
	}, ModelTurnEntry{
		Kind: ModelTurnInput,
		Data: []byte(`{"input":"private"}`),
		At:   testTime,
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteInstanceByID(ctx, user.ID()))

	reopened, err := NewSQLiteStore(ctx, s.db)
	require.NoError(t, err)
	gotReplies, err := reopened.InstanceRepliesBefore(ctx, user.ID(), nil, 10)
	require.NoError(t, err)
	gotTurnEntries, err := reopened.ModelTurnEntries(ctx, turnID)
	require.NoError(t, err)
	type assertionSnapshot struct {
		Replies     []InstanceReplyRecord
		TurnEntries []ModelTurnEntry
	}

	require.Equal(t, assertionSnapshot{
		Replies: records,
	}, assertionSnapshot{
		Replies: gotReplies, TurnEntries: gotTurnEntries,
	})
}

func TestSQLiteStore_pruneEvents_removes_quit_actor_scrollback(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		channel = domain.ChannelName("#dev")
		actor   = domain.InstanceID("inst-gone")
	)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow(channel, testTime)))
	_, err := s.AppendChannelScrollback(ctx, []ChannelScrollbackRecord{{
		InstanceID: actor,
		Channel:    channel,
		Event: domain.Message{Source: domain.LegacyClientSource(

			"gone"), Target: channel, Body: "before quit", At: testTime},
	}})
	require.NoError(t, err)
	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.ChannelScrollback(ctx, actor, channel, 10)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestSQLiteStore_pruneEvents_leaves_a_channel_under_headroom_alone
// pins the no-op case: a channel that never grew past the headroom
// keeps every one of its events.
func TestSQLiteStore_pruneEvents_leaves_a_channel_under_headroom_alone(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#dev", testTime)))
	ids := appendTestEvents(t, s, "#dev", 12)

	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.EventsBefore(ctx, "#dev", nil, 100)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, ids, gotIDs)
}

// TestSQLiteStore_pruneEvents_deletes_events_for_a_deleted_channel
// covers the scenario DeleteWindow's own doc comment files here: the
// channel's row (every spelling of it) is gone, but its events
// outlive it because the log is keyed by channel name under BINARY
// and nothing reads those rows again. Retention removes them.
func TestSQLiteStore_pruneEvents_deletes_events_for_a_deleted_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	// "#gone" was never (re)created after the events below were
	// logged: there is no row for it in channels at all.
	appendTestEvents(t, s, "#gone", 5)

	require.NoError(t, s.pruneEvents(ctx))

	count, err := s.CountEventsFrom(ctx, "#gone", nil)
	require.NoError(t, err)
	require.Zero(t, count)
}

// TestSQLiteStore_pruneEvents_deletes_orphaned_spelling_of_a_live_channel
// covers a database written before the server casemapped channel
// names: "#Dev" and "#dev" exist as two separate live rows in
// channels. GetWindow always answers such a pair with the same one
// (the BINARY-smaller spelling), so events logged under the other
// spelling are unreachable even though the channel itself is alive.
func TestSQLiteStore_pruneEvents_deletes_orphaned_spelling_of_a_live_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	for _, name := range []domain.ChannelName{"#Dev", "#dev"} {
		require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow(name, testTime)))
	}

	canonical, err := s.GetWindow(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#Dev"), canonical.Name(), "the BINARY-smaller spelling wins")

	keptIDs := appendTestEvents(t, s, "#Dev", 3)
	appendTestEvents(t, s, "#dev", 4)

	require.NoError(t, s.pruneEvents(ctx))

	orphanCount, err := s.CountEventsFrom(ctx, "#dev", nil)
	require.NoError(t, err)
	require.Zero(t, orphanCount, "the loser spelling's own events are gone")

	got, err := s.EventsBefore(ctx, "#Dev", nil, 100)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, keptIDs, gotIDs, "the canonical spelling's own events are untouched")
}

// TestSQLiteStore_pruneEvents_deletes_dm_events_for_a_deleted_peer
// covers the DM counterpart to
// TestSQLiteStore_pruneEvents_deletes_events_for_a_deleted_channel:
// DeleteInstanceByID evicts the peer's own row from instances but
// leaves its DM message rows behind (they are keyed by instance id,
// not by a foreign key SQLite could cascade through), and instance
// ids are never reused, so nothing will ever address that thread
// again. Retention removes them the same way it removes an orphaned
// channel's events.
func TestSQLiteStore_pruneEvents_deletes_dm_events_for_a_deleted_peer(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const bottyID domain.InstanceID = "inst-botty"
	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(bottyID, "botty", "test/model", "", nil)))

	_, err := s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
		"iain"), Target: domain.ChannelName(bottyID), Body: "hi", At: testTime})
	require.NoError(t, err)

	_, err = s.AppendEvent(ctx, "", domain.Message{Source: domain.ClientSource(
		bottyID, "botty"), Target: "", Body: "hello", At: testTime.Add(time.Second)})
	require.NoError(t, err)

	require.NoError(t, s.DeleteInstanceByID(ctx, bottyID))

	require.NoError(t, s.pruneEvents(ctx))

	count, err := s.CountDMEventsFrom(ctx, "", bottyID, nil)
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestSQLiteStore_pruneEvents_deletes_both_directions_for_a_deleted_model_peer(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	alice := domain.NewModelInstance("inst-alice", "alice", "test/model", "", nil)
	bob := domain.NewModelInstance("inst-bob", "bob", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, alice))
	require.NoError(t, s.SaveInstance(ctx, bob))

	_, err := s.AppendEvent(ctx, domain.ChannelName(bob.ID()), domain.Message{Source: domain.ClientSource(

		alice.ID(), alice.Nick()), Target: domain.ChannelName(bob.ID()), Body: "from alice", At: testTime})
	require.NoError(t, err)
	_, err = s.AppendEvent(ctx, domain.ChannelName(alice.ID()), domain.Message{Source: domain.ClientSource(

		bob.ID(), bob.Nick()), Target: domain.ChannelName(alice.ID()), Body: "from bob", At: testTime.Add(time.Second)})
	require.NoError(t, err)

	require.NoError(t, s.DeleteInstanceByID(ctx, alice.ID()))
	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.DMEventsBefore(ctx, alice.ID(), bob.ID(), nil, 100)
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestSQLiteStore_pruneEvents_leaves_dm_events_alone pins that a DM
// message's channel value (a bare InstanceID or the empty string,
// never a "#"/"&" prefix) is never mistaken for an orphaned channel
// spelling and swept by pruneOrphanChannelEvents.
func TestSQLiteStore_pruneEvents_leaves_dm_events_alone(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const bottyID domain.InstanceID = "inst-botty"
	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(bottyID, "botty", "test/model", "", nil)))

	toBotty, err := s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
		"iain"), Target: domain.ChannelName(bottyID), Body: "hi", At: testTime})
	require.NoError(t, err)

	toUser, err := s.AppendEvent(ctx, "", domain.Message{Source: domain.ClientSource(
		bottyID, "botty"), Target: "", Body: "hello", At: testTime.Add(time.Second)})
	require.NoError(t, err)

	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.DMEventsBefore(ctx, "", bottyID, nil, 100)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, []int64{toBotty, toUser}, gotIDs)
}

// TestSQLiteStore_pruneEvents_trims_dm_thread_to_headroom pins the
// DM half of retention: a thread whose two directions together carry
// more than eventRetentionHeadroom messages is trimmed to exactly
// that many, counting both directions as one conversation.
func TestSQLiteStore_pruneEvents_trims_dm_thread_to_headroom(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const bottyID domain.InstanceID = "inst-botty"
	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(bottyID, "botty", "test/model", "", nil)))

	total := eventRetentionHeadroom + 51
	ids := make([]int64, total)

	for i := range total {
		at := testTime.Add(time.Duration(i) * time.Second)

		var (
			id  int64
			err error
		)

		if i%2 == 0 {
			id, err = s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
				"iain"), Target: domain.ChannelName(bottyID), Body: "hi", At: at})
		} else {
			id, err = s.AppendEvent(ctx, "", domain.Message{Source: domain.ClientSource(
				bottyID, "botty"), Target: "", Body: "hello", At: at})
		}

		require.NoError(t, err)
		ids[i] = id
	}

	require.NoError(t, s.pruneEvents(ctx))

	count, err := s.CountDMEventsFrom(ctx, "", bottyID, nil)
	require.NoError(t, err)
	require.Equal(t, eventRetentionHeadroom, count)

	got, err := s.DMEventsBefore(ctx, "", bottyID, nil, eventRetentionHeadroom)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, ids[len(ids)-eventRetentionHeadroom:], gotIDs)
}

// TestSQLiteStore_pruneEvents_tolerates_a_stale_channel_cursor is
// red-first for cursor coherence. last_read.event_id is a foreign key
// into events(id) with no ON DELETE clause, so under
// foreign_keys=on (every production and test connection: see
// SQLitePragmaDSN) deleting the exact row a cursor points at fails
// the whole DELETE. A channel that grew past eventRetentionHeadroom
// without a MarkRead in between leaves last_read pointing at a row
// older than the trim boundary; retention must skip that one row
// rather than error the whole pass or lose the cursor.
func TestSQLiteStore_pruneEvents_tolerates_a_stale_channel_cursor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#dev", testTime)))

	ids := appendTestEvents(t, s, "#dev", eventRetentionHeadroom+200)

	staleCursor := slices.Min(ids)
	require.NoError(t, s.SetLastRead(ctx, "#dev", staleCursor))

	require.NoError(t, s.pruneEvents(ctx), "a stale cursor must not fail the retention pass")

	gotCursor, err := s.GetLastRead(ctx, "#dev")
	require.NoError(t, err)

	// The cursor's row is the one exception carved out of an
	// otherwise-full trim to eventRetentionHeadroom, so the channel
	// now holds one more row than headroom: everything headroom kept,
	// plus the stale cursor row itself.
	total, err := s.CountEventsFrom(ctx, "#dev", nil)
	require.NoError(t, err)

	// UnreadCount's `id >= cursor` counting still answers correctly:
	// the cursor's own row is the oldest surviving row, so counting
	// from it counts everything left in the channel.
	fromCursor, err := s.CountEventsFrom(ctx, "#dev", &staleCursor)
	require.NoError(t, err)
	retained, err := s.EventsBefore(ctx, "#dev", nil, eventRetentionHeadroom+1)
	require.NoError(t, err)
	retainedIDs := make([]int64, len(retained))
	for i, event := range retained {
		retainedIDs[i] = event.ID
	}
	expectedIDs := append([]int64{staleCursor}, ids[200:]...)
	type assertionSnapshot struct {
		Cursor     int64
		Total      int
		FromCursor int
		IDs        []int64
	}

	require.Equal(t, assertionSnapshot{
		Cursor:     staleCursor,
		Total:      eventRetentionHeadroom + 1,
		FromCursor: eventRetentionHeadroom + 1,
		IDs:        expectedIDs,
	}, assertionSnapshot{
		Cursor:     gotCursor,
		Total:      total,
		FromCursor: fromCursor,
		IDs:        retainedIDs,
	})
}

// TestSQLiteStore_pruneEvents_tolerates_a_stale_dm_cursor is the same
// case for dm_last_read, whose event_id is likewise a foreign key
// into events(id) with no cascade.
func TestSQLiteStore_pruneEvents_tolerates_a_stale_dm_cursor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const bottyID domain.InstanceID = "inst-botty"
	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(bottyID, "botty", "test/model", "", nil)))

	staleCursor, err := s.AppendEvent(ctx, "", domain.Message{Source: domain.ClientSource(
		bottyID, "botty"), Target: "", Body: "long ago", At: testTime})
	require.NoError(t, err)

	require.NoError(t, s.SetDMLastRead(ctx, bottyID, staleCursor))

	for i := range eventRetentionHeadroom + 200 {
		_, err := s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
			"iain"), Target: domain.ChannelName(bottyID), Body: "hi", At: testTime.Add(time.Duration(i+1) * time.Second)})
		require.NoError(t, err)
	}

	require.NoError(t, s.pruneEvents(ctx), "a stale DM cursor must not fail the retention pass")

	gotCursor, err := s.GetDMLastRead(ctx, bottyID)
	require.NoError(t, err)
	require.Equal(t, staleCursor, gotCursor, "the cursor's own row survives so the foreign key keeps holding")

	total, err := s.CountDMEventsFrom(ctx, "", bottyID, nil)
	require.NoError(t, err)
	require.Equal(t, eventRetentionHeadroom+1, total)

	fromCursor, err := s.CountDMEventsFrom(ctx, "", bottyID, &staleCursor)
	require.NoError(t, err)
	require.Equal(t, total, fromCursor)
}

// TestNewSQLiteStore_prunes_events_on_open pins that a fresh open
// runs the retention pass automatically, rather than requiring a
// caller to invoke it: pruneEvents has no other caller.
func TestNewSQLiteStore_prunes_events_on_open(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#dev", testTime)))
	appendTestEvents(t, s, "#dev", eventRetentionHeadroom+90)

	reopened, err := NewSQLiteStore(ctx, s.db)
	require.NoError(t, err)

	count, err := reopened.CountEventsFrom(ctx, "#dev", nil)
	require.NoError(t, err)
	require.Equal(t, eventRetentionHeadroom, count)
}

// TestSQLitePragmaDSN_sets_incremental_auto_vacuum pins that a fresh
// database opened through SQLitePragmaDSN comes up in
// incremental-vacuum mode, which is what lets pruneEvents's
// PRAGMA incremental_vacuum actually reclaim the space a trim frees.
func TestSQLitePragmaDSN_sets_incremental_auto_vacuum(t *testing.T) {
	s := newTestStore(t)

	var mode int
	require.NoError(t, s.db.QueryRowContext(t.Context(), "PRAGMA auto_vacuum").Scan(&mode))
	require.Equal(t, 2, mode, "2 is SQLite's auto_vacuum=incremental")
}

// TestSQLiteStore_pruneEvents_trims_every_dm_pair pins that a DM
// thread's retention is measured per pair of correspondents, not per
// instance against the user. A model addresses another model with the
// `msg` tool, so a thread neither end of which is the user is
// reachable and grows like any other; a pass that only ever paired an
// instance with the user would leave it growing without bound.
func TestSQLiteStore_pruneEvents_trims_every_dm_pair(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const (
		alphaID domain.InstanceID = "inst-alpha"
		betaID  domain.InstanceID = "inst-beta"
	)

	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(alphaID, "alpha", "test/model", "", nil)))
	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(betaID, "beta", "test/model", "", nil)))

	total := eventRetentionHeadroom + 37
	ids := make([]int64, total)

	for i := range total {
		at := testTime.Add(time.Duration(i) * time.Second)

		from, to := alphaID, betaID
		fromNick := domain.Nick("alpha")

		if i%2 == 1 {
			from, to = betaID, alphaID
			fromNick = "beta"
		}

		id, err := s.AppendEvent(ctx, domain.ChannelName(to), domain.Message{Source: domain.ClientSource(

			from, fromNick), Target: domain.ChannelName(to), Body: "hi", At: at})
		require.NoError(t, err)

		ids[i] = id
	}

	require.NoError(t, s.pruneEvents(ctx))

	count, err := s.CountDMEventsFrom(ctx, alphaID, betaID, nil)
	require.NoError(t, err)
	require.Equal(t, eventRetentionHeadroom, count)

	got, err := s.DMEventsBefore(ctx, alphaID, betaID, nil, eventRetentionHeadroom)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, ids[len(ids)-eventRetentionHeadroom:], gotIDs,
		"the newest eventRetentionHeadroom messages of the pair survive")
}

// TestSQLiteStore_pruneEvents_trims_a_thread_a_client_holds_with_itself
// covers the pair a client forms with itself, which `/msg` against
// your own nick produces: both ends of the row carry the same id, so
// the pair's two halves are the same predicate written twice. It
// trims like any other thread.
func TestSQLiteStore_pruneEvents_trims_a_thread_a_client_holds_with_itself(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	total := eventRetentionHeadroom + 9
	ids := make([]int64, total)

	for i := range total {
		id, err := s.AppendEvent(ctx, "", domain.Message{Source: domain.LegacyClientSource(

			"iain"), Target: "", Body: "note to self", At: testTime.Add(time.Duration(i) * time.Second)})
		require.NoError(t, err)

		ids[i] = id
	}

	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.DMEventsBefore(ctx, "", "", nil, eventRetentionHeadroom+total)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, ids[len(ids)-eventRetentionHeadroom:], gotIDs)
}

// TestSQLiteStore_pruneEvents_removes_context_summaries_outside_actor_visibility
// covers the orphan pass over the two summary tables. Neither has a
// foreign key to channels or to instances, so a departure the ordinary
// path missed leaves both the summary and its verbatim sources behind.
func TestSQLiteStore_pruneEvents_removes_context_summaries_outside_actor_visibility(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	live := domain.NewModelInstance("inst-live", "live", "test/model", "", nil)
	live.JoinChannel("#dev", testTime)
	departed := domain.NewModelInstance("inst-departed", "departed", "test/model", "", nil)
	invited := domain.NewModelInstance("inst-invited", "invited", "test/model", "", nil)
	peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
	for _, inst := range []*domain.Instance{live, departed, invited, peer} {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}

	window := domain.NewChannelWindow("#dev", testTime)
	window.Members.Add(live)
	window.Invitations.Add(invited.ID())
	require.NoError(t, s.SaveWindow(ctx, window))

	type summaryFixture struct {
		actor  domain.InstanceID
		window protocol.WindowTarget
	}
	fixtures := []summaryFixture{
		{actor: live.ID(), window: protocol.ChannelWindowTarget("#dev")},
		{actor: departed.ID(), window: protocol.ChannelWindowTarget("#dev")},
		{actor: invited.ID(), window: protocol.ChannelWindowTarget("#dev")},
		{actor: live.ID(), window: protocol.ChannelWindowTarget("#gone")},
		{actor: live.ID(), window: protocol.DirectWindowTarget(peer.ID())},
		{actor: live.ID(), window: protocol.DirectWindowTarget("inst-gone")},
		{actor: live.ID(), window: protocol.DirectWindowTarget("")},
		{actor: "inst-gone", window: protocol.DirectWindowTarget(peer.ID())},
	}
	for _, fixture := range fixtures {
		_, err := s.CommitContextSummary(ctx, ContextSummaryUpdate{
			InstanceID: fixture.actor, Window: fixture.window, Summary: "earlier context",
			Sources: []protocol.IRCMessage{{
				Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("alice"),
				Body: "a line worth summarising", At: testTime,
			}},
			CreatedAt: testTime,
		})
		require.NoError(t, err)
	}

	require.NoError(t, s.pruneEvents(ctx))

	want := []string{
		"instance_id=inst-invited|window_kind=1|window_key=#dev",
		"instance_id=inst-live|window_kind=1|window_key=#dev",
		"instance_id=inst-live|window_kind=2|window_key=",
		"instance_id=inst-live|window_kind=2|window_key=inst-peer",
	}
	require.Equal(t, want, dumpTable(t, s.db, `
		SELECT instance_id, window_kind, window_key
		FROM context_summaries
		ORDER BY instance_id, window_kind, window_key
	`))
	require.Equal(t, want, dumpTable(t, s.db, `
		SELECT instance_id, window_kind, window_key
		FROM context_summary_sources
		ORDER BY instance_id, window_kind, window_key
	`))
}

// writeTestMemories writes n memories for one instance, one second
// apart, and returns their keys in write order.
func writeTestMemories(t *testing.T, s *SQLiteStore, id domain.InstanceID, n int) []string {
	t.Helper()

	keys := make([]string, n)
	for i := range n {
		keys[i] = fmt.Sprintf("fact_%04d", i)
		require.NoError(t, s.WriteMemory(
			t.Context(), id, keys[i], fmt.Sprintf("value %d", i),
			testTime.Add(time.Duration(i)*time.Second), false,
		))
	}

	return keys
}

func memoryKeys(entries []MemoryEntry) []string {
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}

	return keys
}

// TestSQLiteStore_pruneEvents_trims_each_instance_memories pins the
// per-instance bound: an instance holding more than
// memoryRetentionHeadroom memories keeps exactly that many, the ones
// written most recently.
func TestSQLiteStore_pruneEvents_trims_each_instance_memories(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		actor = domain.InstanceID("inst-botty")
		extra = 23
	)

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(actor, "botty", "test/model", "", nil)))

	keys := writeTestMemories(t, s, actor, memoryRetentionHeadroom+extra)

	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.ReadMemories(ctx, actor)
	require.NoError(t, err)
	require.Equal(t, keys[extra:], memoryKeys(got))
}

// TestSQLiteStore_pruneEvents_trims_every_instance_separately pins
// that the bound is per instance: one instance over the headroom does
// not cost another instance the memories it is under the headroom
// with.
func TestSQLiteStore_pruneEvents_trims_every_instance_separately(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		busy  = domain.InstanceID("inst-busy")
		quiet = domain.InstanceID("inst-quiet")
		extra = 7
	)

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(busy, "busy", "test/model", "", nil)))
	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(quiet, "quiet", "test/model", "", nil)))

	busyKeys := writeTestMemories(t, s, busy, memoryRetentionHeadroom+extra)
	quietKeys := writeTestMemories(t, s, quiet, 3)

	require.NoError(t, s.pruneEvents(ctx))

	gotBusy, err := s.ReadMemories(ctx, busy)
	require.NoError(t, err)
	gotQuiet, err := s.ReadMemories(ctx, quiet)
	require.NoError(t, err)

	type retained struct {
		Busy  []string
		Quiet []string
	}

	require.Equal(t, retained{
		Busy:  busyKeys[extra:],
		Quiet: quietKeys,
	}, retained{
		Busy:  memoryKeys(gotBusy),
		Quiet: memoryKeys(gotQuiet),
	})
}

// TestSQLiteStore_pruneEvents_keeps_a_rewritten_memory pins that
// overwriting a memory makes it recent again. WriteMemory stamps the
// entry's write time and retention orders on that, so rewriting the
// oldest memory carries it past newer ones the instance has left
// alone. Reading a memory does not refresh it; only a write does.
func TestSQLiteStore_pruneEvents_keeps_a_rewritten_memory(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		actor = domain.InstanceID("inst-botty")
		extra = 5
	)

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(actor, "botty", "test/model", "", nil)))

	keys := writeTestMemories(t, s, actor, memoryRetentionHeadroom+extra)

	rewritten := keys[0]
	require.NoError(t, s.WriteMemory(ctx, actor, rewritten, "still true",
		testTime.Add(time.Duration(len(keys))*time.Second), false))

	require.NoError(t, s.pruneEvents(ctx))

	got, err := s.ReadMemories(ctx, actor)
	require.NoError(t, err)

	want := append([]string{rewritten}, keys[extra+1:]...)
	sort.Strings(want)
	require.Equal(t, want, memoryKeys(got))
}

// TestSQLiteStore_pruneEvents_removes_a_departed_instance_memories
// covers the run that ended without the departure that would have
// called DeleteMemoriesByInstance: the memories table has no foreign
// key to cascade through, so nothing else removes those rows.
func TestSQLiteStore_pruneEvents_removes_a_departed_instance_memories(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		present = domain.InstanceID("inst-present")
		gone    = domain.InstanceID("inst-gone")
	)

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(present, "present", "test/model", "", nil)))

	require.NoError(t, s.WriteMemory(ctx, present, "kept", "value", testTime, false))
	require.NoError(t, s.WriteMemory(ctx, gone, "orphaned", "value", testTime, false))

	require.NoError(t, s.pruneEvents(ctx))

	gotPresent, err := s.ReadMemories(ctx, present)
	require.NoError(t, err)
	gotGone, err := s.ReadMemories(ctx, gone)
	require.NoError(t, err)

	type retained struct {
		Present []string
		Gone    []string
	}

	require.Equal(t, retained{
		Present: []string{"kept"},
		Gone:    []string{},
	}, retained{
		Present: memoryKeys(gotPresent),
		Gone:    memoryKeys(gotGone),
	})
}
