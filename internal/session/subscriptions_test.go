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

	resp, err := userClient(t, sess).Send(t.Context(), protocol.Join{Channels: []domain.ChannelName{"#general"}})

	require.NoError(t, err)
	require.Equal(t, protocol.Response{Events: []protocol.Event{domain.JoinedChannel{Channel: "#general"}}}, resp)

	_, ok := userInstance(t, sess).Channels().Get("#general")
	require.True(t, ok)
}

// TestSession_Subscribe_owns_the_identity_it_registers pins the
// attach contract. A subscription's events channel has one reader, so
// the envelope belongs to the client the session allocated it for:
// the same client asking again gets it back, and a different client
// asking for the same identity is refused rather than handed a
// channel it would take deliveries from.
//
// [protocol.UserClientID] is an ordinary identity under all of it.
// The fixture has already registered it, so it is the case where a
// second client asks for one somebody holds.
func TestSession_Subscribe_owns_the_identity_it_registers(t *testing.T) {
	t.Parallel()

	sess, store := newTestSession(t)

	inst := seedInstanceRow(t, store, instanceSpec{Nick: "botty", ModelID: "test/model"})
	owner := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}

	first, err := sess.Subscribe(owner, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.Events())

	second, err := sess.Subscribe(owner, protocol.SubscribeOptions{Instance: inst})
	require.NoError(t, err)
	require.Same(t, first, second, "the owner asking again gets the envelope it holds")

	impostor := &subscribeFakeClient{id: protocol.ClientID(inst.ID())}
	_, err = sess.Subscribe(impostor, protocol.SubscribeOptions{Instance: inst})
	require.ErrorIs(t, err, ErrIdentityInUse)

	userImpostor := &subscribeFakeClient{id: protocol.UserClientID}
	_, err = sess.Subscribe(userImpostor, protocol.SubscribeOptions{
		Instance:     userInstance(t, sess),
		InitialModes: []domain.Mode{domain.ModeOperator},
	})
	require.ErrorIs(t, err, ErrIdentityInUse,
		"the sentinel identity is held by the client that registered it, like any other")

	require.True(t, sess.idHasServerOper(protocol.UserClientID),
		"the refused attach left the registered client's modes alone")
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
