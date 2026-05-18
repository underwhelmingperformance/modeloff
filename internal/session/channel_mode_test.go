package session

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// joinSetupEventsT returns the prefix every channel-mode test
// emits on the user-client's events channel after `sess.Join`:
// the server's bootstrap promotion of the user, the user's own
// Join into the channel, its NamesReply, and ChanServ's
// channel-op grant for the user. The action under test always
// emits after this prefix; tests append their own expected
// ModeChange and assert structurally on the full slice.
//
// Must be called immediately after `sess.Join` — the NamesReply
// event holds a reference into the channel's MemberList, so any
// later mutation (e.g. `seedChannelWithMembers` replacing the
// stored channel) would be visible through the event and break
// the structural compare.
func joinSetupEventsT(t *testing.T, sess *Session, bootAt time.Time, ch domain.ChannelName) []domain.Event {
	t.Helper()

	user := sess.UserInstance()
	w, err := sess.loadChannelWindow(t.Context(), ch)
	require.NoError(t, err)

	return []domain.Event{
		bootstrapModeChange(sess, bootAt),
		domain.Join{
			Target:     ch,
			Nick:       user.Nick(),
			InstanceID: user.ID(),
			Created:    true,
			At:         fixedTime,
			Instance:   user,
		},
		domain.NamesReplyEvent{
			Channel: ch,
			Members: w.Members,
			At:      fixedTime,
		},
	}
}

// TestHandleChannelMode_GrantsMemberOp pins the full event stream
// for `+o` against a member: bootstrap, the test's own Join setup,
// then the ModeChange the action emits. Inter-goroutine ordering
// is fixed here — no model-client dispatch goroutine wakes (botty
// is store-seeded only), so the assertion is a strict ordered
// equality.
func TestHandleChannelMode_GrantsMemberOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

		_, err := sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeOperator, Add: true, Target: "botty"},
			},
		})
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, append(prefix,
			domain.ModeChange{
				Target:     "#chan",
				Nick:       "botty",
				InstanceID: botty.ID(),
				Flag:       domain.ModeOperator,
				Add:        true,
				By:         "testuser",
				At:         fixedTime,
				Instance:   botty,
			},
		), collectEmittedEvents(t, sess))
	})
}

// TestHandleChannelMode_RevokeMemberOp pins the stream when `-o`
// strips an existing op rank. The window pre-state (botty as `@`)
// is set via direct MemberList mutation + persist, neither of
// which emits, so the structural slice is identical to the grant
// case except for the action's ModeChange direction.
func TestHandleChannelMode_RevokeMemberOp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		w.Members.SetMode(botty, domain.ModeOp)
		require.NoError(t, sess.persistChannelWindow(ctx, w))

		_, err = sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeOperator, Add: false, Target: "botty"},
			},
		})
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, append(prefix,
			domain.ModeChange{
				Target:     "#chan",
				Nick:       "botty",
				InstanceID: botty.ID(),
				Flag:       domain.ModeOperator,
				Add:        false,
				By:         "testuser",
				At:         fixedTime,
				Instance:   botty,
			},
		), collectEmittedEvents(t, sess))
	})
}

// TestHandleChannelMode_GrantMemberVoice covers `+v` against a
// member with the same shape as the op grant test.
func TestHandleChannelMode_GrantMemberVoice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

		_, err := sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeChannelVoice, Add: true, Target: "botty"},
			},
		})
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, append(prefix,
			domain.ModeChange{
				Target:     "#chan",
				Nick:       "botty",
				InstanceID: botty.ID(),
				Flag:       domain.ModeChannelVoice,
				Add:        true,
				By:         "testuser",
				At:         fixedTime,
				Instance:   botty,
			},
		), collectEmittedEvents(t, sess))
	})
}

