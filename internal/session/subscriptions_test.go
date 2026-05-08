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
