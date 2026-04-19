package session

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/memory"
)

func TestNewToolRegistry_definitions(t *testing.T) {
	specs := []ToolSpec{
		{Definition: api.ToolDefinition{Name: "alpha", Description: "first"}},
		{Definition: api.ToolDefinition{Name: "beta", Description: "second"}},
	}

	registry := NewToolRegistry(specs...)

	require.Equal(t, []api.ToolDefinition{
		{Name: "alpha", Description: "first"},
		{Name: "beta", Description: "second"},
	}, registry.Definitions())
}

func TestToolRegistry_Find(t *testing.T) {
	spec := ToolSpec{
		Definition: api.ToolDefinition{Name: "alpha"},
		Execute: func(context.Context, ToolContext, json.RawMessage) (ToolResultPayload, error) {
			return ToolResultPayload{OK: true}, nil
		},
	}

	registry := NewToolRegistry(spec)

	t.Run("found", func(t *testing.T) {
		got, ok := registry.Find("alpha")
		require.True(t, ok)
		require.Equal(t, "alpha", got.Definition.Name)
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := registry.Find("missing")
		require.False(t, ok)
	})

	t.Run("nil registry", func(t *testing.T) {
		var nilReg *ToolRegistry
		_, ok := nilReg.Find("alpha")
		require.False(t, ok)
	})
}

func TestToolRegistry_Definitions_nil(t *testing.T) {
	var nilReg *ToolRegistry
	require.Nil(t, nilReg.Definitions())
}

func TestMergeToolRegistries(t *testing.T) {
	a := NewToolRegistry(
		ToolSpec{Definition: api.ToolDefinition{Name: "alpha", Description: "from-a"}},
		ToolSpec{Definition: api.ToolDefinition{Name: "shared", Description: "from-a"}},
	)

	b := NewToolRegistry(
		ToolSpec{Definition: api.ToolDefinition{Name: "beta", Description: "from-b"}},
		ToolSpec{Definition: api.ToolDefinition{Name: "shared", Description: "from-b"}},
	)

	merged := MergeToolRegistries(a, b)

	require.Equal(t, []api.ToolDefinition{
		{Name: "alpha", Description: "from-a"},
		{Name: "shared", Description: "from-a"},
		{Name: "beta", Description: "from-b"},
	}, merged.Definitions())
}

func TestMergeToolRegistries_nil_registries(t *testing.T) {
	a := NewToolRegistry(
		ToolSpec{Definition: api.ToolDefinition{Name: "alpha"}},
	)

	merged := MergeToolRegistries(nil, a, nil)

	require.Equal(t, []api.ToolDefinition{
		{Name: "alpha"},
	}, merged.Definitions())
}

func TestToolResultPayload_JSON_round_trip(t *testing.T) {
	tests := []struct {
		name    string
		payload ToolResultPayload
	}{
		{
			name:    "success with summary",
			payload: ToolResultPayload{OK: true, Summary: "done"},
		},
		{
			name:    "error",
			payload: ToolResultPayload{OK: false, Error: "something broke"},
		},
		{
			name:    "with data",
			payload: ToolResultPayload{OK: true, Data: "extra info"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.payload)
			require.NoError(t, err)

			var got ToolResultPayload
			require.NoError(t, json.Unmarshal(data, &got))

			require.Equal(t, tt.payload.OK, got.OK)
			require.Equal(t, tt.payload.Summary, got.Summary)
			require.Equal(t, tt.payload.Error, got.Error)
		})
	}
}

type fakeMemoryExecutor struct {
	written map[string]string
	deleted []string
}

func newFakeMemoryExecutor() *fakeMemoryExecutor {
	return &fakeMemoryExecutor{written: make(map[string]string)}
}

func (f *fakeMemoryExecutor) WriteMemory(_ context.Context, key, content string) error {
	f.written[key] = content
	return nil
}

func (f *fakeMemoryExecutor) DeleteMemory(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeMemoryExecutor) SearchMemory(_ context.Context, _ string, _ int) ([]memory.SearchResult, error) {
	return []memory.SearchResult{{Entry: memory.Entry{Key: "found", Content: "value"}, Similarity: 0.9}}, nil
}

func TestMemoryToolRegistry_tool_names(t *testing.T) {
	mem := newFakeMemoryExecutor()
	registry := memoryToolRegistry(mem, true)

	var names []string
	for _, def := range registry.Definitions() {
		names = append(names, def.Name)
	}

	require.Equal(t, []string{"write_memory", "delete_memory", "search_memory"}, names)
}

func TestMemoryToolRegistry_without_search(t *testing.T) {
	mem := newFakeMemoryExecutor()
	registry := memoryToolRegistry(mem, false)

	var names []string
	for _, def := range registry.Definitions() {
		names = append(names, def.Name)
	}

	require.Equal(t, []string{"write_memory", "delete_memory"}, names)
}

func TestMemoryToolRegistry_descriptions_hold_memory_guidance(t *testing.T) {
	mem := newFakeMemoryExecutor()
	registry := memoryToolRegistry(mem, true)

	var b strings.Builder
	for _, def := range registry.Definitions() {
		fmt.Fprintf(&b, "=== %s ===\n%s\n", def.Name, def.Description)
	}

	require.Equal(t, loadGolden(t, "memory_tool_descriptions.golden.txt"), b.String())
}

func TestMemoryToolRegistry_nil_executor(t *testing.T) {
	registry := memoryToolRegistry(nil, true)
	require.Nil(t, registry)
}

func TestMemoryToolRegistry_write_executes(t *testing.T) {
	mem := newFakeMemoryExecutor()
	registry := memoryToolRegistry(mem, false)

	spec, ok := registry.Find("write_memory")
	require.True(t, ok)

	args := json.RawMessage(`{"key": "mood", "content": "happy"}`)
	payload, err := spec.Execute(t.Context(), ToolContext{}, args)
	require.NoError(t, err)
	require.True(t, payload.OK)
	require.Equal(t, "happy", mem.written["mood"])
}

func TestMemoryToolRegistry_delete_executes(t *testing.T) {
	mem := newFakeMemoryExecutor()
	registry := memoryToolRegistry(mem, false)

	spec, ok := registry.Find("delete_memory")
	require.True(t, ok)

	args := json.RawMessage(`{"key": "mood"}`)
	payload, err := spec.Execute(t.Context(), ToolContext{}, args)
	require.NoError(t, err)
	require.True(t, payload.OK)
	require.Equal(t, []string{"mood"}, mem.deleted)
}

func TestMemoryToolRegistry_search_executes(t *testing.T) {
	mem := newFakeMemoryExecutor()
	registry := memoryToolRegistry(mem, true)

	spec, ok := registry.Find("search_memory")
	require.True(t, ok)

	args := json.RawMessage(`{"query": "what mood", "limit": 5}`)
	payload, err := spec.Execute(t.Context(), ToolContext{}, args)
	require.NoError(t, err)
	require.True(t, payload.OK)
}
