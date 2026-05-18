package modelclient

import (
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
