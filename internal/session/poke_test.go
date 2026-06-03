package session

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestSession_pokeQuietWindows_skips_recently_active(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#busy", "testuser")
		seedChannelWithMembers(t, sess, s, "#quiet", "testuser")

		_, err := userSendMessage(ctx, t, sess, "#busy", "anyone about?")
		require.NoError(t, err)
		synctest.Wait()

		// Clear the bus of the bootstrap and message-setup events.
		_ = collectEmittedEvents(t, sess)

		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.PokeEvent{Channel: "#quiet", At: fixedTime},
		}, collectEmittedEvents(t, sess))

		// The active flag is consumed by the first drain: a second
		// pass with no fresh traffic pokes both channels.
		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			domain.PokeEvent{Channel: "#busy", At: fixedTime},
			domain.PokeEvent{Channel: "#quiet", At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_PokeNow_pokes_every_channel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#busy", "testuser")
		seedChannelWithMembers(t, sess, s, "#quiet", "testuser")

		_, err := userSendMessage(ctx, t, sess, "#busy", "still here")
		require.NoError(t, err)
		synctest.Wait()
		_ = collectEmittedEvents(t, sess)

		require.NoError(t, sess.PokeNow(ctx))
		synctest.Wait()

		require.ElementsMatch(t, []domain.Event{
			domain.PokeEvent{Channel: "#busy", At: fixedTime},
			domain.PokeEvent{Channel: "#quiet", At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

func TestSession_noteChatActivity_marks_channels_only(t *testing.T) {
	sess, _ := newTestSession(t)

	// A channel message marks the channel; a DM message (bare-id
	// target) and a non-message event do not.
	sess.noteChatActivity(domain.Message{Target: "#general", From: "testuser", Body: "hi", At: fixedTime})
	sess.noteChatActivity(domain.Message{Target: "botty", From: "testuser", Body: "psst", At: fixedTime})
	sess.noteChatActivity(domain.Join{Target: "#general", Nick: "botty", At: fixedTime})

	require.Equal(t, map[domain.ChannelName]struct{}{
		"#general": {},
	}, sess.drainActiveChannels())
}

func TestSession_StartPoking_disabled_schedule_pokes_nothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		// Discard the bootstrap +o mode change.
		_ = collectEmittedEvents(t, sess)

		ctx, cancel := context.WithCancel(t.Context())

		sess.StartPoking(ctx, func(context.Context) (time.Duration, bool) {
			return time.Minute, false
		})

		// Advance well past several disabled poll cycles.
		time.Sleep(5 * time.Minute)
		synctest.Wait()

		require.Empty(t, collectEmittedEvents(t, sess))

		cancel()
		synctest.Wait()
	})
}

func TestSession_StartPoking_pokes_quiet_channel_after_interval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		// Discard the bootstrap +o mode change.
		_ = collectEmittedEvents(t, sess)

		ctx, cancel := context.WithCancel(t.Context())

		// Enabled for exactly one cycle, then paused, so the loop
		// fires a single poke pass and parks on the disabled poll.
		var calls int
		sess.StartPoking(ctx, func(context.Context) (time.Duration, bool) {
			calls++
			return time.Minute, calls == 1
		})

		// Advance past the first (perturbed ≤ 66s) interval but short
		// of the paused poll that follows it.
		time.Sleep(90 * time.Second)
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.PokeEvent{Channel: "#general", At: fixedTime},
		}, collectEmittedEvents(t, sess))

		cancel()
		synctest.Wait()
	})
}
