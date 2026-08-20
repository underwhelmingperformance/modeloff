package session

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// sendTimings issues `count` PRIVMSGs from `client` back to back and
// returns how long after the first one each of them was accepted.
// Under [testing/synctest] the returned offsets are exact: the
// bubble's clock moves only when every goroutine is blocked, so a
// command that was not held back reports the same offset as the one
// before it.
func sendTimings(t *testing.T, client protocol.Client, ch domain.ChannelName, count int) []time.Duration {
	t.Helper()

	start := time.Now()
	offsets := make([]time.Duration, 0, count)

	for i := range count {
		resp, err := client.Send(t.Context(), protocol.PrivMsg{
			Target: protocol.ChannelTarget(ch),
			Body:   fmt.Sprintf("message %d", i),
		})
		require.NoError(t, errors.Join(err, resp.Err))

		offsets = append(offsets, time.Since(start))
	}

	return offsets
}

// TestSession_flood_penalty_paces_a_burst pins RFC 1459 §8.10's
// algorithm: a client may send a short burst at once, after which the
// server hands its commands to the dispatcher one every two seconds.
// Nothing is refused and nothing is dropped: every message in the
// burst reaches the channel, later than the client sent it.
func TestSession_flood_penalty_paces_a_burst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, talker))
		seedChannelWithMembers(t, sess, s, "#busy", "testuser", "talker")

		client := attachBareClient(t, sess, talker)

		// Six commands leave the message timer exactly at the lead the
		// policy allows, so the seventh is the first one held back and
		// each one after it waits a further penalty.
		offsets := sendTimings(t, client, "#busy", 9)

		require.Equal(t, []time.Duration{
			0,
			0,
			0,
			0,
			0,
			0,
			2 * time.Second,
			4 * time.Second,
			6 * time.Second,
		}, offsets)

		events, err := sess.EventsBefore(ctx, "#busy", nil, 100)
		require.NoError(t, err)

		var bodies []string
		for _, se := range events {
			if msg, ok := se.Event.(domain.Message); ok {
				bodies = append(bodies, msg.Body)
			}
		}

		require.Equal(t, []string{
			"message 0", "message 1", "message 2", "message 3", "message 4",
			"message 5", "message 6", "message 7", "message 8",
		}, bodies)
	})
}

// TestSession_flood_penalty_is_per_connection checks that a
// throttle is the flooding connection's alone. A second client sends
// throughout the flood, one command every second, and each of its
// commands is dispatched the moment it is sent.
func TestSession_flood_penalty_is_per_connection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		flooder := domain.NewModelInstance("inst-flooder", "flooder", "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, flooder))

		quiet := domain.NewModelInstance("inst-quiet", "quiet", "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, quiet))

		seedChannelWithMembers(t, sess, s, "#busy", "testuser", "flooder", "quiet")

		flooderClient := attachBareClient(t, sess, flooder)
		quietClient := attachBareClient(t, sess, quiet)

		flooded := make(chan []time.Duration, 1)

		go func() {
			flooded <- sendTimings(t, flooderClient, "#busy", 9)
		}()

		// A command a second for the length of the flood. The flooder
		// is held back for six seconds in total, so these run
		// alongside it throughout.
		var quietDelays []time.Duration

		for i := range 8 {
			time.Sleep(time.Second)

			before := time.Now()

			resp, err := quietClient.Send(ctx, protocol.PrivMsg{
				Target: protocol.ChannelTarget("#busy"),
				Body:   fmt.Sprintf("quiet %d", i),
			})
			require.NoError(t, errors.Join(err, resp.Err))

			quietDelays = append(quietDelays, time.Since(before))
		}

		require.Equal(t, make([]time.Duration, 8), quietDelays)

		require.Equal(t, []time.Duration{
			0,
			0,
			0,
			0,
			0,
			0,
			2 * time.Second,
			4 * time.Second,
			6 * time.Second,
		}, <-flooded)
	})
}

// TestSession_flood_notice_fires_once_per_episode checks the warning
// a throttled client receives: one as the throttle starts, however
// many commands are held back after it, and another only once the
// client has been quiet long enough to come out of the episode and
// flood again.
func TestSession_flood_notice_fires_once_per_episode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, talker))
		seedChannelWithMembers(t, sess, s, "#busy", "testuser", "talker")

		client := attachBareClient(t, sess, talker)

		sendTimings(t, client, "#busy", 9)

		require.Equal(t, []domain.Event{throttleNotice()}, floodNotices(t, client))

		// Waiting for the timer to drain the whole way back ends the
		// episode, so the next flood is a new one and warns again.
		time.Sleep(time.Minute)

		sendTimings(t, client, "#busy", 9)

		require.Equal(t, []domain.Event{throttleNotice()}, floodNotices(t, client))
	})
}

