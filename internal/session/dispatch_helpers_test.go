package session

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
)

// dispatchToChannel runs the synchronous broadcast-to-channel
// dispatch the test suite uses to drive end-to-end model
// behaviour. The session's [ModelClientFactory] arrived from
// [newTestModelClientFactory], which stores the api client + the
// optional memory backing — those are the same handles a
// production manager threads into each [modelclient.ModelClient],
// so the helper reuses them through a type assertion.
func dispatchToChannel(
	ctx context.Context,
	sess *Session,
	ch domain.ChannelName,
	msgs []protocol.IRCMessage,
) ([]domain.ModelReplyEvent, error) {
	f, ok := sess.modelClientFactory.(*testModelClientFactory)
	if !ok {
		return nil, fmt.Errorf("dispatchToChannel: factory is %T, expected *testModelClientFactory", sess.modelClientFactory)
	}
	d := modelclient.NewDispatcher(sess, f.apiClient, f.memStore, nil, nil)
	return d.DispatchToChannel(ctx, ch, msgs)
}

// attachModelClient routes through the session's
// [ModelClientFactory] to attach a model-client for `inst`. The
// returned handle is the factory's canonical entry for the
// instance — the same handle an attach via JOIN / ADDMODEL /
// INVITE produces — so a subsequent QUIT / KILL detach goes
// through the factory's registry and joins the dispatch
// goroutine deterministically.
func attachModelClient(t testing.TB, sess *Session, inst *domain.Instance) protocol.Client {
	t.Helper()

	client, err := sess.modelClientFactory.Attach(t.Context(), sess, inst)
	if err != nil {
		t.Fatalf("attach model client: %v", err)
	}

	return client
}

// testModelClientFactory satisfies [ModelClientFactory] by
// constructing [modelclient.ModelClient]s over the supplied api
// and memory handles. The fixture wires one through `New` so JOIN
// / ADDMODEL / INVITE handlers attach a real modelclient-side
// dispatch goroutine, matching production behaviour, while
// keeping the test fixture independent of the modelmanager
// package.
type testModelClientFactory struct {
	t         testing.TB
	apiClient api.Client
	memStore  memory.Store
	nick      domain.Nick

	mu      sync.Mutex
	clients map[protocol.ClientID]*modelclient.ModelClient
}

func newTestModelClientFactory(t testing.TB, apiClient api.Client) *testModelClientFactory {
	return newTestModelClientFactoryWith(t, apiClient, nil)
}

func newTestModelClientFactoryWith(t testing.TB, apiClient api.Client, memStore memory.Store) *testModelClientFactory {
	f := &testModelClientFactory{
		t:         t,
		apiClient: apiClient,
		memStore:  memStore,
		nick:      "fakenick",
		clients:   make(map[protocol.ClientID]*modelclient.ModelClient),
	}
	t.Cleanup(f.detachAll)
	return f
}

// PrepareInstance returns a fixed persona-trimmed pair so the
// session's `addModelAs` can build a fresh instance without
// reaching for an LLM. Tests that rely on the persona arbitration
// or unique-nick generation paths construct the manager directly.
func (f *testModelClientFactory) PrepareInstance(_ context.Context, _ *Session, _ domain.ModelID, persona string) (domain.Nick, string, error) {
	return f.nick, persona, nil
}

func (f *testModelClientFactory) Attach(ctx context.Context, sess *Session, inst *domain.Instance) (protocol.Client, error) {
	id := protocol.ClientID(inst.ID())

	f.mu.Lock()
	if existing, ok := f.clients[id]; ok {
		f.mu.Unlock()
		return existing, nil
	}

	apiClient := f.apiClient
	mc := modelclient.New(inst, sess, func() api.Client { return apiClient }, f.memStore, nil, nil, sess.baseContext)
	f.clients[id] = mc
	f.mu.Unlock()

	if err := mc.Attach(ctx); err != nil {
		f.mu.Lock()
		delete(f.clients, id)
		f.mu.Unlock()
		return nil, fmt.Errorf("attach: %w", err)
	}

	return mc, nil
}

func (f *testModelClientFactory) Detach(id protocol.ClientID) {
	f.mu.Lock()
	mc, ok := f.clients[id]
	if ok {
		delete(f.clients, id)
	}
	f.mu.Unlock()

	if !ok {
		return
	}

	mc.Detach()
}

func (f *testModelClientFactory) detachAll() {
	f.mu.Lock()
	clients := make([]*modelclient.ModelClient, 0, len(f.clients))
	for _, mc := range f.clients {
		clients = append(clients, mc)
	}
	f.clients = make(map[protocol.ClientID]*modelclient.ModelClient)
	f.mu.Unlock()

	for _, mc := range clients {
		mc.Detach()
	}
}
