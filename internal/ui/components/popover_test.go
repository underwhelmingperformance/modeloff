package components_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/ui"
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

	_, handled, cmd := p.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.True(t, handled, "Enter must accept the highlighted suggestion when accepting it would change the typed text")
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

	_, handled, cmd := p.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.False(t, handled, "a fully-typed command must submit on the first Enter, not require a second")
	require.Nil(t, cmd)
}

func TestPopover_optional_continuation_leaves_Enter_for_submission(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{
		Visible:      true,
		EnterSubmits: true,
		ReplaceStart: 36,
		ReplaceEnd:   36,
		AppendSpace:  true,
		Suggestions: []command.Suggestion{
			{Value: "--persona", Label: "--persona"},
		},
	})

	_, handled, cmd := p.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	require.False(t, handled)
	require.Nil(t, cmd)

	_, handled, cmd = p.HandleKey(tea.KeyPressMsg{Code: tea.KeyTab})

	require.True(t, handled)
	require.NotNil(t, cmd)
	require.Equal(t, components.PopoverAcceptMsg{
		ReplaceStart: 36,
		ReplaceEnd:   36,
		Replacement:  "--persona ",
	}, cmd())
}

func TestPopover_UpDown_cycle_when_multiple_suggestions(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{
		Visible: true,
		Suggestions: []command.Suggestion{
			{Value: "/join", Label: "/join"},
			{Value: "/jobs", Label: "/jobs"},
		},
	})

	next, handled, _ := p.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})

	require.True(t, handled)

	// Accepting now yields the second suggestion, which shows the key
	// moved the selection and was not merely swallowed.
	_, _, cmd := next.HandleKey(tea.KeyPressMsg{Code: tea.KeyTab})

	require.NotNil(t, cmd)
	require.Equal(t, components.PopoverAcceptMsg{Replacement: "/jobs"}, cmd())
}

func TestPopover_short_bounds_keep_the_selection_visible(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{
		Visible: true,
		Suggestions: []command.Suggestion{
			{Value: "first", Label: "first"},
			{Value: "second", Label: "second"},
			{Value: "third", Label: "third"},
		},
	})

	updated, _ := p.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 20, 1)})
	p = updated.(components.Popover)
	moved, _, _ := p.HandleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	p = moved.(components.Popover)

	require.Equal(t, []string{"second"}, visibleLines(renderToBuffer(p, 20, 1)))

	_, _, cmd := p.HandleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	require.NotNil(t, cmd)
	require.Equal(t, components.PopoverAcceptMsg{Replacement: "second"}, cmd())
}

// TestPopover_claims_the_arrows_only_with_suggestions_to_cycle pins which
// keys the popover declines. Up and Down cycle suggestions, so a
// hidden popover, one with no suggestions and one with a single
// suggestion all leave the key to the input bar, where it browses
// input history.
func TestPopover_claims_the_arrows_only_with_suggestions_to_cycle(t *testing.T) {
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

			for _, code := range []rune{tea.KeyUp, tea.KeyDown} {
				_, handled, _ := p.HandleKey(tea.KeyPressMsg{Code: code})

				require.Equal(t, tc.want, handled)
			}
		})
	}
}

func TestPopover_Esc_dismisses_with_or_without_suggestions(t *testing.T) {
	p := newVisiblePopover(t, command.Completion{Visible: true})

	next, handled, _ := p.HandleKey(tea.KeyPressMsg{Code: tea.KeyEsc})

	require.True(t, handled)
	require.False(t, next.(components.Popover).IsVisible())
}
