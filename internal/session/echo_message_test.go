package session

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// passiveClient is a minimal [protocol.Client] whose subscription is
// drained directly by the test, with no dispatch goroutine consuming
// it. It models a client that holds no echo-message capability.
type passiveClient struct {
	id  protocol.ClientID
	sub protocol.Subscription
}

func (c *passiveClient) Identity() protocol.ClientID { return c.id }

func (c *passiveClient) Send(context.Context, protocol.Command) (protocol.Response, error) {
	return protocol.Response{}, nil
}

func (c *passiveClient) Events() <-chan protocol.Delivery {
	if c.sub == nil {
		return nil
	}

	return c.sub.Events()
}

func (c *passiveClient) Caps() command.CapabilityHolder { return command.NoCapabilities() }

// drainDeliveries non-blockingly returns every event currently queued
// on the client's subscription, in arrival order.
func drainDeliveries(c protocol.Client) []domain.Event {
	var events []domain.Event

	for {
		select {
		case d := <-c.Events():
			events = append(events, d.Event)
		default:
			return events
		}
	}
}

// A subscription without echo-message receives no copy of its own
// chat traffic: it is skipped by the membership fan-out as the
// originator (RFC 2812 §3.3.1) and by [Session.echoToOriginator]'s
// capability gate.
func TestEchoToOriginator_without_cap_no_self_echo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sess, s := newTestSession(t)
		ctx := t.Context()

		botty := seedInstance(t, sess, s, instanceSpec{
			Nick:     "botty",
			ModelID:  "test/model",
			Channels: testChannels("#chan"),
		})
		seedChannelWithMembers(t, sess, s, "#chan", "testuser", "botty")

		bc := &passiveClient{id: protocol.ClientID(botty.ID())}
		sub, err := sess.Subscribe(bc, protocol.SubscribeOptions{Instance: botty})
		require.NoError(t, err)
		bc.sub = sub

		drainDeliveries(bc)

		_, err = sess.sendMessageAs(ctx, botty, "#chan", "anyone about?")
		require.NoError(t, err)
		synctest.Wait()

		require.Empty(t, drainDeliveries(bc))
	})
}
