package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestSQLiteStore_ModelTurnEntries_reads_in_producer_order(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	input := ModelTurnEntry{
		Kind: ModelTurnInput, Seq: 0,
		Data: []byte(`{"input":"hello"}`), At: testTime,
	}
	turnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: "inst-botty",
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    "test/model",
		StartedAt:  testTime,
	}, input)
	require.NoError(t, err)

	outcome := ModelTurnEntry{
		Kind: ModelTurnOutcome, Seq: 2,
		Data: []byte(`{"tool_turn_count":1}`), At: testTime.Add(2 * time.Second),
	}
	assistant := ModelTurnEntry{
		Kind: ModelTurnAssistant, Seq: 1,
		Data: []byte(`{"text":"hi"}`), At: testTime.Add(time.Second),
	}
	outcomeErr := s.AppendModelTurnEntry(ctx, turnID, outcome)
	assistantErr := s.AppendModelTurnEntry(ctx, turnID, assistant)

	entries, entriesErr := s.ModelTurnEntries(ctx, turnID)

	type journalState struct {
		OutcomeError   error
		AssistantError error
		EntriesError   error
		Entries        []ModelTurnEntry
	}

	require.Equal(t, journalState{
		Entries: []ModelTurnEntry{input, assistant, outcome},
	}, journalState{
		OutcomeError:   outcomeErr,
		AssistantError: assistantErr,
		EntriesError:   entriesErr,
		Entries:        entries,
	})
}

// TestSQLiteStore_AppendModelTurnEntry_leaves_retention_to_admission
// seeds more turns than the headroom without going through
// [SQLiteStore.BeginModelTurn], so the retention pass has never run
// over them. An append that ran the pass would delete the three
// oldest turns, taking the entry it had just written with them.
func TestSQLiteStore_AppendModelTurnEntry_leaves_retention_to_admission(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const actor = domain.InstanceID("inst-botty")
	const seeded = modelTurnRetentionHeadroom + 3
	_, err := s.db.ExecContext(ctx, `
		WITH RECURSIVE counter(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM counter WHERE n < ?
		)
		INSERT INTO model_turns
			(instance_id, window_kind, window_key, model_id, started_at)
		SELECT ?, 1, '#dev', 'test/model', ? FROM counter
	`, seeded, actor, testTime.Format(time.RFC3339Nano))
	require.NoError(t, err)

	appended := ModelTurnEntry{
		Kind: ModelTurnAssistant, Seq: 1,
		Data: []byte(`{"text":"hi"}`), At: testTime,
	}
	appendErr := s.AppendModelTurnEntry(ctx, 1, appended)

	turns, turnsErr := queryRows(ctx, s.db,
		`SELECT count(*) FROM model_turns`, nil, scalarColumn[int]())
	entries, entriesErr := s.ModelTurnEntries(ctx, 1)

	type journalState struct {
		AppendError  error
		TurnsError   error
		Turns        []int
		EntriesError error
		Entries      []ModelTurnEntry
	}

	require.Equal(t, journalState{
		Turns:   []int{seeded},
		Entries: []ModelTurnEntry{appended},
	}, journalState{
		AppendError:  appendErr,
		TurnsError:   turnsErr,
		Turns:        turns,
		EntriesError: entriesErr,
		Entries:      entries,
	})
}

