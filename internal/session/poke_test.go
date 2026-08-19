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
		// pass with no fresh traffic finds #busy freshly quiet, so its
		// backoff (reset by the traffic) pokes it immediately. #quiet
		// already earned one poke last cycle, so its backoff now
		// wants a second quiet cycle before the next one.
		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.PokeEvent{Channel: "#busy", At: fixedTime},
		}, collectEmittedEvents(t, sess))
	})
}

// TestSession_pokeQuietWindows_backs_off_a_channel_that_never_replies
// pins the exponential backoff schedule: a channel that stays quiet
// pass after pass is poked on a widening cadence, capped at 8x the
// base cadence.
func TestSession_pokeQuietWindows_backs_off_a_channel_that_never_replies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#quiet", "testuser")

		// Discard the bootstrap +o mode change.
		_ = collectEmittedEvents(t, sess)

		want := domain.PokeEvent{Channel: "#quiet", At: fixedTime}

		// multiplier: 1, 2, 4, 8, 8, 8, 8, 8 — poked at cumulative
		// quiet-cycle counts 1, 3, 7, 15, 23, 31, 39, 47.
		pokedAtCycle := map[int]bool{1: true, 3: true, 7: true, 15: true, 23: true, 31: true, 39: true, 47: true}

		for cycle := 1; cycle <= 47; cycle++ {
			require.NoError(t, sess.pokeQuietWindows(ctx))
			synctest.Wait()

			got := collectEmittedEvents(t, sess)

			if pokedAtCycle[cycle] {
				require.Equal(t, []domain.Event{want}, got, "cycle %d", cycle)
			} else {
				require.Empty(t, got, "cycle %d", cycle)
			}
		}
	})
}

// TestSession_pokeQuietWindows_activity_resets_backoff pins the
// other half of the schedule: real chat traffic in a channel resets
// its backoff, so a channel that goes quiet again after a
// conversation starts its next backoff fresh, at the base cadence,
// with its old multiplier gone.
func TestSession_pokeQuietWindows_activity_resets_backoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#quiet", "testuser")

		_ = collectEmittedEvents(t, sess)

		// Two quiet cycles: poked at cycle 1 (multiplier -> 2), then
		// skipped at cycle 2.
		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()
		require.Equal(t, []domain.Event{domain.PokeEvent{Channel: "#quiet", At: fixedTime}}, collectEmittedEvents(t, sess))

		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()
		require.Empty(t, collectEmittedEvents(t, sess))

		// Real traffic resets the backoff.
		_, err := userSendMessage(ctx, t, sess, "#quiet", "still here")
		require.NoError(t, err)
		synctest.Wait()
		_ = collectEmittedEvents(t, sess)

		// The cycle right after the traffic is skipped (the channel
		// was active during it), then the next quiet cycle pokes
		// immediately again, as if the multiplier had never grown.
		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()
		require.Empty(t, collectEmittedEvents(t, sess))

		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()
		require.Equal(t, []domain.Event{domain.PokeEvent{Channel: "#quiet", At: fixedTime}}, collectEmittedEvents(t, sess))
	})
}

// TestSession_pokeQuietWindows_prunes_backoff_for_destroyed_channels
// pins that a channel's backoff entry does not outlive the channel:
// the last member parting destroys the channel (RFC 2811 §2), so it
// never appears in [Session.ChannelWindowNames] again and would
// otherwise sit in pokeBackoffState forever — one entry per channel
// ever created, for the life of the process.
func TestSession_pokeQuietWindows_prunes_backoff_for_destroyed_channels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#temp", "testuser")
		_ = collectEmittedEvents(t, sess)

		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()
		_ = collectEmittedEvents(t, sess)

		require.Contains(t, sess.pokeBackoffState, domain.ChannelName("#temp"))

		require.NoError(t, userPart(ctx, t, sess, "#temp", "bye"))
		synctest.Wait()
		_ = collectEmittedEvents(t, sess)

		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()

		require.NotContains(t, sess.pokeBackoffState, domain.ChannelName("#temp"))
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

// TestSession_PokeNow_does_not_reset_backoff pins the other half of
// PokeNow's contract, alongside the bypass
// TestSession_PokeNow_pokes_every_channel already covers: a manual
// poke does not touch a channel's scheduled-poke backoff. The user
// asked for this poke; the automatic schedule's view of how quiet
// the channel has been carries on unchanged, so a channel that was
// due for its next scheduled poke in three more quiet cycles still
// is, immediately after a manual one.
func TestSession_PokeNow_does_not_reset_backoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#quiet", "testuser")
		_ = collectEmittedEvents(t, sess)

		// Advance the backoff past its base cadence (multiplier 1 ->
		// 2) via one scheduled quiet cycle.
		require.NoError(t, sess.pokeQuietWindows(ctx))
		synctest.Wait()
		_ = collectEmittedEvents(t, sess)

		before := sess.pokeBackoffState["#quiet"]
		require.Equal(t, pokeBackoff{multiplier: 2, quietTicks: 0}, before)

		require.NoError(t, sess.PokeNow(ctx))
		synctest.Wait()

		require.Equal(t, before, sess.pokeBackoffState["#quiet"],
			"a manual poke must not advance or reset the scheduled backoff")
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

// TestSession_WakePoke_interrupts_the_current_wait pins that
// lowering the poke interval takes effect right away: a config
// change that shortens a one-hour interval to five minutes must not
// leave the scheduler waiting out the rest of the hour before it
// notices.
func TestSession_WakePoke_interrupts_the_current_wait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		// Discard the bootstrap +o mode change.
		_ = collectEmittedEvents(t, sess)

		ctx, cancel := context.WithCancel(t.Context())

		// The first call sees a one-hour interval; every call after
		// that sees five minutes, as if `/config poke-interval` had
		// just been lowered and the wake fired in response. Each call
		// is reported on a channel: the test observes it through an
		// ordinary channel receive, a genuine synchronization point
		// with the loop goroutine's writes.
		scheduleCalls := make(chan struct{}, 8)
		var calls int
		sess.StartPoking(ctx, func(context.Context) (time.Duration, bool) {
			calls++
			scheduleCalls <- struct{}{}

			if calls == 1 {
				return time.Hour, true
			}

			return 5 * time.Minute, true
		})

		// Let the loop reach its first (perturbed ~54–66min) sleep
		// before waking it.
		synctest.Wait()
		<-scheduleCalls
		require.Equal(t, 0, len(scheduleCalls))

		sess.WakePoke()
		synctest.Wait()

		// The wake re-consults the schedule without poking, then
		// sleeps on the fresh interval.
		<-scheduleCalls
		require.Equal(t, 0, len(scheduleCalls))
		require.Empty(t, collectEmittedEvents(t, sess))

		// Advance past the shortened interval — well short of the
		// hour the original sleep would have needed.
		time.Sleep(6 * time.Minute)
		synctest.Wait()

		require.Equal(t, []domain.Event{
			domain.PokeEvent{Channel: "#general", At: fixedTime},
		}, collectEmittedEvents(t, sess))

		cancel()
		synctest.Wait()
	})
}
