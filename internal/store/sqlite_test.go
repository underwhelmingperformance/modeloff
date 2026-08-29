package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/driver"
	"github.com/stretchr/testify/require"
	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	"github.com/laney/modeloff/internal/protocol"
)

var testTime = time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)

// storeTestMembers builds a MemberList for tests by constructing a
// synthetic model Instance per nick and persisting it to the store.
// Persisting is required because `GetWindow` resolves stub member
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

func requireWindowEqual(t *testing.T, expected, actual domain.Window) {
	t.Helper()

	require.Equal(t, expected, actual)
}

func requireWindowsEqual(t *testing.T, expected, actual []domain.Window) {
	t.Helper()

	require.Equal(t, expected, actual)
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

func TestSQLiteStore_CommitChannelJoin_rolls_back_every_row(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	actor := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, actor))

	window := domain.NewChannelWindow("#dev", testTime)
	window.Members.Add(actor)
	candidate := actor.Snapshot()
	candidate.JoinChannel("#dev", testTime)

	_, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER reject_join_projection
		BEFORE INSERT ON channel_scrollback
		BEGIN
			SELECT RAISE(ABORT, 'rejected join projection');
		END
	`)
	require.NoError(t, err)

	_, commitErr := s.CommitChannelJoin(ctx, ChannelJoin{
		Window: window, Instance: candidate,
		Event: domain.Join{
			Source: domain.ClientSource(actor.ID(), actor.Nick()),
			Target: "#dev",
			At:     testTime,
		},
		Scrollback: []ChannelScrollbackRecord{{
			InstanceID: actor.ID(), Channel: "#dev",
			Event: domain.Join{
				Source: domain.ClientSource(actor.ID(), actor.Nick()),
				Target: "#dev", At: testTime,
			},
		}},
	})
	_, windowErr := s.GetWindow(ctx, "#dev")
	storedActor, actorErr := s.GetInstanceByID(ctx, actor.ID())
	events, eventsErr := s.EventsBefore(ctx, "#dev", nil, 10)
	scrollback, scrollbackErr := s.ChannelScrollback(ctx, actor.ID(), "#dev", 10)

	type assertionSnapshot struct {
		CommitFailed    bool
		WindowAbsent    bool
		ActorLoadError  error
		ActorInChannel  bool
		EventsLoadError error
		Events          []domain.StoredEvent
		ScrollbackError error
		Scrollback      []domain.StoredEvent
	}

	require.Equal(t, assertionSnapshot{
		CommitFailed: true,
		WindowAbsent: true,
	}, assertionSnapshot{
		CommitFailed:    commitErr != nil,
		WindowAbsent:    errors.Is(windowErr, ErrNoSuchChannel),
		ActorLoadError:  actorErr,
		ActorInChannel:  storedActor.InChannel("#dev"),
		EventsLoadError: eventsErr,
		Events:          events,
		ScrollbackError: scrollbackErr,
		Scrollback:      scrollback,
	})
}

func TestSQLiteStore_CommitChannelJoin_rolls_back_context_resets(t *testing.T) {
	tests := map[string]string{
		"scrollback deletion": `
			CREATE TRIGGER reject_join_scrollback_reset
			BEFORE DELETE ON channel_scrollback
			BEGIN
				SELECT RAISE(ABORT, 'rejected scrollback reset');
			END
		`,
		"reply deletion": `
			CREATE TRIGGER reject_join_reply_reset
			BEFORE DELETE ON instance_replies
			BEGIN
				SELECT RAISE(ABORT, 'rejected reply reset');
			END
		`,
		"turn deletion": `
			CREATE TRIGGER reject_join_turn_reset
			BEFORE DELETE ON model_turns
			BEGIN
				SELECT RAISE(ABORT, 'rejected turn reset');
			END
		`,
		"join event": `
			CREATE TRIGGER reject_join_after_context_reset
			BEFORE INSERT ON events
			WHEN NEW.type = 'join'
			BEGIN
				SELECT RAISE(ABORT, 'rejected join event');
			END
		`,
	}

	for name, trigger := range tests {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			actor := domain.NewModelInstance(
				"inst-botty", "botty", "test/model", "", nil,
			)
			peer := domain.NewModelInstance(
				"inst-peer", "peer", "test/model", "", nil,
			)
			peer.JoinChannel("#dev", testTime)
			require.NoError(t, s.SaveInstance(ctx, actor))
			require.NoError(t, s.SaveInstance(ctx, peer))

			window := domain.NewChannelWindow("#dev", testTime)
			window.Members.Add(peer)
			window.Invitations.Add(actor.ID())
			require.NoError(t, s.SaveWindow(ctx, window))

			message := domain.Message{
				Source: domain.ClientSource(peer.ID(), peer.Nick()),
				Target: "#dev", Body: "previous interval", At: testTime,
			}
			scrollbackIDs, err := s.AppendChannelScrollback(
				ctx, []ChannelScrollbackRecord{{
					InstanceID: actor.ID(), Channel: "#dev", Event: message,
				}},
			)
			require.NoError(t, err)

			reply := domain.TopicInfo{
				Target: "#dev", Topic: "previous topic", At: testTime,
			}
			replyID, err := s.AppendInstanceReply(
				ctx, actor.ID(), protocol.ChannelWindowTarget("#dev"), reply,
			)
			require.NoError(t, err)

			turnEntry := ModelTurnEntry{
				Kind: ModelTurnInput,
				Data: []byte(`{"input":"previous interval"}`),
				At:   testTime,
			}
			turnID, err := s.BeginModelTurn(ctx, ModelTurn{
				InstanceID: actor.ID(),
				Window:     protocol.ChannelWindowTarget("#dev"),
				ModelID:    actor.ModelID,
				StartedAt:  testTime,
			}, turnEntry)
			require.NoError(t, err)

			_, err = s.db.ExecContext(ctx, trigger)
			require.NoError(t, err)

			candidateWindow := window.Clone()
			candidateWindow.Members.Add(actor)
			candidateWindow.Invitations.Remove(actor.ID())
			candidateActor := actor.Snapshot()
			candidateActor.JoinChannel("#dev", testTime)
			_, commitErr := s.CommitChannelJoin(ctx, ChannelJoin{
				Window: candidateWindow, Instance: candidateActor,
				Event: domain.Join{
					Source: domain.ClientSource(actor.ID(), actor.Nick()),
					Target: "#dev", At: testTime,
				},
				ResetContext: true,
			})

			storedWindow, windowErr := s.GetWindow(ctx, "#dev")
			storedActor, actorErr := s.GetInstanceByID(ctx, actor.ID())
			audit, auditErr := s.EventsBefore(ctx, "#dev", nil, 10)
			scrollback, scrollbackErr := s.ChannelScrollback(
				ctx, actor.ID(), "#dev", 10,
			)
			replies, repliesErr := s.InstanceRepliesBefore(ctx, actor.ID(), nil, 10)
			turnEntries, turnEntriesErr := s.ModelTurnEntries(ctx, turnID)

			type joinState struct {
				CommitFailed    bool
				Window          domain.Window
				WindowError     error
				Actor           comparableInstance
				ActorError      error
				Audit           []domain.StoredEvent
				AuditError      error
				Scrollback      []domain.StoredEvent
				ScrollbackError error
				Replies         []InstanceReplyRecord
				RepliesError    error
				TurnEntries     []ModelTurnEntry
				TurnEntriesErr  error
			}
			require.Equal(t, joinState{
				CommitFailed: true,
				Window:       window,
				Actor:        normaliseInstance(actor),
				Scrollback:   []domain.StoredEvent{{ID: scrollbackIDs[0], Event: message}},
				Replies: []InstanceReplyRecord{{
					ID: replyID, Window: protocol.ChannelWindowTarget("#dev"), Event: reply,
				}},
				TurnEntries: []ModelTurnEntry{turnEntry},
			}, joinState{
				CommitFailed:    commitErr != nil,
				Window:          storedWindow,
				WindowError:     windowErr,
				Actor:           normaliseInstance(storedActor),
				ActorError:      actorErr,
				Audit:           audit,
				AuditError:      auditErr,
				Scrollback:      scrollback,
				ScrollbackError: scrollbackErr,
				Replies:         replies,
				RepliesError:    repliesErr,
				TurnEntries:     turnEntries,
				TurnEntriesErr:  turnEntriesErr,
			})
		})
	}
}

func TestSQLiteStore_CommitChannelUpdate_rolls_back_every_row(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, peer))

	window := domain.NewChannelWindow("#dev", testTime)
	window.Members.Add(peer)
	window.Topic = "old topic"
	require.NoError(t, s.SaveWindow(ctx, window))

	_, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER reject_topic_event
		BEFORE INSERT ON events
		WHEN NEW.type = 'topic_change'
		BEGIN
			SELECT RAISE(ABORT, 'rejected topic event');
		END
	`)
	require.NoError(t, err)

	candidate := window.Clone()
	candidate.Topic = "new topic"
	topic := domain.TopicChange{
		Source: domain.ClientSource(peer.ID(), peer.Nick()),
		Target: "#dev", Topic: "new topic", At: testTime,
	}
	_, commitErr := s.CommitChannelUpdate(ctx, ChannelUpdate{
		Window: candidate,
		Event:  topic,
		Scrollback: []ChannelScrollbackRecord{{
			InstanceID: peer.ID(), Channel: "#dev", Event: topic,
		}},
	})

	storedWindow, windowErr := s.GetWindow(ctx, "#dev")
	storedChannel, storedIsChannel := storedWindow.(*domain.ChannelWindow)
	audit, auditErr := s.EventsBefore(ctx, "#dev", nil, 10)
	scrollback, scrollbackErr := s.ChannelScrollback(ctx, peer.ID(), "#dev", 10)

	type assertionSnapshot struct {
		CommitFailed   bool
		WindowError    error
		StoredChannel  *domain.ChannelWindow
		AuditError     error
		Audit          []domain.StoredEvent
		ScrollbackErr  error
		Scrollback     []domain.StoredEvent
		StoredIsWindow bool
	}

	require.Equal(t, assertionSnapshot{
		CommitFailed:   true,
		StoredChannel:  window,
		StoredIsWindow: true,
	}, assertionSnapshot{
		CommitFailed:   commitErr != nil,
		WindowError:    windowErr,
		StoredChannel:  storedChannel,
		AuditError:     auditErr,
		Audit:          audit,
		ScrollbackErr:  scrollbackErr,
		Scrollback:     scrollback,
		StoredIsWindow: storedIsChannel,
	})
}

