package components_test

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

func members(ms ...domain.Member) domain.MemberList {
	ml := domain.NewMemberList()

	for _, m := range ms {
		ml.Add(m.Instance)
		ml.SetModes(m.Instance, m.Modes)
	}

	return ml
}

// member builds a test `domain.Member` with a synthetic `*Instance`
// allocated under the conventional `inst-<nick>` id. Allocating the
// instance here keeps `memberLess`'s InstanceID tiebreaker stable —
// synthetic members must not share the empty ID, which they would
// if the helper returned a bare nick-only struct.
func member(nick string, modes domain.MemberModes) domain.Member {
	inst := domain.NewModelInstance(
		domain.InstanceID("inst-"+nick),
		domain.Nick(nick), "", "", nil,
	)
	return domain.Member{Instance: inst, Nick: domain.Nick(nick), Modes: modes}
}

// op, voiced and plain are the three privilege sets the nick-list
// tests give their members.
var (
	op     = domain.MemberModes{Operator: true}
	voiced = domain.MemberModes{Voice: true}
	plain  = domain.MemberModes{}
)

func TestNickList_View_shows_members(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", op),
		member("charlie", voiced),
		member("bob", plain),
	))

	v := nl.View(20, 10)

	require.Equal(t, []string{"Nicks", "@alice", "+charlie", "bob"}, visibleLines(v))
}

func TestNickList_View_empty(t *testing.T) {
	nl := components.NewNickList(domain.NewMemberList())

	v := nl.View(20, 10)

	require.Equal(t, []string{"No members"}, visibleLines(v))
}

func TestNickList_Update_handles_NickListUpdatedMsg(t *testing.T) {
	nl := components.NewNickList(domain.NewMemberList())

	updated, _ := nl.Update(components.NickListUpdatedMsg{
		Members: members(
			member("eve", voiced),
			member("dave", plain),
		),
	})

	v := updated.View(20, 10)
	require.Equal(t, []string{"Nicks", "+eve", "dave"}, visibleLines(v))
}

func TestNickList_Update_clears_on_empty(t *testing.T) {
	nl := components.NewNickList(members(member("alice", plain)))

	v := nl.View(20, 10)
	require.Equal(t, []string{"Nicks", "alice"}, visibleLines(v))

	updated, _ := nl.Update(components.NickListUpdatedMsg{
		Members: domain.NewMemberList(),
	})

	v = updated.View(20, 10)
	require.Equal(t, []string{"No members"}, visibleLines(v))
}

func TestNickList_View_overflow_fits_height(t *testing.T) {
	ml := domain.NewMemberList()
	for i := range 20 {
		nick := domain.Nick(fmt.Sprintf("user%02d", i))
		ml.Add(domain.NewModelInstance(domain.InstanceID(fmt.Sprintf("inst-%02d", i)), nick, "", "", nil))
	}

	nl := components.NewNickList(ml)

	v := nl.View(20, 5)

	require.Equal(t, []string{"Nicks", "user00", "user01", "user02", "user03"}, visibleLines(v))
	require.Equal(t, 5, lipgloss.Height(v), "rendered height must match the available height")
}

func TestNickList_View_responsive(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", op),
		member("bob", voiced),
	))

	sizes := []struct{ w, h int }{
		{20, 10},
		{14, 5},
		{30, 20},
	}

	for _, sz := range sizes {
		v := nl.View(sz.w, sz.h)
		require.NotEqual(t, []string(nil), renderedLines(v), "View(%d, %d) should not be empty", sz.w, sz.h)
		require.LessOrEqual(t, lipgloss.Width(v), sz.w+1,
			"View(%d, %d) should fit width", sz.w, sz.h)
	}
}

func TestNickList_View_shows_mode_prefixes(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", op),
		member("botty", voiced),
		member("charlie", plain),
	))

	v := nl.View(20, 10)

	require.Equal(t, []string{"Nicks", "@alice", "+botty", "charlie"}, visibleLines(v))
}

func TestNickList_View_shows_thinking_indicator(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", op),
		member("botty", voiced),
		member("claude", voiced),
	))

	updated, _ := nl.Update(components.NickListThinkingMsg{
		Nicks: map[domain.Nick]bool{"botty": true, "claude": true},
	})

	v := updated.View(30, 10)
	require.Equal(t, []string{"Nicks", "@alice", "+botty …", "+claude …"}, visibleLines(v))
}

func TestNickList_View_clears_thinking_indicator(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", op),
		member("botty", voiced),
	))

	updated, _ := nl.Update(components.NickListThinkingMsg{
		Nicks: map[domain.Nick]bool{"botty": true},
	})
	updated, _ = updated.Update(components.NickListThinkingMsg{})

	v := updated.View(30, 10)
	require.Equal(t, []string{"Nicks", "@alice", "+botty"}, visibleLines(v))
}

func TestNickList_ignores_sidebar_cursor_and_activation_keys(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", op),
		member("bob", plain),
	))

	var m ui.Model = nl
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 20, Height: 10}})

	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyDown, Alt: true}, // channel sidebar's Down
		{Type: tea.KeyUp, Alt: true},   // channel sidebar's Up
		{Type: tea.KeyCtrlO},           // channel sidebar's Select
	} {
		updated, cmd := m.Update(key)
		require.Nil(t, cmd, "the nick list has no activation semantics, so %s must be a no-op", key)
		m = updated
	}

	require.Equal(t, []string{"Nicks", "@alice", "bob"}, visibleLines(m.View(20, 10)),
		"rendering must be unaffected by keys the channel sidebar uses for cursor movement")
}

func TestNickList_View_preserves_display_order(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", op),
		member("dave", voiced),
		member("zara", voiced),
		member("bob", plain),
	))

	v := nl.View(30, 10)
	require.Equal(t, []string{"Nicks", "@alice", "+dave", "+zara", "bob"}, visibleLines(v))
}