// TestSession_flood_notice_does_not_flap_across_a_short_pause pins
// where an episode ends. A client that pauses for one penalty period
// drops back under the lead threshold, which is enough to stop it
// being held back but not enough to end the episode. Ending it there
// would let a client sending near the threshold collect a warning
// every few seconds.
func TestSession_flood_notice_does_not_flap_across_a_short_pause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, talker))
		seedChannelWithMembers(t, sess, s, "#busy", "testuser", "talker")

		client := attachBareClient(t, sess, talker)

		sendTimings(t, client, "#busy", 9)
		require.Equal(t, []domain.Event{throttleNotice()}, floodNotices(t, client))

		// Four pauses, each long enough that the command after it runs
		// at once, none long enough to drain the timer.
		for range 4 {
			time.Sleep(3 * time.Second)

			sendTimings(t, client, "#busy", 4)
		}

		require.Empty(t, floodNotices(t, client))
	})
}

// TestSession_user_typing_at_human_speed_is_never_throttled drives
// the user-client at the pace a person types: a line every few
// seconds, over a long conversation. The message timer never runs
// ahead, so no command is held back and no warning is raised.
func TestSession_user_typing_at_human_speed_is_never_throttled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		seedChannelWithMembers(t, sess, s, "#general", "testuser")

		client := userClient(t, sess)
		start := time.Now()

		// Three seconds a line is brisk typing for a full sentence,
		// and it is still slower than the two seconds a message costs.
		for i := range 40 {
			resp, err := client.Send(ctx, protocol.PrivMsg{
				Target: protocol.ChannelTarget("#general"),
				Body:   fmt.Sprintf("line %d", i),
			})
			require.NoError(t, errors.Join(err, resp.Err))

			time.Sleep(3 * time.Second)
		}

		require.Equal(t, 40*3*time.Second, time.Since(start))
		require.Empty(t, floodNotices(t, client))
	})
}

// TestSession_autojoin_spends_the_user_allowance pins the cost of
// autojoin restoring a channel list on connect, whatever its length.
// `userclient.JoinAutojoinChannels` chunks the list to
// [protocol.MaxJoinTargets] channels per JOIN (RFC 2812 §3.2.1), so
// restoring N channels costs ⌈N/MaxJoinTargets⌉ commands, and the
// flood-control penalty (RFC 1459 §8.10) charges each of those
// commands once regardless of how many channels it names.
//
// Every row here stays at two chunks or fewer (twenty channels is
// two chunks of ten), so the message timer never gets more than
// four seconds ahead of now: two commands at two seconds each,
// still well inside the ten-second lead a burst gets before
// anything is held back. Both the JOIN sequence and the first thing
// the person types afterwards therefore land with zero delay for
// every row.
func TestSession_autojoin_spends_the_user_allowance(t *testing.T) {
	tests := []struct {
		name     string
		channels int
	}{
		{name: "six channels", channels: 6},
		{name: "eight channels", channels: 8},
		{name: "twelve channels", channels: 12},
		{name: "twenty channels", channels: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				sess, _ := newTestSession(t)
				ctx := t.Context()

				client := userClient(t, sess)
				start := time.Now()

				channels := make([]domain.ChannelName, tt.channels)
				for i := range channels {
					channels[i] = domain.ChannelName(fmt.Sprintf("#room%d", i))
				}

				// One JOIN per protocol.MaxJoinTargets-sized chunk,
				// which is what `userclient.JoinAutojoinChannels`
				// issues.
				for chunk := range slices.Chunk(channels, protocol.MaxJoinTargets) {
					resp, err := client.Send(ctx, protocol.Join{Channels: chunk})
					require.NoError(t, errors.Join(err, resp.Err))
				}

				joined := time.Since(start)

				resp, err := client.Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#room0"), Body: "hello"})
				require.NoError(t, errors.Join(err, resp.Err))

				require.Equal(t, time.Duration(0), joined)
				require.Equal(t, time.Duration(0), time.Since(start)-joined)
			})
		})
	}
}

// floodNotices drains `client`'s delivery stream and returns the
// throttle warnings on it. The warning is delivered point-to-point
// and filed nowhere, so the client's own stream is the only place it
// appears.
func floodNotices(t *testing.T, client protocol.Client) []domain.Event {
	t.Helper()

	synctest.Wait()

	var notices []domain.Event

	for {
		select {
		case delivery := <-client.Events():
			notice, ok := delivery.Event.(domain.SystemNotice)
			if !ok || notice.Text != throttleNoticeText {
				continue
			}

			notices = append(notices, notice)
		default:
			return notices
		}
	}
}