func TestSQLiteStore_CommitChannelEvent_rolls_back_the_audit_and_projections(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	peer := domain.NewModelInstance("inst-peer", "peer", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, peer))

	_, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER reject_projected_message
		BEFORE INSERT ON channel_scrollback
		BEGIN
			SELECT RAISE(ABORT, 'rejected projected message');
		END
	`)
	require.NoError(t, err)

	message := domain.Message{
		Source: domain.ClientSource(protocol.UserClientID, "testuser"),
		Target: "#dev", Body: "hello", At: testTime,
	}
	_, commitErr := s.CommitChannelEvent(ctx, ChannelEvent{
		Channel: "#dev",
		Event:   message,
		Scrollback: []ChannelScrollbackRecord{{
			InstanceID: peer.ID(), Channel: "#dev", Event: message,
		}},
	})

	audit, auditErr := s.EventsBefore(ctx, "#dev", nil, 10)
	scrollback, scrollbackErr := s.ChannelScrollback(ctx, peer.ID(), "#dev", 10)
	type assertionSnapshot struct {
		CommitFailed  bool
		AuditError    error
		Audit         []domain.StoredEvent
		ScrollbackErr error
		Scrollback    []domain.StoredEvent
	}

	require.Equal(t, assertionSnapshot{
		CommitFailed: true,
	}, assertionSnapshot{
		CommitFailed:  commitErr != nil,
		AuditError:    auditErr,
		Audit:         audit,
		ScrollbackErr: scrollbackErr,
		Scrollback:    scrollback,
	})
}

func TestSQLiteStore_CommitChannelDeparture_rolls_back_every_row(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	actor := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "", nil,
	)
	peer := domain.NewModelInstance(
		"inst-peer", "peer", "test/model", "", nil,
	)
	actor.JoinChannel("#dev", testTime)
	peer.JoinChannel("#dev", testTime)
	require.NoError(t, s.SaveInstance(ctx, actor))
	require.NoError(t, s.SaveInstance(ctx, peer))

	window := domain.NewChannelWindow("#dev", testTime)
	window.Members.Add(actor)
	window.Members.Add(peer)
	require.NoError(t, s.SaveWindow(ctx, window))

	prior := domain.Message{
		Source: domain.ClientSource(peer.ID(), peer.Nick()),
		Target: "#dev",
		Body:   "still visible",
		At:     testTime,
	}
	priorIDs, err := s.AppendChannelScrollback(ctx, []ChannelScrollbackRecord{{
		InstanceID: actor.ID(), Channel: "#dev", Event: prior,
	}})
	require.NoError(t, err)
	require.Equal(t, []int64{1}, priorIDs)

	replyID, err := s.AppendInstanceReply(
		ctx, actor.ID(), protocol.ChannelWindowTarget("#dev"),
		domain.TopicInfo{Target: "#dev", Topic: "release", At: testTime},
	)
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

	_, err = s.db.ExecContext(ctx, `
		CREATE TRIGGER reject_part_projection
		BEFORE INSERT ON channel_scrollback
		WHEN NEW.type = 'part'
		BEGIN
			SELECT RAISE(ABORT, 'rejected part projection');
		END
	`)
	require.NoError(t, err)

	candidateWindow := window.Clone()
	candidateWindow.Members.RemoveInstance(actor)
	candidateActor := actor.Snapshot()
	candidateActor.LeaveChannels("#dev")
	part := domain.Part{
		Source: domain.ClientSource(actor.ID(), actor.Nick()),
		Target: "#dev", Message: "bye", At: testTime,
	}
	_, commitErr := s.CommitChannelDeparture(ctx, ChannelDeparture{
		Window:   candidateWindow,
		Instance: candidateActor,
		Event:    part,
		Scrollback: []ChannelScrollbackRecord{{
			InstanceID: peer.ID(), Channel: "#dev", Event: part,
		}},
	})

	storedWindow, windowErr := s.GetWindow(ctx, "#dev")
	storedChannel, storedIsChannel := storedWindow.(*domain.ChannelWindow)
	var rawActorJSON string
	actorReadErr := s.db.QueryRowContext(ctx,
		`SELECT data FROM instances WHERE instance_id = ?`, actor.ID(),
	).Scan(&rawActorJSON)
	var storedActor domain.Instance
	actorDecodeErr := json.Unmarshal([]byte(rawActorJSON), &storedActor)
	auditEvents, auditErr := s.EventsBefore(ctx, "#dev", nil, 10)
	actorScrollback, actorScrollbackErr := s.ChannelScrollback(ctx, actor.ID(), "#dev", 10)
	peerScrollback, peerScrollbackErr := s.ChannelScrollback(ctx, peer.ID(), "#dev", 10)
	replies, repliesErr := s.InstanceRepliesBefore(ctx, actor.ID(), nil, 10)
	turnEntries, turnEntriesErr := s.ModelTurnEntries(ctx, turnID)

	type departureState struct {
		CommitFailed         bool
		WindowError          error
		WindowIsChannel      bool
		WindowHasActor       bool
		ActorReadError       error
		ActorDecodeError     error
		ActorInChannel       bool
		AuditError           error
		AuditEvents          []domain.StoredEvent
		ActorScrollbackError error
		ActorScrollback      []domain.StoredEvent
		PeerScrollbackError  error
		PeerScrollback       []domain.StoredEvent
		RepliesError         error
		Replies              []InstanceReplyRecord
		TurnEntriesError     error
		TurnEntries          []ModelTurnEntry
	}
	require.Equal(t, departureState{
		CommitFailed:    true,
		WindowIsChannel: true,
		WindowHasActor:  true,
		ActorInChannel:  true,
		ActorScrollback: []domain.StoredEvent{{ID: 1, Event: prior}},
		Replies: []InstanceReplyRecord{{
			ID:     replyID,
			Window: protocol.ChannelWindowTarget("#dev"),
			Event:  domain.TopicInfo{Target: "#dev", Topic: "release", At: testTime},
		}},
		TurnEntries: []ModelTurnEntry{turnEntry},
	}, departureState{
		CommitFailed:         commitErr != nil,
		WindowError:          windowErr,
		WindowIsChannel:      storedIsChannel,
		WindowHasActor:       storedIsChannel && storedChannel.Members.HasInstance(actor),
		ActorReadError:       actorReadErr,
		ActorDecodeError:     actorDecodeErr,
		ActorInChannel:       storedActor.InChannel("#dev"),
		AuditError:           auditErr,
		AuditEvents:          auditEvents,
		ActorScrollbackError: actorScrollbackErr,
		ActorScrollback:      actorScrollback,
		PeerScrollbackError:  peerScrollbackErr,
		PeerScrollback:       peerScrollback,
		RepliesError:         repliesErr,
		Replies:              replies,
		TurnEntriesError:     turnEntriesErr,
		TurnEntries:          turnEntries,
	})
}

func TestSQLiteStore_CommitActorRename_rolls_back_every_row(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	actor := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "keeps context", nil,
	)
	actor.JoinChannel("#Dev", testTime)
	actor.JoinChannel("#Ops", testTime.Add(time.Minute))
	require.NoError(t, s.SaveInstance(ctx, actor))

	dev := domain.NewChannelWindow("#Dev", testTime)
	dev.Topic = "development"
	dev.Members.Add(actor)
	dev.Members.SetModesByNick(actor.Nick(), domain.MemberModes{Operator: true})
	require.NoError(t, s.SaveWindow(ctx, dev))

	ops := domain.NewChannelWindow("#Ops", testTime.Add(time.Minute))
	ops.Topic = "operations"
	ops.Modes.InviteOnly = true
	ops.Members.Add(actor)
	require.NoError(t, s.SaveWindow(ctx, ops))

	candidateActor := actor.Snapshot()
	candidateActor.SetNick("renamed")
	candidateDev := dev.Clone()
	candidateDev.Members.RenameTo(candidateActor, candidateActor.Nick())
	candidateOps := ops.Clone()
	candidateOps.Members.RenameTo(candidateActor, candidateActor.Nick())
	nickChange := domain.NickChange{
		Source:  domain.ClientSource(actor.ID(), actor.Nick()),
		NewNick: candidateActor.Nick(),
		At:      testTime.Add(2 * time.Minute),
	}

	_, err := s.db.ExecContext(ctx, `
		CREATE TRIGGER reject_second_nick_projection
		BEFORE INSERT ON channel_scrollback
		WHEN NEW.type = 'nick_change' AND NEW.channel = '#Ops'
		BEGIN
			SELECT RAISE(ABORT, 'rejected second nick projection');
		END
	`)
	require.NoError(t, err)

	_, commitErr := s.CommitActorRename(ctx, ActorRename{
		Instance: candidateActor,
		Windows:  []*domain.ChannelWindow{candidateDev, candidateOps},
		Events: []ChannelAuditEvent{
			{Channel: dev.Name(), Event: nickChange},
			{Channel: ops.Name(), Event: nickChange},
		},
		Scrollback: []ChannelScrollbackRecord{
			{InstanceID: actor.ID(), Channel: dev.Name(), Event: nickChange},
			{InstanceID: actor.ID(), Channel: ops.Name(), Event: nickChange},
		},
	})

	var rawActorJSON string
	actorReadErr := s.db.QueryRowContext(ctx,
		`SELECT data FROM instances WHERE instance_id = ?`, actor.ID(),
	).Scan(&rawActorJSON)
	var databaseActor domain.Instance
	actorDecodeErr := json.Unmarshal([]byte(rawActorJSON), &databaseActor)
	canonicalActor, canonicalActorErr := s.GetInstanceByID(ctx, actor.ID())
	storedDev, devErr := s.GetWindow(ctx, dev.Name())
	storedOps, opsErr := s.GetWindow(ctx, ops.Name())
	devEvents, devEventsErr := s.EventsBefore(ctx, dev.Name(), nil, 10)
	opsEvents, opsEventsErr := s.EventsBefore(ctx, ops.Name(), nil, 10)
	devScrollback, devScrollbackErr := s.ChannelScrollback(ctx, actor.ID(), dev.Name(), 10)
	opsScrollback, opsScrollbackErr := s.ChannelScrollback(ctx, actor.ID(), ops.Name(), 10)

	type renameState struct {
		CommitFailed              bool
		ActorReadError            error
		ActorDecodeError          error
		DatabaseActor             comparableInstance
		CanonicalActorError       error
		CanonicalActor            comparableInstance
		CanonicalPointerPreserved bool
		DevWindowError            error
		DevWindow                 domain.Window
		OpsWindowError            error
		OpsWindow                 domain.Window
		DevEventsError            error
		DevEvents                 []domain.StoredEvent
		OpsEventsError            error
		OpsEvents                 []domain.StoredEvent
		DevScrollbackError        error
		DevScrollback             []domain.StoredEvent
		OpsScrollbackError        error
		OpsScrollback             []domain.StoredEvent
	}
	require.Equal(t, renameState{
		CommitFailed:              true,
		DatabaseActor:             normaliseInstance(actor),
		CanonicalActor:            normaliseInstance(actor),
		CanonicalPointerPreserved: true,
		DevWindow:                 domain.Window(dev),
		OpsWindow:                 domain.Window(ops),
	}, renameState{
		CommitFailed:              commitErr != nil,
		ActorReadError:            actorReadErr,
		ActorDecodeError:          actorDecodeErr,
		DatabaseActor:             normaliseInstance(&databaseActor),
		CanonicalActorError:       canonicalActorErr,
		CanonicalActor:            normaliseInstance(canonicalActor),
		CanonicalPointerPreserved: canonicalActor == actor,
		DevWindowError:            devErr,
		DevWindow:                 storedDev,
		OpsWindowError:            opsErr,
		OpsWindow:                 storedOps,
		DevEventsError:            devEventsErr,
		DevEvents:                 devEvents,
		OpsEventsError:            opsEventsErr,
		OpsEvents:                 opsEvents,
		DevScrollbackError:        devScrollbackErr,
		DevScrollback:             devScrollback,
		OpsScrollbackError:        opsScrollbackErr,
		OpsScrollback:             opsScrollback,
	})
}

func TestSQLiteStore_CommitInstanceDeletion_rolls_back_every_row(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	actor := domain.NewModelInstance(
		"inst-botty", "botty", "test/model", "keeps context", nil,
	)
	peer := domain.NewModelInstance(
		"inst-peer", "peer", "test/model", "stays connected", nil,
	)
	actor.JoinChannel("#Shared", testTime)
	actor.JoinChannel("#Solo", testTime.Add(time.Minute))
	peer.JoinChannel("#Shared", testTime)
	for _, inst := range []*domain.Instance{actor, peer} {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}

	shared := domain.NewChannelWindow("#Shared", testTime)
	shared.Topic = "still active"
	shared.Members.Add(actor)
	shared.Members.Add(peer)
	shared.Members.SetModesByNick(actor.Nick(), domain.MemberModes{Operator: true})
	require.NoError(t, s.SaveWindow(ctx, shared))

	sole := domain.NewChannelWindow("#Solo", testTime.Add(time.Minute))
	sole.Topic = "destroyed only on commit"
	sole.Modes.InviteOnly = true
	sole.Members.Add(actor)
	require.NoError(t, s.SaveWindow(ctx, sole))

	memory := MemoryEntry{Key: "fact", Content: "likes tea", At: testTime}
	require.NoError(t, s.WriteMemory(
		ctx, actor.ID(), memory.Key, memory.Content, memory.At, memory.Pinned,
	))

	priorMessage := domain.Message{
		Source: domain.ClientSource(peer.ID(), peer.Nick()),
		Target: shared.Name(),
		Body:   "still visible",
		At:     testTime,
	}
	scrollbackIDs, err := s.AppendChannelScrollback(ctx, []ChannelScrollbackRecord{{
		InstanceID: actor.ID(), Channel: shared.Name(), Event: priorMessage,
	}})
	require.NoError(t, err)
	require.Equal(t, []int64{1}, scrollbackIDs)

	actorReply := domain.SystemNotice{Text: "actor reply", At: testTime}
	actorReplyID, err := s.AppendInstanceReply(
		ctx, actor.ID(), protocol.ChannelWindowTarget(shared.Name()), actorReply,
	)
	require.NoError(t, err)
	peerReply := domain.SystemNotice{Text: "peer DM reply", At: testTime.Add(time.Second)}
	peerReplyID, err := s.AppendInstanceReply(
		ctx, peer.ID(), protocol.DirectWindowTarget(actor.ID()), peerReply,
	)
	require.NoError(t, err)

	actorTurnEntry := ModelTurnEntry{
		Kind: ModelTurnInput,
		Data: []byte(`{"input":"actor context"}`),
		At:   testTime,
	}
	actorTurnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: actor.ID(),
		Window:     protocol.ChannelWindowTarget(shared.Name()),
		ModelID:    actor.ModelID,
		StartedAt:  testTime,
	}, actorTurnEntry)
	require.NoError(t, err)
	peerDirectTurnEntry := ModelTurnEntry{
		Kind: ModelTurnInput,
		Data: []byte(`{"input":"peer direct context"}`),
		At:   testTime.Add(time.Second),
	}
	peerDirectTurnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: peer.ID(),
		Window:     protocol.DirectWindowTarget(actor.ID()),
		ModelID:    peer.ModelID,
		StartedAt:  testTime.Add(time.Second),
	}, peerDirectTurnEntry)
	require.NoError(t, err)
	soleTurnEntry := ModelTurnEntry{
		Kind: ModelTurnInput,
		Data: []byte(`{"input":"sole channel context"}`),
		At:   testTime.Add(2 * time.Second),
	}
	soleTurnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: peer.ID(),
		Window:     protocol.ChannelWindowTarget(sole.Name()),
		ModelID:    peer.ModelID,
		StartedAt:  testTime.Add(2 * time.Second),
	}, soleTurnEntry)
	require.NoError(t, err)

	_, err = s.db.ExecContext(ctx, `
		CREATE TRIGGER reject_quit_projection
		BEFORE INSERT ON channel_scrollback
		WHEN NEW.type = 'quit' AND NEW.instance_id = 'inst-peer'
		BEGIN
			SELECT RAISE(ABORT, 'rejected quit projection');
		END
	`)
	require.NoError(t, err)

	quit := domain.Quit{
		Source:  domain.ClientSource(actor.ID(), actor.Nick()),
		Message: "gone away",
		At:      testTime.Add(3 * time.Minute),
	}
	_, commitErr := s.CommitInstanceDeletion(ctx, InstanceDeletion{
		InstanceID: actor.ID(),
		Events: []ChannelAuditEvent{
			{Channel: shared.Name(), Event: quit},
			{Channel: sole.Name(), Event: quit},
		},
		Scrollback: []ChannelScrollbackRecord{{
			InstanceID: peer.ID(), Channel: shared.Name(), Event: quit,
		}},
	})

	var rawActorJSON string
	actorReadErr := s.db.QueryRowContext(ctx,
		`SELECT data FROM instances WHERE instance_id = ?`, actor.ID(),
	).Scan(&rawActorJSON)
	var databaseActor domain.Instance
	actorDecodeErr := json.Unmarshal([]byte(rawActorJSON), &databaseActor)
	canonicalActor, canonicalActorErr := s.GetInstanceByID(ctx, actor.ID())
	storedShared, sharedErr := s.GetWindow(ctx, shared.Name())
	storedSole, soleErr := s.GetWindow(ctx, sole.Name())
	memories, memoriesErr := s.ReadMemories(ctx, actor.ID())
	pending, pendingErr := s.ListPendingMemoryDeletions(ctx)
	actorScrollback, actorScrollbackErr := s.ChannelScrollback(
		ctx, actor.ID(), shared.Name(), 10,
	)
	peerScrollback, peerScrollbackErr := s.ChannelScrollback(
		ctx, peer.ID(), shared.Name(), 10,
	)
	actorReplies, actorRepliesErr := s.InstanceRepliesBefore(ctx, actor.ID(), nil, 10)
	peerReplies, peerRepliesErr := s.InstanceRepliesBefore(ctx, peer.ID(), nil, 10)
	actorTurnEntries, actorTurnErr := s.ModelTurnEntries(ctx, actorTurnID)
	peerDirectTurnEntries, peerDirectTurnErr := s.ModelTurnEntries(ctx, peerDirectTurnID)
	soleTurnEntries, soleTurnErr := s.ModelTurnEntries(ctx, soleTurnID)
	sharedEvents, sharedEventsErr := s.EventsBefore(ctx, shared.Name(), nil, 10)
	soleEvents, soleEventsErr := s.EventsBefore(ctx, sole.Name(), nil, 10)

	type deletionState struct {
		CommitFailed              bool
		ActorReadError            error
		ActorDecodeError          error
		DatabaseActor             comparableInstance
		CanonicalActorError       error
		CanonicalActor            comparableInstance
		CanonicalPointerPreserved bool
		SharedWindowError         error
		SharedWindow              domain.Window
		SoleWindowError           error
		SoleWindow                domain.Window
		MemoriesError             error
		Memories                  []MemoryEntry
		PendingError              error
		Pending                   []domain.InstanceID
		ActorScrollbackError      error
		ActorScrollback           []domain.StoredEvent
		PeerScrollbackError       error
		PeerScrollback            []domain.StoredEvent
		ActorRepliesError         error
		ActorReplies              []InstanceReplyRecord
		PeerRepliesError          error
		PeerReplies               []InstanceReplyRecord
		ActorTurnError            error
		ActorTurn                 []ModelTurnEntry
		PeerDirectTurnError       error
		PeerDirectTurn            []ModelTurnEntry
		SoleTurnError             error
		SoleTurn                  []ModelTurnEntry
		SharedEventsError         error
		SharedEvents              []domain.StoredEvent
		SoleEventsError           error
		SoleEvents                []domain.StoredEvent
	}
	require.Equal(t, deletionState{
		CommitFailed:              true,
		DatabaseActor:             normaliseInstance(actor),
		CanonicalActor:            normaliseInstance(actor),
		CanonicalPointerPreserved: true,
		SharedWindow:              domain.Window(shared),
		SoleWindow:                domain.Window(sole),
		Memories:                  []MemoryEntry{memory},
		ActorScrollback:           []domain.StoredEvent{{ID: 1, Event: priorMessage}},
		ActorReplies: []InstanceReplyRecord{{
			ID: actorReplyID, Window: protocol.ChannelWindowTarget(shared.Name()), Event: actorReply,
		}},
		PeerReplies: []InstanceReplyRecord{{
			ID: peerReplyID, Window: protocol.DirectWindowTarget(actor.ID()), Event: peerReply,
		}},
		ActorTurn:      []ModelTurnEntry{actorTurnEntry},
		PeerDirectTurn: []ModelTurnEntry{peerDirectTurnEntry},
		SoleTurn:       []ModelTurnEntry{soleTurnEntry},
	}, deletionState{
		CommitFailed:              commitErr != nil,
		ActorReadError:            actorReadErr,
		ActorDecodeError:          actorDecodeErr,
		DatabaseActor:             normaliseInstance(&databaseActor),
		CanonicalActorError:       canonicalActorErr,
		CanonicalActor:            normaliseInstance(canonicalActor),
		CanonicalPointerPreserved: canonicalActor == actor,
		SharedWindowError:         sharedErr,
		SharedWindow:              storedShared,
		SoleWindowError:           soleErr,
		SoleWindow:                storedSole,
		MemoriesError:             memoriesErr,
		Memories:                  memories,
		PendingError:              pendingErr,
		Pending:                   pending,
		ActorScrollbackError:      actorScrollbackErr,
		ActorScrollback:           actorScrollback,
		PeerScrollbackError:       peerScrollbackErr,
		PeerScrollback:            peerScrollback,
		ActorRepliesError:         actorRepliesErr,
		ActorReplies:              actorReplies,
		PeerRepliesError:          peerRepliesErr,
		PeerReplies:               peerReplies,
		ActorTurnError:            actorTurnErr,
		ActorTurn:                 actorTurnEntries,
		PeerDirectTurnError:       peerDirectTurnErr,
		PeerDirectTurn:            peerDirectTurnEntries,
		SoleTurnError:             soleTurnErr,
		SoleTurn:                  soleTurnEntries,
		SharedEventsError:         sharedEventsErr,
		SharedEvents:              sharedEvents,
		SoleEventsError:           soleEventsErr,
		SoleEvents:                soleEvents,
	})
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

// txModeCase is one transaction opened on the store's own DSN while a
// second pooled connection writes to the same database.
type txModeCase struct {
	name string
	opts *sql.TxOptions
	// writesAfterRead says whether the transaction attempts a write of
	// its own once the second connection has had its turn. A read-only
	// transaction cannot.
	writesAfterRead bool
}

// txModeEffect records how the two connections fared. Each write is
// recorded as what actually happened, so an unexpected error is
// distinguishable from success: reducing a write to "was it SQLITE_BUSY"
// makes every other failure, including one that never inserted anything,
// look the same as a clean run.
type txModeEffect struct {
	// SecondWrite is the second connection's write: "ok", "busy", or the
	// error it failed with.
	SecondWrite string
	// HeldWrite is the transaction's own write after its read: "ok",
	// "busy_snapshot", "skipped", or the error it failed with.
	HeldWrite string
	// Committed reports whether the transaction committed.
	Committed bool
	// Keys is what the state table holds afterwards, so a write that
	// reported success is shown to have landed.
	Keys []string
}

// writeOutcome names what one write did, so the assertion compares an
// outcome and not a boolean that hides every case it does not name.
func writeOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, sqlite3.BUSY_SNAPSHOT):
		return "busy_snapshot"
	case errors.Is(err, sqlite3.BUSY):
		return "busy"
	}

	return err.Error()
}

// TestSQLitePragmaDSN_begins_write_transactions_immediately covers the
// two transaction modes the store opens. A read-write transaction takes
// the write lock at BEGIN, so a second connection writing during it is
// refused with SQLITE_BUSY, which the busy handler retries for as long
// as `busy_timeout` allows. A read-only transaction takes no write lock
// and the second connection writes straight through.
//
// Without the write lock at BEGIN, the read fixes a WAL snapshot the
// second connection's commit then moves past, and the transaction's own
// write is refused with SQLITE_BUSY_SNAPSHOT. SQLite does not invoke
// the busy handler for that code, so no `busy_timeout` recovers it and
// a read-modify-write fails outright.
//
// The DSN appends a second `busy_timeout` of zero so contention is
// reported as soon as it occurs. Both connections are taken from the
// pool before the transaction opens, because a connection opened during
// it applies `journal_mode(WAL)` and blocks on the same write lock.
func TestSQLitePragmaDSN_begins_write_transactions_immediately(t *testing.T) {
	cases := []struct {
		txModeCase

		want txModeEffect
	}{
		{
			txModeCase: txModeCase{
				name:            "read-write transaction holds the write lock",
				opts:            nil,
				writesAfterRead: true,
			},
			want: txModeEffect{
				SecondWrite: "busy",
				HeldWrite:   "ok",
				Committed:   true,
				Keys:        []string{"held", "schema_version"},
			},
		},
		{
			txModeCase: txModeCase{
				name:            "read-only transaction leaves writers alone",
				opts:            &sql.TxOptions{ReadOnly: true},
				writesAfterRead: false,
			},
			want: txModeEffect{
				SecondWrite: "ok",
				HeldWrite:   "skipped",
				Committed:   true,
				Keys:        []string{"schema_version", "second"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			dsn := SQLitePragmaDSN(filepath.Join(t.TempDir(), "txmode.db")) + "&_pragma=busy_timeout(0)"
			db, err := sql.Open("sqlite3", dsn)
			require.NoError(t, err)

			s, err := NewSQLiteStore(ctx, db)
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() })

			held, err := db.Conn(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = held.Close() })

			other, err := db.Conn(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = other.Close() })

			tx, err := held.BeginTx(ctx, tc.opts)
			require.NoError(t, err)

			var version string
			require.NoError(t, tx.QueryRowContext(ctx,
				`SELECT value FROM state WHERE key = 'schema_version'`).Scan(&version))

			_, secondErr := other.ExecContext(ctx,
				`INSERT INTO state (key, value) VALUES ('second', '1')`)

			got := txModeEffect{
				SecondWrite: writeOutcome(secondErr),
				HeldWrite:   "skipped",
			}

			if tc.writesAfterRead {
				_, writeErr := tx.ExecContext(ctx,
					`INSERT INTO state (key, value) VALUES ('held', '1')`)
				got.HeldWrite = writeOutcome(writeErr)
			}

			got.Committed = tx.Commit() == nil
			got.Keys = stateKeys(ctx, t, db)

			require.Equal(t, tc.want, got)
		})
	}
}

// stateKeys reads every key the state table holds, in order, so a test
// can show which writes landed.
func stateKeys(ctx context.Context, t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.QueryContext(ctx, `SELECT key FROM state ORDER BY key`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	keys := []string{}
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(t, rows.Err())

	return keys
}

// contendedWriteEffect is what a competing writer saw.
type contendedWriteEffect struct {
	Err  error
	Keys []string
}

// TestSQLitePragmaDSN_makes_a_contended_write_wait_and_succeed pins the
// user-visible result, which the mode test does not reach: a second
// store operation arriving during a write transaction waits for the
// lock and then completes.
//
// The mode test disables `busy_timeout` so contention is visible at all,
// and that leaves it verifying only that BEGIN IMMEDIATE takes the lock.
// A regression that made a contended writer fail rather than wait would
// pass it.
func TestSQLitePragmaDSN_makes_a_contended_write_wait_and_succeed(t *testing.T) {
	ctx := t.Context()

	db, err := sql.Open("sqlite3", SQLitePragmaDSN(filepath.Join(t.TempDir(), "contended.db")))
	require.NoError(t, err)

	s, err := NewSQLiteStore(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	held, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })

	other, err := db.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })

	tx, err := held.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `INSERT INTO state (key, value) VALUES ('held', '1')`)
	require.NoError(t, err)

	contended := make(chan error, 1)
	go func() {
		_, execErr := other.ExecContext(ctx,
			`INSERT INTO state (key, value) VALUES ('second', '1')`)
		contended <- execErr
	}()

	require.NoError(t, tx.Commit())

	got := contendedWriteEffect{Err: <-contended}
	got.Keys = stateKeys(ctx, t, db)

	require.Equal(t, contendedWriteEffect{
		Keys: []string{"held", "schema_version", "second"},
	}, got)
}

// TestWipe_removes_database_and_sidecars exercises the startup
// `--wipe` path against a path the test owns: a populated database
// file with WAL and SHM sidecars is removed, and a follow-up wipe
// over a missing file is a no-op.
func TestWipe_removes_database_and_sidecars(t *testing.T) {
	base := filepath.Join(t.TempDir(), "modeloff.db")

	for _, suffix := range []string{"", "-wal", "-shm"} {
		require.NoError(t, os.WriteFile(base+suffix, []byte("x"), 0o600))
	}

	require.NoError(t, Wipe(base))

	for _, suffix := range []string{"", "-wal", "-shm"} {
		_, err := os.Stat(base + suffix)
		require.ErrorIs(t, err, os.ErrNotExist, "file %s should be gone after wipe", base+suffix)
	}

	require.NoError(t, Wipe(base), "wipe over missing files is a no-op")
}

// --- Windows ---

func TestSQLiteStore_ListWindowsEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListWindows(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SaveAndGetWindow_channel_with_members(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	want := domain.NewChannelWindow("#general", testTime)
	want.Topic = "General chat"
	want.Members = storeTestMembers(t, s, "alice", "bob")

	require.NoError(t, s.SaveWindow(ctx, want))

	got, err := s.GetWindow(ctx, "#general")
	require.NoError(t, err)
	requireWindowEqual(t, want, got)
}

func TestSQLiteStore_ChannelRoundtripPersistsModes(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	want := domain.NewChannelWindow("#chan", testTime)
	want.Modes = domain.ChannelModes{
		TopicLock:  true,
		NoExternal: true,
		Moderated:  true,
		UserLimit:  10,
		Key:        "secret",
	}
	want.Invitations.Add("inst-alice")
	want.Invitations.Add("inst-bravo")
	want.Members = storeTestMembers(t, s, "alice", "bob")

	require.NoError(t, s.SaveWindow(ctx, want))

	got, err := s.GetWindow(ctx, "#chan")
	require.NoError(t, err)

	cw, ok := got.(*domain.ChannelWindow)
	require.True(t, ok)
	require.Equal(t, want.Modes, cw.Modes)
	require.True(t, cw.Invitations.Contains("inst-alice"))
	require.True(t, cw.Invitations.Contains("inst-bravo"))
	require.Equal(t, 2, len(cw.Invitations))
}

func TestSQLiteStore_ChannelLegacyRowHydratesZeroModes(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	// A row saved before the modes field existed has no `modes`
	// or `invitations` keys. Persist a channel without setting
	// them; the round-trip hydrates with the Go zero value.
	want := domain.NewChannelWindow("#legacy", testTime)
	want.Members = storeTestMembers(t, s, "alice")

	require.NoError(t, s.SaveWindow(ctx, want))

	got, err := s.GetWindow(ctx, "#legacy")
	require.NoError(t, err)

	cw, ok := got.(*domain.ChannelWindow)
	require.True(t, ok)
	require.Equal(t, domain.ChannelModes{}, cw.Modes)
	require.Equal(t, 0, len(cw.Invitations))
}

func TestSQLiteStore_SaveWindow_recordsSpan(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)
	ctx := t.Context()
	s := newTestStore(t).WithTracerProvider(provider)

	cw := domain.NewChannelWindow("#observability", testTime)
	cw.Members = storeTestMembers(t, s, "alice")

	require.NoError(t, s.SaveWindow(ctx, cw))

	span := oteltest.FindSpan(t, recorder, "store.sqlite.save_window")
	require.Equal(t, "store.sqlite.save_window", oteltest.AttrValue(span.Attributes(), observability.AttrOperation))
	require.Equal(t, "#observability", oteltest.AttrValue(span.Attributes(), observability.AttrChannel))
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(span.Attributes(), observability.AttrResult))
}

func TestSQLiteStore_GetWindowNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetWindow(t.Context(), "#nonexistent")
	require.ErrorIs(t, err, ErrNoSuchChannel)
}

func TestSQLiteStore_ListWindows(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	windows := []domain.Window{
		domain.NewChannelWindow("#alpha", testTime),
		domain.NewChannelWindow("#beta", testTime.Add(time.Hour)),
	}

	for _, w := range windows {
		require.NoError(t, s.SaveWindow(ctx, w))
	}

	got, err := s.ListWindows(ctx)
	require.NoError(t, err)
	requireWindowsEqual(t, windows, got)
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

// TestSQLiteStore_SaveWindow_rejects_dm pins the policy that DM
// windows are not persisted: they are pure in-memory UI state
// owned by the chat-screen sidebar cache, and `SaveWindow` is a
// programming error if called with one.
func TestSQLiteStore_SaveWindow_rejects_dm(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	err := s.SaveWindow(ctx, domain.WindowKey("id-1"))
	require.Error(t, err)
}

func TestSQLiteStore_ListWindows_mixed(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	status := domain.NewStatusWindow(testTime)
	channel := domain.NewChannelWindow("#general", testTime.Add(time.Hour))

	require.NoError(t, s.SaveWindow(ctx, status))
	require.NoError(t, s.SaveWindow(ctx, channel))

	got, err := s.ListWindows(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Window{channel, status}, got)
}

func TestSQLiteStore_DeleteWindow(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#general", testTime)))
	require.NoError(t, s.DeleteWindow(ctx, "#general"))

	_, err := s.GetWindow(ctx, "#general")
	require.ErrorIs(t, err, ErrNoSuchChannel)
}

func TestSQLiteStore_DeleteWindow_removes_model_turns_for_the_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#Dev", testTime)))
	fixtures := []protocol.WindowTarget{
		protocol.ChannelWindowTarget("#Dev"),
		protocol.ChannelWindowTarget("#other"),
		protocol.DirectWindowTarget("inst-peer"),
	}
	var turnIDs []ModelTurnID
	for _, window := range fixtures {
		turnID, err := s.BeginModelTurn(ctx, ModelTurn{
			InstanceID: "inst-botty",
			Window:     window,
			ModelID:    "test/model",
			StartedAt:  testTime,
		}, ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime})
		require.NoError(t, err)
		turnIDs = append(turnIDs, turnID)
	}

	require.NoError(t, s.DeleteWindow(ctx, "#dev"))

	got := make([][]ModelTurnEntry, 0, len(turnIDs))
	for _, turnID := range turnIDs {
		entries, err := s.ModelTurnEntries(ctx, turnID)
		require.NoError(t, err)
		got = append(got, entries)
	}
	require.Equal(t, [][]ModelTurnEntry{
		nil,
		{{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime}},
		{{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime}},
	}, got)
}

func TestSQLiteStore_SaveWindowOverwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	cw := domain.NewChannelWindow("#evolving", testTime)
	require.NoError(t, s.SaveWindow(ctx, cw))

	cw.Topic = "Updated topic"
	cw.Members = storeTestMembers(t, s, "charlie")
	require.NoError(t, s.SaveWindow(ctx, cw))

	got, err := s.GetWindow(ctx, "#evolving")
	require.NoError(t, err)
	requireWindowEqual(t, cw, got)
}

func TestSQLiteStore_SaveAndGetWindow_withTopicMetadata(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	cw := domain.NewChannelWindow("#dev", testTime)
	cw.Topic = "Go development"
	cw.TopicSetBy = "alice"
	cw.TopicSetAt = testTime
	cw.Members = storeTestMembers(t, s, "alice")

	require.NoError(t, s.SaveWindow(ctx, cw))

	got, err := s.GetWindow(ctx, "#dev")
	require.NoError(t, err)
	requireWindowEqual(t, cw, got)
}

// --- Event log ---

func TestSQLiteStore_AppendAndReadEvent(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	event := domain.Join{Source: domain.LegacyClientSource(

		"alice"), Target: "#general", At: testTime}

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

func TestSQLiteStore_ChannelScrollback_resets_membership_interval(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	const (
		channel = domain.ChannelName("#general")
		actor   = domain.InstanceID("inst-botty")
	)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow(channel, testTime)))

	appendScrollback := func(event domain.PersistableEvent) {
		t.Helper()
		_, err := s.AppendChannelScrollback(ctx, []ChannelScrollbackRecord{{
			InstanceID: actor,
			Channel:    channel,
			Event:      event,
		}})
		require.NoError(t, err)
	}

	appendScrollback(domain.Join{Target: channel, Source: domain.ClientSource(actor, "botty"), At: testTime})
	appendScrollback(domain.Message{Source: domain.LegacyClientSource("alice"), Target: channel, Body: "first interval", At: testTime})
	require.NoError(t, s.DeleteChannelScrollback(ctx, actor, channel))

	secondJoin := domain.Join{Target: channel, Source: domain.ClientSource(actor, "botty"), At: testTime}
	secondMessage := domain.Message{Source: domain.LegacyClientSource("alice"), Target: channel, Body: "second interval", At: testTime}
	appendScrollback(secondJoin)
	appendScrollback(secondMessage)

	got, err := s.ChannelScrollback(ctx, actor, channel, 100)
	require.NoError(t, err)
	require.Equal(t, []domain.PersistableEvent{secondJoin, secondMessage}, storedEvents(got))
}

func storedEvents(events []domain.StoredEvent) []domain.PersistableEvent {
	result := make([]domain.PersistableEvent, len(events))
	for i, event := range events {
		result[i] = event.Event
	}

	return result
}

// TestSQLiteStore_EventsBefore_skips_unrecognised_row pins the
// decode-resilience of the channel-log read path. An older database
// may hold rows whose type discriminator this build no longer knows
// (here a legacy `help` row). Such a row is skipped and the rest of
// the batch is returned intact, so one stale row cannot wedge a
// channel's history.
func TestSQLiteStore_EventsBefore_skips_unrecognised_row(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	legacy := `{"type":"help","data":{"channel":"#general","at":"2025-01-15T10:29:00Z"}}`
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (channel, type, data, at) VALUES (?, ?, ?, ?)`,
		"#general", "help", legacy, formatTime(testTime.Add(-time.Minute)))
	require.NoError(t, err)

	good := domain.Join{Source: domain.LegacyClientSource("alice"), Target: "#general", At: testTime}
	id, err := s.AppendEvent(ctx, "#general", good)
	require.NoError(t, err)

	got, err := s.EventsBefore(ctx, "#general", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{
		{ID: id, Event: good},
	}, got)
}

