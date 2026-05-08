package session

import (
	"context"

	"github.com/laney/modeloff/internal/protocol"
)

// serverClient is the session-side concrete implementation of
// [protocol.Client]. One instance per subscription: the user-client
// is created at session bootstrap. The struct keeps a back-reference
// to its owning session so `Send` can route through [Session.Handle].
//
// The mode set is constructed once and never mutated; reads need no
// synchronisation.
type serverClient struct {
	sess   *Session
	id     protocol.ClientID
	events chan protocol.Event
	modes  map[protocol.UserMode]struct{}
}

// newServerClient constructs a subscription with the given identity
// and modes.
func newServerClient(sess *Session, id protocol.ClientID, modes ...protocol.UserMode) *serverClient {
	modeSet := make(map[protocol.UserMode]struct{}, len(modes))
	for _, m := range modes {
		modeSet[m] = struct{}{}
	}

	return &serverClient{
		sess:   sess,
		id:     id,
		events: make(chan protocol.Event, eventBufSize),
		modes:  modeSet,
	}
}

func (c *serverClient) Identity() protocol.ClientID { return c.id }

func (c *serverClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return c.sess.Handle(ctx, c, cmd)
}

func (c *serverClient) Events() <-chan protocol.Event { return c.events }

func (c *serverClient) HasMode(m protocol.UserMode) bool {
	_, ok := c.modes[m]
	return ok
}
