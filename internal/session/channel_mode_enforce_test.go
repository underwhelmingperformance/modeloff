package session

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// setChannelModes is a test helper that mutates the channel's
// attribute modes via direct persistence — it skips the wire
// `protocol.ChannelMode` path so the enforcement-under-test
// isn't itself the path being exercised.
func setChannelModes(t *testing.T, sess *Session, ch domain.ChannelName, modes domain.ChannelModes) {
	t.Helper()

	w, err := sess.loadChannelWindow(t.Context(), ch)
	require.NoError(t, err)
	w.Modes = modes
	require.NoError(t, sess.persistChannelWindow(t.Context(), w))
}

// TestSetTopicAs_TopicLockGate covers `+t`: when the channel is
// `-t`, any member can change the topic; when `+t`, only ops can.
func TestSetTopicAs_TopicLockGate(t *testing.T) {
	t.Run("without +t any member can change topic", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
				Channels: testChannels("#chan"),
			})
			seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

			require.NoError(t, sess.setTopicAs(ctx, botty, "#chan", "new topic"))

			w, err := sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			require.Equal(t, "new topic", w.Topic)
		})
	})

	t.Run("with +t only ops can change topic", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
				Channels: testChannels("#chan"),
			})
			seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")
			setChannelModes(t, sess, "#chan", domain.ChannelModes{TopicLock: true})

			err := sess.setTopicAs(ctx, botty, "#chan", "blocked")
			var copReq domain.ChanOpRequiredError
			require.ErrorAs(t, err, &copReq)

			w, err := sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			require.Equal(t, "", w.Topic, "topic must not change when op-gate rejects")
		})
	})
}

// TestInviteAs_InviteOnlyGate covers `+i`'s effect on INVITE:
// when the channel is `-i`, any member can invite; when `+i`,
// only ops can.
func TestInviteAs_InviteOnlyGate(t *testing.T) {
	t.Run("without +i any member can invite", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
				Channels: testChannels("#chan"),
			})
			seedInstance(t, sess, s, instanceSpec{Nick: "helper", ModelID: "test/model-b"})
			seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

			_, err := sess.inviteAs(ctx, botty, "helper", "#chan")
			require.NoError(t, err)

			w, err := sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			require.True(t, w.InvitedNicks.Contains("helper"))
		})
	})

	t.Run("with +i only ops can invite", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
				Channels: testChannels("#chan"),
			})
			seedInstance(t, sess, s, instanceSpec{Nick: "helper", ModelID: "test/model-b"})
			seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")
			setChannelModes(t, sess, "#chan", domain.ChannelModes{InviteOnly: true})

			_, err := sess.inviteAs(ctx, botty, "helper", "#chan")
			var copReq domain.ChanOpRequiredError
			require.ErrorAs(t, err, &copReq)

			w, err := sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			require.False(t, w.InvitedNicks.Contains("helper"))
		})
	})
}

// TestJoinAs_KeyGate covers `+k`: a JOIN against a keyed channel
// requires the matching key; missing or wrong rejects with
// `ChannelKeyMismatchError`.
func TestJoinAs_KeyGate(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "matching key admits", key: "secret", wantErr: false},
		{name: "wrong key rejects", key: "guess", wantErr: true},
		{name: "missing key rejects", key: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess, s := newTestSession(t)
				ctx := t.Context()

				require.NoError(t, userJoin(ctx, t, sess, "#chan"))
				setChannelModes(t, sess, "#chan", domain.ChannelModes{Key: "secret"})

				botty := seedInstance(t, sess, s, instanceSpec{
					Nick: "botty", ModelID: "test/model",
				})

				err := sess.joinAs(ctx, botty, "#chan", tt.key)

				if tt.wantErr {
					var keyErr domain.ChannelKeyMismatchError
					require.ErrorAs(t, err, &keyErr)
					return
				}

				require.NoError(t, err)
				w, err := sess.loadChannelWindow(ctx, "#chan")
				require.NoError(t, err)
				require.True(t, w.Members.HasInstance(botty))
			})
		})
	}
}

