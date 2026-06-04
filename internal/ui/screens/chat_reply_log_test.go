package screens

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// replyLogFixture is a chat-screen over a session whose reply log the
// test reads back to assert the user-client's durable writes.
type replyLogFixture struct {
	screen ChatScreen
	sess   *session.Session
}

func newReplyLogFixture(t *testing.T) replyLogFixture {
	t.Helper()

	s := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(t, s, stubAPI{}, nil, nil, "", "", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindStatus)
	require.NoError(t, err)

	*screen.active = "#general"

	return replyLogFixture{screen: screen, sess: sess}
}

// userReplies reads the user-client's reply log, keyed by its empty
// identity, with each event's timestamp zeroed so callers compare
// against a wall-clock-free expected value.
func userReplies(t *testing.T, sess *session.Session) []domain.PersistableEvent {
	t.Helper()

	stored, err := sess.InstanceRepliesBefore(t.Context(), domain.InstanceID(protocol.UserClientID), nil, 100)
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
	case domain.PersonasList:
		e.At = time.Time{}
		return e
	default:
		return ev
	}
}

func TestChatScreen_PersonasList_persists_to_user_reply_log(t *testing.T) {
	f := newReplyLogFixture(t)

	personas := []domain.Persona{
		{ID: "p1", Description: "first", Origin: domain.PersonaGenerated},
	}

	_, cmd := f.screen.Update(chatcmd.PersonasListResult(personas))
	collectMsgs(cmd)

	require.Equal(t, []domain.PersistableEvent{
		domain.PersonasList{Personas: personas},
	}, userReplies(t, f.sess))
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
	}, userReplies(t, f.sess))
}

func TestChatScreen_ConfigSet_persists_nothing_to_user_reply_log(t *testing.T) {
	f := newReplyLogFixture(t)

	_, cmd := f.screen.Update(chatcmd.SmallModelSetResult{ModelID: "test/model"})
	collectMsgs(cmd)

	require.Empty(t, userReplies(t, f.sess))
}
