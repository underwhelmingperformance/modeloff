package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// checkSendGates enforces the PRIVMSG / Action preconditions for a
// channel target. A channel that does not exist is refused with
// [domain.NoSuchChannelError]: RFC 2812 §3.3.1 answers a PRIVMSG
// naming an unknown target with 401 ERR_NOSUCHNICK, so a message
// addressed to nowhere is refused at the gate and never reaches the
// event log. The mode gates follow: `+n` requires the sender to be a
// channel member; `+m` requires the sender to hold voice or op; `+q`
// silences everyone except ops; `+f` caps how many messages the
// channel relays per flood window. DMs (non-channel targets) skip
// the whole check, because they have no member list and no channel
// modes.
//
// `+f` is checked last, and it is the one gate that changes state:
// a message an earlier gate refuses was never going to be relayed,
// so it must not spend the channel's flood budget.
//
// Each rejection names a typed [domain.SendBlockReason] so
// renderers can format the right message without parsing a
// free-form error string.
//
// The first return is the target's canonical name: for a channel,
// the spelling the channel exists under, so a PRIVMSG to `#Dev` is
// logged and broadcast against `#dev`. For a DM the target is
// returned unchanged.
func (s *Session) checkSendGates(ctx context.Context, actor *domain.Instance, ch domain.ChannelName) (domain.ChannelName, error) {
	if domain.InferChannelKind(ch) != domain.KindChannel {
		return ch, nil
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if err != nil {
		if errors.Is(err, store.ErrNoSuchChannel) {
			return ch, domain.NoSuchChannelError{Channel: ch, At: s.now()}
		}

		return ch, fmt.Errorf("load channel: %w", err)
	}

	ch = window.Name()

	member, isMember := window.Members.GetByInstance(actor)

	if window.Modes.NoExternal && !isMember {
		return ch, domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockNoExternal, At: s.now()}
	}

	if window.Modes.Quiet && (!isMember || !member.Modes.Operator) {
		return ch, domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockQuiet, At: s.now()}
	}

	if window.Modes.Moderated {
		if !isMember || (!member.Modes.Operator && !member.Modes.Voice) {
			return ch, domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockModerated, At: s.now()}
		}
	}

	return ch, s.checkChannelFlood(ch, window.Modes)
}

// checkJoinGates enforces the per-channel JOIN preconditions
// every existing channel imposes via its mode set: `+i` admits
// only previously-invited nicks (and consumes the invitation on
// success); `+l` rejects when the member count reaches the
// limit; `+k` rejects on key mismatch.
//
// Returns nil for a channel with no gates active. On `+i` the
// consume happens on success — the next attempt by the same
// nick fails unless re-invited.
func (s *Session) checkJoinGates(window *domain.ChannelWindow, actorNick domain.Nick, key string) error {
	if window.Modes.Key != "" && key != window.Modes.Key {
		return domain.ChannelKeyMismatchError{Channel: window.Name(), At: s.now()}
	}

	if window.Modes.UserLimit > 0 && window.Members.Len() >= window.Modes.UserLimit {
		return domain.ChannelFullError{Channel: window.Name(), At: s.now()}
	}

	if window.Modes.InviteOnly {
		if !window.InvitedNicks.Contains(actorNick) {
			return domain.ChannelInviteOnlyError{Channel: window.Name(), At: s.now()}
		}
		window.InvitedNicks.Remove(actorNick)
	}

	return nil
}

// requireChannelOp returns [domain.ChanOpRequiredError] when the
// actor lacks `@` in `window`. Used by channel-op-gated commands
// (`MODE`, `KICK`, `+t`-conditional `TOPIC`, `+i`-conditional
// `INVITE`) to short-circuit before mutation.
//
// Server operators (`+o` user-mode) override the channel-op
// requirement — RFC 2812 §3.7 and common ircd practice: a
// network operator can act on any channel regardless of channel-
// op status. In modeloff the user-client is the only server-OPER
// today; this is what lets the user `/kick` or `/topic` on
// channels where they joined without picking up `@`.
func (s *Session) requireChannelOp(actor *domain.Instance, window *domain.ChannelWindow, cmd string, ch domain.ChannelName) error {
	if s.actorHasServerOper(actor) {
		return nil
	}

	member, ok := window.Members.GetByInstance(actor)
	if !ok || !member.Modes.Operator {
		return domain.ChanOpRequiredError{Command: cmd, Channel: ch, At: s.now()}
	}
	return nil
}

// actorHasServerOper reports whether the actor's wire client
// carries `+o` user-mode. Used by [requireChannelOp] to honour
// the server-operator override on channel-op-gated commands.
func (s *Session) actorHasServerOper(actor *domain.Instance) bool {
	return s.idHasServerOper(actor.ID())
}

// idHasServerOper reports whether the subscription registered under
// `id` carries `+o`. The dispatcher's operator gate reads this so an
// [protocol.Oper] elevation written to the serverClient is honoured
// without the issuing client object changing.
func (s *Session) idHasServerOper(id protocol.ClientID) bool {
	sc := s.lookupClientHandle(id)
	return sc != nil && sc.HasMode(domain.ModeOperator)
}
