package modelclient

import (
	"context"
	"fmt"
	"slices"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// providerTargetProjection maps session-internal DM recipient IDs to
// the current IRC nicks in the copy of the transcript sent upstream.
// The window and stored events keep their stable IDs; this value lasts
// for one provider request and holds no conversation state.
type providerTargetProjection struct {
	direct   bool
	selfID   domain.InstanceID
	selfNick domain.Nick
	peerID   domain.InstanceID
	peerNick domain.Nick
}

func newProviderTargetProjection(
	ctx context.Context,
	sess Session,
	window protocol.WindowContext,
	selfID domain.InstanceID,
	selfNick domain.Nick,
) (providerTargetProjection, error) {
	peer, direct := protocol.DirectWindowPeer(window.Target())
	if !direct {
		return providerTargetProjection{}, nil
	}

	peerID := peer
	peerNick, err := sess.ResolveInstanceByID(ctx, peerID)
	if err != nil {
		return providerTargetProjection{}, fmt.Errorf("resolve DM counterpart %q: %w", peerID, err)
	}

	return providerTargetProjection{
		direct:   true,
		selfID:   selfID,
		selfNick: selfNick,
		peerID:   peerID,
		peerNick: peerNick,
	}, nil
}

func (p providerTargetProjection) messages(
	messages []protocol.IRCMessage,
) []protocol.IRCMessage {
	if !p.direct {
		return messages
	}

	projected := slices.Clone(messages)
	for i := range projected {
		projected[i] = p.message(projected[i])
	}

	return projected
}

func (p providerTargetProjection) reply(
	window protocol.WindowTarget,
	message protocol.IRCMessage,
) protocol.IRCMessage {
	if window == nil {
		return message
	}

	return p.message(message)
}

func (p providerTargetProjection) bindCommand(cmd protocol.Command) protocol.Command {
	switch value := cmd.(type) {
	case protocol.PrivMsg:
		value.Target = p.bindMessageTarget(value.Target)

		return value
	case protocol.Action:
		value.Target = p.bindMessageTarget(value.Target)

		return value
	default:
		return cmd
	}
}

func (p providerTargetProjection) bindMessageTarget(target protocol.MsgTarget) protocol.MsgTarget {
	if !p.direct {
		return target
	}

	nick, ok := target.(protocol.NickTarget)
	if !ok || !domain.EqualNick(domain.Nick(nick), p.peerNick) {
		return target
	}

	return protocol.ClientTarget(p.peerID)
}

func (p providerTargetProjection) message(message protocol.IRCMessage) protocol.IRCMessage {
	switch message.Kind {
	case protocol.KindPrivMsg, protocol.KindAction:
	case protocol.KindServerReply:
		// A WHOIS or LIST answer describes the server and not a
		// window, so it carries no target. The user's InstanceID is
		// the empty sentinel too, and matching the two would address
		// every such answer to the person the model is talking to.
		if message.Target == "" {
			return message
		}
	default:
		return message
	}

	switch message.Target {
	case string(p.selfID):
		message.Target = string(p.selfNick)
	case string(p.peerID):
		message.Target = string(p.peerNick)
	}

	return message
}
