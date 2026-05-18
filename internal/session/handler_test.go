package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// closedProtocolEvents is a shared closed channel used as the
// inert `Events()` return value for every [fakeClient]. Reusing
// one channel avoids per-call allocation across test rows.
var closedProtocolEvents = func() <-chan protocol.Delivery {
	ch := make(chan protocol.Delivery)
	close(ch)
	return ch
}()

// fakeClient is a minimal in-test [protocol.Client] for handler
// tests. The dispatcher only reads `Identity()` and `HasMode()`, so
// `Send` and `Events` are inert satisfiers of the interface.
type fakeClient struct {
	id    protocol.ClientID
	modes map[domain.Mode]struct{}
}

func newOperatorClient(id protocol.ClientID) *fakeClient {
	return &fakeClient{
		id:    id,
		modes: map[domain.Mode]struct{}{domain.ModeOperator: {}},
	}
}

func newPlainClient(id protocol.ClientID) *fakeClient {
	return &fakeClient{id: id}
}

func (c *fakeClient) Identity() protocol.ClientID { return c.id }

func (c *fakeClient) Send(_ context.Context, _ protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}

func (c *fakeClient) Events() <-chan protocol.Delivery { return closedProtocolEvents }

func (c *fakeClient) HasMode(m domain.Mode) bool {
	_, ok := c.modes[m]
	return ok
}

func (c *fakeClient) Caps() command.CapabilityHolder { return c }

func (c *fakeClient) Has(cap command.Capability) bool {
	if cap == protocol.CapOperator {
		return c.HasMode(domain.ModeOperator)
	}
	return false
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
				require.NoError(t, sess.joinAs(t.Context(), sess.user, "#general", ""))
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
				require.NoError(t, sess.joinAs(t.Context(), sess.user, "#general", ""))
			},
			client: userClient,
			cmd:    protocol.PrivMsg{Target: "#general", Body: "hello"},
			want: protocol.Response{Events: []protocol.Event{domain.Message{
				Target: "#general",
				From:   "testuser",
				Body:   "hello",
				At:     fixedTime,
			}}},
			verify: func(t *testing.T, _ *Session, s *storemod.SQLiteStore) {
				require.Equal(t, []string{"join", "message"}, channelEventTypes(t, s, "#general"))
			},
		},
		{
			name: "action sends action message to channel",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.joinAs(t.Context(), sess.user, "#general", ""))
			},
			client: userClient,
			cmd:    protocol.Action{Target: "#general", Body: "waves"},
			want: protocol.Response{Events: []protocol.Event{domain.Message{
				Target: "#general",
				From:   "testuser",
				Body:   "waves",
				Action: true,
				At:     fixedTime,
			}}},
			verify: func(t *testing.T, _ *Session, s *storemod.SQLiteStore) {
				require.Equal(t, []string{"join", "message"}, channelEventTypes(t, s, "#general"))
			},
		},
		{
			name: "topic updates channel topic",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.joinAs(t.Context(), sess.user, "#general", ""))
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
				require.NoError(t, sess.joinAs(t.Context(), sess.user, "#general", ""))
				seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
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
				require.NoError(t, sess.joinAs(t.Context(), sess.user, "#general", ""))
				inst := seedInstance(t, sess, s, instanceSpec{
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
			name: "whois returns the Whois snapshot in Response.Events",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
			},
			client: userClient,
			cmd:    protocol.Whois{Nick: "botty"},
			want: protocol.Response{Events: []domain.ProtocolEvent{
				domain.Whois{
					Nick:    "botty",
					ModelID: "test/model",
					At:      fixedTime,
				},
			}},
		},
		{
			name:   "list returns a closing ListEnd when no channels exist",
			client: userClient,
			cmd:    protocol.List{},
			want: protocol.Response{Events: []domain.ProtocolEvent{
				domain.ListEnd{At: fixedTime},
			}},
		},
		{
			name: "addmodel delegates to addModelAs for operators",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.Join(t.Context(), "#dev"))
			},
			client: userClient,
			cmd:    protocol.AddModel{Channel: "#dev", Model: "anthropic/claude", Persona: "p"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				window, err := sess.loadChannelWindow(t.Context(), "#dev")
				require.NoError(t, err)
				require.Equal(t, 2, window.Members.Len())
			},
		},
		{
			name:   "addmodel rejects non-operator with NotOperatorError",
			client: func() protocol.Client { return newPlainClient("inst-1") },
			cmd:    protocol.AddModel{Channel: "#dev", Model: "anthropic/claude", Persona: "p"},
			want:   protocol.Response{Err: protocol.NotOperatorError{Command: "ADDMODEL", At: fixedTime}},
		},
		{
			name:   "quit delegates to quitAs for the user-client",
			client: userClient,
			cmd:    protocol.Quit{Reason: "gone"},
			want:   protocol.Response{},
		},
		{
			name: "kill delegates to killAs for operators",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
			},
			client: userClient,
			cmd:    protocol.Kill{Nick: "botty", Reason: "spam"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				_, err := sess.ResolveNick(t.Context(), "botty")
				require.Error(t, err)
			},
		},
		{
			name:   "kill rejects non-operator with NotOperatorError",
			client: func() protocol.Client { return newPlainClient("inst-1") },
			cmd:    protocol.Kill{Nick: "botty", Reason: "spam"},
			want:   protocol.Response{Err: protocol.NotOperatorError{Command: "KILL", At: fixedTime}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess, store := newTestSession(t)
			if c.setup != nil {
				c.setup(t, sess, store)
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
