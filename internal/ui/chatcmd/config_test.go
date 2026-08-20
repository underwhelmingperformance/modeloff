package chatcmd

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
)

// fakeManagerAPI is a hand-rolled [modelclient.ManagerAPI] whose
// model-validation methods return caller-set errors, so `/config`
// tests can exercise the small-model and embedding-model validation
// paths without a live OpenRouter client.
type fakeManagerAPI struct {
	hasAPIKey bool

	structuredOutputErr error

	embeddingSearchable bool
	embeddingProbeErr   error

	lastSmallModel domain.ModelID
}

func (f *fakeManagerAPI) SetAPIKey(context.Context, string, string) error { return nil }
func (f *fakeManagerAPI) SetBaseURL(context.Context, string) error        { return nil }

func (f *fakeManagerAPI) SetSmallModel(_ context.Context, modelID domain.ModelID) {
	f.lastSmallModel = modelID
}

func (f *fakeManagerAPI) SetPersona(context.Context, string, string) error { return nil }

func (f *fakeManagerAPI) ListPersonas(context.Context) ([]domain.Persona, error) { return nil, nil }

func (f *fakeManagerAPI) RegeneratePersonas(context.Context) ([]domain.Persona, error) {
	return nil, nil
}

func (f *fakeManagerAPI) ResetPersonas(context.Context) (int, error) { return 0, nil }

func (f *fakeManagerAPI) HasAPIKey() bool { return f.hasAPIKey }

func (f *fakeManagerAPI) EnsureStructuredOutputModel(context.Context, domain.ModelID) error {
	return f.structuredOutputErr
}

func (f *fakeManagerAPI) EmbeddingSearchable() (bool, error) {
	return f.embeddingSearchable, f.embeddingProbeErr
}

// newConfigTestContext builds a [Context] backed by a real
// [config.FileStore] rooted in a fresh temp directory and the given
// manager fake. Using the real store exercises the actual Load and
// Update behaviour these commands depend on, not a hand-rolled
// substitute.
func newConfigTestContext(t *testing.T, mgr *fakeManagerAPI) (Context, *config.FileStore) {
	t.Helper()

	store := config.NewFileStore(t.TempDir())

	return Context{
		Manager: mgr,
		Config:  store,
		Active:  "#test",
	}, store
}

// runConfigCmd parses raw through the production grammar, so
// ancestor flags such as `--reset` resolve exactly as they do for a
// real `/config` invocation, then runs the resulting command and
// returns its single message.
func runConfigCmd(t *testing.T, rc Context, raw string) tea.Msg {
	t.Helper()

	invocation, err := testParser.ParseInvocation(raw)
	require.NoError(t, err)

	cmd, ok := invocation.Leaf().(Command)
	require.True(t, ok, "parsed value %T does not implement Command", invocation.Leaf())

	rc.Invocation = invocation

	run := cmd.Run(t.Context(), rc)
	require.NotNil(t, run)

	return run()
}

// runConfigCmdAll is runConfigCmd for a command whose reply is a
// [tea.Batch] of more than one message (APIKeyConfig's small-model
// re-validation warning, alongside its own APIKeySetResult). A
// single-message reply comes back as a one-element slice.
func runConfigCmdAll(t *testing.T, rc Context, raw string) []tea.Msg {
	t.Helper()

	msg := runConfigCmd(t, rc, raw)

	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}

	msgs := make([]tea.Msg, len(batch))
	for i, cmd := range batch {
		msgs[i] = cmd()
	}

	return msgs
}

// requireSystemNotice asserts msg is a [domain.SystemNotice] with
// the given target and text, ignoring the notice's timestamp.
func requireSystemNotice(t *testing.T, msg tea.Msg, wantTarget domain.ChannelName, wantText string) {
	t.Helper()

	notice, ok := msg.(domain.SystemNotice)
	require.True(t, ok, "want domain.SystemNotice, got %T (%+v)", msg, msg)
	require.Equal(t, domain.SystemNotice{Target: wantTarget, Text: wantText, At: notice.At}, notice)
}

// requireErrorEvent asserts msg is a [domain.ErrorEvent] with the
// given operation and wrapped error. It compares the wrapped error
// with its "At" field zeroed on both sides, since these commands
// stamp typed validation errors with time.Now().
func requireErrorEvent(t *testing.T, msg tea.Msg, wantOperation string, wantErr error) {
	t.Helper()

	evt, ok := msg.(domain.ErrorEvent)
	require.True(t, ok, "want domain.ErrorEvent, got %T (%+v)", msg, msg)
	require.Equal(t, wantOperation, evt.Operation)
	require.Equal(t, zeroAt(wantErr), zeroAt(evt.Err))
}

