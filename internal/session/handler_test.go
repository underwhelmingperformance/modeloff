package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// closedProtocolEvents is a shared closed channel used as the
// inert `Events()` return value for every [fakeClient]. Reusing
// one channel avoids per-call allocation across test rows.
var closedProtocolEvents = func() <-chan protocol.Event {
	ch := make(chan protocol.Event)
	close(ch)
	return ch
}()

// fakeClient is a minimal in-test [protocol.Client] for handler
// tests. The dispatcher only reads `Identity()` and `HasMode()`, so
// `Send` and `Events` are inert satisfiers of the interface.
type fakeClient struct {
	id    protocol.ClientID
	modes map[protocol.UserMode]struct{}
}

func newOperatorClient(id protocol.ClientID) *fakeClient {
	return &fakeClient{
		id:    id,
		modes: map[protocol.UserMode]struct{}{protocol.ModeOperator: {}},
	}
}

func newPlainClient(id protocol.ClientID) *fakeClient {
	return &fakeClient{id: id}
}

func (c *fakeClient) Identity() protocol.ClientID { return c.id }

func (c *fakeClient) Send(_ context.Context, _ protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}

func (c *fakeClient) Events() <-chan protocol.Event { return closedProtocolEvents }

func (c *fakeClient) HasMode(m protocol.UserMode) bool {
	_, ok := c.modes[m]
	return ok
}

