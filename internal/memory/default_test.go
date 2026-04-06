package memory

import (
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
