// Package modelclient holds the model-client implementation of
// [protocol.Client]. A model-client represents a single LLM
// instance participating in the session: it attaches itself to the
// session via [Session.Subscribe], holds the resulting
// [protocol.Subscription], and acts as the actor for any commands
// the LLM issues during a dispatch turn.
//
// The package's current scope is the client handle and its
// attach/detach lifecycle. The dispatch goroutine and prompt
// assembly still live in the `session` package; a future commit
// moves them here and lets [Session] drop its `api.Client`,
// `memory.Store`, and tool-registry fields.
package modelclient

import (
	"context"
	"fmt"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// Session is the dependency surface a [ModelClient] needs from the
// session: subscription registration and the command-handling
// entry point its `Send` routes through.
type Session interface {
	Subscribe(c protocol.Client, opts protocol.SubscribeOptions) (protocol.Subscription, error)
	Handle(ctx context.Context, c protocol.Client, cmd protocol.Command) (protocol.Response, error)
}

// ModelClient is the [protocol.Client] backing a single LLM
// instance. Construct one per instance and call [ModelClient.Attach]
// to register it with a session; call [ModelClient.Detach] to
// release the subscription.
type ModelClient struct {
	instance *domain.Instance
	sess     Session
	sub      protocol.Subscription
}

// New returns an unattached `ModelClient` for `inst`. The client
// is inert until [ModelClient.Attach] runs.
func New(inst *domain.Instance, sess Session) *ModelClient {
	return &ModelClient{instance: inst, sess: sess}
}

// Instance returns the canonical actor handle.
func (mc *ModelClient) Instance() *domain.Instance { return mc.instance }

// Identity reports the client's stable id, equal to the instance's
// id by construction.
func (mc *ModelClient) Identity() protocol.ClientID {
	return protocol.ClientID(mc.instance.ID())
}

// Send routes `cmd` through the session's dispatcher with this
// client as the issuing actor.
func (mc *ModelClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return mc.sess.Handle(ctx, mc, cmd)
}

// Events returns the per-subscription delivery stream, or nil if
// the client has not been attached.
func (mc *ModelClient) Events() <-chan protocol.Delivery {
	if mc.sub == nil {
		return nil
	}
	return mc.sub.Events()
}

// HasMode reports false for any mode: model-clients carry no
// user modes. Operator promotion would happen via a wire `OPER`
// exchange the dispatcher would track on the per-subscription
// envelope; until that path exists the answer is always false.
func (mc *ModelClient) HasMode(_ domain.Mode) bool { return false }

// Caps exposes a static capability holder reporting no
// capabilities. The chatcmd grammar's `caps:` filter therefore
// hides operator-gated tools from model invocations.
func (mc *ModelClient) Caps() command.CapabilityHolder { return modelCaps{} }

// Attach registers the client with its session and stores the
// resulting subscription handle. Returns the registration error
// from [Session.Subscribe]; the client remains inert on failure.
func (mc *ModelClient) Attach() error {
	sub, err := mc.sess.Subscribe(mc, protocol.SubscribeOptions{Instance: mc.instance})
	if err != nil {
		return fmt.Errorf("attach model client %q: %w", mc.instance.ID(), err)
	}

	mc.sub = sub
	return nil
}

// Detach releases the subscription. Idempotent on an already-
// detached or never-attached client.
func (mc *ModelClient) Detach() {
	if mc.sub == nil {
		return
	}

	mc.sub.Unsubscribe()
	mc.sub = nil
}

// modelCaps is the no-capabilities holder returned by
// [ModelClient.Caps].
type modelCaps struct{}

func (modelCaps) Has(_ command.Capability) bool { return false }
