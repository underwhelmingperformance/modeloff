package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestSession_join_reaches_an_existing_channel_in_another_case
// covers RFC 2812 §2.2: channel names compare case-insensitively, so
// a second JOIN spelled differently is the same channel, not a fork
// of it. The reply names the spelling the channel exists under.
func TestSession_join_reaches_an_existing_channel_in_another_case(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#Dev"))

	botty := domain.NewModelInstance("m1", "botty", "test/model", "", nil)
	require.NoError(t, sess.store.SaveInstance(ctx, botty))

	joined, err := sess.joinAs(ctx, botty, clientJoin, "#dEV", "")
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#Dev"), joined)

	window, err := sess.loadChannelWindow(ctx, "#DEV")
	require.NoError(t, err)
	require.Equal(t, domain.ChannelName("#Dev"), window.Name())
	require.Equal(t, []domain.Nick{userNick(t, sess), "botty"}, memberNicks(window))

	requireChannels(t, botty.Channels(), "#Dev")

	names, err := sess.ChannelWindowNames(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.ChannelName{"#Dev"}, names)
}

// TestSession_handleJoin_reports_the_canonical_name pins that the
// JOIN reply carries the channel's own spelling, so a client that
// asked for `#Dev` files the window under `#dev` and does not open a
// second one.
func TestSession_handleJoin_reports_the_canonical_name(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#dev"))
	require.NoError(t, userPart(ctx, t, sess, "#dev", ""))
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	resp, err := userClient(t, sess).Send(ctx, protocol.Join{
		Channels: []domain.ChannelName{"#DEV"},
	})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	require.Equal(t, []protocol.Event{domain.JoinedChannel{Channel: "#dev"}}, resp.Events)
}

// TestSession_message_to_a_channel_in_another_case_uses_its_name
// covers the send path: the message the channel relays, and the row
// the event log keeps, both carry the channel's own spelling.
func TestSession_message_to_a_channel_in_another_case_uses_its_name(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	require.NoError(t, userJoin(ctx, t, sess, "#Dev"))

	resp, err := userClient(t, sess).Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#dev"), Body: "hello"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)

	msg, ok := resp.Events[0].(domain.Message)
	require.True(t, ok)
	require.Equal(t, domain.ChannelName("#Dev"), msg.Target)

	stored, err := sess.EventsBefore(ctx, "#Dev", nil, 10)
	require.NoError(t, err)
	require.Len(t, stored, 2, "the JOIN and the message are both filed under the one name")
}

// TestSession_resolves_a_nick_in_another_case covers RFC 2812 §2.2
// for nicks: a command naming `Botty` reaches `botty`.
func TestSession_resolves_a_nick_in_another_case(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	botty := domain.NewModelInstance("m1", "botty", "test/model", "", nil)
	require.NoError(t, sess.store.SaveInstance(ctx, botty))

	resolved, err := sess.ResolveNick(ctx, "BoTTy")
	require.NoError(t, err)
	require.Same(t, botty, resolved)

	user, err := sess.ResolveNick(ctx, domain.Nick(strings.ToUpper(string(userNick(t, sess)))))
	require.NoError(t, err)
	require.Same(t, userInstance(t, sess), user)
}

// TestSession_nick_refuses_a_taken_nick_in_another_case pins that
// the collision check runs under the casemapping, so `botty` and
// `BOTTY` cannot both be held.
func TestSession_nick_refuses_a_taken_nick_in_another_case(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	botty := domain.NewModelInstance("m1", "botty", "test/model", "", nil)
	require.NoError(t, sess.store.SaveInstance(ctx, botty))

	resp, err := userClient(t, sess).Send(ctx, protocol.Nick{New: "BOTTY"})
	require.NoError(t, err)

	var inUse domain.NickInUseError
	require.ErrorAs(t, resp.Err, &inUse)
	require.Equal(t, domain.NickInUseError{Nick: "BOTTY", At: fixedTime}, inUse)
}

// TestSession_nick_allows_recasing_your_own_nick pins the other
// side of the collision check: the nick a client already holds is
// not a collision with itself, so a client may change how its own
// nick is spelled.
func TestSession_nick_allows_recasing_your_own_nick(t *testing.T) {
	sess, _ := newTestSession(t)
	ctx := t.Context()

	resp, err := userClient(t, sess).Send(ctx, protocol.Nick{New: "TestUser"})
	require.NoError(t, err)
	require.NoError(t, resp.Err)
	require.Equal(t, domain.Nick("TestUser"), userNick(t, sess))
}