func TestSQLiteStore_ModelTurnsForInstanceBefore(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	windows := []protocol.WindowTarget{
		protocol.ChannelWindowTarget("#dev"),
		protocol.DirectWindowTarget("inst-peer"),
		protocol.ChannelWindowTarget("#ops"),
	}
	want := make([]ModelTurnRecord, 0, len(windows))
	for i, window := range windows {
		startedAt := testTime.Add(time.Duration(i) * time.Second)
		turnID, err := s.BeginModelTurn(ctx, ModelTurn{
			InstanceID: "inst-botty",
			Window:     window,
			ModelID:    "test/model",
			StartedAt:  startedAt,
		}, ModelTurnEntry{
			Kind: ModelTurnInput,
			Data: fmt.Appendf(nil, `{"turn":%d}`, i),
			At:   startedAt,
		})
		require.NoError(t, err)
		want = append(want, ModelTurnRecord{
			ID:         turnID,
			InstanceID: "inst-botty",
			Window:     window,
			ModelID:    "test/model",
			StartedAt:  startedAt,
		})
	}

	_, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: "inst-other",
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    "test/model",
		StartedAt:  testTime,
	}, ModelTurnEntry{
		Kind: ModelTurnInput, Data: []byte(`{"turn":"other"}`), At: testTime,
	})
	require.NoError(t, err)

	all, allErr := s.ModelTurnsForInstanceBefore(ctx, "inst-botty", nil, 10)
	newest, newestErr := s.ModelTurnsForInstanceBefore(ctx, "inst-botty", nil, 2)
	cursor := want[1].ID
	older, olderErr := s.ModelTurnsForInstanceBefore(ctx, "inst-botty", &cursor, 10)
	none, noneErr := s.ModelTurnsForInstanceBefore(ctx, "inst-quiet", nil, 10)

	type listingState struct {
		AllError    error
		All         []ModelTurnRecord
		NewestError error
		Newest      []ModelTurnRecord
		OlderError  error
		Older       []ModelTurnRecord
		NoneError   error
		None        []ModelTurnRecord
	}

	require.Equal(t, listingState{
		All:    want,
		Newest: want[1:],
		Older:  want[:1],
	}, listingState{
		AllError:    allErr,
		All:         all,
		NewestError: newestErr,
		Newest:      newest,
		OlderError:  olderErr,
		Older:       older,
		NoneError:   noneErr,
		None:        none,
	})
}

// TestSQLiteStore_CommitChannelDeparture_retains_the_actor_journal
// covers the difference between the actor's own context and the
// operator's record of its provider traffic. Leaving a channel
// discards what the model can still see; the journal is read by an
// operator and never replayed into a prompt, so the departure leaves
// it alone.
func TestSQLiteStore_CommitChannelDeparture_retains_the_actor_journal(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	actor := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
	actor.JoinChannel("#dev", testTime)
	peer.JoinChannel("#dev", testTime)
	require.NoError(t, s.SaveInstance(ctx, actor))
	require.NoError(t, s.SaveInstance(ctx, peer))

	window := domain.NewChannelWindow("#dev", testTime)
	window.Members.Add(actor)
	window.Members.Add(peer)
	require.NoError(t, s.SaveWindow(ctx, window))

	_, err := s.AppendChannelScrollback(ctx, []ChannelScrollbackRecord{{
		InstanceID: actor.ID(), Channel: "#dev",
		Event: domain.Message{
			Source: domain.ClientSource(peer.ID(), peer.Nick()),
			Target: "#dev", Body: "discarded", At: testTime,
		},
	}})
	require.NoError(t, err)

	turnEntry := ModelTurnEntry{
		Kind: ModelTurnInput,
		Data: []byte(`{"input":"channel context"}`),
		At:   testTime,
	}
	turnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: actor.ID(),
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    actor.ModelID,
		StartedAt:  testTime,
	}, turnEntry)
	require.NoError(t, err)

	departedWindow := window.Clone()
	departedWindow.Members.RemoveInstance(actor)
	departedActor := actor.Snapshot()
	departedActor.LeaveChannels("#dev")
	part := domain.Part{
		Source: domain.ClientSource(actor.ID(), actor.Nick()),
		Target: "#dev", Message: "bye", At: testTime,
	}
	_, commitErr := s.CommitChannelDeparture(ctx, ChannelDeparture{
		Window:   departedWindow,
		Instance: departedActor,
		Event:    part,
		Scrollback: []ChannelScrollbackRecord{{
			InstanceID: peer.ID(), Channel: "#dev", Event: part,
		}},
	})

	scrollback, scrollbackErr := s.ChannelScrollback(ctx, actor.ID(), "#dev", 10)
	entries, entriesErr := s.ModelTurnEntries(ctx, turnID)

	type departureState struct {
		CommitError     error
		ScrollbackError error
		Scrollback      []domain.StoredEvent
		EntriesError    error
		Entries         []ModelTurnEntry
	}

	require.Equal(t, departureState{
		Entries: []ModelTurnEntry{turnEntry},
	}, departureState{
		CommitError:     commitErr,
		ScrollbackError: scrollbackErr,
		Scrollback:      scrollback,
		EntriesError:    entriesErr,
		Entries:         entries,
	})
}
