package chatcmd

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelclient"
)

func sortedToolNames(reg *modelclient.ToolRegistry) []string {
	defs := reg.Definitions()
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

func removedToolNames(all, subset []string) []string {
	keep := make(map[string]struct{}, len(subset))
	for _, n := range subset {
		keep[n] = struct{}{}
	}

	var removed []string
	for _, n := range all {
		if _, ok := keep[n]; !ok {
			removed = append(removed, n)
		}
	}
	return removed
}

// TestBuildToolRegistry_filters_by_caps_and_kind proves a model's
// per-turn tool set omits operator-gated tools — it holds no
// capabilities — and additionally omits channel-only tools when the
// window is a DM.
func TestBuildToolRegistry_filters_by_caps_and_kind(t *testing.T) {
	reg, err := BuildToolRegistry()
	require.NoError(t, err)

	noCaps := command.NoCapabilities()
	all := sortedToolNames(reg)
	channel := sortedToolNames(reg.Filter(noCaps, domain.KindChannel))
	dm := sortedToolNames(reg.Filter(noCaps, domain.KindDM))

	// Operator-gated tools never reach a no-capability model, in any window.
	require.Equal(t, []string{"add_model", "kill"}, removedToolNames(all, channel))

	// A DM additionally drops the channel-only tools.
	require.Equal(t,
		[]string{"add_model", "invite", "kick", "kill", "mode", "topic"},
		removedToolNames(all, dm))
}