// zeroAt returns a copy of err with any struct field named "At" of
// type time.Time set to its zero value, via reflection, so error
// comparisons in tests do not depend on when the command ran.
func zeroAt(err error) error {
	v := reflect.ValueOf(err)
	if v.Kind() != reflect.Struct {
		return err
	}

	cp := reflect.New(v.Type()).Elem()
	cp.Set(v)

	if f := cp.FieldByName("At"); f.IsValid() && f.CanSet() && f.Type() == reflect.TypeFor[time.Time]() {
		f.Set(reflect.Zero(f.Type()))
	}

	result, ok := cp.Interface().(error)
	if !ok {
		return err
	}

	return result
}

func loadGolden(t *testing.T, name string) string {
	t.Helper()

	root, err := os.OpenRoot("testdata")
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	f, err := root.Open(name)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	require.NoError(t, err)

	buf := make([]byte, info.Size())
	_, err = f.Read(buf)
	require.NoError(t, err)

	return string(buf)
}

func TestConfigCommand_Run_bareShowsAllSettings(t *testing.T) {
	rc, store := newConfigTestContext(t, &fakeManagerAPI{})

	require.NoError(t, store.Save(t.Context(), config.Config{
		APIKey:              "sk-or-v1-abcd1234wxyz",
		BaseURL:             "https://openrouter.ai/api/v1",
		PokeInterval:        5 * time.Minute,
		DrainTimeout:        10 * time.Second,
		SmallModel:          "openai/gpt-5.4-mini",
		EmbeddingModel:      "openai/text-embedding-3-small",
		HighlightWords:      []string{"$nick"},
		DefaultChannelModes: "+nt",
	}))

	msg := runConfigCmd(t, rc, "/config")

	requireSystemNotice(t, msg, "#test", loadGolden(t, "config_show.golden.txt"))
}

func TestConfigCommand_Run_bareMasksAPIKeyAndShowsUnsetState(t *testing.T) {
	rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

	msg := runConfigCmd(t, rc, "/config")

	requireSystemNotice(t, msg, "#test", loadGolden(t, "config_show_unset.golden.txt"))
}

func TestAPIKeyConfig_Run(t *testing.T) {
	t.Run("set persists the key", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config api-key sk-new-key")
		require.Equal(t, APIKeySetResult{}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, "sk-new-key", cfg.APIKey)
	})

	t.Run("bare shows the masked key", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{APIKey: "sk-abcd1234wxyz"}))

		msg := runConfigCmd(t, rc, "/config api-key")

		requireSystemNotice(t, msg, "#test", "api-key = set (…wxyz)")
	})

	t.Run("bare shows not set when no key is configured", func(t *testing.T) {
		rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config api-key")

		requireSystemNotice(t, msg, "#test", "api-key = not set")
	})

	t.Run("reset clears the key", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{APIKey: "sk-old"}))

		msg := runConfigCmd(t, rc, "/config --reset api-key")
		require.Equal(t, APIKeySetResult{Reset: true}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, "", cfg.APIKey)
	})

	t.Run("re-validates a stored small-model that now fails and warns alongside the confirmation", func(t *testing.T) {
		wantErr := domain.UnsupportedModelError{ModelID: "stale/model"}
		mgr := &fakeManagerAPI{structuredOutputErr: wantErr}
		rc, store := newConfigTestContext(t, mgr)
		require.NoError(t, store.Save(t.Context(), config.Config{SmallModel: "stale/model"}))

		msgs := runConfigCmdAll(t, rc, "/config api-key sk-new-key")

		require.Len(t, msgs, 2)
		require.Contains(t, msgs, tea.Msg(APIKeySetResult{}))

		var warned bool
		for _, msg := range msgs {
			if notice, ok := msg.(domain.SystemNotice); ok {
				require.Equal(t, "#test", string(notice.Target))
				require.Equal(t, "warning: the stored small-model stale/model failed catalogue validation: "+wantErr.Error(), notice.Text)
				warned = true
			}
		}
		require.True(t, warned, "expected a warning SystemNotice among %+v", msgs)
	})

	t.Run("a stored small-model that still validates produces only the confirmation", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{SmallModel: "still/fine"}))

		msg := runConfigCmd(t, rc, "/config api-key sk-new-key")
		require.Equal(t, APIKeySetResult{}, msg)
	})
}

