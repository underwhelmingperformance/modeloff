// Package testclient provides a minimal [protocol.Client]
// implementation for tests that need a non-user actor on the
// session bus without the dispatch goroutine, history buffer, and
// prompt-assembly machinery a real [modelclient.ModelClient]
// carries. Tests that only need a synthetic instance to issue a
// PrivMsg, a Join, or to be a recipient of broadcast traffic
// construct a [TestClient] and drive it through its [Send] method.
package testclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
)

// TestClient is a [protocol.Client] backed by a synthetic
// `*domain.Instance`. Construct one with [New], call [TestClient.Attach]
// to register it with the session, send commands through
// [TestClient.Send], and call [TestClient.Detach] when the test is
// done. Each test owns the lifecycle of its own client.
type TestClient struct {
	instance *domain.Instance
	sess     *session.Session
	store    instanceStore

	mu  sync.Mutex
	sub protocol.Subscription
}

type instanceStore interface {
	SaveInstance(context.Context, *domain.Instance) error
}

// Option configures a [TestClient] at construction time.
type Option func(*config)

type config struct {
	instanceID domain.InstanceID
	modelID    domain.ModelID
	persona    string
	channels   []domain.ChannelName
}

// WithInstanceID overrides the default `"test-"+nick` instance id.
func WithInstanceID(id domain.InstanceID) Option {
	return func(c *config) { c.instanceID = id }
}

// WithModelID overrides the default `"test/model"` model id.
func WithModelID(id domain.ModelID) Option {
	return func(c *config) { c.modelID = id }
}

// WithPersona sets the persona string stored on the synthetic
// instance.
func WithPersona(p string) Option {
	return func(c *config) { c.persona = p }
}

// WithChannels seeds the synthetic instance's channel membership.
// The channels are not joined through the wire — callers that want
// a JOIN event on the bus should issue [protocol.Join] through
// [TestClient.Send] after [TestClient.Attach].
func WithChannels(channels ...domain.ChannelName) Option {
	return func(c *config) { c.channels = append(c.channels, channels...) }
}

// New returns an unattached [TestClient] for `nick`. The client is
// inert until [TestClient.Attach] runs.
func New(nick domain.Nick, sess *session.Session, opts ...Option) *TestClient {
	return newClient(nick, sess, nil, opts...)
}

// NewStored returns an unattached TestClient whose synthetic instance
// can be persisted during Attach. Test fixtures pass their store
// explicitly so the production Session API does not expose an actor-
// registration bypass.
func NewStored(
	nick domain.Nick,
	sess *session.Session,
	store instanceStore,
	opts ...Option,
) *TestClient {
	return newClient(nick, sess, store, opts...)
}

func newClient(
	nick domain.Nick,
	sess *session.Session,
	store instanceStore,
	opts ...Option,
) *TestClient {
	cfg := config{
		instanceID: domain.InstanceID("test-" + nick),
		modelID:    "test/model",
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	channels := buildChannelMembership(cfg.channels)

	return &TestClient{
		instance: domain.NewModelInstance(cfg.instanceID, nick, cfg.modelID, cfg.persona, channels),
		sess:     sess,
		store:    store,
	}
}

// Instance returns a snapshot of the synthetic actor.
func (tc *TestClient) Instance() *domain.Instance { return tc.instance.Snapshot() }

// Identity reports the instance's id.
func (tc *TestClient) Identity() protocol.ClientID {
	return protocol.ClientID(tc.instance.ID())
}

// Send routes `cmd` through the session's dispatcher with this
// client as the issuing actor.
func (tc *TestClient) Send(ctx context.Context, cmd protocol.Command) (protocol.Response, error) {
	return tc.sess.Handle(ctx, tc, cmd)
}

// Events returns the per-subscription delivery stream, or nil if
// the client has not been attached.
func (tc *TestClient) Events() <-chan protocol.Delivery {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.sub == nil {
		return nil
	}

	return tc.sub.Events()
}

// Caps returns an empty capability holder. The chatcmd visibility
// filter is not exercised through a [TestClient].
func (tc *TestClient) Caps() command.CapabilityHolder {
	return command.NoCapabilities()
}

// Attach persists the synthetic instance and registers the client
// with the session.
//
// Attach is idempotent: a repeat call on an already-attached
// client returns nil.
func (tc *TestClient) Attach(ctx context.Context) error {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.sub != nil {
		return nil
	}

	if tc.store == nil {
		return fmt.Errorf("attach test client %q: no test store", tc.instance.ID())
	}

	if err := tc.store.SaveInstance(ctx, tc.instance); err != nil {
		return fmt.Errorf("save test instance %q: %w", tc.instance.ID(), err)
	}

	sub, err := tc.sess.AttachClient(ctx, tc)
	if err != nil {
		return fmt.Errorf("attach test client %q: %w", tc.instance.ID(), err)
	}

	tc.sub = sub
	return nil
}

// Detach releases the subscription. Idempotent on a never-attached
// or already-detached client.
func (tc *TestClient) Detach() {
	tc.mu.Lock()
	sub := tc.sub
	tc.sub = nil
	tc.mu.Unlock()

	if sub != nil {
		sub.Unsubscribe()
	}
}

func buildChannelMembership(channels []domain.ChannelName) *orderedmap.OrderedMap[domain.ChannelName, time.Time] {
	if len(channels) == 0 {
		return nil
	}

	m := orderedmap.New[domain.ChannelName, time.Time]()
	for _, ch := range channels {
		m.Set(ch, time.Time{})
	}
	return m
}
