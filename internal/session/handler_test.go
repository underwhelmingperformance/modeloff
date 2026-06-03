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
// tests. The dispatcher reads `Identity()`; operator capability is
// read off the session-side `serverClient` keyed by that identity,
// so `Send` and `Events` are inert satisfiers of the interface.
type fakeClient struct {
	id protocol.ClientID
}

func newPlainClient(id protocol.ClientID) *fakeClient {
	return &fakeClient{id: id}
}

func (c *fakeClient) Identity() protocol.ClientID { return c.id }

func (c *fakeClient) Send(_ context.Context, _ protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}

func (c *fakeClient) Events() <-chan protocol.Delivery { return closedProtocolEvents }

func (c *fakeClient) Caps() command.CapabilityHolder { return command.NoCapabilities() }

// TestSession_operator_gate_honours_oper_elevation proves the
// operator gate for ADDMODEL and KILL consults the issuing client's
// live `serverClient` modes. A model self-elevates through the wire
// OPER path — which writes `+o` to the serverClient only — and then
// clears the gate, even though its client object reports no
// capability.
func TestSession_operator_gate_honours_oper_elevation(t *testing.T) {
	cases := []struct {
		name string
		cmd  protocol.Command
	}{
		{name: "addmodel", cmd: protocol.AddModel{Channel: "#dev", Model: "anthropic/claude", Persona: "p"}},
		{name: "kill", cmd: protocol.Kill{Nick: "victim", Reason: "spam"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess, store := newTestSession(t)
			ctx := t.Context()

			sess.SetOperAuthenticator(func(protocol.Client, string, string) bool { return true })
			require.NoError(t, userJoin(ctx, t, sess, "#dev"))
			seedInstance(t, sess, store, instanceSpec{Nick: "victim", ModelID: "test/model"})

			// A model-like client whose client object reports no
			// capability: the operator signal lives only on its
			// serverClient, written by OPER.
			inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
			fc := newPlainClient(protocol.ClientID(inst.ID()))
			_, err := sess.Subscribe(fc, protocol.SubscribeOptions{Instance: inst})
			require.NoError(t, err)

			operResp, err := sess.Handle(ctx, fc, protocol.Oper{})
			require.NoError(t, err)
			require.NoError(t, operResp.Err)

			got, err := sess.Handle(ctx, fc, c.cmd)
			require.NoError(t, err)
			require.Equal(t, protocol.Response{}, got)
		})
	}
}

// TestSession_operator_gate_rejects_subscribed_non_operator covers
// the production deny path: a registered model-client that holds no
// `+o` on its serverClient is refused ADDMODEL and KILL. The
// dispatcher's gate distinguishes this from an unregistered issuer,
// which is rejected on the nil-handle branch.
func TestSession_operator_gate_rejects_subscribed_non_operator(t *testing.T) {
	cases := []struct {
		name    string
		cmd     protocol.Command
		command string
	}{
		{name: "addmodel", cmd: protocol.AddModel{Channel: "#dev", Model: "m", Persona: "p"}, command: "ADDMODEL"},
		{name: "kill", cmd: protocol.Kill{Nick: "victim", Reason: "x"}, command: "KILL"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sess, store := newTestSession(t)
			ctx := t.Context()
			seedInstance(t, sess, store, instanceSpec{Nick: "victim", ModelID: "test/model"})

			inst := domain.NewModelInstance("inst-plain", "plain", "test/model", "", nil)
			fc := newPlainClient(protocol.ClientID(inst.ID()))
			_, err := sess.Subscribe(fc, protocol.SubscribeOptions{Instance: inst})
			require.NoError(t, err)

			got, err := sess.Handle(ctx, fc, c.cmd)
			require.NoError(t, err)
			require.Equal(t,
				protocol.Response{Err: protocol.NotOperatorError{Command: c.command, At: fixedTime}},
				got)
		})
	}
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
		// wantFn lets a case compute its expected response from the
		// post-setup session state — needed when the expected
		// response refers to identities (instance pointers, ids)
		// allocated during setup. When non-nil, `wantFn` takes
		// precedence over `want`.
		wantFn  func(t *testing.T, sess *Session, s *storemod.SQLiteStore) protocol.Response
		wantErr error
		verify  func(t *testing.T, sess *Session, s *storemod.SQLiteStore)
	}

	userClient := func() protocol.Client { return newPlainClient(protocol.UserClientID) }

	cases := []tc{
		{
			name:   "join creates and joins channel",
			client: userClient,
			cmd:    protocol.Join{Channel: "#general"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				_, ok := userInstance(t, sess).Channels().Get("#general")
				require.True(t, ok)
			},
		},
		{
			name: "part removes user from channel",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.joinAs(t.Context(), userInstance(t, sess), "#general", ""))
			},
			client: userClient,
			cmd:    protocol.Part{Channel: "#general", Reason: "bye"},
			want:   protocol.Response{},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				_, ok := userInstance(t, sess).Channels().Get("#general")
				require.False(t, ok)
			},
		},
		{
			name: "privmsg sends to channel",
			setup: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				require.NoError(t, sess.joinAs(t.Context(), userInstance(t, sess), "#general", ""))
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
				require.NoError(t, sess.joinAs(t.Context(), userInstance(t, sess), "#general", ""))
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
				require.NoError(t, sess.joinAs(t.Context(), userInstance(t, sess), "#general", ""))
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
			name: "invite records known nick on the channel's invitedNicks set",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				require.NoError(t, sess.joinAs(t.Context(), userInstance(t, sess), "#general", ""))
				seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
			},
			client: userClient,
			cmd:    protocol.Invite{Nick: "botty", Channel: "#general"},
			wantFn: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) protocol.Response {
				botty, err := s.ResolveNick(t.Context(), "botty")
				require.NoError(t, err)
				return protocol.Response{
					Events: []domain.ProtocolEvent{domain.ModelInvited{
						Target:       "#general",
						Nick:         "botty",
						InstanceID:   botty.ID(),
						By:           "testuser",
						ByInstanceID: "",
						At:           fixedTime,
						Instance:     botty,
					}},
				}
			},
			verify: func(t *testing.T, sess *Session, _ *storemod.SQLiteStore) {
				cw, err := sess.loadChannelWindow(t.Context(), "#general")
				require.NoError(t, err)

				_, isMember := cw.Members.GetByNick("botty")
				require.False(t, isMember,
					"INVITE does not mutate membership; the invited model joins "+
						"via its own dispatch turn")

				require.True(t, cw.InvitedNicks.Contains("botty"),
					"INVITE records the nick on InvitedNicks so a follow-up "+
						"JOIN clears `+i`")
			},
		},
		{
			name: "kick removes nick from channel",
			setup: func(t *testing.T, sess *Session, s *storemod.SQLiteStore) {
				require.NoError(t, sess.joinAs(t.Context(), userInstance(t, sess), "#general", ""))
				inst := seedInstance(t, sess, s, instanceSpec{
					Nick:    "botty",
					ModelID: "test/model",
				})
				require.NoError(t, sess.joinAs(t.Context(), inst, "#general", ""))
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
				require.Equal(t, domain.Nick("renamed"), userInstance(t, sess).Nick())
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
				require.NoError(t, userJoin(t.Context(), t, sess, "#dev"))
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

			want := c.want
			if c.wantFn != nil {
				want = c.wantFn(t, sess, store)
			}
			require.Equal(t, want, got)

			if c.verify != nil {
				c.verify(t, sess, store)
			}
		})
	}
}
