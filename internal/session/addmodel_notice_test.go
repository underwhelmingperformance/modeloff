package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestAddModel_answers_a_preparation_warning_with_a_notice covers
// what the operator is told when preparing an instance falls short
// without failing. The persona pool is the case that exists: the
// model joins and behaves for the rest of its life without the
// character it was meant to have, and until this reply carried the
// warning the only record was a log line the user never sees.
//
// The notice rides the `ADDMODEL` reply, which is the same slot a
// refused INVITE's notice uses, so the chat-screen renders it through
// the arm it already has.
func TestAddModel_answers_a_preparation_warning_with_a_notice(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	const warning = "no persona was assigned to test/model (pool is empty); it joins without one"

	factory := sess.modelClientFactory.(*testModelClientFactory)
	factory.prepareWarnings = []string{warning}

	seedChannelWithMembers(t, sess, s, "#dev", userNick(t, sess))

	resp, err := userClient(t, sess).Send(ctx, protocol.AddModel{
		Channel: "#dev",
		Model:   "test/model",
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	require.Equal(t, []protocol.Event{
		domain.SystemNotice{Target: "#dev", Text: warning, At: fixedTime},
	}, resp.Events)

	// The notice is the issuer's own point-to-point reply, so it is
	// filed to the issuer's reply log the way every other one is.
	replies, err := s.InstanceRepliesBefore(ctx, "", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []domain.StoredEvent{
		{ID: 1, Event: domain.SystemNotice{Target: "#dev", Text: warning, At: fixedTime}},
	}, replies)
}

// TestAddModel_reports_no_notice_when_preparation_is_clean pins the
// quiet path: an `ADDMODEL` whose preparation had nothing to report
// answers with an empty reply, so a clean add does not put a line in
// the channel.
func TestAddModel_reports_no_notice_when_preparation_is_clean(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	seedChannelWithMembers(t, sess, s, "#dev", userNick(t, sess))

	resp, err := userClient(t, sess).Send(ctx, protocol.AddModel{
		Channel: "#dev",
		Model:   "test/model",
		Persona: "a terse reviewer",
	})
	require.NoError(t, err)
	require.Equal(t, protocol.Response{}, resp)
}
