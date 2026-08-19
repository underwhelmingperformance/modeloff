package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/userclient"
)

// managerFakeAPI is the integration-test fake. The callbacks are
// optional; the default `GenerateNick` numbers retries by
// exclusion-list length so AddModel-twice tests get distinct nicks
// without each case maintaining its own counter.
type managerFakeAPI struct {
	listModelsFn       func(context.Context) ([]api.ModelInfo, error)
	sendEventsFn       func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error)
	generateNickFn     func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error)
	generatePersonasFn func(context.Context, domain.ModelID) ([]domain.Persona, error)
}

func (f *managerFakeAPI) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.listModelsFn != nil {
		return f.listModelsFn(ctx)
	}

	return nil, nil
}

func (f *managerFakeAPI) SendEvents(
	ctx context.Context,
	modelID domain.ModelID,
	selfInstanceID domain.InstanceID,
	system string,
	history []protocol.IRCMessage,
	events []protocol.IRCMessage,
	_ ...api.ToolDefinition,
) (api.CompletionResult, error) {
	if f.sendEventsFn != nil {
		return f.sendEventsFn(ctx, modelID, selfInstanceID, system, history, events)
	}

	return api.CompletionResult{}, nil
}

func (f *managerFakeAPI) ContinueWithToolResults(
	context.Context,
	*api.Conversation,
	[]api.ToolResult,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}

func (f *managerFakeAPI) GenerateNick(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (api.NicknameResult, error) {
	if f.generateNickFn != nil {
		nick, err := f.generateNickFn(ctx, smallModel, persona, exclude)
		return api.NicknameResult{Nick: nick}, err
	}

	nick := domain.Nick("fakenick")
	if len(exclude) > 0 {
		nick = domain.Nick(fmt.Sprintf("fakenick%d", len(exclude)))
	}

	return api.NicknameResult{Nick: nick}, nil
}

func (f *managerFakeAPI) GeneratePersonas(ctx context.Context, smallModel domain.ModelID) ([]domain.Persona, error) {
	if f.generatePersonasFn != nil {
		return f.generatePersonasFn(ctx, smallModel)
	}

	return nil, nil
}

// listModelsCountingClient records the number of `ListModels` calls
// so short-circuit tests can assert the upstream is not re-hit after
// a known failure.
type listModelsCountingClient struct {
	managerFakeAPI

	calls atomic.Int32
	err   error
	infos []api.ModelInfo
}

func (c *listModelsCountingClient) ListModels(context.Context) ([]api.ModelInfo, error) {
	c.calls.Add(1)

	if c.err != nil {
		return nil, c.err
	}

	return c.infos, nil
}

// logBuffer is a thread-safe bytes.Buffer that captures slog JSON
// output and allows searching for records by message.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (lb *logBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	return lb.buf.Write(p)
}

func (lb *logBuffer) find(msg string) map[string]any {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	for line := range bytes.SplitSeq(lb.buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			continue
		}

		if record["msg"] == msg {
			return record
		}
	}

	return nil
}

func installLogCapture(t *testing.T) *logBuffer {
	t.Helper()

	buf := &logBuffer{}
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	return buf
}

func testPersonas() []domain.Persona {
	return []domain.Persona{
		{ID: "grumpy-sysadmin", Description: "Runs FreeBSD on everything.", Origin: domain.PersonaGenerated},
		{ID: "lurker-larry", Description: "Only corrects RFC citations.", Origin: domain.PersonaGenerated},
		{ID: "retro-gamer", Description: "Speedruns Doom on a toaster.", Origin: domain.PersonaGenerated},
	}
}

// newTestSessionWithManager constructs a `*session.Session` backed
// by a real `*modelmanager.Manager`. The manager's `PrepareInstance`
// runs the full persona arbitration and unique-nick loop against
// the supplied `apiClient`.
func newTestSessionWithManager(
	t *testing.T,
	apiClient api.Client,
	apiKey string,
) (*session.Session, *storemod.SQLiteStore, *modelmanager.Manager, *userclient.UserClient) {
	t.Helper()

	store := storetest.NewMemoryStore(t)

	mgr := modelmanager.New(modelmanager.Config{
		Store:         store,
		APIClient:     apiClient,
		InitialAPIKey: apiKey,
		BaseContext:   t.Context,
	})
	t.Cleanup(mgr.DetachAll)

	sess := session.New(t.Context, store, mgr, nil)
	// The manager's cleanup was registered first, so it runs last:
	// `Shutdown` closes the gate the pumps exit on, and `DetachAll`
	// then joins every dispatch goroutine, the released ones
	// included.
	t.Cleanup(func() { _ = sess.Shutdown(t.Context()) })

	user := userclient.New("testuser", sess, store, userclient.NewStoreReplyLog(store))
	require.NoError(t, user.Attach(t.Context()))

	return sess, store, mgr, user
}

