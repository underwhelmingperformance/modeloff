package memory

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"

	"github.com/adrg/xdg"
	chromem "github.com/philippgille/chromem-go"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/store"
)

// NewDefaultStore creates a memory Store using the given data store
// for persistence and chromem-go for vector search. If the vector
// index cannot be created, it falls back to a plain store adapter.
func NewDefaultStore(dataStore store.Store, cfg config.Config, cfgStore config.Store) (Store, error) {
	adapter := NewStoreAdapter(dataStore)

	indexDir := filepath.Join(xdg.DataHome, "modeloff", "memory_index")

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

	indexed, err := NewIndexedStore(adapter, indexDir, embeddingFunc)
	if err != nil {
		slog.Default().Warn("vector index unavailable, falling back to store adapter",
			"error", err)

		return adapter, nil
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