func TestPokeIntervalConfig_Run(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  tea.Msg
	}{
		{name: "valid interval", input: "/config poke-interval 10m", want: PokeIntervalSetResult{Interval: 10 * time.Minute}},
		{name: "at the floor", input: "/config poke-interval 30s", want: PokeIntervalSetResult{Interval: 30 * time.Second}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

			msg := runConfigCmd(t, rc, tt.input)
			require.Equal(t, tt.want, msg)
		})
	}

	rangeTests := []struct {
		name     string
		interval string
	}{
		{name: "zero", interval: "0s"},
		{name: "negative", interval: "-5m"},
		{name: "below the floor", interval: "5s"},
	}

	for _, tt := range rangeTests {
		t.Run(tt.name, func(t *testing.T) {
			rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

			interval, err := time.ParseDuration(tt.interval)
			require.NoError(t, err)

			msg := runConfigCmd(t, rc, "/config poke-interval "+tt.interval)
			requireErrorEvent(t, msg, "config poke-interval", domain.PokeIntervalOutOfRangeError{
				Interval: interval,
				Floor:    config.MinPokeInterval,
			})
		})
	}

	t.Run("unparseable duration", func(t *testing.T) {
		rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

		_, wantErr := time.ParseDuration("banana")

		msg := runConfigCmd(t, rc, "/config poke-interval banana")
		requireErrorEvent(t, msg, "config poke-interval", domain.InvalidDurationError{Input: "banana", Err: wantErr})
	})

	t.Run("bare shows the current value", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{PokeInterval: 15 * time.Minute}))

		msg := runConfigCmd(t, rc, "/config poke-interval")

		requireSystemNotice(t, msg, "#test", "poke-interval = 15m0s")
	})

	t.Run("reset restores the default", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{PokeInterval: time.Hour}))

		msg := runConfigCmd(t, rc, "/config --reset poke-interval")
		require.Equal(t, PokeIntervalSetResult{Interval: config.DefaultPokeInterval, Reset: true}, msg)
	})
}

func TestDrainTimeoutConfig_Run(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  tea.Msg
	}{
		{name: "valid timeout", input: "/config drain-timeout 30s", want: DrainTimeoutSetResult{Timeout: 30 * time.Second}},
		{name: "at the floor", input: "/config drain-timeout 1s", want: DrainTimeoutSetResult{Timeout: time.Second}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

			msg := runConfigCmd(t, rc, tt.input)
			require.Equal(t, tt.want, msg)
		})
	}

	rangeTests := []struct {
		name    string
		timeout string
	}{
		{name: "zero", timeout: "0s"},
		{name: "negative", timeout: "-5s"},
		{name: "below the floor", timeout: "500ms"},
	}

	for _, tt := range rangeTests {
		t.Run(tt.name, func(t *testing.T) {
			rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

			timeout, err := time.ParseDuration(tt.timeout)
			require.NoError(t, err)

			msg := runConfigCmd(t, rc, "/config drain-timeout "+tt.timeout)
			requireErrorEvent(t, msg, "config drain-timeout", domain.DrainTimeoutOutOfRangeError{
				Timeout: timeout,
				Floor:   config.MinDrainTimeout,
			})
		})
	}

	t.Run("unparseable duration", func(t *testing.T) {
		rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

		_, wantErr := time.ParseDuration("banana")

		msg := runConfigCmd(t, rc, "/config drain-timeout banana")
		requireErrorEvent(t, msg, "config drain-timeout", domain.InvalidDurationError{Input: "banana", Err: wantErr})
	})

	t.Run("bare shows the current value", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{DrainTimeout: 15 * time.Second}))

		msg := runConfigCmd(t, rc, "/config drain-timeout")

		requireSystemNotice(t, msg, "#test", "drain-timeout = 15s")
	})

	t.Run("reset restores the default", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{DrainTimeout: time.Minute}))

		msg := runConfigCmd(t, rc, "/config --reset drain-timeout")
		require.Equal(t, DrainTimeoutSetResult{Timeout: config.DefaultDrainTimeout, Reset: true}, msg)
	})
}

