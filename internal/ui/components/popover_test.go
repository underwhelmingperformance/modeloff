package components_test

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/ui/components"
)

// fakeCompleter returns a fixed command.Completion regardless of the
// input it's asked to complete, letting tests drive Popover through
// exact, hand-built suggestion states.
type fakeCompleter struct {
	completion command.Completion
}

func (f fakeCompleter) Complete(string, int) command.Completion {
	return f.completion
}

func newVisiblePopover(t *testing.T, completion command.Completion) components.Popover {
	t.Helper()

	p := components.NewPopover()

	updated, _ := p.Update(components.PopoverApplyMsg{
		Completer: fakeCompleter{completion: completion},
		Raw:       "/jo",
		Cursor:    3,
	})

	return updated.(components.Popover)
}

func TestPopover_Enter_accepts_when_it_would_change_the_input(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{
		Visible:      true,
		TypedPrefix:  "/jo",
		ReplaceStart: 0,
		ReplaceEnd:   3,
		AppendSpace:  true,
		Suggestions: []command.Suggestion{
			{Value: "/join", Label: "/join"},
		},
	})

	updated, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next := updated.(components.Popover)

	require.True(t, next.Handled(), "Enter must accept the highlighted suggestion when accepting it would change the typed text")
	require.NotNil(t, cmd)
	require.Equal(t, components.PopoverAcceptMsg{ReplaceStart: 0, ReplaceEnd: 3, Replacement: "/join "}, cmd())
}

func TestPopover_Enter_falls_through_when_typed_text_already_matches(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{
		Visible:      true,
		TypedPrefix:  "/join",
		ReplaceStart: 0,
		ReplaceEnd:   5,
		Suggestions: []command.Suggestion{
			{Value: "/join", Label: "/join"},
		},
	})

	updated, cmd := p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next := updated.(components.Popover)

	require.False(t, next.Handled(), "a fully-typed command must submit on the first Enter, not require a second")
	require.Nil(t, cmd)
}

func TestPopover_UpDown_cycle_when_multiple_suggestions(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{
		Visible: true,
		Suggestions: []command.Suggestion{
			{Value: "/join", Label: "/join"},
			{Value: "/jobs", Label: "/jobs"},
		},
	})

	updated, _ := p.Update(tea.KeyMsg{Type: tea.KeyDown})
	next := updated.(components.Popover)

	require.True(t, next.Handled())
	require.True(t, next.BlocksHistory())
}

func TestPopover_UpDown_fall_through_with_at_most_one_suggestion(t *testing.T) {
	cases := []struct {
		name        string
		suggestions []command.Suggestion
	}{
		{name: "no suggestions", suggestions: nil},
		{name: "one suggestion", suggestions: []command.Suggestion{{Value: "/join", Label: "/join"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newVisiblePopover(t, command.Completion{Visible: true, Suggestions: tc.suggestions})

			updated, cmd := p.Update(tea.KeyMsg{Type: tea.KeyDown})
			next := updated.(components.Popover)

			require.False(t, next.Handled(), "with at most one suggestion, Up/Down must reach input history instead")
			require.Nil(t, cmd)
			require.False(t, next.BlocksHistory())
		})
	}
}

func TestPopover_BlocksHistory(t *testing.T) {
	cases := []struct {
		name        string
		visible     bool
		suggestions []command.Suggestion
		want        bool
	}{
		{name: "hidden", visible: false, want: false},
		{name: "visible, no suggestions", visible: true, want: false},
		{
			name:        "visible, one suggestion",
			visible:     true,
			suggestions: []command.Suggestion{{Value: "/join"}},
			want:        false,
		},
		{
			name:    "visible, multiple suggestions",
			visible: true,
			suggestions: []command.Suggestion{
				{Value: "/join"}, {Value: "/jobs"},
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newVisiblePopover(t, command.Completion{Visible: tc.visible, Suggestions: tc.suggestions})

			require.Equal(t, tc.want, p.BlocksHistory())
		})
	}
}

func TestPopover_Esc_dismisses_with_or_without_suggestions(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{Visible: true})

	updated, _ := p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	next := updated.(components.Popover)

	require.True(t, next.Handled())
	require.False(t, next.IsVisible())
}
