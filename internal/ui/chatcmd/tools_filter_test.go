package chatcmd

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
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

// TestBuildToolRegistry_filters_by_caps proves that capabilities
// determine the advertised tool set, while the current window does
// not. This keeps the provider prefix stable between channel and DM
// turns. The executor applies each tool's window requirement.
func TestBuildToolRegistry_filters_by_caps(t *testing.T) {
	reg, err := BuildToolRegistry()
	require.NoError(t, err)

	noCaps := command.NoCapabilities()
	all := sortedToolNames(reg)
	filtered := sortedToolNames(reg.Filter(noCaps))

	// Operator-gated tools never reach a no-capability model, in any window.
	require.Equal(t, []string{"add_model", "kill"}, removedToolNames(all, filtered))
}
