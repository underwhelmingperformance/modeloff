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
)

// NewDefaultStore creates a memory Store using the given data store
// for persistence and chromem-go for vector search. The returned
// store is the IndexedStore unless the vector index itself cannot be
// opened (e.g. its on-disk directory can't be created) — that is the
// only condition that falls back to the plain, non-vector store
// adapter, since a construction-time probe failure of the embedding
// endpoint is not necessarily permanent: a fresh install has no API
// key yet by design (AGENTS.md's first-run flow directs the user to
// `/config`), and OnChange re-probes on every subsequent config
// change relevant to the embedder, in both directions, so search
// availability tracks the user fixing (or breaking) their
// configuration without a restart. Callers determine whether
// semantic search is actually usable through the returned store's
// Searchable(), not through its concrete type.
func NewDefaultStore(ctx context.Context, dataStore DataStore, cfg config.Config, cfgStore config.Store) (Store, error) {
	adapter := NewStoreAdapter(dataStore)

	var embeddingPtr atomic.Pointer[chromem.EmbeddingFunc]
	storeEmbedder(&embeddingPtr, cfg)

	embeddingFunc := buildEmbeddingFunc(&embeddingPtr)

	indexDir := filepath.Join(xdg.DataHome, "modeloff", "memory_index")

	indexed, err := NewIndexedStore(ctx, adapter, indexDir, embeddingFunc)
	if err != nil {
		slog.Default().Warn("vector index unavailable, falling back to store adapter",
			"error", err)

		return adapter, nil
	}

	cfgStore.OnChange(func(prev, curr config.Config) {
		if prev.APIKey == curr.APIKey &&
			prev.BaseURL == curr.BaseURL &&
			prev.EmbeddingModel == curr.EmbeddingModel {
			return
		}

		storeEmbedder(&embeddingPtr, curr)

		// ChangeFunc carries no request-scoped context: OnChange
		// fires synchronously from within Save, itself driven by a
		// UI command with its own lifetime already over by the time
		// any listener not itself passed that command's context
		// runs. This re-probe uses context.Background(), since no
		// live context in scope describes its lifetime.
		indexed.RefreshSearchable(context.Background())
	})

	return indexed, nil
}

// buildEmbeddingFunc returns the chromem.EmbeddingFunc an IndexedStore
// uses for both indexing and probing. It reads ptr on every call, so
// a later storeEmbedder swap — driven by a config change — takes
// effect without reattaching the store. A nil ptr (no API key
// configured, a knowable-in-advance failure) returns an error without
// making any request, so probing this function costs nothing when
// the endpoint has no chance of working.
func buildEmbeddingFunc(ptr *atomic.Pointer[chromem.EmbeddingFunc]) chromem.EmbeddingFunc {
	return func(ctx context.Context, text string) ([]float32, error) {
		fn := ptr.Load()
		if fn == nil {
			return nil, fmt.Errorf("no API key configured")
		}

		return (*fn)(ctx, text)
	}
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
