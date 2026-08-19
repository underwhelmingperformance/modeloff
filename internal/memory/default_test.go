package memory

import (
	"context"
	"sync/atomic"
	"testing"

	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
)

func testConfig(overrides ...func(*config.Config)) config.Config {
	cfg := config.Config{
		APIKey:         "sk-test",
		BaseURL:        "https://openrouter.ai/api/v1",
		EmbeddingModel: domain.ModelID("openai/text-embedding-3-small"),
	}

	for _, o := range overrides {
		o(&cfg)
	}

	return cfg
}

func TestStoreEmbedder_empty_api_key_stores_nil(t *testing.T) {
	var ptr atomic.Pointer[chromem.EmbeddingFunc]

	storeEmbedder(&ptr, testConfig(func(c *config.Config) { c.APIKey = "" }))

	require.Nil(t, ptr.Load())
}

func TestStoreEmbedder_with_api_key_stores_non_nil(t *testing.T) {
	var ptr atomic.Pointer[chromem.EmbeddingFunc]

	storeEmbedder(&ptr, testConfig())

	require.NotNil(t, ptr.Load())
}

func TestStoreEmbedder_swap_on_config_change(t *testing.T) {
	var ptr atomic.Pointer[chromem.EmbeddingFunc]

	storeEmbedder(&ptr, testConfig())
	first := ptr.Load()
	require.NotNil(t, first)

	storeEmbedder(&ptr, testConfig(func(c *config.Config) { c.APIKey = "sk-second" }))
	second := ptr.Load()
	require.NotNil(t, second)

	require.NotSame(t, first, second)
}

func TestStoreEmbedder_swap_to_nil_on_key_removal(t *testing.T) {
	var ptr atomic.Pointer[chromem.EmbeddingFunc]

	storeEmbedder(&ptr, testConfig())
	require.NotNil(t, ptr.Load())

	storeEmbedder(&ptr, testConfig(func(c *config.Config) { c.APIKey = "" }))
	require.Nil(t, ptr.Load())
}

// TestBuildEmbeddingFunc_no_api_key_errors_without_a_call pins that
// probing the closure NewDefaultStore hands to IndexedStore makes no
// request at all when no API key is configured — a knowable-in-advance
// failure, not worth a wasted HTTP round trip.
func TestBuildEmbeddingFunc_no_api_key_errors_without_a_call(t *testing.T) {
	var ptr atomic.Pointer[chromem.EmbeddingFunc]
	storeEmbedder(&ptr, testConfig(func(c *config.Config) { c.APIKey = "" }))

	fn := buildEmbeddingFunc(&ptr)

	_, err := fn(t.Context(), "probe")
	require.Error(t, err)
	require.False(t, probeEmbeddingFunc(t.Context(), fn))
}

// TestBuildEmbeddingFunc_reads_ptr_live pins that the returned
// closure re-reads ptr on every call: that live indirection is the
// mechanism that lets IndexedStore.RefreshSearchable observe a
// storeEmbedder swap without the store being reattached.
func TestBuildEmbeddingFunc_reads_ptr_live(t *testing.T) {
	var ptr atomic.Pointer[chromem.EmbeddingFunc]
	fn := buildEmbeddingFunc(&ptr)

	_, err := fn(t.Context(), "probe")
	require.Error(t, err, "ptr starts nil, so the closure must not have captured a stale non-nil value")

	var calls atomic.Int32
	working := chromem.EmbeddingFunc(func(context.Context, string) ([]float32, error) {
		calls.Add(1)
		return []float32{1.0}, nil
	})
	ptr.Store(&working)

	_, err = fn(t.Context(), "probe")
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
}
