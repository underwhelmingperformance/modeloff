package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestSession_User_returns_user_client_with_operator_mode(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	user := userClient(t, sess)

	require.NotNil(t, user)
	require.Equal(t, protocol.UserClientID, user.Identity())

	sc := sess.lookupClientHandle(protocol.UserClientID)
	require.NotNil(t, sc)
	require.True(t, sc.HasMode(domain.ModeOperator))
	require.False(t, sc.HasMode(domain.Mode('w')))
}

func TestSession_User_Send_routes_through_Handle(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	resp, err := userClient(t, sess).Send(t.Context(), protocol.Join{Channel: "#general"})

	require.NoError(t, err)
	require.Equal(t, protocol.Response{}, resp)

	_, ok := userInstance(t, sess).Channels().Get("#general")
	require.True(t, ok)
}

// TestSession_Subscribe_registers_model_client pins the public
// attach API: the foreign client (here a [subscribeFakeClient]
// satisfying [protocol.Client]) is registered under its identity,
// the returned subscription's `Events()` is the per-client delivery
// stream, and the dispatcher resolves the actor instance through
// the registered envelope.
func TestSession_Subscribe_registers_model_client(t *testing.T) {
	t.Parallel()

	sess, store := newTestSession(t)

	inst := seedInstance(t, sess, store, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	fake := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}
	sub, err := sess.Subscribe(fake, protocol.SubscribeOptions{Instance: inst})

	require.NoError(t, err)
	require.NotNil(t, sub)
	require.NotNil(t, sub.Events())
}

// TestSession_Subscribe_is_idempotent_per_identity pins the
// "subscribing the same identity twice returns the existing
// envelope" contract. The factory wrapper in cmd/modeloff relies on
// this so a re-attach (e.g. a fresh INVITE for an already-registered
// model) is a no-op rather than an error.
func TestSession_Subscribe_is_idempotent_per_identity(t *testing.T) {
	t.Parallel()

	sess, store := newTestSession(t)

	inst := seedInstance(t, sess, store, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	fake := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}

	first, err := sess.Subscribe(fake, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)

	second, err := sess.Subscribe(fake, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)

	require.Same(t, first, second)
}

// TestSession_Subscribe_requires_instance pins the precondition:
// the session needs the actor handle to satisfy
// `resolveClientActor`. A nil `opts.Instance` is a structural
// bug that should fail loudly.
func TestSession_Subscribe_requires_instance(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	fake := &subscribeFakeClient{id: "inst-1"}
	_, err := sess.Subscribe(fake, protocol.SubscribeOptions{})
	require.Error(t, err)
}

// TestSession_Subscribe_accepts_user_client_id pins the symmetry
// the user-client extraction relies on: [protocol.UserClientID]
// is just another identity the session registers via the public
// `Subscribe` API, with `+o` granted through
// [protocol.SubscribeOptions.InitialModes]. The fixture's
// `newTestSession` already attached the user-client, so the
// repeat-call branch of `ensureSubscription` returns the same
// envelope.
func TestSession_Subscribe_accepts_user_client_id(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)
	inst := userInstance(t, sess)

	fake := &subscribeFakeClient{id: protocol.UserClientID}
	sub, err := sess.Subscribe(fake, protocol.SubscribeOptions{
		Instance:     inst,
		InitialModes: []domain.Mode{domain.ModeOperator},
	})

	require.NoError(t, err)
	require.NotNil(t, sub)
	require.NotNil(t, sub.Events())

	require.True(t, sess.idHasServerOper(protocol.UserClientID))
}

// subscribeFakeClient is the minimal [protocol.Client] satisfier
// used by the Subscribe contract tests. The session reads only the
// client's identity at subscribe time; the other interface methods
// are inert.
type subscribeFakeClient struct {
	id protocol.ClientID
}

func (c *subscribeFakeClient) Identity() protocol.ClientID { return c.id }
func (c *subscribeFakeClient) Send(_ context.Context, _ protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}
func (c *subscribeFakeClient) Events() <-chan protocol.Delivery { return nil }
func (c *subscribeFakeClient) Caps() command.CapabilityHolder   { return subscribeFakeCaps{} }

type subscribeFakeCaps struct{}

func (subscribeFakeCaps) Has(_ command.Capability) bool { return false }