// TestJoinAs_UserLimitGate covers `+l`: a JOIN against a full
// channel rejects with `ChannelFullError`.
func TestJoinAs_UserLimitGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#chan"))
		setChannelModes(t, sess, "#chan", domain.ChannelModes{UserLimit: 1})

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})

		err := sess.joinAs(ctx, botty, "#chan", "")
		var fullErr domain.ChannelFullError
		require.ErrorAs(t, err, &fullErr)

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		require.False(t, w.Members.HasInstance(botty))
	})
}

// TestJoinAs_InviteOnlyGate covers `+i`: a JOIN against an
// invite-only channel without prior INVITE rejects; with a
// prior INVITE succeeds and consumes the invitation.
func TestJoinAs_InviteOnlyGate(t *testing.T) {
	t.Run("uninvited rejects", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))
			setChannelModes(t, sess, "#chan", domain.ChannelModes{InviteOnly: true})

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
			})

			err := sess.joinAs(ctx, botty, "#chan", "")
			var ioErr domain.ChannelInviteOnlyError
			require.ErrorAs(t, err, &ioErr)
		})
	})

	t.Run("invited admits and consumes invitation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))

			w, err := sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			w.Modes.InviteOnly = true
			w.InvitedNicks.Add("botty")
			require.NoError(t, sess.persistChannelWindow(ctx, w))

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
			})

			require.NoError(t, sess.joinAs(ctx, botty, "#chan", ""))

			w, err = sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			require.True(t, w.Members.HasInstance(botty))
			require.False(t, w.InvitedNicks.Contains("botty"),
				"invitation must be consumed on successful join")

			// A second join attempt by the same nick (after part)
			// fails because the invitation was single-use.
			require.NoError(t, sess.partAs(ctx, botty, "#chan", ""))
			err = sess.joinAs(ctx, botty, "#chan", "")
			var ioErr domain.ChannelInviteOnlyError
			require.ErrorAs(t, err, &ioErr)
		})
	})
}

// TestSendMessageAs_ModeratedGate covers `+m`: only members with
// voice or op may PRIVMSG; non-voiced/non-op rejects.
func TestSendMessageAs_ModeratedGate(t *testing.T) {
	t.Run("voiced sender admits", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))
			setChannelModes(t, sess, "#chan", domain.ChannelModes{Moderated: true})

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
				Channels: testChannels("#chan"),
			})
			seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")
			setChannelModes(t, sess, "#chan", domain.ChannelModes{Moderated: true})

			w, err := sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			w.Members.SetMode(botty, domain.ModeVoice)
			require.NoError(t, sess.persistChannelWindow(ctx, w))

			_, err = sess.sendMessageAs(ctx, botty, "#chan", "voiced")
			require.NoError(t, err)
		})
	})

	t.Run("unvoiced sender rejects", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			sess, s := newTestSession(t)
			ctx := t.Context()

			require.NoError(t, userJoin(ctx, t, sess, "#chan"))

			botty := seedInstance(t, sess, s, instanceSpec{
				Nick: "botty", ModelID: "test/model",
				Channels: testChannels("#chan"),
			})
			seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

			// Strip botty's default voice so the +m gate rejects.
			w, err := sess.loadChannelWindow(ctx, "#chan")
			require.NoError(t, err)
			w.Members.SetMode(botty, domain.ModeNone)
			w.Modes.Moderated = true
			require.NoError(t, sess.persistChannelWindow(ctx, w))

			_, err = sess.sendMessageAs(ctx, botty, "#chan", "silenced")

			var blockErr domain.CannotSendToChannelError
			require.ErrorAs(t, err, &blockErr)
			require.Equal(t, domain.SendBlockModerated, blockErr.Reason)
		})
	})
}

// TestSendMessageAs_NoExternalGate covers `+n`: non-members
// cannot send.
func TestSendMessageAs_NoExternalGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#chan"))
		setChannelModes(t, sess, "#chan", domain.ChannelModes{NoExternal: true})

		// botty is not in the channel.
		botty := seedInstance(t, sess, s, instanceSpec{
			Nick: "botty", ModelID: "test/model",
		})

		_, err := sess.sendMessageAs(ctx, botty, "#chan", "from outside")

		var blockErr domain.CannotSendToChannelError
		require.ErrorAs(t, err, &blockErr)
		require.Equal(t, domain.SendBlockNoExternal, blockErr.Reason)
	})
}