// seedChannel JOINs the channel via the user-client to create it
// and grant the user the channel-creator `+o` rank.
func seedChannel(t *testing.T, user *userclient.UserClient, ch domain.ChannelName) {
	t.Helper()

	require.NoError(t, user.Join(t.Context(), ch))
}

// seedStoreInstance writes a model-instance row directly to the store
// so `Session.ResolveNick` resolves the nick. The manager's nick-
// uniqueness check goes through `Session.ResolveNick`, so a seeded
// nick collides with newly-generated suggestions of the same name.
func seedStoreInstance(t *testing.T, store *storemod.SQLiteStore, nick domain.Nick, modelID domain.ModelID) *domain.Instance {
	t.Helper()

	inst := domain.NewModelInstance(
		domain.InstanceID("inst-"+string(nick)),
		nick,
		modelID,
		"",
		nil,
	)
	require.NoError(t, store.SaveInstance(t.Context(), inst))

	return inst
}

// addModelViaWire sends an [protocol.AddModel] through the
// user-client.
func addModelViaWire(ctx context.Context, t testing.TB, user *userclient.UserClient, ch domain.ChannelName, model domain.ModelID, persona string) error {
	t.Helper()

	resp, err := user.Send(ctx, protocol.AddModel{
		Channel: ch,
		Model:   model,
		Persona: persona,
	})
	if err != nil {
		return err
	}

	return resp.Err
}

// collectUserEvents drains every event currently buffered on the
// user-client subscription's protocol bus. Callers under
// `synctest.Test` must `synctest.Wait()` first to make sure all
// producer goroutines have run.
func collectUserEvents(user *userclient.UserClient) []domain.Event {
	var events []domain.Event

	for {
		select {
		case delivery := <-user.Events():
			events = append(events, delivery.Event)
		default:
			return events
		}
	}
}

// drainUserEvents reads every event currently buffered on the user-
// client subscription's protocol bus. Tests call it after seed
// helpers to discard the bootstrap mode change, the user JOIN, and
// the NamesReply that the seed produced, so the subsequent
// assertion sees only the AddModel-emitted events.
func drainUserEvents(user *userclient.UserClient) {
	for {
		select {
		case <-user.Events():
		default:
			return
		}
	}
}

func TestSession_AddModel_retries_on_nick_collision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		suggestions := []domain.Nick{"taken", "alsotaken", "fresh"}
		var seenExclusions [][]domain.Nick

		fake := &managerFakeAPI{
			generateNickFn: func(_ context.Context, _ domain.ModelID, _ string, exclude []domain.Nick) (domain.Nick, error) {
				seenExclusions = append(seenExclusions, slices.Clone(exclude))

				return suggestions[len(exclude)], nil
			},
		}

		_, store, _, user := newTestSessionWithManager(t, fake, "")
		ctx := t.Context()

		seedStoreInstance(t, store, "taken", "test/model")
		seedStoreInstance(t, store, "alsotaken", "test/model")
		seedChannel(t, user, "#dev")
		synctest.Wait()
		drainUserEvents(user)

		emittedAt := time.Now()
		require.NoError(t, addModelViaWire(ctx, t, user, "#dev", "test/model", "Helpful assistant"))
		synctest.Wait()

		fresh, err := store.ResolveNick(ctx, "fresh")
		require.NoError(t, err)

		// The model's own JOIN is delivered but raises no dispatch
		// turn — it has nothing to say about its own arrival.
		require.Equal(t, []domain.Event{
			domain.Join{
				Target:     "#dev",
				Nick:       "fresh",
				InstanceID: fresh.ID(),
				At:         emittedAt,
				Instance:   fresh,
			},
		}, collectUserEvents(user))

		require.Equal(t, [][]domain.Nick{
			nil,
			{"taken"},
			{"taken", "alsotaken"},
		}, seenExclusions,
			"each retry must pass the previously rejected suggestions to the model")
	})
}

func TestSession_AddModel_gives_up_after_max_attempts(t *testing.T) {
	fake := &managerFakeAPI{
		generateNickFn: func(_ context.Context, _ domain.ModelID, _ string, _ []domain.Nick) (domain.Nick, error) {
			return "taken", nil
		},
	}

	_, store, _, user := newTestSessionWithManager(t, fake, "")
	ctx := t.Context()

	seedStoreInstance(t, store, "taken", "test/model")
	seedChannel(t, user, "#dev")

	err := addModelViaWire(ctx, t, user, "#dev", "test/model", "Helpful assistant")
	require.EqualError(t, err,
		"generate nick: 3 attempts exhausted, all suggestions collided")
}

