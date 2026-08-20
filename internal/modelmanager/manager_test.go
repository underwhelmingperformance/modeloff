package modelmanager_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/api/apitest"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/observability/oteltest"
	storemod "github.com/laney/modeloff/internal/store"
	"github.com/laney/modeloff/internal/store/storetest"
)

var fixedTime = time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

// listModelsCountingClient records the number of `ListModels` calls
// so short-circuit tests can assert the upstream is not re-hit after
// a known failure. When block is non-nil, ListModels waits on it
// before returning, so a single-flight test can hold a fetch open
// while concurrent callers pile up behind it.
type listModelsCountingClient struct {
	apitest.Fake

	calls atomic.Int32
	err   error
	infos []api.ModelInfo
	block chan struct{}
}

func (c *listModelsCountingClient) ListModels(context.Context) ([]api.ModelInfo, error) {
	c.calls.Add(1)

	if c.block != nil {
		<-c.block
	}

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
	t.Cleanup(func() { _ = mgr.DetachAll(context.Background()) })
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
	initial := &apitest.Fake{}
	replacement := &apitest.Fake{}

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
	initial := &apitest.Fake{}

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
	newClient := &apitest.Fake{}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     &apitest.Fake{},
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

// TestManager_SetBaseURL_invalidates_catalogue pins that a base-URL
// change drops the previous provider's catalogue cache, the same way
// SetAPIKey does. A cache left in place after a provider switch would
// validate add-model requests against a catalogue that belongs to the
// old endpoint.
func TestManager_SetBaseURL_invalidates_catalogue(t *testing.T) {
	client := &listModelsCountingClient{infos: fakeCatalogue()}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "test-key",
	})

	fx.mgr.SetAPIFactory(func(string, string) (api.Client, error) {
		return client, nil
	})

	require.NoError(t, fx.mgr.EnsureKnownModel(t.Context(), "openai/gpt-5.4-mini"))
	require.True(t, fx.mgr.SupportedModelsReady())

	require.NoError(t, fx.mgr.SetBaseURL(t.Context(), "https://custom.example.com"))

	require.False(t, fx.mgr.SupportedModelsReady())
	require.Nil(t, fx.mgr.SupportedModels())
	require.Equal(t, modelmanager.ListStateNone, fx.mgr.ListState())
}

// TestManager_SetBaseURL_noFactory_leavesCatalogueAlone pins that a
// base-URL change which rebuilds no client (no factory configured, or
// no API key yet) leaves the existing catalogue cache untouched: it
// still describes the client actually in use.
func TestManager_SetBaseURL_noFactory_leavesCatalogueAlone(t *testing.T) {
	client := &listModelsCountingClient{infos: fakeCatalogue()}

	fx := newTestManager(t, modelmanager.Config{
		APIClient:     client,
		InitialAPIKey: "test-key",
	})

	require.NoError(t, fx.mgr.EnsureKnownModel(t.Context(), "openai/gpt-5.4-mini"))
	require.True(t, fx.mgr.SupportedModelsReady())

	require.NoError(t, fx.mgr.SetBaseURL(t.Context(), "https://custom.example.com"))

	require.True(t, fx.mgr.SupportedModelsReady())
}

func TestManager_runtimeConfigOperations_recordSpans(t *testing.T) {
	recorder, provider := oteltest.NewSpanRecorder(t)

	fx := newTestManager(t, modelmanager.Config{
		APIClient:      &apitest.Fake{},
		InitialAPIKey:  "test-key",
		TracerProvider: provider,
	})

	fx.mgr.SetAPIFactory(func(_, _ string) (api.Client, error) {
		return &apitest.Fake{}, nil
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

func TestManager_EnsurePersonas_lazy_generation(t *testing.T) {
	calls := 0
	fake := &apitest.Fake{
		GeneratePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
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
		APIClient: &apitest.Fake{},
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

// TestManager_RandomPersona_excludes_personas_held_by_live_instances
// pins that the draw skips a persona description already assigned to
// a connected model instance, so a fresh invite does not hand out a
// duplicate while an unused persona is still available.
func TestManager_RandomPersona_excludes_personas_held_by_live_instances(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &apitest.Fake{},
	})

	ctx := t.Context()
	personas := testPersonas()
	for _, p := range personas {
		require.NoError(t, fx.store.SavePersona(ctx, p))
	}

	for i, nick := range []domain.Nick{"grumpybot", "lurkerbot"} {
		inst := domain.NewModelInstance(domain.GenerateInstanceID(), nick, "openai/gpt-5.4-mini", personas[i].Description, nil)
		require.NoError(t, fx.store.SaveInstance(ctx, inst))
	}

	// Only personas[2] is unheld; repeating the draw rules out a lucky
	// hit from a plain uniform draw over all three personas.
	for range 30 {
		got, err := fx.mgr.RandomPersona(ctx)
		require.NoError(t, err)
		require.Equal(t, personas[2], got)
	}
}

// TestManager_RandomPersona_allows_duplicates_when_pool_exhausted pins
// that the draw falls back to the full pool, duplicates included,
// once every persona in it is already held by a live instance.
func TestManager_RandomPersona_allows_duplicates_when_pool_exhausted(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &apitest.Fake{},
	})

	ctx := t.Context()
	personas := testPersonas()
	for _, p := range personas {
		require.NoError(t, fx.store.SavePersona(ctx, p))
	}

	for i, p := range personas {
		inst := domain.NewModelInstance(domain.GenerateInstanceID(), domain.Nick(fmt.Sprintf("bot%d", i)), "openai/gpt-5.4-mini", p.Description, nil)
		require.NoError(t, fx.store.SaveInstance(ctx, inst))
	}

	got, err := fx.mgr.RandomPersona(ctx)
	require.NoError(t, err)

	ids := make(map[string]bool)
	for _, p := range personas {
		ids[p.ID] = true
	}
	require.True(t, ids[got.ID], "random persona %q not in pool", got.ID)
}

func TestManager_RandomPersona_empty_pool(t *testing.T) {
	fx := newTestManager(t, modelmanager.Config{
		APIClient: &apitest.Fake{},
	})

	_, err := fx.mgr.RandomPersona(t.Context())
	require.EqualError(t, err, "no personas available")
}

func TestManager_RegeneratePersonas_preserves_user_defined(t *testing.T) {
	fake := &apitest.Fake{
		GeneratePersonasFn: func(_ context.Context, _ domain.ModelID) ([]domain.Persona, error) {
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
		APIClient: &apitest.Fake{},
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
		APIClient: &apitest.Fake{},
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
		return &apitest.Fake{}, nil
	})

	_, err := fx.mgr.ListModels(t.Context())
	require.Error(t, err)
	require.Equal(t, modelmanager.ListStateFailed, fx.mgr.ListState())

	require.NoError(t, fx.mgr.SetAPIKey(t.Context(), "next-key", ""))
	require.Equal(t, modelmanager.ListStateNone, fx.mgr.ListState())
	require.False(t, fx.mgr.SupportedModelsReady())
	require.Nil(t, fx.mgr.SupportedModels())
}
