package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/protocol"
)

func TestSession_User_returns_user_client_with_operator_mode(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	user := sess.User()

	require.NotNil(t, user)
	require.Equal(t, protocol.UserClientID, user.Identity())
	require.True(t, user.HasMode(protocol.ModeOperator))
	require.False(t, user.HasMode(protocol.UserMode('w')))
}

func TestSession_User_Send_routes_through_Handle(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	resp, err := sess.User().Send(t.Context(), protocol.Join{Channel: "#general"})

	require.NoError(t, err)
	require.Equal(t, protocol.Response{}, resp)

	_, ok := sess.UserInstance().Channels().Get("#general")
	require.True(t, ok)
}

func TestSession_Model_returns_handle_for_known_instance(t *testing.T) {
	t.Parallel()

	sess, store := newTestSession(t)
	ctx := t.Context()

	inst := seedInstance(t, store, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	type clientShape struct {
		identity protocol.ClientID
		operator bool
	}

	client := sess.Model(ctx, inst.ID())
	require.NotNil(t, client)

	got := clientShape{
		identity: client.Identity(),
		operator: client.HasMode(protocol.ModeOperator),
	}

	require.Equal(t, clientShape{
		identity: protocol.ClientID(inst.ID()),
		operator: false,
	}, got)
}

func TestSession_Model_returns_same_pointer_on_repeat_lookup(t *testing.T) {
	t.Parallel()

	sess, store := newTestSession(t)
	ctx := t.Context()

	inst := seedInstance(t, store, instanceSpec{
		Nick:    "botty",
		ModelID: "test/model",
	})

	first := sess.Model(ctx, inst.ID())
	second := sess.Model(ctx, inst.ID())

	require.NotNil(t, first)
	require.Same(t, first, second)
}

func TestSession_Model_returns_nil_for_unknown_id(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	require.Nil(t, sess.Model(t.Context(), "no-such-instance"))
}

func TestSession_Model_returns_nil_for_user_client_id(t *testing.T) {
	t.Parallel()

	sess, _ := newTestSession(t)

	require.Nil(t, sess.Model(t.Context(), protocol.UserClientID))
}
