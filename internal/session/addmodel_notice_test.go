package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

// TestAddModel_answers_a_preparation_warning_with_a_notice covers
// what the operator is told when preparing an instance falls short
// without failing. Nick generation is the case that exists: the model
// joins under a name derived from its model id, and without this
// reply the only record is a log line the user never sees.
//
// The notice rides the `ADDMODEL` reply, which is the same slot a
// refused INVITE's notice uses, so the chat-screen renders it through
// the arm it already has.
func TestAddModel_answers_a_preparation_warning_with_a_notice(t *testing.T) {
	sess, s := newTestSession(t)
	ctx := t.Context()

	const warning = "could not generate a nick for test/model: upstream unreachable. " +
		"It joins as test, derived from its model id."

	factory := sess.modelClientFactory.(*testModelClientFactory)
	factory.prepareWarnings = []string{warning}

	seedChannelWithMembers(t, sess, s, "#Dev", userNick(t, sess))

	resp, err := userClient(t, sess).Send(ctx, protocol.AddModel{
		Channel: "#DEV",
		Model:   "test/model",
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	require.Equal(t, []protocol.Event{
		domain.SystemNotice{Target: "#Dev", Text: warning, At: fixedTime},
	}, resp.Events)

	// The notice is the issuer's own point-to-point reply, so it is
	// filed to the issuer's reply log the way every other one is.
	replies, err := s.InstanceRepliesBefore(ctx, "", nil, 10)
	require.NoError(t, err)
	require.Equal(t, []storemod.InstanceReplyRecord{
		{ID: 1, Window: protocol.ChannelWindowTarget("#Dev"), Event: domain.SystemNotice{Target: "#Dev", Text: warning, At: fixedTime}},
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