// TestHandleChannelMode_SetBooleanAttributes flips every boolean
// attribute flag in a single MODE batch and asserts every change
// produces its broadcast ModeChange in the same order.
func TestHandleChannelMode_SetBooleanAttributes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		flags := []domain.Mode{
			domain.ModeAnonymous,
			domain.ModeInviteOnly,
			domain.ModeModerated,
			domain.ModeNoExternal,
			domain.ModePrivate,
			domain.ModeQuiet,
			domain.ModeSecret,
			domain.ModeTopicLock,
		}

		changes := make([]protocol.ChannelModeChange, 0, len(flags))
		for _, f := range flags {
			changes = append(changes, protocol.ChannelModeChange{Flag: f, Add: true})
		}

		_, err := sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: changes,
		})
		require.NoError(t, err)
		synctest.Wait()

		expected := append([]domain.Event(nil), prefix...)
		for _, f := range flags {
			expected = append(expected, domain.ModeChange{
				Target: "#chan",
				Flag:   f,
				Add:    true,
				By:     "testuser",
				At:     fixedTime,
			})
		}
		require.Equal(t, expected, collectEmittedEvents(t, sess))

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		require.True(t, w.Modes.Anonymous && w.Modes.InviteOnly && w.Modes.Moderated &&
			w.Modes.NoExternal && w.Modes.Private && w.Modes.Quiet && w.Modes.Secret &&
			w.Modes.TopicLock)
	})
}

// TestHandleChannelMode_SetUserLimit captures the parametric form
// of `+l`: the broadcast ModeChange carries the numeric param.
func TestHandleChannelMode_SetUserLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		_, err := sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeUserLimit, Add: true, Param: "10"},
			},
		})
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, append(prefix,
			domain.ModeChange{
				Target: "#chan",
				Flag:   domain.ModeUserLimit,
				Add:    true,
				Param:  "10",
				By:     "testuser",
				At:     fixedTime,
			},
		), collectEmittedEvents(t, sess))

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		require.Equal(t, 10, w.Modes.UserLimit)
	})
}

// TestHandleChannelMode_SetKey captures the parametric form of
// `+k`: the broadcast ModeChange carries the key as its param.
func TestHandleChannelMode_SetKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		_, err := sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeKey, Add: true, Param: "secret"},
			},
		})
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, append(prefix,
			domain.ModeChange{
				Target: "#chan",
				Flag:   domain.ModeKey,
				Add:    true,
				Param:  "secret",
				By:     "testuser",
				At:     fixedTime,
			},
		), collectEmittedEvents(t, sess))

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		require.Equal(t, "secret", w.Modes.Key)
	})
}

// TestHandleChannelMode_ClearParametric covers the remove form:
// `-l` and `-k` clear the field and carry no param on the
// broadcast event.
func TestHandleChannelMode_ClearParametric(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		w.Modes.UserLimit = 10
		w.Modes.Key = "secret"
		require.NoError(t, sess.persistChannelWindow(ctx, w))

		_, err = sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeUserLimit, Add: false},
				{Flag: domain.ModeKey, Add: false},
			},
		})
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, append(prefix,
			domain.ModeChange{Target: "#chan", Flag: domain.ModeUserLimit, Add: false, By: "testuser", At: fixedTime},
			domain.ModeChange{Target: "#chan", Flag: domain.ModeKey, Add: false, By: "testuser", At: fixedTime},
		), collectEmittedEvents(t, sess))

		w, err = sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		require.Equal(t, 0, w.Modes.UserLimit)
		require.Equal(t, "", w.Modes.Key)
	})
}

// TestHandleChannelMode_NonOpRejected: a model-client without `@`
// receives `ChanOpRequiredError`. The user-client's stream sees
// only the join-setup events; no action ModeChange is emitted.
func TestHandleChannelMode_NonOpRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

		bClient := sess.ensureModelClient(ctx, botty)
		require.NotNil(t, bClient)

		resp, err := bClient.Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{{Flag: domain.ModeTopicLock, Add: true}},
		})
		require.NoError(t, err)

		var copReq domain.ChanOpRequiredError
		require.ErrorAs(t, resp.Err, &copReq)
		require.Equal(t, domain.ChannelName("#chan"), copReq.Channel)

		synctest.Wait()
		require.Equal(t, prefix, collectEmittedEvents(t, sess))

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		require.False(t, w.Modes.TopicLock)
	})
}

