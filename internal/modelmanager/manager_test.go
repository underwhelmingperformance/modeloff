package modelmanager_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

var fixedTime = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

// fakeAPIClient is a hand-rolled `api.Client` whose `ListModels`,
// `GeneratePersonas`, and `GenerateNick` methods route through
// per-test callbacks. The default behaviour returns empty / silence
// so a test only needs to set the field it cares about.
type fakeAPIClient struct {
	listModelsFn       func(context.Context) ([]api.ModelInfo, error)
	generateNickFn     func(context.Context, domain.ModelID, string, []domain.Nick) (domain.Nick, error)
	generatePersonasFn func(context.Context, domain.ModelID) ([]domain.Persona, error)
}

func (f *fakeAPIClient) ListModels(ctx context.Context) ([]api.ModelInfo, error) {
	if f.listModelsFn != nil {
		return f.listModelsFn(ctx)
	}

	return nil, nil
}

func (f *fakeAPIClient) SendEvents(
	context.Context,
	domain.ModelID,
	domain.InstanceID,
	string,
	[]protocol.IRCMessage,
	[]protocol.IRCMessage,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}

func (f *fakeAPIClient) ContinueWithToolResults(
	context.Context,
	*api.Conversation,
	[]api.ToolResult,
	...api.ToolDefinition,
) (api.CompletionResult, error) {
	return api.CompletionResult{}, nil
}

func (f *fakeAPIClient) GenerateNick(ctx context.Context, smallModel domain.ModelID, persona string, exclude []domain.Nick) (api.NicknameResult, error) {
	if f.generateNickFn != nil {
		nick, err := f.generateNickFn(ctx, smallModel, persona, exclude)
		return api.NicknameResult{Nick: nick}, err
	}

	return api.NicknameResult{Nick: "fakenick"}, nil
}

func (f *fakeAPIClient) GeneratePersonas(ctx context.Context, smallModel domain.ModelID) ([]domain.Persona, error) {
	if f.generatePersonasFn != nil {
		return f.generatePersonasFn(ctx, smallModel)
	}

	return nil, nil
}

// listModelsCountingClient records the number of `ListModels` calls
// so short-circuit tests can assert the upstream is not re-hit after
// a known failure.
type listModelsCountingClient struct {
	fakeAPIClient

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

func testPersonas() []domain.Persona {
	return []domain.Persona{
		{ID: "grumpy-sysadmin", Description: "Runs FreeBSD on everything.", Origin: domain.PersonaGenerated},
		{ID: "lurker-larry", Description: "Only corrects RFC citations.", Origin: domain.PersonaGenerated},
		{ID: "retro-gamer", Description: "Speedruns Doom on a toaster.", Origin: domain.PersonaGenerated},
	}
}

type managerFixture struct {
	mgr   *modelmanager.Manager
	store *storemod.SQLiteStore
	mem   *memory.StoreAdapter
}

func newTestManager(t *testing.T, cfg modelmanager.Config) *managerFixture {
	t.Helper()

	if cfg.Store == nil {
		cfg.Store = storetest.NewMemoryStore(t)
	}

	if cfg.BaseContext == nil {
		cfg.BaseContext = t.Context
	}

	mgr := modelmanager.New(cfg)
	t.Cleanup(mgr.DetachAll)
	mgr.SetClock(func() time.Time { return fixedTime })

	fixture := &managerFixture{
		mgr:   mgr,
		store: cfg.Store.(*storemod.SQLiteStore),
	}

	if adapter, ok := cfg.Memory.(*memory.StoreAdapter); ok {
		fixture.mem = adapter
	}

	return fixture
}

func TestManager_SetAPIKey(t *testing.T) {
	initial := &fakeAPIClient{}
	replacement := &fakeAPIClient{}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     initial,
		InitialAPIKey: "",
	})

	fx.mgr.SetAPIFactory(func(apiKey, _ string) (api.Client, error) {
		require.Equal(t, "test-key", apiKey)
		return replacement, nil
	})

	require.NoError(t, fx.mgr.SetAPIKey(t.Context(), "test-key", ""))
	require.True(t, fx.mgr.HasAPIKey())
	require.Same(t, replacement, fx.mgr.APIClient())
}