func TestSession_AddModelGenerateNickError(t *testing.T) {
	fake := &managerFakeAPI{
		generateNickFn: func(_ context.Context, _ domain.ModelID, _ string, _ []domain.Nick) (domain.Nick, error) {
			return "", fmt.Errorf("API unavailable")
		},
	}

	_, _, _, user := newTestSessionWithManager(t, fake, "")
	ctx := t.Context()

	seedChannel(t, user, "#dev")

	require.Error(t, addModelViaWire(ctx, t, user, "#dev", "anthropic/claude-3-haiku", ""))
}

func TestSession_AddModel_creates_new_instance_per_invocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, store, _, user := newTestSessionWithManager(t, &managerFakeAPI{}, "")
		ctx := t.Context()

		seedChannel(t, user, "#general")
		seedChannel(t, user, "#random")
		synctest.Wait()
		drainUserEvents(user)

		emittedAt := time.Now()

		require.NoError(t, addModelViaWire(ctx, t, user, "#general", "test/model", "Helpful assistant"))
		synctest.Wait()
		require.NoError(t, addModelViaWire(ctx, t, user, "#random", "test/model", ""))
		synctest.Wait()

		// The default fake `GenerateNick` returns "fakenick" first and
		// then "fakenick1" on the second invocation (numbered by
		// exclusion-list length); both names resolve to distinct
		// instances.
		first, err := store.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)
		second, err := store.ResolveNick(ctx, "fakenick1")
		require.NoError(t, err)

		// Each model's own JOIN is delivered but raises no dispatch
		// turn — neither has anything to say about its own arrival.
		require.ElementsMatch(t, []domain.Event{
			domain.Join{
				Target:     "#general",
				Nick:       "fakenick",
				InstanceID: first.ID(),
				At:         emittedAt,
				Instance:   first,
			},
			domain.Join{
				Target:     "#random",
				Nick:       "fakenick1",
				InstanceID: second.ID(),
				At:         emittedAt,
				Instance:   second,
			},
		}, collectUserEvents(user))

		// Each invocation produces a fresh `*Instance` with its own id.
		require.NotEqual(t, first.ID(), second.ID())
		require.NotSame(t, first, second)

		instances, err := store.ListInstances(ctx)
		require.NoError(t, err)

		ids := make([]domain.InstanceID, len(instances))
		for i, inst := range instances {
			ids[i] = inst.ID()
		}

		require.ElementsMatch(t, []domain.InstanceID{first.ID(), second.ID()}, ids)
	})
}

// TestManager_DetachAll_joins_a_client_released_mid_session pins
// where the join reaches. A KILL, a QUIT, a send-queue disconnect
// and a failed ADDMODEL all release their client through
// `Manager.Detach`, which frees the identity immediately — but the
// dispatch goroutine behind it can still be inside an upstream call,
// and `DetachAll` is the only thing that waits for it. A client the
// manager forgot at release would take its goroutine out of every
// join's reach, and the process would exit with a turn still
// running against a store that is closing.
//
// The fixture parks a turn upstream, kills the model under it, and
// asserts the join does not finish while that turn is still in
// flight.
func TestManager_DetachAll_joins_a_client_released_mid_session(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		upstream := make(chan struct{})

		// The park ignores cancellation, which is what a real upstream
		// call that has already been handed to the network does.
		fake := &managerFakeAPI{
			sendEventsFn: func(context.Context, domain.ModelID, domain.InstanceID, string, []protocol.IRCMessage, []protocol.IRCMessage) (api.CompletionResult, error) {
				<-upstream
				return api.CompletionResult{}, nil
			},
		}

		sess, store, mgr, user := newTestSessionWithManager(t, fake, "")
		ctx := t.Context()

		seedChannel(t, user, "#general")
		require.NoError(t, addModelViaWire(ctx, t, user, "#general", "test/model", ""))
		synctest.Wait()

		// The model's own JOIN raises no turn; a channel message does,
		// and this one wakes the turn that parks upstream.
		_, err := user.SendMessage(ctx, "#general", "anyone about?")
		require.NoError(t, err)
		synctest.Wait()

		bot, err := store.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)

		resp, err := user.Send(ctx, protocol.Kill{Nick: "fakenick", Reason: "time to go"})
		require.NoError(t, err)
		require.NoError(t, resp.Err)

		joined := make(chan struct{})
		go func() {
			defer close(joined)
			mgr.DetachAll()
		}()

		synctest.Wait()

		select {
		case <-joined:
			t.Fatal("DetachAll returned while a released client's turn was still running")
		default:
		}

		close(upstream)
		synctest.Wait()

		<-joined

		// The kill did what a kill does, and the join waited for what
		// was left of it.
		require.Nil(t, sess.LookupClient(protocol.ClientID(bot.ID())))

		_, err = store.GetInstanceByID(ctx, bot.ID())
		require.Error(t, err)
	})
}