// TestHandleChannelMode_UnknownFlagRejected: an unknown flag in
// the batch rejects the whole MODE before any earlier valid
// change applies. The user-client's stream sees only the
// join-setup events.
func TestHandleChannelMode_UnknownFlagRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, _ := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		resp, err := sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeTopicLock, Add: true},
				{Flag: domain.Mode('x'), Add: true},
			},
		})
		require.NoError(t, err)

		var unknown domain.UnknownModeFlagError
		require.ErrorAs(t, resp.Err, &unknown)
		require.Equal(t, domain.Mode('x'), unknown.Flag)

		synctest.Wait()
		require.Equal(t, prefix, collectEmittedEvents(t, sess))

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)
		require.False(t, w.Modes.TopicLock, "rejected batch must not apply its earlier valid changes")
	})
}

// TestHandleChannelMode_MissingParamRejected pins the
// `MissingModeParamError` shape across the parametric forms that
// can be missing a value: `+o` without target, `+l` add without
// integer, `+k` add without key.
func TestHandleChannelMode_MissingParamRejected(t *testing.T) {
	tests := []struct {
		name   string
		change protocol.ChannelModeChange
	}{
		{name: "+o no target", change: protocol.ChannelModeChange{Flag: domain.ModeOperator, Add: true}},
		{name: "+v no target", change: protocol.ChannelModeChange{Flag: domain.ModeChannelVoice, Add: true}},
		{name: "+l no param", change: protocol.ChannelModeChange{Flag: domain.ModeUserLimit, Add: true}},
		{name: "+l non-numeric", change: protocol.ChannelModeChange{Flag: domain.ModeUserLimit, Add: true, Param: "ten"}},
		{name: "+l zero", change: protocol.ChannelModeChange{Flag: domain.ModeUserLimit, Add: true, Param: "0"}},
		{name: "+k empty", change: protocol.ChannelModeChange{Flag: domain.ModeKey, Add: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				bootAt := time.Now()
				sess, _ := newTestSession(t)
				ctx := t.Context()

				require.NoError(t, sess.Join(ctx, "#chan"))
				prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

				resp, err := sess.User().Send(ctx, protocol.ChannelMode{
					Channel: "#chan",
					Changes: []protocol.ChannelModeChange{tt.change},
				})
				require.NoError(t, err)

				var missing domain.MissingModeParamError
				require.ErrorAs(t, resp.Err, &missing)
				require.Equal(t, tt.change.Flag, missing.Flag)

				synctest.Wait()
				require.Equal(t, prefix, collectEmittedEvents(t, sess))
			})
		})
	}
}

// TestHandleChannelMode_BatchAppliesInOrder pins the structural
// ordering for a compound MODE: every change emits its own
// ModeChange, in order, after the join-setup prefix. The state
// reflects every successful mutation.
func TestHandleChannelMode_BatchAppliesInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bootAt := time.Now()
		sess, s := newTestSession(t)
		ctx := t.Context()

		require.NoError(t, sess.Join(ctx, "#chan"))
		prefix := joinSetupEventsT(t, sess, bootAt, "#chan")

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

		_, err := sess.User().Send(ctx, protocol.ChannelMode{
			Channel: "#chan",
			Changes: []protocol.ChannelModeChange{
				{Flag: domain.ModeOperator, Add: true, Target: "botty"},
				{Flag: domain.ModeTopicLock, Add: true},
				{Flag: domain.ModeUserLimit, Add: true, Param: "5"},
			},
		})
		require.NoError(t, err)
		synctest.Wait()

		require.Equal(t, append(prefix,
			domain.ModeChange{
				Target: "#chan", Nick: "botty", InstanceID: botty.ID(),
				Flag: domain.ModeOperator, Add: true,
				By: "testuser", At: fixedTime, Instance: botty,
			},
			domain.ModeChange{
				Target: "#chan", Flag: domain.ModeTopicLock, Add: true,
				By: "testuser", At: fixedTime,
			},
			domain.ModeChange{
				Target: "#chan", Flag: domain.ModeUserLimit, Add: true, Param: "5",
				By: "testuser", At: fixedTime,
			},
		), collectEmittedEvents(t, sess))

		w, err := sess.loadChannelWindow(ctx, "#chan")
		require.NoError(t, err)

		m, ok := w.Members.GetByInstance(botty)
		require.True(t, ok)
		require.Equal(t, domain.ModeOp, m.Mode)
		require.True(t, w.Modes.TopicLock)
		require.Equal(t, 5, w.Modes.UserLimit)
	})
}