func TestManager_SetAPIKey_factory_failure_keeps_existing_client(t *testing.T) {
	initial := &fakeAPIClient{}

	fx := newTestManager(t, modelmanager.Config{
		APIClient: initial,
	})

	fx.mgr.SetAPIFactory(func(string, string) (api.Client, error) {
		return nil, fmt.Errorf("boom")
	})

	err := fx.mgr.SetAPIKey(t.Context(), "test-key", "")
	require.Error(t, err)
	require.Same(t, initial, fx.mgr.APIClient())
	require.False(t, fx.mgr.HasAPIKey())
}

func TestManager_SetBaseURL(t *testing.T) {
	var factoryBaseURL string
	factoryCalls := 0
	newClient := &fakeAPIClient{}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     &fakeAPIClient{},
		InitialAPIKey: "test-key",
	})

	fx.mgr.SetAPIFactory(func(_, baseURL string) (api.Client, error) {
		factoryCalls++
		factoryBaseURL = baseURL
		return newClient, nil
	})

	require.NoError(t, fx.mgr.SetBaseURL(t.Context(), "https://custom.example.com"))
	require.Equal(t, 1, factoryCalls)
	require.Equal(t, "https://custom.example.com", factoryBaseURL)
}

func TestManager_runtimeConfigOperations_recordSpans(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)

	fx := newTestManager(t, modelmanager.Config{
		APIClient:      &fakeAPIClient{},
		InitialAPIKey:  "test-key",
		TracerProvider: provider,
	})

	fx.mgr.SetAPIFactory(func(_, _ string) (api.Client, error) {
		return &fakeAPIClient{}, nil
	})

	require.NoError(t, fx.mgr.SetAPIKey(t.Context(), "next-key", "https://openrouter.ai/api/v1"))
	fx.mgr.SetSmallModel(t.Context(), "anthropic/claude-haiku-4.5")
	require.NoError(t, fx.mgr.SetBaseURL(t.Context(), "https://custom.example.com"))

	apiKeySpan := oteltest.FindSpan(t, recorder, "modelmanager.set_api_key")
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(apiKeySpan.Attributes(), observability.AttrResult))

	smallModelSpan := oteltest.FindSpan(t, recorder, "modelmanager.set_small_model")
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(smallModelSpan.Attributes(), observability.AttrResult))
	require.Equal(t, "anthropic/claude-haiku-4.5", oteltest.AttrValue(smallModelSpan.Attributes(), observability.AttrModelID))

	baseURLSpan := oteltest.FindSpan(t, recorder, "modelmanager.set_base_url")
	require.Equal(t, observability.ResultOK, oteltest.AttrValue(baseURLSpan.Attributes(), observability.AttrResult))
}

func TestManager_Reset(t *testing.T) {
	store := storetest.NewMemoryStore(t)
	memStore := memory.NewStoreAdapter(storetest.NewMemoryStore(t))

	fx := newTestManager(t, modelmanager.Config{
		Store:     store,
		Memory:    memStore,
		APIClient: &fakeAPIClient{},
	})

	ctx := t.Context()

	require.NoError(t, store.SavePersona(ctx, domain.Persona{
		ID: "p1", Description: "x", Origin: domain.PersonaUser,
	}))
	require.NoError(t, memStore.Write(ctx, "inst-botty", memory.Entry{Key: "mood", Content: "happy"}))

	require.NoError(t, fx.mgr.Reset(ctx))

	personas, err := store.ListPersonas(ctx)
	require.NoError(t, err)
	require.Empty(t, personas)

	memories, err := memStore.Read(ctx, "inst-botty")
	require.NoError(t, err)
	require.Empty(t, memories)
}

func TestManager_Reset_nil_memory_store(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &fakeAPIClient{},
	})

	require.NoError(t, fx.mgr.Reset(t.Context()))
}

func TestManager_EnsurePersonas_lazy_generation(t *testing.T) {
	calls := 0
	fake := &fakeAPIClient{
		generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
			calls++
			return testPersonas(), nil
		},
	}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     fake,
		InitialAPIKey: "test-key",
	})

	ctx := t.Context()

	require.NoError(t, fx.mgr.EnsurePersonas(ctx))
	require.Equal(t, 1, calls)

	got, err := fx.store.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, testPersonas(), got)

	// Second call must not regenerate — the pool is already populated.
	require.NoError(t, fx.mgr.EnsurePersonas(ctx))
	require.Equal(t, 1, calls)
}