func TestSession_Invite_without_persona_assigns_from_pool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := &managerFakeAPI{
			generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
				return testPersonas(), nil
			},
		}

		_, store, _, user := newTestSessionWithManager(t, fake, "")
		ctx := t.Context()

		seedChannel(t, user, "#dev")
		synctest.Wait()
		drainUserEvents(user)

		emittedAt := time.Now()
		require.NoError(t, addModelViaWire(ctx, t, user, "#dev", "anthropic/claude-3-haiku", ""))
		synctest.Wait()

		inst, err := store.ResolveNick(ctx, "fakenick")
		require.NoError(t, err)

		// The model's own JOIN is delivered but raises no dispatch
		// turn — a model has nothing to say about its own arrival,
		// and a turn nobody asked for is a paid API call for nothing.
		require.Equal(t, []domain.Event{
			domain.Join{
				Target:     "#dev",
				Nick:       "fakenick",
				InstanceID: inst.ID(),
				At:         emittedAt,
				Instance:   inst,
			},
		}, collectUserEvents(user))

		require.NotEmpty(t, inst.Persona())

		descriptions := make(map[string]bool)
		for _, p := range testPersonas() {
			descriptions[p.Description] = true
		}

		require.True(t, descriptions[inst.Persona()],
			"assigned persona %q not in pool", inst.Persona())
	})
}

func TestSession_AddModel_short_circuits_after_ListModels_failure(t *testing.T) {
	logs := installLogCapture(t)

	upstreamErr := fmt.Errorf("upstream unreachable")
	client := &listModelsCountingClient{err: upstreamErr}

	_, _, mgr, user := newTestSessionWithManager(t, client, "test-key")
	ctx := t.Context()

	seedChannel(t, user, "#dev")

	_, err := mgr.ListModels(ctx)
	require.ErrorIs(t, err, upstreamErr)
	require.Equal(t, modelmanager.ListStateFailed, mgr.ListState())

	addErr := addModelViaWire(ctx, t, user, "#dev", "anthropic/claude-3-haiku", "")
	require.ErrorIs(t, addErr, modelclient.ErrModelListUnavailable)

	require.Equal(t, int32(1), client.calls.Load(),
		"AddModel must short-circuit on the cached failed state and not re-hit ListModels")

	transition := logs.find("model list state transitioned")
	require.NotNil(t, transition, "expected transition log record")
	require.Equal(t, "WARN", transition["level"])
	require.Equal(t, "modelmanager", transition["component"])
	require.Equal(t, "none", transition["from"])
	require.Equal(t, "failed", transition["to"])
	require.Equal(t, upstreamErr.Error(), transition["error"])

	shortCircuit := logs.find("add-model short-circuited: model list unavailable")
	require.NotNil(t, shortCircuit, "expected short-circuit log record")
	require.Equal(t, "INFO", shortCircuit["level"])
	require.Equal(t, "modelmanager", shortCircuit["component"])
	require.Equal(t, "anthropic/claude-3-haiku", shortCircuit["model_id"])
}

func TestSession_AddModel_lazy_loads_when_state_none(t *testing.T) {
	client := &listModelsCountingClient{infos: []api.ModelInfo{{ID: "anthropic/claude-3-haiku"}}}

	_, _, mgr, user := newTestSessionWithManager(t, client, "test-key")
	ctx := t.Context()

	seedChannel(t, user, "#dev")

	require.Equal(t, modelmanager.ListStateNone, mgr.ListState())
	require.NoError(t, addModelViaWire(ctx, t, user, "#dev", "anthropic/claude-3-haiku", ""))
	require.Equal(t, modelmanager.ListStateOK, mgr.ListState())
	require.Equal(t, int32(1), client.calls.Load())
}

func TestSession_AddModel_returns_unsupported_when_model_missing_from_cache(t *testing.T) {
	client := &listModelsCountingClient{infos: []api.ModelInfo{{ID: "openai/gpt-5"}}}

	_, _, mgr, user := newTestSessionWithManager(t, client, "test-key")
	ctx := t.Context()

	seedChannel(t, user, "#dev")

	_, err := mgr.ListModels(ctx)
	require.NoError(t, err)
	require.Equal(t, modelmanager.ListStateOK, mgr.ListState())

	addErr := addModelViaWire(ctx, t, user, "#dev", "anthropic/claude-3-haiku", "")
	var unsupported domain.UnsupportedModelError
	require.ErrorAs(t, addErr, &unsupported)
	require.Equal(t, domain.ModelID("anthropic/claude-3-haiku"), unsupported.ModelID)
}