// TestSession_Handle_delegates exercises every concrete
// [protocol.Command] variant against a real session and asserts
// `Handle` returns the empty success [protocol.Response] (or the
// stub error for the four "not yet implemented" cases). For
// commands that delegate to an existing `*As` method, `setup`
// builds preconditions and `verify` confirms the delegation took
// effect.
func TestSession_Handle_delegates(t *testing.T) {
	type tc struct {
		name    string
		setup   func(t *testing.T, sess *Session, s *storemod.SQLiteStore)
		client  func() protocol.Client
		cmd     protocol.Command
		want    protocol.Response
		wantErr error
		verify  func(t *testing.T, sess *Session, s *storemod.SQLiteStore)
	}

	userClient := func() protocol.Client { return newOperatorClient(protocol.UserClientID) }

	cases := []tc{
		{
			name:   "join creates and joins channel",
			client: userClient,
			cmd:    protocol.Join{Channel: "#general"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				_, ok := sess.user.Channels().Get("#general")
				require.True(t, ok)
			},
		},
		{
			name: "part removes user from channel",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.JoinAs(t.Context(), sess.user, "#general"))
			},
			client: userClient,
			cmd:    protocol.Part{Channel: "#general", Reason: "bye"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				_, ok := sess.user.Channels().Get("#general")
				require.False(t, ok)
			},
		},
		{
			name: "privmsg sends to channel",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.JoinAs(t.Context(), sess.user, "#general"))
			},
			client: userClient,
			cmd:    protocol.PrivMsg{Target: "#general", Body: "hello"},
			want:   protocol.Response{},
			verify: func(t *testing.T, _ *Session, s *storemod.SQLiteStore) {
				require.Equal(t, []string{"join", "mode_change", "message"}, channelEventTypes(t, s, "#general"))
			},
		},
		{
			name: "action sends action message to channel",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.JoinAs(t.Context(), sess.user, "#general"))
			},
			client: userClient,
			cmd:    protocol.Action{Target: "#general", Body: "waves"},
			want:   protocol.Response{},
			verify: func(t *testing.T, _ *Session, s *storemod.SQLiteStore) {
				require.Equal(t, []string{"join", "mode_change", "message"}, channelEventTypes(t, s, "#general"))
			},
		},
		{
			name: "topic updates channel topic",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.JoinAs(t.Context(), sess.user, "#general"))
			},
			client: userClient,
			cmd:    protocol.Topic{Channel: "#general", Body: "discuss"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				cw, err := sess.loadChannelWindow(t.Context(), "#general")
				require.NoError(t, err)
				require.Equal(t, "discuss", cw.Topic)
			},
		},
		{
			name: "invite adds known nick to channel",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				require.NoError(t, sess.JoinAs(t.Context(), sess.user, "#general"))
				seedInstance(t, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
			},
			client: userClient,
			cmd:    protocol.Invite{Nick: "botty", Channel: "#general"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				cw, err := sess.loadChannelWindow(t.Context(), "#general")
				require.NoError(t, err)
				_, ok := cw.Members.GetByNick("botty")
				require.True(t, ok)
			},
		},
		{
			name: "kick removes nick from channel",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				require.NoError(t, sess.JoinAs(t.Context(), sess.user, "#general"))
				inst := seedInstance(t, s, instanceSpec{
					Nick:     "botty",
					ModelID:  "test/model",
					Channels: testChannels("#general"),
				})
				require.NoError(t, sess.attachInstanceToChannel(t.Context(), "#general", inst, sess.user))
			},
			client: userClient,
			cmd:    protocol.Kick{Nick: "botty", Channel: "#general"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				cw, err := sess.loadChannelWindow(t.Context(), "#general")
				require.NoError(t, err)
				_, ok := cw.Members.GetByNick("botty")
				require.False(t, ok)
			},
		},
		{
			name:   "nick changes user display name",
			client: userClient,
			cmd:    protocol.Nick{New: "renamed"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.Equal(t, domain.Nick("renamed"), sess.user.Nick())
			},
		},
		{
			name: "whois delegates to session.Whois",
			setup: func(t *testing.T, _ *Session, s *storemod.SQLiteStore) {
				seedInstance(t, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
			},
			client: userClient,
			cmd:    protocol.Whois{Nick: "botty"},
			want:   protocol.Response{},
		},
		{
			name:   "list delegates to session.DirectoryChannels",
			client: userClient,
			cmd:    protocol.List{},
			want:   protocol.Response{},
		},
		{
			name:    "opendm is not yet implemented",
			client:  userClient,
			cmd:     protocol.OpenDM{Counterpart: "botty"},
			wantErr: errHandlerNotYetImplemented,
		},
		{
			name:    "addmodel is not yet implemented for operators",
			client:  userClient,
			cmd:     protocol.AddModel{Model: "anthropic/claude", Persona: "p"},
			wantErr: errHandlerNotYetImplemented,
		},
		{
			name:   "addmodel rejects non-operator with NotOperatorError",
			client: func() protocol.Client { return newPlainClient("inst-1") },
			cmd:    protocol.AddModel{Model: "anthropic/claude", Persona: "p"},
			want:   protocol.Response{Err: domain.NotOperatorError{Command: "ADDMODEL"}},
		},
		{
			name:    "quit is not yet implemented",
			client:  userClient,
			cmd:     protocol.Quit{Reason: "gone"},
			wantErr: errHandlerNotYetImplemented,
		},
		{
			name:    "kill is not yet implemented for operators",
			client:  userClient,
			cmd:     protocol.Kill{Nick: "botty", Reason: "spam"},
			wantErr: errHandlerNotYetImplemented,
		},
		{
			name:   "kill rejects non-operator with NotOperatorError",
			client: func() protocol.Client { return newPlainClient("inst-1") },
			cmd:    protocol.Kill{Nick: "botty", Reason: "spam"},
			want:   protocol.Response{Err: domain.NotOperatorError{Command: "KILL"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess, store := newTestSession(t)
			if c.setup != nil {
				c.setup(t, sess, store)
				drainSessionEvents(sess)
			}

			got, err := sess.Handle(t.Context(), c.client(), c.cmd)

			if c.wantErr != nil {
				require.ErrorIs(t, err, c.wantErr)
				require.Equal(t, protocol.Response{}, got)
				return
			}

			require.NoError(t, err)
			require.Equal(t, c.want, got)

			if c.verify != nil {
				c.verify(t, sess, store)
			}
		})
	}
}

// drainSessionEvents empties the session's event channel of any
// events queued by the test setup so the handler call under test
// runs against a quiescent channel.
func drainSessionEvents(sess *Session) {
	for {
		select {
		case <-sess.events:
		default:
			return
		}
	}
}