func TestSmallModelConfig_Run(t *testing.T) {
	t.Run("validates against the catalogue when a key is configured", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{hasAPIKey: true})

		msg := runConfigCmd(t, rc, "/config small-model openai/gpt-5.4-mini")
		require.Equal(t, SmallModelSetResult{ModelID: "openai/gpt-5.4-mini"}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, domain.ModelID("openai/gpt-5.4-mini"), cfg.SmallModel)
	})

	t.Run("rejects a model the catalogue does not support", func(t *testing.T) {
		wantErr := domain.UnsupportedModelError{ModelID: "typo/model"}
		rc, store := newConfigTestContext(t, &fakeManagerAPI{hasAPIKey: true, structuredOutputErr: wantErr})

		msg := runConfigCmd(t, rc, "/config small-model typo/model")
		requireErrorEvent(t, msg, "config small-model", wantErr)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, config.DefaultSmallModel, cfg.SmallModel)
	})

	t.Run("defers validation and persists when no API key is configured", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{hasAPIKey: false})

		msg := runConfigCmd(t, rc, "/config small-model unvalidated/model")

		requireSystemNotice(t, msg, "#test",
			"small-model set to unvalidated/model (not validated: no API key configured yet; "+
				"it will be checked the next time an API key is set).")

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, domain.ModelID("unvalidated/model"), cfg.SmallModel)
	})

	t.Run("bare shows the current value", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{SmallModel: "openai/gpt-5.4-mini"}))

		msg := runConfigCmd(t, rc, "/config small-model")

		requireSystemNotice(t, msg, "#test", "small-model = openai/gpt-5.4-mini")
	})

	t.Run("reset restores the default", func(t *testing.T) {
		rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config --reset small-model")
		require.Equal(t, SmallModelSetResult{ModelID: config.DefaultSmallModel, Reset: true}, msg)
	})
}

// TestEmbeddingModelConfig_Run pins that /config embedding-model no
// longer validates the id against the model catalogue: OpenRouter's
// chat-completions catalogue carries no embedding models at all, so
// that check would reject every value. Persistence always succeeds;
// the reply instead reports the outcome of the functional probe
// memory.NewDefaultStore's own config-change listener already runs.
func TestEmbeddingModelConfig_Run(t *testing.T) {
	t.Run("a key configured and a working endpoint confirms the set", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{hasAPIKey: true, embeddingSearchable: true})

		msg := runConfigCmd(t, rc, "/config embedding-model openai/text-embedding-3-large")
		require.Equal(t, EmbeddingModelSetResult{ModelID: "openai/text-embedding-3-large"}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, domain.ModelID("openai/text-embedding-3-large"), cfg.EmbeddingModel)
	})

	t.Run("a key configured but the probe failing persists the value and warns why", func(t *testing.T) {
		probeErr := fmt.Errorf("upstream 404")
		rc, store := newConfigTestContext(t, &fakeManagerAPI{
			hasAPIKey:           true,
			embeddingSearchable: false,
			embeddingProbeErr:   probeErr,
		})

		msg := runConfigCmd(t, rc, "/config embedding-model typo/embed")
		requireSystemNotice(t, msg, "#test", "embedding-model set to typo/embed; semantic search is unavailable: upstream 404.")

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, domain.ModelID("typo/embed"), cfg.EmbeddingModel)
	})

	t.Run("a key configured but no embedding endpoint to probe persists the value and says so", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{hasAPIKey: true, embeddingSearchable: false, embeddingProbeErr: nil})

		msg := runConfigCmd(t, rc, "/config embedding-model some/model")
		requireSystemNotice(t, msg, "#test", "embedding-model set to some/model; semantic search is not available this session.")

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, domain.ModelID("some/model"), cfg.EmbeddingModel)
	})

	t.Run("defers the probe and persists when no API key is configured", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{hasAPIKey: false})

		msg := runConfigCmd(t, rc, "/config embedding-model unvalidated/embed")

		requireSystemNotice(t, msg, "#test",
			"embedding-model set to unvalidated/embed (not probed: no API key configured yet; "+
				"semantic search will be probed the next time an API key is set).")

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, domain.ModelID("unvalidated/embed"), cfg.EmbeddingModel)
	})

	t.Run("bare shows the current value", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{EmbeddingModel: "openai/text-embedding-3-small"}))

		msg := runConfigCmd(t, rc, "/config embedding-model")

		requireSystemNotice(t, msg, "#test", "embedding-model = openai/text-embedding-3-small")
	})
}

func TestHighlightConfig_applyHighlightWords(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  []string
	}{
		{name: "nick absent gets restored first", words: []string{"urgent"}, want: []string{"$nick", "urgent"}},
		{name: "nick present stays put", words: []string{"urgent", "$nick"}, want: []string{"urgent", "$nick"}},
		{name: "explicit removal drops nick", words: []string{"urgent", "-$nick"}, want: []string{"urgent"}},
		{name: "explicit removal alone empties the list", words: []string{"-$nick"}, want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, applyHighlightWords(tt.words))
		})
	}
}

