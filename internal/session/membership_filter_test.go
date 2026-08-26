package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// The user-client rides the same membership filter as model-clients:
// it receives window events only for channels it has joined. Server
// handshake numerics (Welcome) reach it point-to-point, not through
// this filter.
func TestServerClient_userClient_membership_filter(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#joined"))

	uc := sess.lookupClientHandle(protocol.UserClientID)
	require.NotNil(t, uc)

	cases := []struct {
		name string
		ev   domain.ProtocolEvent
		want bool
	}{
		{"message in a joined channel", domain.Message{Source: domain.ClientSource(testMemberID("botty"), "botty"), Target: "#joined", Body: "hi", At: fixedTime}, true},
		{"message in an unjoined channel", domain.Message{Source: domain.ClientSource(testMemberID("botty"), "botty"), Target: "#elsewhere", Body: "hi", At: fixedTime}, false},
		{"welcome does not ride the membership filter", domain.Welcome{ServerName: domain.StatusServerName, Nick: "testuser", At: fixedTime}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, uc.canReceive(tc.ev, nil))
		})
	}
}

// Dispatch-failure notices are operator diagnostics: the user-client,
// which holds +o, receives them for every window — joined channel,
// unjoined channel, and DM alike — while a non-operator model-client
// does not.
func TestServerClient_modelUnavailableError_is_operator_scoped(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#joined"))

	uc := sess.lookupClientHandle(protocol.UserClientID)
	require.NotNil(t, uc)

	botty := seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#joined"),
	})
	model := fakeServerClient(t, botty)

	cases := []struct {
		name   string
		client *serverClient
		want   bool
	}{
		{"operator", uc, true},
		{"non-operator model", model, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := domain.ModelUnavailableError{Source: domain.LegacyClientSource("botty"), At: fixedTime}
			require.Equal(t, tc.want, tc.client.canReceive(ev, nil))
		})
	}
}

// A notice the session raises for server-side work nobody asked for
// reaches the operator's bus. Its counterpart, a notice answering a
// command, reaches its issuer point to point and never through this
// filter.
func TestSession_NoticeOperators_reaches_the_operator(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#joined"))

	botty := seedInstance(t, sess, s, instanceSpec{
		Nick:     "botty",
		ModelID:  "test/model",
		Channels: testChannels("#joined"),
	})
	model := fakeServerClient(t, botty)

	notice := domain.SystemNotice{
		Target: domain.StatusChannelName,
		Text:   "Reflection for botty accepted a revision.",
		At:     fixedTime,
	}
	sess.NoticeOperators(ctx, notice)

	require.Equal(t, []domain.SystemNotice{notice},
		eventsOfType[domain.SystemNotice](collectEmittedEvents(t, sess)))
	require.False(t, model.canReceive(notice, nil))
}
