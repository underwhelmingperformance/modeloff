package screens

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// replyLogFixture is a chat-screen over a session whose reply log the
// test reads back to assert the user-client's durable writes.
type replyLogFixture struct {
	screen ChatScreen
	store  *storemod.SQLiteStore
}

func newReplyLogFixture(t *testing.T) replyLogFixture {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(t, s, stubAPI{}, nil, nil, "", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Time{})))
	screen, _ = screen.focus("#general")

	return replyLogFixture{screen: screen, store: s}
}

// userReplies reads the user-client's reply log, keyed by its empty
// identity, with each event's timestamp zeroed so callers compare
// against a wall-clock-free expected value.
func userReplies(t *testing.T, store *storemod.SQLiteStore) []domain.PersistableEvent {
	t.Helper()

	stored, err := store.InstanceRepliesBefore(t.Context(), domain.InstanceID(protocol.UserClientID), nil, 100)
	require.NoError(t, err)

	out := make([]domain.PersistableEvent, len(stored))
	for i, ev := range stored {
		out[i] = withoutAt(ev.Event)
	}

	return out
}

// withoutAt returns the persistable event with its timestamp zeroed.
func withoutAt(ev domain.PersistableEvent) domain.PersistableEvent {
	switch e := ev.(type) {
	case domain.CommandError:
		e.At = time.Time{}
		return e
	default:
		return ev
	}
}

func TestChatScreen_CommandError_persists_to_user_reply_log(t *testing.T) {
	f := newReplyLogFixture(t)

	_, cmd := f.screen.Update(domain.ErrorEvent{
		Operation: "whois",
		Err:       errors.New("no such nick"),
		At:        time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC),
	})
	collectMsgs(cmd)

	require.Equal(t, []domain.PersistableEvent{
		domain.CommandError{Target: "#general", Err: "whois: no such nick"},
	}, userReplies(t, f.store))
}

func TestChatScreen_CommandError_preserves_the_landing_window_kind(t *testing.T) {
	at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		prepare    func(*ChatScreen)
		target     domain.ChannelName
		wantWindow protocol.WindowTarget
	}{
		{
			name:   "status is global",
			target: domain.StatusChannelName,
		},
		{
			name: "self direct message retains the empty peer id",
			prepare: func(screen *ChatScreen) {
				screen.channels.Insert(newWindow(newDMWindow("", "testuser", time.Time{})))
			},
			wantWindow: protocol.DirectWindowTarget(""),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newReplyLogFixture(t)
			if tt.prepare != nil {
				tt.prepare(&f.screen)
			}

			_, cmd := f.screen.handleErrorEvent(domain.ErrorEvent{
				Operation: "whois", Err: errors.New("failed"), Target: tt.target, At: at,
			})
			collectMsgs(cmd)

			got, err := f.store.InstanceRepliesBefore(t.Context(), "", nil, 10)
			require.NoError(t, err)
			require.Equal(t, []storemod.InstanceReplyRecord{{
				ID: 1, Window: tt.wantWindow,
				Event: domain.CommandError{Target: tt.target, Err: "whois: failed", At: at},
			}}, got)
		})
	}
}

func TestChatScreen_CommandError_preserves_a_closed_issuing_window(t *testing.T) {
	f := newReplyLogFixture(t)
	issuing, open := f.screen.windowByName("#general")
	require.True(t, open)
	f.screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))
	f.screen, _ = f.screen.focus("#other")
	next, closeCmd := f.screen.closeWindow("#general", time.Now())
	f.screen = next
	collectMsgs(closeCmd)
	f.screen.channels.Insert(newWindow(domain.NewChannelWindow("#general", time.Now())))
	at := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	updated, cmd, handled := f.screen.routeReplies(chatcmd.CommandResult{
		IssuingWindow: issuing.Window,
		Message: chatcmd.CommandErrorResult{Error: domain.ErrorEvent{
			Operation: "topic", Err: errors.New("store unavailable"),
			Target: "#general", At: at,
		}},
	})
	collectMsgs(cmd)

	stored, err := f.store.InstanceRepliesBefore(t.Context(), "", nil, 10)
	require.NoError(t, err)
	type assertionSnapshot struct {
		Handled bool
		Active  domain.ChannelName
		Stored  []storemod.InstanceReplyRecord
	}

	require.Equal(t, assertionSnapshot{
		Handled: true,
		Active:  "#other",
		Stored: []storemod.InstanceReplyRecord{{
			ID:     1,
			Window: protocol.ChannelWindowTarget("#general"),
			Event: domain.CommandError{
				Target: "#other", Err: "topic: store unavailable", At: at,
			},
		}},
	}, assertionSnapshot{
		Handled: handled,
		Active:  updated.activeName(),
		Stored:  stored,
	})
}

func TestChatScreen_ConfigSet_persists_nothing_to_user_reply_log(t *testing.T) {
	f := newReplyLogFixture(t)

	_, cmd := f.screen.Update(chatcmd.SmallModelSetResult{ModelID: "test/model"})
	collectMsgs(cmd)

	require.Empty(t, userReplies(t, f.store))
}