func TestHighlightConfig_Run(t *testing.T) {
	t.Run("set preserves nick by default", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config highlight urgent")
		require.Equal(t, HighlightWordsSetResult{Words: []string{"$nick", "urgent"}}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, []string{"$nick", "urgent"}, cfg.HighlightWords)
	})

	t.Run("bare shows the current list", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{HighlightWords: []string{"$nick", "urgent"}}))

		msg := runConfigCmd(t, rc, "/config highlight")

		requireSystemNotice(t, msg, "#test", "highlight = $nick urgent")
	})

	t.Run("reset restores the default word list", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{HighlightWords: []string{"urgent"}}))

		msg := runConfigCmd(t, rc, "/config --reset highlight")
		require.Equal(t, HighlightWordsSetResult{Words: []string{"$nick"}, Reset: true}, msg)
	})
}

func TestDefaultModesConfig_Run(t *testing.T) {
	t.Run("valid modes persist canonicalised", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config default-modes +tn")
		requireSystemNotice(t, msg, "#test", "default-modes = +nt")

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, "+nt", cfg.DefaultChannelModes)
	})

	t.Run("rejects a member mode", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config default-modes +o")
		requireErrorEvent(t, msg, "config default-modes", domain.UnknownModeFlagError{Flag: domain.ModeOperator})

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, config.DefaultChannelModesSpec, cfg.DefaultChannelModes)
	})

	t.Run("rejects a malformed string", func(t *testing.T) {
		rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config default-modes nt")
		requireErrorEvent(t, msg, "config default-modes", domain.MalformedChannelModeError{Input: "nt"})
	})

	t.Run("bare shows the current value", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{DefaultChannelModes: "+nt"}))

		msg := runConfigCmd(t, rc, "/config default-modes")

		requireSystemNotice(t, msg, "#test", "default-modes = +nt")
	})

	t.Run("reset restores the default", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		require.NoError(t, store.Save(t.Context(), config.Config{DefaultChannelModes: "+s"}))

		msg := runConfigCmd(t, rc, "/config --reset default-modes")
		requireSystemNotice(t, msg, "#test", "default-modes = "+config.DefaultChannelModesSpec)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, config.DefaultChannelModesSpec, cfg.DefaultChannelModes)
	})
}

func TestTimestampFormatConfig_Run(t *testing.T) {
	t.Run("bare prints usage and does not disable timestamps", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		custom := "%H:%M"
		require.NoError(t, store.Save(t.Context(), config.Config{TimestampFormat: &custom}))

		msg := runConfigCmd(t, rc, "/config timestamp-format")
		require.Equal(t, UsageError{Command: "config", Usage: "/config timestamp-format <format>"}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, &custom, cfg.TimestampFormat)
	})

	t.Run(`explicit "" disables timestamps`, func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, `/config timestamp-format ""`)

		disabled := ""
		require.Equal(t, TimestampFormatSetResult{Format: &disabled}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Equal(t, &disabled, cfg.TimestampFormat)
	})

	t.Run("a format string sets it", func(t *testing.T) {
		rc, _ := newConfigTestContext(t, &fakeManagerAPI{})

		msg := runConfigCmd(t, rc, "/config timestamp-format %H:%M")

		want := "%H:%M"
		require.Equal(t, TimestampFormatSetResult{Format: &want}, msg)
	})

	t.Run("reset restores the locale default", func(t *testing.T) {
		rc, store := newConfigTestContext(t, &fakeManagerAPI{})
		custom := "%H:%M"
		require.NoError(t, store.Save(t.Context(), config.Config{TimestampFormat: &custom}))

		msg := runConfigCmd(t, rc, "/config --reset timestamp-format")
		require.Equal(t, TimestampFormatSetResult{Format: nil, Reset: true}, msg)

		cfg, err := store.Load(t.Context())
		require.NoError(t, err)
		require.Nil(t, cfg.TimestampFormat)
	})
}

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{name: "empty", key: "", want: "not set"},
		{name: "very short key is fully masked", key: "abc", want: "set (masked)"},
		{name: "key at the reveal threshold is fully masked", key: "12345678", want: "set (masked)"},
		{name: "key just over the reveal threshold shows last four", key: "123456789", want: "set (…6789)"},
		{name: "long key shows last four", key: "sk-or-v1-abcd1234wxyz", want: "set (…wxyz)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, maskAPIKey(tt.key))
		})
	}
}