// TestSQLiteStore_DMEventsBefore_unions_both_directions pins
// the bidirectional fetch: events the user sent into the DM
// (channel = peer.ID(), instance_id = "") and events the model
// sent back (channel = "", instance_id = peer.ID()) both come
// back, ordered chronologically. Foreign DM traffic between
// two other parties does not appear in either party's view.
func TestSQLiteStore_DMEventsBefore_unions_both_directions(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const userID domain.InstanceID = ""
	const bottyID domain.InstanceID = "inst-botty"
	const helperID domain.InstanceID = "inst-helper"

	mustAppend := func(channel domain.ChannelName, evt domain.Message) int64 {
		t.Helper()
		id, err := s.AppendEvent(ctx, channel, evt)
		require.NoError(t, err)
		return id
	}

	// User → botty.
	id1 := mustAppend(domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
		"iain"), Target: domain.ChannelName(bottyID), Body: "hi", At: testTime})

	// Botty → user.
	id2 := mustAppend("", domain.Message{Source: domain.ClientSource(
		bottyID, "botty"), Target: "", Body: "hello", At: testTime.Add(time.Second)})

	// User → botty again.
	id3 := mustAppend(domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
		"iain"), Target: domain.ChannelName(bottyID), Body: "still here", At: testTime.Add(2 * time.Second)})

	// Foreign DM: helper → botty. Should not appear in the
	// user↔botty view.
	mustAppend(domain.ChannelName(bottyID), domain.Message{Source: domain.ClientSource(
		helperID, "helper"), Target: domain.ChannelName(bottyID), Body: "side chat", At: testTime.Add(3 * time.Second)})

	got, err := s.DMEventsBefore(ctx, userID, bottyID, nil, 10)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}
	require.Equal(t, []int64{id1, id2, id3}, gotIDs)
}

