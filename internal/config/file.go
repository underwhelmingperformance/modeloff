package config

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
)

// FileStore implements Store by reading and writing a JSON file on disc.
type FileStore struct {
	path string

	mu        sync.Mutex
	callbacks map[int64]ChangeFunc
	nextID    atomic.Int64

	// tracerProvider is the OTel `TracerProvider` the store uses for
	// its spans. Defaults to `otel.GetTracerProvider()`; tests inject
	// a per-test recorder via `WithTracerProvider`.
	tracerProvider trace.TracerProvider
}

// NewFileStore creates a FileStore that persists configuration to the
// given directory. The configuration file will be stored as
// config.json within that directory.
func NewFileStore(dir string) *FileStore {
	return &FileStore{
		path:           filepath.Join(dir, "config.json"),
		callbacks:      make(map[int64]ChangeFunc),
		tracerProvider: otel.GetTracerProvider(),
	}
}

// WithTracerProvider overrides the OTel `TracerProvider` the store
// uses for its spans. Tests inject a per-test recorder so span
// recordings stay scoped to a single test rather than relying on the
// global provider's swap-and-restore.
func (s *FileStore) WithTracerProvider(tp trace.TracerProvider) *FileStore {
	s.tracerProvider = tp

	return s
}

// NewDefaultFileStore creates a FileStore using the system's default
// configuration directory (~/.config/modeloff or equivalent).
func NewDefaultFileStore() (*FileStore, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}

	return NewFileStore(filepath.Join(base, "modeloff")), nil
}

func defaults() Config {
	nick := "user"

	if u := os.Getenv("USER"); u != "" {
		nick = u
	}

	return Config{
		BaseURL:        DefaultBaseURL,
		UserNick:       nick,
		PokeInterval:   DefaultPokeInterval,
		SmallModel:     DefaultSmallModel,
		EmbeddingModel: DefaultEmbeddingModel,
		HighlightWords: append([]string(nil), DefaultHighlightWords...),
	}
}

// inSpan brackets fn with a span and result-recording on the store's
// tracer provider. See `observability.SpanRunner` for the wrapper's
// shape; persistence failures are tagged `ErrorKindStore`.
func (s *FileStore) inSpan(
	ctx context.Context,
	op string,
	fn func(ctx context.Context, span trace.Span) error,
) error {
	return observability.SpanRunner{
		Tracer:         s.tracerProvider.Tracer("github.com/laney/modeloff/internal/config"),
		DefaultErrKind: observability.ErrorKindStore,
	}.Run(ctx, op, nil, fn)
}

// Load reads the configuration from disk, returning defaults if the
// file does not yet exist.
func (s *FileStore) Load(ctx context.Context) (Config, error) {
	cfg := defaults()
	err := s.inSpan(ctx, "config.file.load", func(_ context.Context, _ trace.Span) error {
		data, err := os.ReadFile(s.path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}

		if err := json.Unmarshal(data, &cfg); err != nil {
			return err
		}

		// Backward compat: migrate the old "nick_model" key.
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err == nil {
			if _, hasNew := raw["small_model"]; !hasNew {
				if v, ok := raw["nick_model"]; ok {
					var old domain.ModelID
					if err := json.Unmarshal(v, &old); err == nil && old != "" {
						cfg.SmallModel = old
					}
				}
			}
		}

		return nil
	})
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Save writes the configuration to disk, creating the directory if
// necessary. Registered change callbacks are fired after a
// successful write with the old and new values.
func (s *FileStore) Save(ctx context.Context, cfg Config) error {
	return s.inSpan(ctx, "config.file.save", func(ctx context.Context, _ trace.Span) error {
		old, _ := s.Load(ctx)

		dir := filepath.Dir(s.path)

		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}

		data, err := json.MarshalIndent(cfg, "", "  ") //nolint:gosec // G117: API key is intentionally persisted to the config file.
		if err != nil {
			return err
		}

		if err := os.WriteFile(s.path, data, 0o600); err != nil {
			return err
		}

		s.mu.Lock()
		cbs := make([]ChangeFunc, 0, len(s.callbacks))
		for _, fn := range s.callbacks {
			cbs = append(cbs, fn)
		}
		s.mu.Unlock()

		for _, fn := range cbs {
			fn(old, cfg)
		}

		return nil
	})
}

// OnChange registers a callback to be invoked after every successful
// Save with the old and new configuration values. The returned
// function removes the callback when called.
func (s *FileStore) OnChange(fn ChangeFunc) UnsubscribeFunc {
	id := s.nextID.Add(1)

	s.mu.Lock()
	s.callbacks[id] = fn
	s.mu.Unlock()

	return func() {
		s.mu.Lock()
		delete(s.callbacks, id)
		s.mu.Unlock()
	}
}
