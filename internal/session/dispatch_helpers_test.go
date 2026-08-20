package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/ui/chatcmd"
)

// chatcmdToolRegistry is the chatcmd-derived tool registry the
// test fixture wires into every modelclient it constructs. The
// chatcmd grammar is the production source of truth for msg / me /
// pass and the channel-management tools the dispatch loop now
// drives.
var chatcmdToolRegistry = func() *modelclient.ToolRegistry {
	r, err := chatcmd.BuildToolRegistry()
	if err != nil {
		panic(fmt.Errorf("build chatcmd tool registry: %w", err))
	}
	return r
}()

// msgToolCalls builds a [api.CompletionResult] whose PendingToolCalls
// invoke the `msg` tool once per body — the wire-shape the new
// dispatch loop expects when a model wants to say something. The
// `body` field on MsgCommand is a `[]string`, so the JSON shape is
// an array of words (one element here per call).
func msgToolCalls(t testing.TB, target domain.ChannelName, bodies ...string) api.CompletionResult {
	t.Helper()

	calls := make([]api.PendingToolCall, 0, len(bodies))
	for i, body := range bodies {
		args, err := json.Marshal(map[string]any{
			"target": string(target),
			"body":   []string{body},
		})
		require.NoError(t, err)

		calls = append(calls, api.PendingToolCall{
			ID:   fmt.Sprintf("call_msg_%d", i),
			Name: "msg",
			Args: args,
		})
	}

	return api.CompletionResult{PendingToolCalls: calls}
}

// whoisToolCall builds a [api.CompletionResult] whose
// PendingToolCalls invoke the `whois` tool for `nick`. A model that
// runs this in a dispatch turn receives a [domain.Whois] reply, which
// [modelclient.ModelClient.Send] files into its private replies ring.
func whoisToolCall(t testing.TB, nick domain.Nick) api.CompletionResult {
	t.Helper()

	args, err := json.Marshal(map[string]any{"nick": string(nick)})
	require.NoError(t, err)

	return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
		{ID: "call_whois_0", Name: "whois", Args: args},
	}}
}

// meToolCall builds a [api.CompletionResult] whose PendingToolCalls
// invoke the `me` tool with the given action body.
func meToolCall(t testing.TB, target domain.ChannelName, body string) api.CompletionResult {
	t.Helper()

	args, err := json.Marshal(map[string]any{
		"target": string(target),
		"action": []string{body},
	})
	require.NoError(t, err)

	return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
		{ID: "call_me_0", Name: "me", Args: args},
	}}
}

// continueOnceWith builds a `continueWithToolResultsFn` that
// captures the first turn's tool results into `*captured` and
// returns `first`; every subsequent turn returns an empty result,
// which terminates the tool loop. This lets tests pin the
// tool-result shape from the initial round-trip without their fake
// driving the loop forever.
func continueOnceWith(captured *[]api.ToolResult, first api.CompletionResult) func(context.Context, *api.Conversation, []api.ToolResult) (api.CompletionResult, error) {
	turn := 0
	return func(_ context.Context, _ *api.Conversation, results []api.ToolResult) (api.CompletionResult, error) {
		defer func() { turn++ }()
		if turn == 0 {
			*captured = results
			return first, nil
		}
		return api.CompletionResult{}, nil
	}
}

// msgSpansToolCall builds a [api.CompletionResult] whose
// PendingToolCalls invoke the `msg` tool with structured spans
// rather than a plain body. The dispatch loop's `msg` tool encodes
// styled spans into IRC wire control characters via `ircfmt`; tests
// that pin the encoded shape use this helper.
func msgSpansToolCall(t testing.TB, target domain.ChannelName, spans []protocol.ReplySpan) api.CompletionResult {
	t.Helper()

	args, err := json.Marshal(map[string]any{
		"target": string(target),
		"spans":  spans,
	})
	require.NoError(t, err)

	return api.CompletionResult{PendingToolCalls: []api.PendingToolCall{
		{ID: "call_msg_spans_0", Name: "msg", Args: args},
	}}
}

// dispatchUserMessage sends `body` to `ch` as the user and returns
// once every dispatch turn it woke has finished, including the
// turns a model's reply woke in its peers.
//
// This is the production path: the message fans out over the bus and
// each model-client's own dispatch goroutine decides whether to take
// a turn. Settling is [synctest.Wait], so the caller must be inside
// a [synctest.Test] bubble and the wait is exact — no polling and no
// deadline.
func dispatchUserMessage(ctx context.Context, t testing.TB, sess *Session, ch domain.ChannelName, body string) domain.Message {
	t.Helper()

	msg, err := userSendMessage(ctx, t, sess, ch, body)
	require.NoError(t, err)

	synctest.Wait()

	return msg
}