// TestSession_throttle_notice_is_not_persisted checks that the
// warning stays out of the issuer's reply log. That log holds a
// client's own lookup results, which a model replays as things it
// asked for and still knows; a throttle was true for a few seconds,
// and replaying it would tell a model to slow down on every later
// turn.
func TestSession_throttle_notice_is_not_persisted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "", testChannels("#busy"))
		require.NoError(t, s.SaveInstance(ctx, talker))
		seedChannelWithMembers(t, sess, s, "#busy", "testuser", "talker")

		client := attachBareClient(t, sess, talker)

		sendTimings(t, client, "#busy", 9)

		require.Equal(t, []domain.Event{throttleNotice()}, floodNotices(t, client))

		replies, err := sess.InstanceRepliesBefore(ctx, talker.ID(), nil, 100)
		require.NoError(t, err)
		require.Empty(t, replies)
	})
}

// throttleNotice is the warning a throttled client receives.
func throttleNotice() domain.SystemNotice {
	return domain.SystemNotice{
		Target: domain.StatusChannelName,
		Text:   throttleNoticeText,
		At:     fixedTime,
	}
}

// TestSession_channel_flood_limit refuses traffic past a channel's
// `+f` limit while leaving a channel without the mode alone. The
// refusal is the ERR_CANNOTSENDTOCHAN shape every other send gate
// uses, so the sender is told which mode stopped it.
func TestSession_channel_flood_limit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		unpaceFlood(sess)

		ctx := t.Context()

		talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "",
			testChannels("#limited", "#open"))
		require.NoError(t, s.SaveInstance(ctx, talker))

		seedChannelWithMembers(t, sess, s, "#limited", "testuser", "talker")
		seedChannelWithMembers(t, sess, s, "#open", "testuser", "talker")

		client := attachBareClient(t, sess, talker)

		resp, err := userClient(t, sess).Send(ctx, protocol.ChannelMode{
			Channel: "#limited",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeFloodLimit, Add: true, Param: "3"},
			},
		})
		require.NoError(t, errors.Join(err, resp.Err))

		window, err := sess.loadChannelWindow(ctx, "#limited")
		require.NoError(t, err)
		require.Equal(t, domain.ChannelModes{FloodLimit: 3}, window.Modes)

		for _, ch := range []domain.ChannelName{"#limited", "#open"} {
			for i := range 3 {
				resp, err := client.Send(ctx, protocol.PrivMsg{
					Target: protocol.ChannelTarget(ch),
					Body:   fmt.Sprintf("message %d", i),
				})
				require.NoError(t, errors.Join(err, resp.Err))
			}
		}

		limited, err := client.Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#limited"), Body: "one too many"})
		require.NoError(t, err)
		require.Equal(t, domain.CannotSendToChannelError{
			Channel: "#limited",
			Reason:  domain.SendBlockFlood,
			At:      fixedTime,
		}, limited.Err)

		// The quieter channel sets no limit of its own and is
		// unaffected by its neighbour's.
		open, err := client.Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#open"), Body: "still fine"})
		require.NoError(t, errors.Join(err, open.Err))
	})
}

// TestSession_channel_flood_limit_window_reopens checks that the
// limit is per window: a channel that has spent its budget takes
// traffic again once the window has passed.
func TestSession_channel_flood_limit_window_reopens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		unpaceFlood(sess)

		ctx := t.Context()

		talker := domain.NewModelInstance("inst-talker", "talker", "test/model", "", testChannels("#limited"))
		require.NoError(t, s.SaveInstance(ctx, talker))
		seedChannelWithMembers(t, sess, s, "#limited", "testuser", "talker")

		client := attachBareClient(t, sess, talker)

		resp, err := userClient(t, sess).Send(ctx, protocol.ChannelMode{
			Channel: "#limited",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeFloodLimit, Add: true, Param: "2"},
			},
		})
		require.NoError(t, errors.Join(err, resp.Err))

		first, err := client.Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#limited"), Body: "one"})
		require.NoError(t, errors.Join(err, first.Err))

		second, err := client.Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#limited"), Body: "two"})
		require.NoError(t, errors.Join(err, second.Err))

		third, err := client.Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#limited"), Body: "three"})
		require.NoError(t, err)
		require.ErrorAs(t, third.Err, &domain.CannotSendToChannelError{})

		time.Sleep(channelFloodWindow)

		fourth, err := client.Send(ctx, protocol.PrivMsg{Target: protocol.ChannelTarget("#limited"), Body: "four"})
		require.NoError(t, errors.Join(err, fourth.Err))
	})
}
