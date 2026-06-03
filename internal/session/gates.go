package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// checkSendGates enforces the per-channel PRIVMSG / Action
// preconditions tied to channel modes: `+n` requires the sender
// to be a channel member; `+m` requires the sender to hold voice
// or op; `+q` silences everyone except ops. DMs (non-channel
// targets) skip the check — they have no member list and no
// channel modes.
//
// Each rejection carries a typed [domain.SendBlockReason] so
// renderers can format the right message without parsing a
// free-form error string.
func (s *Session) checkSendGates(ctx context.Context, actor *domain.Instance, ch domain.ChannelName) error {
	if domain.InferChannelKind(ch) != domain.KindChannel {
		return nil
	}

	window, err := s.loadChannelWindow(ctx, ch)
	if errors.Is(err, store.ErrNoSuchChannel) {
		// A missing channel has no gates of its own; the
		// append/emit path produces the right error downstream.
		return nil
	}
	if err != nil {
		return fmt.Errorf("load channel: %w", err)
	}

	member, isMember := window.Members.GetByInstance(actor)

	if window.Modes.NoExternal && !isMember {
		return domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockNoExternal, At: s.now()}
	}

	if window.Modes.Quiet && (!isMember || member.Mode != domain.ModeOp) {
		return domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockQuiet, At: s.now()}
	}

	if window.Modes.Moderated {
		if !isMember || (member.Mode != domain.ModeOp && member.Mode != domain.ModeVoice) {
			return domain.CannotSendToChannelError{Channel: ch, Reason: domain.SendBlockModerated, At: s.now()}
		}
	}

	return nil
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
	if !ok || member.Mode != domain.ModeOp {
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