// dispatchRecorder captures the triggers each model was dispatched
// with, flattened across turns.
//
// Turn boundaries are not a stable fact about a burst: a model that
// is mid-turn when two events arrive takes both in one coalesced
// turn, and one that is idle takes them in two, so where the splits
// fall depends on when each dispatch goroutine last read its queue.
// Which triggers reach which model does not depend on that, and it
// is what the echo gate and the membership filter promise.
type dispatchRecorder struct {
	mu       sync.Mutex
	triggers map[domain.ModelID][]protocol.IRCMessage
}

func newDispatchRecorder() *dispatchRecorder {
	return &dispatchRecorder{triggers: make(map[domain.ModelID][]protocol.IRCMessage)}
}

// record files one turn's triggers under the dispatched model.
func (r *dispatchRecorder) record(modelID domain.ModelID, events []protocol.IRCMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.triggers[modelID] = append(r.triggers[modelID], events...)
}

// byModel returns the triggers each model saw, in the order its own
// dispatch loop read them.
func (r *dispatchRecorder) byModel() map[domain.ModelID][]protocol.IRCMessage {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make(map[domain.ModelID][]protocol.IRCMessage, len(r.triggers))
	for id, msgs := range r.triggers {
		out[id] = append([]protocol.IRCMessage(nil), msgs...)
	}

	return out
}

// triggeredBy reports whether a dispatch batch carries a message
// from `nick`. A batch can carry several triggers, so a fixture that
// answers one participant asks about the whole batch.
func triggeredBy(events []protocol.IRCMessage, nick domain.Nick) bool {
	return slices.ContainsFunc(events, func(e protocol.IRCMessage) bool {
		return e.From == string(nick)
	})
}

// dispatchUserMessageAwaitingTurns sends `body` to `ch` as the user
// and returns once `turns` dispatch turns have reported
// `ModelDispatchDone` on the user-client's bus.
//
// [dispatchUserMessage] is the settle to reach for. This one is for
// the fixtures that include an HTTP server: net/http parks a
// connection's read loop on the network for as long as the
// connection is pooled, and a goroutine parked there never parks
// durably — so a bubble containing one never settles and
// [synctest.Wait] would sit there until the test timed out.
func dispatchUserMessageAwaitingTurns(ctx context.Context, t *testing.T, sess *Session, ch domain.ChannelName, body string, turns int) {
	t.Helper()

	_, err := userSendMessage(ctx, t, sess, ch, body)
	require.NoError(t, err)

	drainEvents(t, sess, turns)
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

	// attachErr, when set, fails every attach. A model instance whose
	// client cannot connect is what the ADDMODEL unwind is about.
	attachErr error

	// clients and draining mirror the production manager's two sets:
	// attached model-clients, and released ones whose dispatch
	// goroutines are still unwinding. `detachAll` joins both, so a
	// test that quits or kills a model does not leave its goroutine
	// running past the end of the test with the `t`-scoped store
	// still in hand.
	mu       sync.Mutex
	clients  map[protocol.ClientID]*modelclient.ModelClient
	draining []*modelclient.ModelClient
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
	if f.attachErr != nil {
		return nil, f.attachErr
	}

	id := protocol.ClientID(inst.ID())

	f.mu.Lock()
	if existing, ok := f.clients[id]; ok {
		f.mu.Unlock()
		return existing, nil
	}

	apiClient := f.apiClient
	mc := modelclient.New(inst, sess, func() api.Client { return apiClient }, f.memStore, chatcmdToolRegistry, nil, nil, sess.baseContext, nil)
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

// Detach mirrors the production manager: release only, so a model
// that ends its own connection from inside a dispatch turn does not
// wait on the goroutine it is running on, with the released client
// held for `detachAll` to join.
func (f *testModelClientFactory) Detach(id protocol.ClientID) {
	f.mu.Lock()
	mc, ok := f.clients[id]
	if ok {
		delete(f.clients, id)
		f.draining = append(f.draining, mc)
	}
	f.mu.Unlock()

	if !ok {
		return
	}

	mc.Release()
}

// attached returns the identities the factory currently holds a
// model-client for.
func (f *testModelClientFactory) attached() []protocol.ClientID {
	f.mu.Lock()
	defer f.mu.Unlock()

	ids := make([]protocol.ClientID, 0, len(f.clients))
	for id := range f.clients {
		ids = append(ids, id)
	}

	return ids
}

func (f *testModelClientFactory) detachAll() {
	f.mu.Lock()
	clients := make([]*modelclient.ModelClient, 0, len(f.clients)+len(f.draining))
	for _, mc := range f.clients {
		clients = append(clients, mc)
	}
	clients = append(clients, f.draining...)
	f.clients = make(map[protocol.ClientID]*modelclient.ModelClient)
	f.draining = nil
	f.mu.Unlock()

	for _, mc := range clients {
		mc.Release()
	}

	for _, mc := range clients {
		mc.Wait()
	}
}
