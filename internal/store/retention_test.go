package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
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

	_, err := s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{
		Target: domain.ChannelName(bottyID), From: "iain", Body: "hi", At: testTime,
	})
	require.NoError(t, err)

	_, err = s.AppendEvent(ctx, "", domain.Message{
		Target: "", From: "botty", InstanceID: bottyID, Body: "hello", At: testTime.Add(time.Second),
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteInstanceByID(ctx, bottyID))

	require.NoError(t, s.pruneEvents(ctx))

	count, err := s.CountDMEventsFrom(ctx, "", bottyID, nil)
	require.NoError(t, err)
	require.Zero(t, count)
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

	toBotty, err := s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{
		Target: domain.ChannelName(bottyID), From: "iain", Body: "hi", At: testTime,
	})
	require.NoError(t, err)

	toUser, err := s.AppendEvent(ctx, "", domain.Message{
		Target: "", From: "botty", InstanceID: bottyID, Body: "hello", At: testTime.Add(time.Second),
	})
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
			id, err = s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{
				Target: domain.ChannelName(bottyID), From: "iain", Body: "hi", At: at,
			})
		} else {
			id, err = s.AppendEvent(ctx, "", domain.Message{
				Target: "", From: "botty", InstanceID: bottyID, Body: "hello", At: at,
			})
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

	staleCursor := ids[0]
	require.NoError(t, s.SetLastRead(ctx, "#dev", staleCursor))

	require.NoError(t, s.pruneEvents(ctx), "a stale cursor must not fail the retention pass")

	gotCursor, err := s.GetLastRead(ctx, "#dev")
	require.NoError(t, err)
	require.Equal(t, staleCursor, gotCursor, "the cursor's own row survives so the foreign key keeps holding")

	// The cursor's row is the one exception carved out of an
	// otherwise-full trim to eventRetentionHeadroom, so the channel
	// now holds one more row than headroom: everything headroom kept,
	// plus the stale cursor row itself.
	total, err := s.CountEventsFrom(ctx, "#dev", nil)
	require.NoError(t, err)
	require.Equal(t, eventRetentionHeadroom+1, total)

	// UnreadCount's `id >= cursor` counting still answers correctly:
	// the cursor's own row is the oldest surviving row, so counting
	// from it counts everything left in the channel.
	fromCursor, err := s.CountEventsFrom(ctx, "#dev", &staleCursor)
	require.NoError(t, err)
	require.Equal(t, total, fromCursor)
}

// TestSQLiteStore_pruneEvents_tolerates_a_stale_dm_cursor is the same
// case for dm_last_read, whose event_id is likewise a foreign key
// into events(id) with no cascade.
func TestSQLiteStore_pruneEvents_tolerates_a_stale_dm_cursor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const bottyID domain.InstanceID = "inst-botty"
	require.NoError(t, s.SaveInstance(ctx, domain.NewModelInstance(bottyID, "botty", "test/model", "", nil)))

	staleCursor, err := s.AppendEvent(ctx, "", domain.Message{
		Target: "", From: "botty", InstanceID: bottyID, Body: "long ago", At: testTime,
	})
	require.NoError(t, err)

	require.NoError(t, s.SetDMLastRead(ctx, bottyID, staleCursor))

	for i := range eventRetentionHeadroom + 200 {
		_, err := s.AppendEvent(ctx, domain.ChannelName(bottyID), domain.Message{
			Target: domain.ChannelName(bottyID), From: "iain", Body: "hi",
			At: testTime.Add(time.Duration(i+1) * time.Second),
		})
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

		id, err := s.AppendEvent(ctx, domain.ChannelName(to), domain.Message{
			Target:     domain.ChannelName(to),
			From:       fromNick,
			InstanceID: from,
			Body:       "hi",
			At:         at,
		})
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
		id, err := s.AppendEvent(ctx, "", domain.Message{
			Target: "",
			From:   "iain",
			Body:   "note to self",
			At:     testTime.Add(time.Duration(i) * time.Second),
		})
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
