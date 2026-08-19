package modelclient

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/memory"
)

func TestBuildSystemPrompt(t *testing.T) {
	botty := domain.NewModelInstance("inst-botty", "botty", "test/model", "grumpy sysadmin", nil)
	cw := domain.NewChannelWindow("#dev", time.Time{})
	cw.Topic = "go stuff"

	user := domain.NewUserInstance("testuser")
	cw.Members.Add(user)
	cw.Members.Add(botty)

	prompt := buildSystemPrompt(cw, botty, nil)

	require.Equal(t, loadGolden(t, "system_prompt.golden.txt"), prompt)
}

func TestBuildSystemPrompt_with_memories(t *testing.T) {
	cw := domain.NewChannelWindow("#dev", time.Time{})
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)
	memories := []memory.Entry{
		{Key: "mood", Content: "curious"},
		{Key: "goal", Content: "learn go"},
	}

	prompt := buildSystemPrompt(cw, inst, memories)

	require.Equal(t, loadGolden(t, "system_prompt_with_memories.golden.txt"), prompt)
}

// TestCapMemoriesForPrompt covers the bound on the block of memories
// inlined into every system prompt: unbounded memory growth would
// otherwise be paid for, in tokens, on every single turn — including
// poke turns nothing prompted.
func TestCapMemoriesForPrompt(t *testing.T) {
	// Zero-padded so every key in a table is the same length,
	// keeping each entry's byte cost identical and the arithmetic in
	// each case's wantCount easy to check by hand.
	entries := func(n, contentSize int) []memory.Entry {
		out := make([]memory.Entry, n)
		for i := range out {
			out[i] = memory.Entry{Key: fmt.Sprintf("k%02d", i), Content: strings.Repeat("x", contentSize)}
		}

		return out
	}

	tests := []struct {
		name          string
		entries       []memory.Entry
		wantCount     int
		wantTruncated bool
	}{
		{
			name:          "empty is not truncated",
			entries:       nil,
			wantCount:     0,
			wantTruncated: false,
		},
		{
			name:          "under both caps keeps everything",
			entries:       entries(3, 10),
			wantCount:     3,
			wantTruncated: false,
		},
		{
			name:          "over the entry-count cap truncates to the cap",
			entries:       entries(maxMemoryEntries+5, 1),
			wantCount:     maxMemoryEntries,
			wantTruncated: true,
		},
		{
			// Each entry costs 103 bytes (a 3-byte key + 100-byte
			// content); 38 of them fit in maxMemoryBytes (3914), a
			// 39th would not (4017).
			name:          "several ordinary entries together over the byte cap truncate to what fits",
			entries:       entries(maxMemoryEntries-1, 100),
			wantCount:     38,
			wantTruncated: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			capped, truncated := capMemoriesForPrompt(tc.entries)

			require.Equal(t, tc.entries[:tc.wantCount], capped)
			require.Equal(t, tc.wantTruncated, truncated)
		})
	}
}

// TestCapMemoriesForPrompt_single_oversized_entry_is_truncated_not_dropped
// covers the edge the table above can't: a single memory bigger than
// maxMemoryBytes on its own. Dropping it entirely would leave the
// prompt with a truncation note pointing at memories the model can't
// see any of; keeping a truncated version, the way
// [trimToTokenBudget] always keeps at least the newest transcript
// event, gives the model something real instead.
func TestCapMemoriesForPrompt_single_oversized_entry_is_truncated_not_dropped(t *testing.T) {
	oversized := memory.Entry{Key: "big", Content: strings.Repeat("x", maxMemoryBytes)}
	other := memory.Entry{Key: "small", Content: "v"}

	capped, truncated := capMemoriesForPrompt([]memory.Entry{oversized, other})

	require.True(t, truncated)
	require.Equal(t, []memory.Entry{
		{Key: "big", Content: strings.Repeat("x", maxMemoryBytes-len("big"))},
	}, capped)
}

// TestBuildSystemPrompt_with_truncated_memories pins that the prompt
// text tells the model when memories were left out, so it knows the
// shown block is a partial view and reaches for search_memory to see
// the rest.
func TestBuildSystemPrompt_with_truncated_memories(t *testing.T) {
	cw := domain.NewChannelWindow("#dev", time.Time{})
	inst := domain.NewModelInstance("inst-botty", "botty", "test/model", "", nil)

	many := make([]memory.Entry, maxMemoryEntries+1)
	for i := range many {
		many[i] = memory.Entry{Key: fmt.Sprintf("k%d", i), Content: "v"}
	}

	prompt := buildSystemPrompt(cw, inst, many)

	require.Equal(t, loadGolden(t, "system_prompt_with_truncated_memories.golden.txt"), prompt)
}
