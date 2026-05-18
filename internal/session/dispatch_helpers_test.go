package session

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

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
) error {
	f, ok := sess.modelClientFactory.(*testModelClientFactory)
	if !ok {
		return fmt.Errorf("dispatchToChannel: factory is %T, expected *testModelClientFactory", sess.modelClientFactory)
	}
	d := modelclient.NewDispatcher(sess, f.apiClient, f.memStore, chatcmdToolRegistry, nil)
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
	mc := modelclient.New(inst, sess, func() api.Client { return apiClient }, f.memStore, chatcmdToolRegistry, nil, sess.baseContext)
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