func TestManager_RandomPersona(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &fakeAPIClient{},
	})

	ctx := t.Context()
	for _, p := range testPersonas() {
		require.NoError(t, fx.store.SavePersona(ctx, p))
	}

	got, err := fx.mgr.RandomPersona(ctx)
	require.NoError(t, err)

	ids := make(map[string]bool)
	for _, p := range testPersonas() {
		ids[p.ID] = true
	}

	require.True(t, ids[got.ID], "random persona %q not in pool", got.ID)
}

func TestManager_RandomPersona_empty_pool(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &fakeAPIClient{},
	})

	_, err := fx.mgr.RandomPersona(t.Context())
	require.EqualError(t, err, "no personas available")
}

func TestManager_RegeneratePersonas_preserves_user_defined(t *testing.T) {
	fake := &fakeAPIClient{
		generatePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
			return []domain.Persona{
				{ID: "new-gen", Description: "Freshly generated.", Origin: domain.PersonaGenerated},
			}, nil
		},
	}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     fake,
		InitialAPIKey: "test-key",
	})

	ctx := t.Context()

	require.NoError(t, fx.store.SavePersona(ctx, domain.Persona{
		ID: "my-persona", Description: "User defined.", Origin: domain.PersonaUser,
	}))
	require.NoError(t, fx.store.SavePersona(ctx, domain.Persona{
		ID: "old-gen", Description: "Old generated.", Origin: domain.PersonaGenerated,
	}))

	got, err := fx.mgr.RegeneratePersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "new-gen", Description: "Freshly generated.", Origin: domain.PersonaGenerated},
	}, got)

	all, err := fx.store.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "my-persona", Description: "User defined.", Origin: domain.PersonaUser},
		{ID: "new-gen", Description: "Freshly generated.", Origin: domain.PersonaGenerated},
	}, all)
}

func TestManager_SetPersona(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &fakeAPIClient{},
	})

	ctx := t.Context()

	require.NoError(t, fx.mgr.SetPersona(ctx, "custom-bot", "A friendly helper."))

	got, err := fx.store.GetPersona(ctx, "custom-bot")
	require.NoError(t, err)
	require.Equal(t, domain.Persona{
		ID:          "custom-bot",
		Description: "A friendly helper.",
		Origin:      domain.PersonaUser,
	}, got)
}

func TestManager_ResetPersonas_removes_user_keeps_generated(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &fakeAPIClient{},
	})

	ctx := t.Context()

	require.NoError(t, fx.store.SavePersona(ctx, domain.Persona{
		ID: "my-persona", Description: "User defined.", Origin: domain.PersonaUser,
	}))
	require.NoError(t, fx.store.SavePersona(ctx, domain.Persona{
		ID: "gen-persona", Description: "Generated.", Origin: domain.PersonaGenerated,
	}))

	removed, err := fx.mgr.ResetPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	got, err := fx.store.ListPersonas(ctx)
	require.NoError(t, err)
	require.Equal(t, []domain.Persona{
		{ID: "gen-persona", Description: "Generated.", Origin: domain.PersonaGenerated},
	}, got)
}

func TestManager_SetAPIKey_resets_listState(t *testing.T) {
	client := &listModelsCountingClient{err: fmt.Errorf("upstream unreachable")}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "initial-key",
	})

	fx.mgr.SetAPIFactory(func(string, string) (api.Client, error) {
		return &fakeAPIClient{}, nil
	})

	_, err := fx.mgr.ListModels(t.Context())
	require.Error(t, err)
	require.Equal(t, modelmanager.ListStateFailed, fx.mgr.ListState())

	require.NoError(t, fx.mgr.SetAPIKey(t.Context(), "next-key", ""))
	require.Equal(t, modelmanager.ListStateNone, fx.mgr.ListState())
	require.False(t, fx.mgr.SupportedModelsReady())
	require.Nil(t, fx.mgr.SupportedModels())
}

func TestManager_Reset_clears_listState(t *testing.T) {
	client := &listModelsCountingClient{err: fmt.Errorf("upstream unreachable")}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "test-key",
	})

	_, err := fx.mgr.ListModels(t.Context())
	require.Error(t, err)
	require.Equal(t, modelmanager.ListStateFailed, fx.mgr.ListState())

	require.NoError(t, fx.mgr.Reset(t.Context()))
	require.Equal(t, modelmanager.ListStateNone, fx.mgr.ListState())
	require.False(t, fx.mgr.SupportedModelsReady())
	require.Nil(t, fx.mgr.SupportedModels())
}
