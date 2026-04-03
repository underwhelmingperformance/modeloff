package components_test

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

func TestNickList_View_shows_sorted_members(t *testing.T) {
	nl := components.NewNickList([]domain.Nick{"charlie", "alice", "bob"})

	v := nl.View(20, 10)

	require.Contains(t, v, "alice")
	require.Contains(t, v, "bob")
	require.Contains(t, v, "charlie")
	require.Contains(t, v, "Users")
}

func TestNickList_View_empty(t *testing.T) {
	nl := components.NewNickList(nil)

	v := nl.View(20, 10)

	require.Contains(t, v, "No members")
}

func TestNickList_Update_handles_NickListUpdatedMsg(t *testing.T) {
	nl := components.NewNickList(nil)

	updated, _ := nl.Update(components.NickListUpdatedMsg{
		Members: []domain.Nick{"dave", "eve"},
	})

	v := updated.View(20, 10)
	require.Contains(t, v, "dave")
	require.Contains(t, v, "eve")
	require.NotContains(t, v, "No members")
}

func TestNickList_Update_clears_on_empty(t *testing.T) {
	nl := components.NewNickList([]domain.Nick{"alice"})

	v := nl.View(20, 10)
	require.Contains(t, v, "alice")

	updated, _ := nl.Update(components.NickListUpdatedMsg{Members: nil})

	v = updated.View(20, 10)
	require.Contains(t, v, "No members")
}

func TestNickList_View_overflow_fits_height(t *testing.T) {
	// Create more members than can fit in the given height.
	members := make([]domain.Nick, 20)
	for i := range members {
		members[i] = domain.Nick(fmt.Sprintf("user%02d", i))
	}

	nl := components.NewNickList(members)

	// Height 5: 1 line for the header, 4 lines for members.
	// The viewport should constrain the output to fit.
	v := nl.View(20, 5)

	require.Equal(t, 5, lipgloss.Height(v),
		"rendered height must match the available height")

	// First member (sorted: user00) should be visible.
	require.Contains(t, v, "user00")

	// Last member (user19) should not be visible — it's below the fold.
	require.NotContains(t, v, "user19")
}

func TestNickList_View_responsive(t *testing.T) {
	nl := components.NewNickList([]domain.Nick{"alice", "bob"})

	sizes := []struct{ w, h int }{
		{20, 10},
		{14, 5},
		{30, 20},
	}

	for _, sz := range sizes {
		v := nl.View(sz.w, sz.h)
		require.NotEmpty(t, v, "View(%d, %d) should not be empty", sz.w, sz.h)
		require.LessOrEqual(t, lipgloss.Width(v), sz.w+1,
			"View(%d, %d) should fit width", sz.w, sz.h)
	}
}
