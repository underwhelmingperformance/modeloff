package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

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

// Path returns the on-disk path this store reads and writes.
func (s *FileStore) Path() string {
	return s.path
}

// RecoverCorrupt moves the store's file to a timestamped backup path
// beside it, so a caller whose Load failed to parse it can retry and
// get defaults, without repeating the same failure. It returns the
// backup path, or an empty path and a nil error if there was no file
// to move. That case means the earlier Load failure had some other
// cause, a permissions problem for instance, that recovering from a
// corrupt file would not fix.
func (s *FileStore) RecoverCorrupt() (string, error) {
	backup := fmt.Sprintf("%s.corrupt-%s", s.path, time.Now().UTC().Format("20060102T150405Z"))

	if err := os.Rename(s.path, backup); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}

		return "", err
	}

	return backup, nil
}

func defaults() Config {
	nick := "user"

	if u := os.Getenv("USER"); u != "" {
		nick = u
	}

	return Config{
		BaseURL:             DefaultBaseURL,
		UserNick:            nick,
		PokeInterval:        DefaultPokeInterval,
		DrainTimeout:        DefaultDrainTimeout,
		SmallModel:          DefaultSmallModel,
		EmbeddingModel:      DefaultEmbeddingModel,
		HighlightWords:      append([]string(nil), DefaultHighlightWords...),
		DefaultChannelModes: DefaultChannelModesSpec,
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

// ErrCorrupt wraps a JSON decode failure so a caller such as
// RecoverCorrupt can tell "the file's content is unreadable" apart
// from any other Load failure, a permissions problem for instance,
// that recovering from a corrupt file would not fix.
var ErrCorrupt = errors.New("config file is corrupt")

// Load reads the configuration from disk, returning defaults if the
// file does not yet exist. A file that exists but fails to decode
// returns an error that wraps ErrCorrupt, so callers can check
// errors.Is(err, ErrCorrupt) to tell that apart from a read failure
// unrelated to the file's content.
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
			return fmt.Errorf("%w: %w", ErrCorrupt, err)
		}

		return nil
	})
	if err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// Save writes the configuration to disk, creating the directory if
// necessary, and fires registered change callbacks with the old and
// new values. The read-modify-write of the previous value and the
// write itself run under s.mu (see writeLocked) so two concurrent
// Save calls can't interleave — without that, the second of two
// racing Saves could read the first's pre-write value as "old" and
// hand every callback the wrong diff. The lock is released before
// any callback runs, so a listener that itself calls Save or
// OnChange doesn't self-deadlock on s.mu, which is not reentrant.
//
// Each callback is handed the span-scoped context this call runs
// under, so a listener that starts further work under it is bound by
// whatever deadline or cancellation the caller of Save already
// carries.
func (s *FileStore) Save(ctx context.Context, cfg Config) error {
	return s.inSpan(ctx, "config.file.save", func(ctx context.Context, _ trace.Span) error {
		old, cbs, err := s.writeLocked(ctx, cfg)
		if err != nil {
			return err
		}

		for _, fn := range cbs {
			fn(ctx, old, cfg)
		}

		return nil
	})
}

// writeLocked performs Save's read-modify-write under s.mu: reading
// the previous value, persisting the new one via persistLocked, and
// returning the registered callbacks to run once the lock is
// released.
func (s *FileStore) writeLocked(ctx context.Context, cfg Config) (Config, []ChangeFunc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, _ := s.Load(ctx)

	cbs, err := s.persistLocked(cfg)
	if err != nil {
		return Config{}, nil, err
	}

	return old, cbs, nil
}

// persistLocked marshals cfg and writes it atomically to s.path,
// creating the directory if necessary, and snapshots the registered
// callbacks to run once the caller releases s.mu. Callers must
// already hold s.mu; persistLocked does not lock.
func (s *FileStore) persistLocked(cfg Config) ([]ChangeFunc, error) {
	dir := filepath.Dir(s.path)

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}

	data, err := json.MarshalIndent(cfg, "", "  ") //nolint:gosec // G117: API key is intentionally persisted to the config file.
	if err != nil {
		return nil, err
	}

	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return nil, err
	}

	cbs := make([]ChangeFunc, 0, len(s.callbacks))
	for _, fn := range s.callbacks {
		cbs = append(cbs, fn)
	}

	return cbs, nil
}

// Update loads the current configuration, applies fn, and persists
// the result under the same s.mu-guarded read-modify-write Save's
// writeLocked uses (see updateLocked). Two concurrent Update calls,
// or an Update racing a Save, therefore serialise: neither can
// clobber the other's change with a value read before it landed.
// It returns the configuration fn produced.
func (s *FileStore) Update(ctx context.Context, fn func(Config) Config) (Config, error) {
	var next Config

	err := s.inSpan(ctx, "config.file.update", func(ctx context.Context, _ trace.Span) error {
		old, cbs, err := s.updateLocked(ctx, fn, &next)
		if err != nil {
			return err
		}

		for _, cb := range cbs {
			cb(ctx, old, next)
		}

		return nil
	})

	return next, err
}

// updateLocked runs Update's read-apply-write under s.mu: reading
// the previous value, computing the next one via fn, and delegating
// the write to persistLocked, the same helper writeLocked uses, so
// both paths persist identically. next receives the computed value
// so the caller can return it even though this method's own return
// only carries the previous value and the callbacks to run.
func (s *FileStore) updateLocked(ctx context.Context, fn func(Config) Config, next *Config) (Config, []ChangeFunc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, _ := s.Load(ctx)
	*next = fn(old)

	cbs, err := s.persistLocked(*next)
	if err != nil {
		return Config{}, nil, err
	}

	return old, cbs, nil
}

// writeFileAtomic writes data to path by first writing to a temp
// file in the same directory, then renaming it into place. The
// rename replaces any existing file at path in a single filesystem
// operation, so a reader or a crash never observes a partially
// written config.json — os.WriteFile's truncate-then-write can leave
// an empty or half-written file behind if the process dies mid-call.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	// Removing an already-renamed-away temp path is a no-op; this
	// only cleans up after a failure between here and the rename.
	defer func() { _ = os.Remove(tmpPath) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

// OnChange registers a callback to be invoked after every successful
// Save or Update, with the call's context and the old and new
// configuration values. The returned function removes the callback
// when called.
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