// TestSQLiteStore_DMEventsBefore_excludes_unobserved_actor_events
// pins that a peer's channel-scoped QUIT does not appear in a DM
// thread merely because the peer was its actor.
func TestSQLiteStore_DMEventsBefore_excludes_unobserved_actor_events(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const userID domain.InstanceID = ""
	const bottyID domain.InstanceID = "inst-botty"

	mustAppend := func(channel domain.ChannelName, evt domain.ChannelActivity) int64 {
		t.Helper()
		id, err := s.AppendEvent(ctx, channel, evt)
		require.NoError(t, err)
		return id
	}

	// User → botty.
	id1 := mustAppend(domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
		"iain"), Target: domain.ChannelName(bottyID), Body: "hi", At: testTime})

	// Botty's quit fanned out per-channel (in real life, one row
	// per channel botty was in). Persisted under different
	// `channel` columns but carrying the same Quit payload.
	quit := domain.Quit{
		Source:  domain.ClientSource(bottyID, "botty"),
		Message: "shutting down",
		At:      testTime.Add(time.Second),
	}
	mustAppend("#general", quit)
	mustAppend("#dev", quit)

	got, err := s.DMEventsBefore(ctx, userID, bottyID, nil, 10)
	require.NoError(t, err)

	gotIDs := make([]int64, len(got))
	for i, e := range got {
		gotIDs[i] = e.ID
	}

	require.Equal(t, []int64{id1}, gotIDs)
}