func TestSession_AddModel_short_circuits_when_lazy_load_fails(t *testing.T) {
	upstreamErr := fmt.Errorf("upstream unreachable")
	client := &listModelsCountingClient{err: upstreamErr}

	_, _, mgr, user := newTestSessionWithManager(t, client, "test-key")
	ctx := t.Context()

	seedChannel(t, user, "#dev")

	first := addModelViaWire(ctx, t, user, "#dev", "anthropic/claude-3-haiku", "")
	require.ErrorIs(t, first, upstreamErr,
		"first AddModel should surface the underlying upstream error from the lazy load")
	require.Equal(t, modelmanager.ListStateFailed, mgr.ListState())

	second := addModelViaWire(ctx, t, user, "#dev", "anthropic/claude-3-haiku", "")
	require.ErrorIs(t, second, modelclient.ErrModelListUnavailable)
	require.Equal(t, int32(1), client.calls.Load(),
		"second AddModel must short-circuit and not re-hit ListModels")
}

// TestDispatch_transcript_token_budget_from_catalogue_context_len
// proves the transcript token budget engages through the real
// dispatch/turn call graph, not just [history]'s own unit tests: a
// model whose cached [api.ModelInfo.ContextLen] is small gets a
// trimmed `history` argument on its `SendEvents` call once the
// channel's transcript grows past the budget, while a model whose
// catalogue entry reports no context length (the common shape for a
// listing OpenRouter didn't attach one to) keeps the legacy
// event-count-only bound and sees everything.
//
// Each of three long messages is sent and waited on individually so
// it becomes its own turn — the model's `history` argument for turn
// N is the ring's contents from turns 1..N-1, growing one entry at a
// time exactly the way a live channel would.
func TestDispatch_transcript_token_budget_from_catalogue_context_len(t *testing.T) {
	tests := []struct {
		name       string
		contextLen int
		wantThird  []string
	}{
		{
			name:       "small ContextLen trims the transcript to the newest entry",
			contextLen: 2000,
			wantThird:  []string{"second"},
		},
		{
			// The leading "" is the model's own JOIN, filed into
			// history (but not dispatched — see the self-join guard
			// in dispatchTrigger) when it was added to the channel;
			// with no token budget in effect it survives alongside
			// both messages.
			name:       "context length OpenRouter didn't report keeps every entry",
			contextLen: 0,
			wantThird:  []string{"", "first", "second"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var histories [][]protocol.IRCMessage

				fake := &managerFakeAPI{
					listModelsFn: func(context.Context) ([]api.ModelInfo, error) {
						return []api.ModelInfo{{ID: "test/model", ContextLen: tc.contextLen}}, nil
					},
					sendEventsFn: func(_ context.Context, _ domain.ModelID, _ domain.InstanceID, _ string, history []protocol.IRCMessage, _ []protocol.IRCMessage) (api.CompletionResult, error) {
						histories = append(histories, history)
						return api.CompletionResult{}, nil
					},
				}

				_, _, mgr, user := newTestSessionWithManager(t, fake, "test-key")
				ctx := t.Context()

				// Warm the catalogue cache up front so CachedContextLen
				// has something to report from the model's very first
				// turn, not just after it happens to lazy-load.
				_, err := mgr.ListModels(ctx)
				require.NoError(t, err)

				seedChannel(t, user, "#dev")
				require.NoError(t, addModelViaWire(ctx, t, user, "#dev", "test/model", ""))
				synctest.Wait()

				// Each body is long enough that two of them together
				// overflow even the smallest budget
				// [tokenBudgetForContextLen]'s floor allows, while one
				// alone still fits comfortably under it.
				big := strings.Repeat("x", 4000)

				for _, label := range []string{"first", "second", "third"} {
					_, err := user.SendMessage(ctx, "#dev", label+" "+big)
					require.NoError(t, err)
					synctest.Wait()
				}

				require.Len(t, histories, 3)

				var thirdTurnLabels []string
				for _, msg := range histories[2] {
					label, _, _ := strings.Cut(msg.Body, " ")
					thirdTurnLabels = append(thirdTurnLabels, label)
				}

				require.Equal(t, tc.wantThird, thirdTurnLabels)
			})
		})
	}
}