// TestSendMessageAs_QuietGate covers `+q`: only ops may speak.
func TestSendMessageAs_QuietGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#chan"))

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick: "botty", ModelID: "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		w.Modes.Quiet = true
		w.Members.SetMode(botty, domain.ModeVoice)
		require.NoError(t, sess.persistChannelWindow(ctx, w))

		_, err = sess.sendMessageAs(ctx, botty, "#chan", "silenced")

		var blockErr domain.CannotSendToChannelError
		require.ErrorAs(t, err, &blockErr)
		require.Equal(t, domain.SendBlockQuiet, blockErr.Reason)

		// The op (the user-client owner) may still speak.
		_, err = userSendMessage(ctx, t, sess, "#chan", "i'm an op")
		require.NoError(t, err)
	})
}

// TestFanOutProtocol_AnonymousRewritesSender covers `+a`: chat
// traffic delivered to subscribers carries the sentinel nick
// `"anonymous"` rather than the real sender (RFC 2811 §4.2.1).
// The dispatch goroutine triggered by the PRIVMSG sees the
// rewritten IRCMessage in its trigger list; the test asserts on
// the full collected trigger slice structurally.
func TestFanOutProtocol_AnonymousRewritesSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var triggers []protocol.IRCMessage

		fake := &fakeAPIClient{
			sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, _ []protocol.IRCMessage, events []protocol.IRCMessage) (api.CompletionResult, error) {
				triggers = append(triggers, events...)
				return api.CompletionResult{}, nil
			},
		}

		sess, s := newTestSessionWithAPI(t, fake)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#chan"))

		seedInstance(t, sess, s, instanceSpec{
			Nick: "botty", ModelID: "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")
		setChannelModes(t, sess, "#chan", domain.ChannelModes{Anonymous: true})

		_, err := userSendMessage(ctx, t, sess, "#chan", "secret")
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, []protocol.IRCMessage{{
			Kind:   protocol.KindPrivMsg,
			From:   "anonymous",
			Target: "#chan",
			Body:   "secret",
			At:     fixedTime,
		}}, triggers)

		// The user-client holds echo-message; its own echoed line is
		// anonymised too (RFC 2811 §4.2.1).
		require.Contains(t, collectEmittedEvents(t, sess), domain.Message{
			Target: "#chan",
			From:   "anonymous",
			Body:   "secret",
			At:     fixedTime,
		})
	})
}

// TestDirectoryChannels_SecretHiddenAndPrivateHasNoTopic covers
// `+s` (channel omitted from /list) and `+p` (channel listed
// without topic). Both are RFC 2811 §4.2.6/§4.2.7.
func TestDirectoryChannels_SecretHiddenAndPrivateHasNoTopic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, userJoin(ctx, t, sess, "#public"))
		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#public", "public topic"))

		require.NoError(t, userJoin(ctx, t, sess, "#private"))
		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#private", "private topic"))
		setChannelModes(t, sess, "#private", domain.ChannelModes{Private: true})

		require.NoError(t, userJoin(ctx, t, sess, "#secret"))
		require.NoError(t, sess.setTopicAs(ctx, userInstance(t, sess), "#secret", "secret topic"))
		setChannelModes(t, sess, "#secret", domain.ChannelModes{Secret: true})

		entries, err := sess.DirectoryChannels(ctx)
		require.NoError(t, err)

		names := make(map[domain.ChannelName]domain.ChannelDirectoryEntry, len(entries))
		for _, e := range entries {
			names[e.Channel] = e
		}

		require.Contains(t, names, domain.ChannelName("#public"))
		require.Equal(t, "public topic", names["#public"].Topic)

		require.Contains(t, names, domain.ChannelName("#private"))
		require.Equal(t, "", names["#private"].Topic, "private channels hide topic")

		require.NotContains(t, names, domain.ChannelName("#secret"),
			"secret channels do not appear in /list")
	})
}