// TestSQLiteStore_CountDMEventsFrom pins the count behind a DM's
// unread badge. It spans both directions of the thread, which are
// logged under their recipients, and it leaves out DM traffic
// between two other parties and the peer's actor-scoped events.
func TestSQLiteStore_CountDMEventsFrom(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	const userID domain.InstanceID = ""
	const bottyID domain.InstanceID = "inst-botty"
	const helperID domain.InstanceID = "inst-helper"

	mustAppend := func(channel domain.ChannelName, evt domain.ChannelActivity) int64 {
		t.Helper()
		id, err := s.AppendEvent(ctx, channel, evt)
		require.NoError(t, err)
		return id
	}

	// User → botty.
	id1 := mustAppend(domain.ChannelName(bottyID), domain.Message{Source: domain.LegacyClientSource(
		"iain"), Target: domain.ChannelName(bottyID), Body: "hi", At: testTime})

	// Botty → user, twice.
	id2 := mustAppend("", domain.Message{Source: domain.ClientSource(
		bottyID, "botty"), Target: "", Body: "hello", At: testTime.Add(time.Second)})
	id3 := mustAppend("", domain.Message{Source: domain.ClientSource(
		bottyID, "botty"), Target: "", Body: "still here", At: testTime.Add(2 * time.Second)})

	// Helper → botty: a DM the user is not party to.
	mustAppend(domain.ChannelName(bottyID), domain.Message{Source: domain.ClientSource(
		helperID, "helper"), Target: domain.ChannelName(bottyID), Body: "side chat", At: testTime.Add(3 * time.Second)})

	// Botty's quit, which belongs to the channel it was in.
	mustAppend("#general", domain.Quit{
		Source: domain.ClientSource(bottyID, "botty"), Message: "bye", At: testTime.Add(4 * time.Second),
	})

	tests := []struct {
		name string
		from *int64
		want int
	}{
		{name: "nil counts the whole thread", from: nil, want: 3},
		{name: "cursor at the first id counts all", from: &id1, want: 3},
		{name: "cursor mid-thread counts inclusive of the cursor", from: &id2, want: 2},
		{name: "cursor past the last id counts none", from: new(id3 + 1), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.CountDMEventsFrom(ctx, userID, bottyID, tt.from)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
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

func TestSQLiteStore_CountEventsFrom(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	ids := appendTestEvents(t, s, "#general", 5)

	tests := []struct {
		name string
		from *int64
		want int
	}{
		{name: "nil counts every event in the channel", from: nil, want: 5},
		{name: "cursor at the first id counts all", from: &ids[0], want: 5},
		{name: "cursor mid-stream counts inclusive of the cursor", from: &ids[2], want: 3},
		{name: "cursor past the last id counts none", from: new(ids[4] + 1), want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.CountEventsFrom(ctx, "#general", tt.from)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestSQLiteStore_CountEventsFrom_empty_channel(t *testing.T) {
	got, err := newTestStore(t).CountEventsFrom(t.Context(), "#empty", nil)
	require.NoError(t, err)
	require.Zero(t, got)
}

func TestSQLiteStore_CountEventsFrom_isolated_by_channel(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	appendTestEvents(t, s, "#alpha", 3)
	appendTestEvents(t, s, "#beta", 2)

	gotAlpha, err := s.CountEventsFrom(ctx, "#alpha", nil)
	require.NoError(t, err)
	require.Equal(t, 3, gotAlpha)

	gotBeta, err := s.CountEventsFrom(ctx, "#beta", nil)
	require.NoError(t, err)
	require.Equal(t, 2, gotBeta)
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

	events := []domain.ChannelActivity{
		domain.Message{Source: domain.LegacyClientSource("alice"), Target: "#general", Body: "hello", At: testTime},
		domain.Join{Source: domain.LegacyClientSource("bob"), Target: "#general", At: testTime},
		domain.Part{Source: domain.LegacyClientSource("bob"), Target: "#general", At: testTime},
		domain.TopicChange{Source: domain.LegacyClientSource("alice"), Target: "#general", Topic: "new", At: testTime},
		domain.ChannelModeChange{Source: domain.LegacyClientSource("ChanServ"), Target: "#general", Subject: "bob", Flag: domain.ModeChannelVoice, Add: true, At: testTime},
		domain.Invited{Source: domain.LegacyClientSource("alice"), Target: "#general", Invitee: "botty", At: testTime},
		domain.Kicked{Source: domain.LegacyClientSource("alice"), Target: "#general", Subject: "botty", At: testTime},
		domain.NickChange{Source: domain.LegacyClientSource("bob"), NewNick: "robert", At: testTime},
	}

	for _, e := range events {
		_, err := s.AppendEvent(ctx, "#general", e)
		require.NoError(t, err)
	}

	got, err := s.EventsFrom(ctx, "#general", nil, 100)
	require.NoError(t, err)

	want := make([]domain.PersistableEvent, len(events))
	for i, e := range events {
		want[i] = e
	}

	gotEvents := make([]domain.PersistableEvent, len(got))
	for i, se := range got {
		gotEvents[i] = se.Event
	}

	require.Equal(t, want, gotEvents)
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
	pending, err := s.ListPendingMemoryDeletions(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID{"inst-temp"}, pending)

	require.NoError(t, s.DeletePendingMemoryDeletion(ctx, "inst-temp"))
	pending, err = s.ListPendingMemoryDeletions(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID(nil), pending)
}

func TestSQLiteStore_DeleteInstanceByID_does_not_queue_user_memory(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	user := domain.NewUserInstance("alice")
	require.NoError(t, s.SaveInstance(ctx, user))

	require.NoError(t, s.DeleteInstanceByID(ctx, user.ID()))
	pending, err := s.ListPendingMemoryDeletions(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID(nil), pending)
}

func TestSQLiteStore_pending_instance_deletions_are_not_active(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	active := domain.NewModelInstance("inst-active", "active", "test/model", "", nil)
	pending := domain.NewModelInstance("inst-pending", "pending", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, active))
	require.NoError(t, s.SaveInstance(ctx, pending))
	require.NoError(t, s.MarkInstancePendingDeletion(ctx, pending.ID()))

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	pendingIDs, err := s.ListPendingInstanceDeletions(ctx)
	require.NoError(t, err)
	_, byIDErr := s.GetInstanceByID(ctx, pending.ID())
	_, byNickErr := s.ResolveNick(ctx, pending.Nick())

	require.Equal(t, []comparableInstance{normaliseInstance(active)}, func() []comparableInstance {
		result := make([]comparableInstance, len(instances))
		for index, inst := range instances {
			result[index] = normaliseInstance(inst)
		}

		return result
	}())
	require.Equal(t, []domain.InstanceID{pending.ID()}, pendingIDs)
	require.ErrorIs(t, byIDErr, sql.ErrNoRows)
	require.ErrorIs(t, byNickErr, ErrNoSuchNick)

	require.NoError(t, s.DeleteInstanceByID(ctx, pending.ID()))
	pendingIDs, err = s.ListPendingInstanceDeletions(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.InstanceID(nil), pendingIDs)
}

func TestSQLiteStore_DeleteInstanceByID_removes_model_turns_with_the_actor(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	departing := domain.NewModelInstance("inst-departing", "departing", "test/model", "", nil)
	other := domain.NewModelInstance("inst-other", "other", "test/model", "", nil)
	for _, inst := range []*domain.Instance{departing, other} {
		require.NoError(t, s.SaveInstance(ctx, inst))
	}

	type turnFixture struct {
		actor  domain.InstanceID
		window protocol.WindowTarget
	}
	fixtures := []turnFixture{
		{actor: departing.ID(), window: protocol.ChannelWindowTarget("#dev")},
		{actor: other.ID(), window: protocol.DirectWindowTarget(departing.ID())},
		{actor: other.ID(), window: protocol.DirectWindowTarget("")},
	}
	var turnIDs []ModelTurnID
	for _, fixture := range fixtures {
		turnID, err := s.BeginModelTurn(ctx, ModelTurn{
			InstanceID: fixture.actor,
			Window:     fixture.window,
			ModelID:    "test/model",
			StartedAt:  testTime,
		}, ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime})
		require.NoError(t, err)
		turnIDs = append(turnIDs, turnID)
	}

	require.NoError(t, s.DeleteInstanceByID(ctx, departing.ID()))

	got := make([][]ModelTurnEntry, 0, len(turnIDs))
	for _, turnID := range turnIDs {
		entries, err := s.ModelTurnEntries(ctx, turnID)
		require.NoError(t, err)
		got = append(got, entries)
	}
	require.Equal(t, [][]ModelTurnEntry{
		nil,
		nil,
		{{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime}},
	}, got)
}

// TestSQLiteStore_WriteMemory_records_at pins that a written entry
// carries the write time its caller supplied, read back exactly.
func TestSQLiteStore_WriteMemory_records_at(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.NewModelInstance("inst-temp", "temp", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.WriteMemory(ctx, "inst-temp", "fact", "likes tea", testTime, false))

	entries, err := s.ReadMemories(ctx, "inst-temp")
	require.NoError(t, err)
	require.Equal(t, []MemoryEntry{{Key: "fact", Content: "likes tea", At: testTime}}, entries)
}

// TestSQLiteStore_WriteMemory_overwrite_updates_at pins that
// overwriting an existing key's content also updates its write time,
// so a memory touched again is fresh again for recency ordering.
func TestSQLiteStore_WriteMemory_overwrite_updates_at(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.NewModelInstance("inst-temp", "temp", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.WriteMemory(ctx, "inst-temp", "mood", "happy", testTime, false))

	later := testTime.Add(time.Hour)
	require.NoError(t, s.WriteMemory(ctx, "inst-temp", "mood", "excited", later, false))

	entries, err := s.ReadMemories(ctx, "inst-temp")
	require.NoError(t, err)
	require.Equal(t, []MemoryEntry{{Key: "mood", Content: "excited", At: later}}, entries)
}

func TestSQLiteStore_WriteMemory_preserves_and_updates_the_pin(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.NewModelInstance("inst-temp", "temp", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.WriteMemory(
		ctx, "inst-temp", "identity", "Laney uses Go", testTime, true,
	))

	entries, err := s.ReadMemories(ctx, "inst-temp")
	require.NoError(t, err)
	require.Equal(t, []MemoryEntry{{
		Key: "identity", Content: "Laney uses Go", At: testTime, Pinned: true,
	}}, entries)

	later := testTime.Add(time.Hour)
	require.NoError(t, s.WriteMemory(
		ctx, "inst-temp", "identity", "Laney uses Rust", later, false,
	))
	entries, err = s.ReadMemories(ctx, "inst-temp")
	require.NoError(t, err)
	require.Equal(t, []MemoryEntry{{
		Key: "identity", Content: "Laney uses Rust", At: later,
	}}, entries)
}

// TestSQLiteStore_ReadMemories_legacy_row_has_zero_at pins the
// backfill story for a row written before the `at` column existed: a
// direct INSERT that never sets it (mirroring a pre-migration row,
// which the migration's `DEFAULT` backfill leaves exactly this way)
// reads back as the zero time rather than failing or fabricating
// one.
func TestSQLiteStore_ReadMemories_legacy_row_has_zero_at(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.NewModelInstance("inst-temp", "temp", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, inst))
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (instance_id, key, content) VALUES (?, ?, ?)`,
		"inst-temp", "fact", "likes tea")
	require.NoError(t, err)

	entries, err := s.ReadMemories(ctx, "inst-temp")
	require.NoError(t, err)
	require.Equal(t, []MemoryEntry{{Key: "fact", Content: "likes tea", At: time.Time{}}}, entries)
}

// TestSQLiteStore_DeleteInstanceByID_removes_memories pins that
// deleting an instance also deletes its `memories` rows in the same
// operation: instance ids are never reused, so a memories row left
// behind after its owning instance is gone would never be reachable
// again.
func TestSQLiteStore_DeleteInstanceByID_removes_memories(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	inst := domain.NewModelInstance("inst-temp", "temp", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, inst))
	require.NoError(t, s.WriteMemory(ctx, "inst-temp", "fact", "likes tea", testTime, false))

	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-temp"))

	entries, err := s.ReadMemories(ctx, "inst-temp")
	require.NoError(t, err)
	require.Empty(t, entries)
}

// TestSQLiteStore_DeleteInstanceByID_leaves_other_instances_memories
// pins that the memories cleanup is scoped to the deleted instance.
func TestSQLiteStore_DeleteInstanceByID_leaves_other_instances_memories(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	gone := domain.NewModelInstance("inst-gone", "gone", "test/model", "", nil)
	kept := domain.NewModelInstance("inst-kept", "kept", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, gone))
	require.NoError(t, s.SaveInstance(ctx, kept))
	require.NoError(t, s.WriteMemory(ctx, "inst-gone", "fact", "likes tea", testTime, false))
	require.NoError(t, s.WriteMemory(ctx, "inst-kept", "fact", "likes coffee", testTime, false))

	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-gone"))

	entries, err := s.ReadMemories(ctx, "inst-kept")
	require.NoError(t, err)
	require.Equal(t, []MemoryEntry{{Key: "fact", Content: "likes coffee", At: testTime}}, entries)
}

func TestSQLiteStore_DeleteInstanceByID_removes_channel_memberships(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	gone := domain.NewModelInstance("inst-gone", "gone", "test/model", "", nil)
	kept := domain.NewModelInstance("inst-kept", "kept", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, gone))
	require.NoError(t, s.SaveInstance(ctx, kept))

	sole := domain.NewChannelWindow("#sole", testTime)
	sole.Members.Add(gone)
	sole.Modes.InviteOnly = true
	require.NoError(t, s.SaveWindow(ctx, sole))

	shared := domain.NewChannelWindow("#shared", testTime)
	shared.Topic = "still here"
	shared.Members.Add(gone)
	shared.Members.Add(kept)
	require.NoError(t, s.SaveWindow(ctx, shared))
	soleTurn, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: kept.ID(), Window: protocol.ChannelWindowTarget(sole.Name()),
		ModelID: kept.ModelID, StartedAt: testTime,
	}, ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"sole"}`), At: testTime})
	require.NoError(t, err)
	sharedTurn, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: kept.ID(), Window: protocol.ChannelWindowTarget(shared.Name()),
		ModelID: kept.ModelID, StartedAt: testTime,
	}, ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"shared"}`), At: testTime})
	require.NoError(t, err)

	require.NoError(t, s.DeleteInstanceByID(ctx, gone.ID()))

	_, err = s.GetWindow(ctx, sole.Name())
	require.ErrorIs(t, err, ErrNoSuchChannel)
	got, err := s.GetWindow(ctx, shared.Name())
	require.NoError(t, err)
	soleEntries, err := s.ModelTurnEntries(ctx, soleTurn)
	require.NoError(t, err)
	sharedEntries, err := s.ModelTurnEntries(ctx, sharedTurn)
	require.NoError(t, err)
	want := domain.NewChannelWindow("#shared", testTime)
	want.Topic = "still here"
	want.Members.Add(kept)
	type assertionSnapshot struct {
		Window        domain.Window
		SoleEntries   []ModelTurnEntry
		SharedEntries []ModelTurnEntry
	}

	require.Equal(t, assertionSnapshot{
		Window: domain.Window(want),
		SharedEntries: []ModelTurnEntry{{
			Kind: ModelTurnInput, Data: []byte(`{"input":"shared"}`), At: testTime,
		}},
	}, assertionSnapshot{
		Window:        got,
		SoleEntries:   soleEntries,
		SharedEntries: sharedEntries,
	})
}

func TestSQLiteStore_DeleteInstanceByID_removes_channel_shadow_spellings(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	gone := domain.NewModelInstance("inst-gone", "gone", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, gone))
	canonical := domain.NewChannelWindow("#Dev", testTime)
	canonical.Members.Add(gone)
	require.NoError(t, s.SaveWindow(ctx, canonical))
	shadow := domain.NewChannelWindow("#dev", testTime.Add(time.Hour))
	shadow.Topic = "stale topic"
	shadow.Modes.InviteOnly = true
	require.NoError(t, s.SaveWindow(ctx, shadow))

	require.NoError(t, s.DeleteInstanceByID(ctx, gone.ID()))

	_, err := s.GetWindow(ctx, "#Dev")
	require.ErrorIs(t, err, ErrNoSuchChannel)
}

func TestSQLiteStore_DeleteInstanceByID_preserves_a_live_channel_beside_an_empty_shadow(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	gone := domain.NewModelInstance("inst-gone", "gone", "test/model", "", nil)
	kept := domain.NewModelInstance("inst-kept", "kept", "test/model", "", nil)
	require.NoError(t, s.SaveInstance(ctx, gone))
	require.NoError(t, s.SaveInstance(ctx, kept))
	canonical := domain.NewChannelWindow("#Dev", testTime)
	canonical.Topic = "live topic"
	canonical.Members.Add(gone)
	canonical.Members.Add(kept)
	require.NoError(t, s.SaveWindow(ctx, canonical))
	shadow := domain.NewChannelWindow("#dev", testTime.Add(time.Hour))
	shadow.Topic = "stale topic"
	shadow.Members.Add(gone)
	require.NoError(t, s.SaveWindow(ctx, shadow))

	require.NoError(t, s.DeleteInstanceByID(ctx, gone.ID()))

	got, err := s.GetWindow(ctx, "#dev")
	require.NoError(t, err)
	want := domain.NewChannelWindow("#Dev", testTime)
	want.Topic = "live topic"
	want.Members.Add(kept)
	require.Equal(t, domain.Window(want), got)

	dbRows, err := s.db.QueryContext(ctx,
		`SELECT name FROM channels WHERE name = ? COLLATE NOCASE ORDER BY name`, "#Dev")
	require.NoError(t, err)
	t.Cleanup(func() { _ = dbRows.Close() })
	var names []string
	for dbRows.Next() {
		var name string
		require.NoError(t, dbRows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, dbRows.Err())
	require.Equal(t, []string{"#Dev"}, names)
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

func TestSQLiteStore_GetWindow_drops_dead_member_references(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	cw := domain.NewChannelWindow("#dev", testTime)
	cw.Members = storeTestMembers(t, s, "alice", "bob", "carol")
	require.NoError(t, s.SaveWindow(ctx, cw))

	// Delete adjacent backing instances. The channel membership record
	// still references both ids, but neither instance row remains.
	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-bob"))
	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-carol"))

	got, err := s.GetWindow(ctx, "#dev")
	require.NoError(t, err)

	// The surviving member is alice; both dead stubs are dropped. Compare
	// the nick snapshots so the assertion doesn't depend on pointer
	// identity of the canonical handles.
	gotChannel, ok := got.(*domain.ChannelWindow)
	require.True(t, ok)

	gotNicks := make([]domain.Nick, 0, gotChannel.Members.Len())
	for m := range gotChannel.Members.All() {
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

	addresses := func(insts []*domain.Instance) []uintptr {
		out := make([]uintptr, len(insts))
		for i, inst := range insts {
			out[i] = reflect.ValueOf(inst).Pointer()
		}

		return out
	}

	require.Equal(t, addresses(got), addresses(got2),
		"ListInstances must return the same canonical pointer for each instance across calls")
}

// --- Last window state ---

func TestSQLiteStore_GetLastWindowAbsent(t *testing.T) {
	s := newTestStore(t)

	got, err := s.GetLastWindow(t.Context())
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSQLiteStore_SetAndGetLastWindow(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastWindow(ctx, domain.WindowKey("#general")))

	got, err := s.GetLastWindow(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.WindowKey("#general"), got)
}

func TestSQLiteStore_SetLastWindow_preserves_an_empty_self_DM(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SetLastWindow(ctx, domain.WindowKey("#first")))
	require.NoError(t, s.SetLastWindow(ctx, domain.WindowKey("")))

	got, err := s.GetLastWindow(ctx)
	require.NoError(t, err)
	require.Equal(t, domain.WindowKey(""), got)

	require.NoError(t, s.ClearLastWindow(ctx))

	got, err = s.GetLastWindow(ctx)
	require.NoError(t, err)
	require.Nil(t, got)
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

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow(ch, testTime)))

	id, err := s.AppendEvent(ctx, ch, domain.Join{Source: domain.LegacyClientSource(
		"testuser"), Target: ch, At: testTime})
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
	id2, err := s.AppendEvent(ctx, "#general", domain.Message{Source: domain.LegacyClientSource(
		"alice"), Target: "#general", Body: "hello", At: testTime})
	require.NoError(t, err)

	require.NoError(t, s.SetLastRead(ctx, "#general", 1))
	require.NoError(t, s.SetLastRead(ctx, "#general", id2))

	got, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Equal(t, id2, got)
}

// seedDMWithEvent saves peer's instance row and appends one message
// from it, returning the message's event id. dm_last_read.instance_id
// references instances(instance_id), so the instance has to exist
// before a cursor can be recorded against it.
func seedDMWithEvent(t *testing.T, s *SQLiteStore, peer domain.InstanceID) int64 {
	t.Helper()
	ctx := t.Context()

	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance(peer, domain.Nick(peer), "test/model", "", nil),
	))

	id, err := s.AppendEvent(ctx, domain.ChannelName(peer), domain.Message{Source: domain.LegacyClientSource(
		"testuser"), Target: domain.ChannelName(peer), Body: "hi", At: testTime})
	require.NoError(t, err)

	return id
}

func TestSQLiteStore_GetDMLastReadEmpty(t *testing.T) {
	got, err := newTestStore(t).GetDMLastRead(t.Context(), "inst-botty")
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}

func TestSQLiteStore_SetAndGetDMLastRead(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	eventID := seedDMWithEvent(t, s, "inst-botty")
	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", eventID))

	got, err := s.GetDMLastRead(ctx, "inst-botty")
	require.NoError(t, err)
	require.Equal(t, eventID, got)
}

func TestSQLiteStore_SetDMLastRead_independent_per_peer(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	id1 := seedDMWithEvent(t, s, "inst-botty")
	id2 := seedDMWithEvent(t, s, "inst-helper")

	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", id1))
	require.NoError(t, s.SetDMLastRead(ctx, "inst-helper", id2))

	b, err := s.GetDMLastRead(ctx, "inst-botty")
	require.NoError(t, err)
	require.Equal(t, id1, b)

	h, err := s.GetDMLastRead(ctx, "inst-helper")
	require.NoError(t, err)
	require.Equal(t, id2, h)
}

func TestSQLiteStore_SetDMLastRead_overwrites(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	seedDMWithEvent(t, s, "inst-botty")
	// Append a second event to get a different ID.
	id2, err := s.AppendEvent(ctx, "inst-botty", domain.Message{Source: domain.ClientSource(
		"inst-botty", "botty"), Target: "inst-botty", Body: "again", At: testTime})
	require.NoError(t, err)

	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", 1))
	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", id2))

	got, err := s.GetDMLastRead(ctx, "inst-botty")
	require.NoError(t, err)
	require.Equal(t, id2, got)
}

// TestSQLiteStore_SetDMLastRead_requires_existing_instance pins the
// same referential-integrity rule dm_windows already enforces: a
// cursor cannot be recorded against a counterpart the store has never
// seen.
func TestSQLiteStore_SetDMLastRead_requires_existing_instance(t *testing.T) {
	err := newTestStore(t).SetDMLastRead(t.Context(), "inst-ghost", 1)
	require.Error(t, err)
}

// TestSQLiteStore_DeleteInstanceByID_cascades_dm_last_read pins that
// deleting a model instance drops its DM read cursor too, via
// dm_last_read.instance_id's ON DELETE CASCADE — the same guarantee
// TestSQLiteStore_DeleteInstanceByID_cascades_dm_window already gives
// dm_windows, so a deleted counterpart never leaves a stale cursor
// behind.
func TestSQLiteStore_DeleteInstanceByID_cascades_dm_last_read(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	eventID := seedDMWithEvent(t, s, "inst-botty")
	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", eventID))

	require.NoError(t, s.DeleteInstanceByID(ctx, "inst-botty"))

	got, err := s.GetDMLastRead(ctx, "inst-botty")
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}

// --- Reset ---

func TestSQLiteStore_Reset(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#general", testTime)))
	eventID, err := s.AppendEvent(ctx, "#general", domain.Join{Source: domain.LegacyClientSource(
		"alice"), Target: "#general", At: testTime})
	require.NoError(t, err)
	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
	))
	require.NoError(t, s.SetLastWindow(ctx, domain.WindowKey("#general")))
	require.NoError(t, s.SetLastRead(ctx, "#general", eventID))
	_, err = s.AppendInstanceReply(ctx, "inst-botty", protocol.ChannelWindowTarget("#general"), domain.Whois{At: testTime})
	require.NoError(t, err)
	turnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: "inst-botty",
		Window:     protocol.ChannelWindowTarget("#general"),
		ModelID:    "test/model",
		StartedAt:  testTime,
	}, ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime})
	require.NoError(t, err)
	require.NoError(t, s.AddDMWindow(ctx, "inst-botty"))
	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", eventID))

	require.NoError(t, s.Reset(ctx))

	windows, err := s.ListWindows(ctx)
	require.NoError(t, err)
	require.Empty(t, windows)

	events, err := s.EventsBefore(ctx, "#general", nil, 10)
	require.NoError(t, err)
	require.Empty(t, events)

	instances, err := s.ListInstances(ctx)
	require.NoError(t, err)
	require.Empty(t, instances)

	lastWindow, err := s.GetLastWindow(ctx)
	require.NoError(t, err)
	require.Nil(t, lastWindow)

	lastRead, err := s.GetLastRead(ctx, "#general")
	require.NoError(t, err)
	require.Empty(t, lastRead)

	dmLastRead, err := s.GetDMLastRead(ctx, "inst-botty")
	require.NoError(t, err)
	require.Empty(t, dmLastRead)

	replies, err := s.InstanceRepliesBefore(ctx, "inst-botty", nil, 10)
	require.NoError(t, err)
	require.Empty(t, replies)

	turnEntries, err := s.ModelTurnEntries(ctx, turnID)
	require.NoError(t, err)
	require.Empty(t, turnEntries)

	dmWindows, err := s.ListDMWindows(ctx)
	require.NoError(t, err)
	require.Empty(t, dmWindows)
}

// TestSQLiteStore_Reset_invalidates_instance_registry pins that
// Reset clears the canonical instance-pointer registry, not just the
// `instances` table: a `SaveInstance` after Reset for an id that
// existed before must hand back a fresh handle, never the pre-Reset
// pointer, since that pointer's in-memory state (nick, persona,
// channels) belongs to an instance Reset just erased.
func TestSQLiteStore_Reset_invalidates_instance_registry(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	before := domain.NewModelInstance("inst-botty", "botty", "test/model", "original", nil)
	require.NoError(t, s.SaveInstance(ctx, before))

	require.NoError(t, s.Reset(ctx))

	after := domain.NewModelInstance("inst-botty", "botty", "test/model", "rebuilt", nil)
	require.NoError(t, s.SaveInstance(ctx, after))

	got, err := s.GetInstanceByID(ctx, "inst-botty")
	require.NoError(t, err)
	require.NotSame(t, before, got)
	require.Same(t, after, got)
	require.Equal(t, "rebuilt", got.Persona())
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

// --- Persona templates ---

func TestSQLiteStore_ListPersonaTemplatesEmpty(t *testing.T) {
	s := newTestStore(t)

	got, err := s.ListPersonaTemplates(t.Context())
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_SaveAndGetPersona(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	p := domain.PersonaTemplate{
		ID:          "grumpy-sysadmin",
		Description: "A grumpy sysadmin who has seen it all.",
		Origin:      domain.PersonaGenerated,
	}

	require.NoError(t, s.SavePersonaTemplate(ctx, p))

	got, err := s.GetPersonaTemplate(ctx, "grumpy-sysadmin")
	require.NoError(t, err)
	require.Equal(t, p, got)
}

func TestSQLiteStore_GetPersonaNotFound(t *testing.T) {
	s := newTestStore(t)

	_, err := s.GetPersonaTemplate(t.Context(), "ghost")
	require.Error(t, err)
}

func TestSQLiteStore_SavePersona_upsert(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	original := domain.PersonaTemplate{
		ID:          "the-optimist",
		Description: "Always looks on the bright side.",
		Origin:      domain.PersonaGenerated,
	}

	require.NoError(t, s.SavePersonaTemplate(ctx, original))

	updated := domain.PersonaTemplate{
		ID:          "the-optimist",
		Description: "Relentlessly positive.",
		Origin:      domain.PersonaUser,
	}

	require.NoError(t, s.SavePersonaTemplate(ctx, updated))

	got, err := s.GetPersonaTemplate(ctx, "the-optimist")
	require.NoError(t, err)
	require.Equal(t, updated, got)
}

func TestSQLiteStore_ListPersonaTemplates_ordered(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	templates := []domain.PersonaTemplate{
		{ID: "alpha", Description: "First", Origin: domain.PersonaUser},
		{ID: "beta", Description: "Second", Origin: domain.PersonaGenerated},
		{ID: "gamma", Description: "Third", Origin: domain.PersonaGenerated},
	}

	for _, p := range templates {
		require.NoError(t, s.SavePersonaTemplate(ctx, p))
	}

	got, err := s.ListPersonaTemplates(ctx)
	require.NoError(t, err)
	require.Equal(t, templates, got)
}

func TestSQLiteStore_DeletePersonaTemplatesByOrigin(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	templates := []domain.PersonaTemplate{
		{ID: "gen-one", Description: "Generated one", Origin: domain.PersonaGenerated},
		{ID: "gen-two", Description: "Generated two", Origin: domain.PersonaGenerated},
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
	}

	for _, p := range templates {
		require.NoError(t, s.SavePersonaTemplate(ctx, p))
	}

	require.NoError(t, s.DeletePersonaTemplatesByOrigin(ctx, domain.PersonaGenerated))

	got, err := s.ListPersonaTemplates(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.PersonaTemplate{
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
	}, got)
}

func TestSQLiteStore_ReplaceGeneratedPersonaTemplates(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	initial := []domain.PersonaTemplate{
		{ID: "gen-one", Description: "Generated one", Origin: domain.PersonaGenerated},
		{ID: "gen-two", Description: "Generated two", Origin: domain.PersonaGenerated},
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
	}

	for _, p := range initial {
		require.NoError(t, s.SavePersonaTemplate(ctx, p))
	}

	replacements := []domain.PersonaTemplate{
		{ID: "new-a", Description: "New A", Origin: domain.PersonaGenerated},
		{ID: "new-b", Description: "New B", Origin: domain.PersonaGenerated},
		{ID: "new-c", Description: "New C", Origin: domain.PersonaGenerated},
	}

	require.NoError(t, s.ReplaceGeneratedPersonaTemplates(ctx, replacements))

	got, err := s.ListPersonaTemplates(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.PersonaTemplate{
		{ID: "custom", Description: "User custom", Origin: domain.PersonaUser},
		{ID: "new-a", Description: "New A", Origin: domain.PersonaGenerated},
		{ID: "new-b", Description: "New B", Origin: domain.PersonaGenerated},
		{ID: "new-c", Description: "New C", Origin: domain.PersonaGenerated},
	}, got)
}

func TestSQLiteStore_DeletePersonaTemplatesByOrigin_noop_when_none(t *testing.T) {
	s := newTestStore(t)

	require.NoError(t, s.DeletePersonaTemplatesByOrigin(t.Context(), domain.PersonaGenerated))
}

func TestSQLiteStore_Reset_includes_persona_templates(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	require.NoError(t, s.SavePersonaTemplate(ctx, domain.PersonaTemplate{
		ID: "test", Description: "Test persona", Origin: domain.PersonaUser,
	}))

	require.NoError(t, s.Reset(ctx))

	got, err := s.ListPersonaTemplates(ctx)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSQLiteStore_model_turn_journal_preserves_entry_order(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	input := ModelTurnEntry{Kind: ModelTurnInput, Data: []byte(`{"input":"hello"}`), At: testTime}
	turnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: "inst-botty",
		Window:     protocol.ChannelWindowTarget("#dev"),
		ModelID:    "test/model",
		StartedAt:  testTime,
	}, input)
	require.NoError(t, err)

	followingEntries := []ModelTurnEntry{
		{Kind: ModelTurnAssistant, Data: []byte(`{"tools":["msg"]}`), At: testTime.Add(time.Second)},
		{Kind: ModelTurnToolResults, Data: []byte(`{"ok":true}`), At: testTime.Add(2 * time.Second)},
		{Kind: ModelTurnOutcome, Data: []byte(`{"pass_reason":""}`), At: testTime.Add(3 * time.Second)},
	}
	for _, entry := range followingEntries {
		require.NoError(t, s.AppendModelTurnEntry(ctx, turnID, entry))
	}
	entries := append([]ModelTurnEntry{input}, followingEntries...)

	got, err := s.ModelTurnEntries(ctx, turnID)
	require.NoError(t, err)
	require.Equal(t, entries, got)
}

func TestSQLiteStore_DeleteModelTurnsForWindow_is_actor_scoped(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)

	type turnFixture struct {
		actor  domain.InstanceID
		window protocol.WindowTarget
	}

	fixtures := []turnFixture{
		{actor: "inst-botty", window: protocol.ChannelWindowTarget("#dev")},
		{actor: "inst-botty", window: protocol.ChannelWindowTarget("#other")},
		{actor: "inst-other", window: protocol.ChannelWindowTarget("#dev")},
		{actor: "inst-botty", window: protocol.DirectWindowTarget("inst-peer")},
	}
	for _, fixture := range fixtures {
		_, err := s.BeginModelTurn(ctx, ModelTurn{
			InstanceID: fixture.actor,
			Window:     fixture.window,
			ModelID:    "test/model",
			StartedAt:  testTime,
		}, ModelTurnEntry{
			Kind: ModelTurnInput,
			Data: []byte(`{"input":"hello"}`),
			At:   testTime,
		})
		require.NoError(t, err)
	}

	require.NoError(t, s.DeleteModelTurnsForWindow(
		ctx,
		"inst-botty",
		protocol.ChannelWindowTarget("#dev"),
	))

	got := dumpTable(t, s.db, `
		SELECT instance_id, window_kind, window_key
		FROM model_turns
		ORDER BY instance_id, window_kind, window_key
	`)
	require.Equal(t, []string{
		"instance_id=inst-botty|window_kind=1|window_key=#other",
		"instance_id=inst-botty|window_kind=2|window_key=inst-peer",
		"instance_id=inst-other|window_kind=1|window_key=#dev",
	}, got)

	entryCount := dumpTable(t, s.db, `SELECT count(*) FROM model_turn_entries`)
	require.Equal(t, []string{"count(*)=3"}, entryCount)
}

func TestSQLiteStore_AppendModelTurnEntry_refuses_a_deleted_turn(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	window := protocol.ChannelWindowTarget("#dev")

	turnID, err := s.BeginModelTurn(ctx, ModelTurn{
		InstanceID: "inst-botty",
		Window:     window,
		ModelID:    "test/model",
		StartedAt:  testTime,
	}, ModelTurnEntry{
		Kind: ModelTurnInput,
		Data: []byte(`{"input":"hello"}`),
		At:   testTime,
	})
	require.NoError(t, err)

	require.NoError(t, s.DeleteModelTurnsForWindow(ctx, "inst-botty", window))
	err = s.AppendModelTurnEntry(ctx, turnID, ModelTurnEntry{
		Kind: ModelTurnOutcome,
		Data: []byte(`{"pass_reason":""}`),
		At:   testTime.Add(time.Second),
	})
	require.ErrorIs(t, err, ErrModelTurnClosed)

	got, err := s.ModelTurnEntries(ctx, turnID)
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

	require.NoError(t, s.SaveWindow(ctx, domain.NewChannelWindow("#general", testTime)))
	eventID, err := s.AppendEvent(ctx, "#general", domain.Join{Source: domain.LegacyClientSource(
		"alice"), Target: "#general", At: testTime})
	require.NoError(t, err)
	require.NoError(t, s.SaveInstance(ctx,
		domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil),
	))
	require.NoError(t, s.SetLastWindow(ctx, domain.WindowKey("#general")))
	require.NoError(t, s.SetLastRead(ctx, "#general", eventID))
	require.NoError(t, s.SavePersonaTemplate(ctx, domain.PersonaTemplate{
		ID:          "grumpy-sysadmin",
		Description: "A grumpy sysadmin who has seen it all.",
		Origin:      domain.PersonaGenerated,
	}))
	require.NoError(t, s.SetAutojoinChannels(ctx, []domain.ChannelName{"#general"}))
	_, err = s.AppendInstanceReply(ctx, "inst-botty", protocol.ChannelWindowTarget("#general"), domain.Whois{At: testTime})
	require.NoError(t, err)
	require.NoError(t, s.AddDMWindow(ctx, "inst-botty"))
	require.NoError(t, s.SetDMLastRead(ctx, "inst-botty", eventID))

	// `memories` is one of the tables Reset deletes from; dropping it
	// after seeding the others guarantees the DELETE targeting it
	// fails with "no such table" partway through Reset's transaction,
	// which must roll back every DELETE that ran before it.
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
	// other tables Reset touches.
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
		event := domain.Message{Source: domain.LegacyClientSource(

			"alice"), Target: ch, Body: "message", At: testTime.Add(time.Duration(i) * time.Second)}

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
		"last_read":                `SELECT * FROM last_read`,
		"channels":                 `SELECT * FROM channels`,
		"events":                   `SELECT * FROM events`,
		"dm_windows":               `SELECT * FROM dm_windows`,
		"dm_last_read":             `SELECT * FROM dm_last_read`,
		"instance_replies":         `SELECT * FROM instance_replies`,
		"model_turn_entries":       `SELECT * FROM model_turn_entries`,
		"model_turns":              `SELECT * FROM model_turns`,
		"pending_memory_deletions": `SELECT * FROM pending_memory_deletions`,
		"instances":                `SELECT * FROM instances`,
		"memories":                 `SELECT * FROM memories`,
		"personas":                 `SELECT * FROM personas`,
		"state":                    `SELECT * FROM state`,
		"autojoin":                 `SELECT * FROM autojoin`,
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
