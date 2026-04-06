package memory

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	chromem "github.com/philippgille/chromem-go"

	"github.com/laney/modeloff/internal/config"
)

// NewDefaultStore creates a memory Store using the system's default
// data directories. It returns an IndexedStore backed by a FileStore
// and a chromem-go vector index when possible, falling back to a plain
// FileStore if index creation fails.
//
// The embedding function is rebuilt automatically whenever the API key,
// base URL, or embedding model changes in the config store.
func NewDefaultStore(cfg config.Config, cfgStore config.Store) (Store, error) {
	files, err := NewDefaultFileStore()
	if err != nil {
		return nil, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	indexDir := filepath.Join(home, ".local", "share", "modeloff", "memory_index")

	var embeddingPtr atomic.Pointer[chromem.EmbeddingFunc]
	storeEmbedder(&embeddingPtr, cfg)

	cfgStore.OnChange(func(prev, curr config.Config) {
		if prev.APIKey == curr.APIKey &&
			prev.BaseURL == curr.BaseURL &&
			prev.EmbeddingModel == curr.EmbeddingModel {
			return
		}

		storeEmbedder(&embeddingPtr, curr)
	})

	embeddingFunc := func(ctx context.Context, text string) ([]float32, error) {
		fn := embeddingPtr.Load()
		if fn == nil {
			return nil, fmt.Errorf("no API key configured")
		}

		return (*fn)(ctx, text)
	}

	indexed, err := NewIndexedStore(files, indexDir, embeddingFunc)
	if err != nil {
		slog.Default().Warn("vector index unavailable, falling back to file store",
			"error", err)

		return files, nil
	}

	return indexed, nil
}

func storeEmbedder(ptr *atomic.Pointer[chromem.EmbeddingFunc], cfg config.Config) {
	if cfg.APIKey == "" {
		ptr.Store(nil)
		return
	}

	fn := chromem.NewEmbeddingFuncOpenAICompat(
		cfg.BaseURL,
		cfg.APIKey,
		string(cfg.EmbeddingModel),
		nil,
	)

	ptr.Store(&fn)
}
