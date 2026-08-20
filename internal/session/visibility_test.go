package session

import (
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// grantOperator promotes an already-subscribed instance to server
// operator, the way [Session.handleOper] would on a session with a
// real authenticator behind it.
func grantOperator(t *testing.T, sess *Session, inst *domain.Instance) {
	t.Helper()

	sc := sess.lookupClientHandle(protocol.ClientID(inst.ID()))
	require.NotNil(t, sc, "the instance must be subscribed before it can be promoted")

	sess.setUserModeAs(t.Context(), "", sc, domain.ModeOperator, true)
}

// seedVisibilityChannels creates one channel of each visibility
// (public, `+p` and `+s`), with the user as the only member of each
// and a topic on each.
func seedVisibilityChannels(t *testing.T, sess *Session) {
	t.Helper()

	ctx := t.Context()
	user := userInstance(t, sess)

	for _, ch := range []struct {
		name  domain.ChannelName
		topic string
		modes domain.ChannelModes
	}{
		{name: "#public", topic: "public topic"},
		{name: "#private", topic: "private topic", modes: domain.ChannelModes{Private: true}},
		{name: "#secret", topic: "secret topic", modes: domain.ChannelModes{Secret: true}},
	} {
		require.NoError(t, userJoin(ctx, t, sess, ch.name))
		require.NoError(t, sess.setTopicAs(ctx, user, ch.name, ch.topic))

		if ch.modes != (domain.ChannelModes{}) {
			setChannelModes(t, sess, ch.name, ch.modes)
		}
	}
}

// TestDirectoryChannels_hides_secret_and_private_from_a_non_member
// covers the visibility predicate's default: RFC 2811 §4.2.5 and
// §4.2.6 keep a `+s` or `+p` channel out of a stranger's directory
// altogether. `+p` hides the channel outright, the way modern ircds
// read it: listing it by name with a blank topic would give away
// what the mode exists to hide.
func TestDirectoryChannels_hides_secret_and_private_from_a_non_member(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedVisibilityChannels(t, sess)

		stranger := seedInstance(t, sess, s, instanceSpec{Nick: "stranger", ModelID: "test/model"})

		entries, err := sess.DirectoryChannels(ctx, stranger)
		require.NoError(t, err)

		require.Equal(t, []domain.ChannelDirectoryEntry{
			{Channel: "#public", Members: 1, Topic: "public topic"},
		}, entries)
	})
}

// TestDirectoryChannels_shows_everything_to_an_operator covers the
// other exemption: a server operator sees the whole session (RFC
// 2812 §3.6.2), which is what makes the user-client's `/list` a
// complete view without any bypass of the delivery filter.
func TestDirectoryChannels_shows_everything_to_an_operator(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedVisibilityChannels(t, sess)

		// A model that is on none of the three, promoted to `+o`.
		oper := seedInstance(t, sess, s, instanceSpec{Nick: "oper", ModelID: "test/model"})
		grantOperator(t, sess, oper)

		entries, err := sess.DirectoryChannels(ctx, oper)
		require.NoError(t, err)

		require.Equal(t, []domain.ChannelDirectoryEntry{
			{Channel: "#private", Members: 1, Topic: "private topic"},
			{Channel: "#public", Members: 1, Topic: "public topic"},
			{Channel: "#secret", Members: 1, Topic: "secret topic"},
		}, entries)
	})
}

// TestWhois_hides_channels_the_issuer_may_not_see covers RFC 2812
// §3.6.2's condition on `RPL_WHOISCHANNELS`: the reply names the
// target's channels except those the issuer may not see. Without the
// filter, a client could read a `+s` channel's name out of a WHOIS on
// one of its members, which is the one thing `+s` exists to prevent.
func TestWhois_hides_channels_the_issuer_may_not_see(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedVisibilityChannels(t, sess)

		// botty is in all three channels alongside the user.
		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
		for _, ch := range []domain.ChannelName{"#public", "#private", "#secret"} {
			require.NoError(t, joinAs(ctx, sess, botty, ch, ""))
		}

		// stranger shares only #public with botty.
		stranger := seedInstance(t, sess, s, instanceSpec{Nick: "stranger", ModelID: "test/model-b"})
		require.NoError(t, joinAs(ctx, sess, stranger, "#public", ""))

		resp, err := sess.Handle(ctx, sess.LookupClient(protocol.ClientID(stranger.ID())), protocol.Whois{
			Nick:    "botty",
			Channel: "#public",
		})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		require.Equal(t, []protocol.Event{domain.Whois{
			Target:   "#public",
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: []domain.ChannelName{"#public"},
			At:       fixedTime,
		}}, resp.Events)
	})
}

// TestWhois_shows_a_member_the_channels_they_share pins the member
// exemption on the same reply: the user is on all three, so a WHOIS
// on botty names all three.
func TestWhois_shows_a_member_the_channels_they_share(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedVisibilityChannels(t, sess)

		botty := seedInstance(t, sess, s, instanceSpec{Nick: "botty", ModelID: "test/model"})
		for _, ch := range []domain.ChannelName{"#public", "#private", "#secret"} {
			require.NoError(t, joinAs(ctx, sess, botty, ch, ""))
		}

		resp, err := userClient(t, sess).Send(ctx, protocol.Whois{Nick: "botty", Channel: "#public"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		require.Equal(t, []protocol.Event{domain.Whois{
			Target:   "#public",
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: []domain.ChannelName{"#public", "#private", "#secret"},
			At:       fixedTime,
		}}, resp.Events)
	})
}
