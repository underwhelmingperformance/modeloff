package components_test

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
)

func members(ms ...domain.Member) domain.MemberList {
	ml := domain.NewMemberList()

	for _, m := range ms {
		ml.Add(m.Nick)
		ml.SetMode(m.Nick, m.Mode)
	}

	return ml
}

func member(nick string, mode domain.NickMode) domain.Member {
	return domain.Member{Nick: domain.Nick(nick), Mode: mode}
}

func TestNickList_View_shows_members(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", domain.ModeOp),
		member("charlie", domain.ModeVoice),
		member("bob", domain.ModeNone),
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
			member("eve", domain.ModeVoice),
			member("dave", domain.ModeNone),
		),
	})

	v := updated.View(20, 10)
	require.Equal(t, []string{"Nicks", "+eve", "dave"}, visibleLines(v))
}

func TestNickList_Update_clears_on_empty(t *testing.T) {
	nl := components.NewNickList(members(member("alice", domain.ModeNone)))

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
		ml.Add(domain.Nick(fmt.Sprintf("user%02d", i)))
	}

	nl := components.NewNickList(ml)

	v := nl.View(20, 5)

	require.Equal(t, []string{"Nicks", "user00", "user01", "user02", "user03"}, visibleLines(v))
	require.Equal(t, 5, lipgloss.Height(v), "rendered height must match the available height")
}

func TestNickList_View_responsive(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", domain.ModeOp),
		member("bob", domain.ModeVoice),
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
		member("alice", domain.ModeOp),
		member("botty", domain.ModeVoice),
		member("charlie", domain.ModeNone),
	))

	v := nl.View(20, 10)

	require.Equal(t, []string{"Nicks", "@alice", "+botty", "charlie"}, visibleLines(v))
}

func TestNickList_View_shows_thinking_indicator(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", domain.ModeOp),
		member("botty", domain.ModeVoice),
		member("claude", domain.ModeVoice),
	))

	updated, _ := nl.Update(components.NickListThinkingMsg{
		Nicks: map[domain.Nick]bool{"botty": true, "claude": true},
	})

	v := updated.View(30, 10)
	require.Equal(t, []string{"Nicks", "@alice", "+botty …", "+claude …"}, visibleLines(v))
}

func TestNickList_View_clears_thinking_indicator(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", domain.ModeOp),
		member("botty", domain.ModeVoice),
	))

	updated, _ := nl.Update(components.NickListThinkingMsg{
		Nicks: map[domain.Nick]bool{"botty": true},
	})
	updated, _ = updated.Update(components.NickListThinkingMsg{})

	v := updated.View(30, 10)
	require.Equal(t, []string{"Nicks", "@alice", "+botty"}, visibleLines(v))
}

func TestNickList_View_preserves_display_order(t *testing.T) {
	nl := components.NewNickList(members(
		member("alice", domain.ModeOp),
		member("dave", domain.ModeVoice),
		member("zara", domain.ModeVoice),
		member("bob", domain.ModeNone),
	))

	v := nl.View(30, 10)
	require.Equal(t, []string{"Nicks", "@alice", "+dave", "+zara", "bob"}, visibleLines(v))
}
