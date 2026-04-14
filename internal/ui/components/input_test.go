package components_test

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

func typeText(t *testing.T, m ui.Model, text string) ui.Model {
	t.Helper()

	for _, r := range text {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	return m
}

func enter(t *testing.T, m ui.Model) (ui.Model, tea.Cmd) {
	t.Helper()

	return m.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

// viewText renders the InputBar and strips ANSI escape sequences so
// tests can assert on visible text without coupling to styling.
func viewText(m ui.Model) string {
	return ansi.Strip(m.View(80, 1))
}

func TestInputBar_type_and_submit_message(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "hello world")
	m, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.MessageSubmitMsg{Text: "hello world"}, msg)

	// Buffer should be cleared after submit.
	v := viewText(m)
	require.NotContains(t, v, "hello world")
}

func TestInputBar_submit_command(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "/join #general")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	require.Equal(t, components.CommandSubmitMsg{Raw: "/join #general"}, msg)
}

func TestInputBar_submit_command_no_args(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "/list")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)

	msg := cmd()
	sub := msg.(components.CommandSubmitMsg)
	require.Equal(t, "/list", sub.Raw)
}

func TestInputBar_submit_rich_message_as_irc_formatting(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}, Alt: true})
	m = typeText(t, m, "bold")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)
	require.Equal(t, components.MessageSubmitMsg{Text: "\x02bold\x0f"}, cmd())
}

func TestInputBar_space_key_inserts_space(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m = typeText(t, m, "world")

	require.Contains(t, viewText(m), "hello world")
}

func TestInputBar_empty_submit_does_nothing(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	_, cmd := enter(t, m)
	require.Nil(t, cmd)

	// Whitespace-only should also be ignored.
	m = typeText(t, m, "   ")
	_, cmd = enter(t, m)
	require.Nil(t, cmd)
}

func TestInputBar_backspace(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "abc")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})

	v := viewText(m)
	require.Contains(t, v, "ab")
	require.NotContains(t, v, "abc")
}

func TestInputBar_cursor_movement(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "abcd")

	// Move left twice.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyLeft})

	// Type at cursor position.
	m = typeText(t, m, "X")

	require.Contains(t, viewText(m), "abXcd")
}

func TestInputBar_home_end(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "hello")

	// Home, then type.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = typeText(t, m, "X")

	require.Contains(t, viewText(m), "Xhello")

	// End, then type.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	m = typeText(t, m, "Y")

	require.Contains(t, viewText(m), "XhelloY")
}

func TestInputBar_ctrl_u_does_not_kill_to_start(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "abcde")

	// Ctrl+U is reserved for sidebar navigation and should not
	// modify the input buffer.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})

	require.Contains(t, viewText(m), "abcde")
}

func TestInputBar_ctrl_k_kills_to_end(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "abcde")

	// Move to position 2, then ctrl-k kills to end.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlK})

	v := viewText(m)
	require.Contains(t, v, "ab")
	require.NotContains(t, v, "abcde")
}

func TestInputBar_delete_key(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "abc")

	// Move to start, delete the first character.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDelete})

	v := viewText(m)
	require.Contains(t, v, "bc")
	require.NotContains(t, v, "abc")
}

func TestInputBar_View_contains_prompt(t *testing.T) {
	b := components.NewInputBar("")
	v := b.View(40, 1)

	require.Contains(t, v, ">")
}

func TestInputBar_View_includes_user_nick_and_fits_width(t *testing.T) {
	b := components.NewInputBar("testuser")

	v := b.View(20, 1)

	require.Contains(t, v, "testuser")
	require.Contains(t, v, ">")
	require.LessOrEqual(t, lipgloss.Width(v), 20)
}

func TestInputBar_set_cursor_from_cell(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "hello")
	b = m.(components.InputBar).SetCursorFromCell(4)
	require.Equal(t, 1, b.Cursor())
	m = b
	m = typeText(t, m, "X")

	require.Equal(t, "hXello", m.(components.InputBar).Value())
}

func TestInputBar_history_up_down(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	// Submit three messages.
	m = typeText(t, m, "first")
	m, _ = enter(t, m)
	m = typeText(t, m, "second")
	m, _ = enter(t, m)
	m = typeText(t, m, "third")
	m, _ = enter(t, m)

	// Up once = most recent.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "third")

	// Up again = previous.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "second")

	// Down = back to most recent.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	require.Contains(t, viewText(m), "third")

	// Down again = back to empty draft.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	v := viewText(m)
	require.NotContains(t, v, "third")
	require.NotContains(t, v, "second")
	require.NotContains(t, v, "first")
}

func TestInputBar_history_preserves_draft(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "old message")
	m, _ = enter(t, m)

	// Start typing a new message.
	m = typeText(t, m, "draft")

	// Up enters history, saving the draft.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "old message")

	// Down restores the draft.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	require.Contains(t, viewText(m), "draft")
}

func TestInputBar_history_no_duplicate_consecutive(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "same")
	m, _ = enter(t, m)
	m = typeText(t, m, "same")
	m, _ = enter(t, m)

	// Up once = "same", up again should stay (only one entry).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "same")

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "same")
}

func TestInputBar_history_up_with_no_history(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	// Up with no history should do nothing.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})

	v := viewText(m)
	require.Contains(t, v, ">")
}

func TestInputBar_history_ring_buffer_overflow(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	// Submit 51 messages (one more than historySize of 50).
	for i := 0; i <= 50; i++ {
		m = typeText(t, m, fmt.Sprintf("msg %d", i))
		m, _ = enter(t, m)
	}

	// Up once = most recent (msg 50).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "msg 50")

	// Navigate all the way up: 49 more presses to reach the oldest.
	for range 49 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	}

	// Oldest should be msg 1, not msg 0 (evicted by overflow).
	v := viewText(m)
	require.Contains(t, v, "msg 1")
	require.NotContains(t, v, "msg 0")

	// One more up should stay at the oldest.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "msg 1")
}

func TestInputBar_ctrl_a_moves_to_start(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = typeText(t, m, "X")

	require.Contains(t, viewText(m), "Xhello")
}

func TestInputBar_ctrl_e_moves_to_end(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "hello")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m = typeText(t, m, "X")

	require.Contains(t, viewText(m), "helloX")
}

func TestInputBar_ctrl_w_deletes_word_backward(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "hello world")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})

	v := viewText(m)
	require.Contains(t, v, "hello")
	require.NotContains(t, v, "world")
}

func TestInputBar_editing_shortcuts_work_after_history_recall(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "first message")
	m, _ = enter(t, m)

	// Recall from history.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	require.Contains(t, viewText(m), "first message")

	// Ctrl+A to start, type prefix.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	m = typeText(t, m, "re: ")

	require.Contains(t, viewText(m), "re: first message")

	// Ctrl+W to delete last word.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlW})

	v := viewText(m)
	require.Contains(t, v, "re: first")
	require.NotContains(t, v, "re: first message")
}

func TestInputBar_ctrl_d_does_not_delete_forward(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "abcde")

	// Move to start, then Ctrl+D — reserved for sidebar navigation
	// and should not delete the character under the cursor.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})

	require.Contains(t, viewText(m), "abcde")
}

func TestInputBar_history_preserves_rich_formatting(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}, Alt: true})
	m = typeText(t, m, "bold")
	m, _ = enter(t, m)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)
	require.Equal(t, components.MessageSubmitMsg{Text: "\x02bold\x0f"}, cmd())
}

func TestInputBar_command_mode_disables_formatting_shortcuts(t *testing.T) {
	b := components.NewInputBar()
	var m ui.Model = b

	m = typeText(t, m, "/join ")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}, Alt: true})
	m = typeText(t, m, "#general")
	_, cmd := enter(t, m)

	require.NotNil(t, cmd)
	require.Equal(t, components.CommandSubmitMsg{Raw: "/join #general"}, cmd())
}

func TestInputBar_keybindings_include_rich_shortcuts(t *testing.T) {
	b := components.NewInputBar()

	bindings := keyMapByHelp(b.KeyBindings())
	require.Contains(t, bindings, "M-B\x00bold")
	require.Contains(t, bindings, "^←\x00word ←")
	require.Contains(t, bindings, "^W\x00del word")

	b = typeText(t, b, "/join").(components.InputBar)
	bindings = keyMapByHelp(b.KeyBindings())
	require.NotContains(t, bindings, "M-B\x00bold")
}

func keyMapByHelp(bindings []ui.KeyBinding) map[string]struct{} {
	index := map[string]struct{}{}
	for _, binding := range bindings {
		help := binding.Help()
		index[help.Key+"\x00"+help.Desc] = struct{}{}
	}

	return index
}

func TestInputBar_ignores_non_key_messages(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m, cmd := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	require.Nil(t, cmd)

	v := viewText(m)
	require.Contains(t, v, ">")
}

func inputBarWithNicks(nicks []domain.Nick) ui.Model {
	members := domain.NewMemberList()

	for _, n := range nicks {
		members.Add(n)
	}

	var m ui.Model = components.NewInputBar("")
	m, _ = m.Update(components.NickListUpdatedMsg{Members: members})

	return m
}

func tabKey() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyTab}
}

func TestInputBar_nick_completion(t *testing.T) {
	nicks := []domain.Nick{"alice", "bob", "charlie"}

	tests := []struct {
		name     string
		input    string
		tabs     int
		wantText string
	}{
		{
			name:     "complete at start of line appends colon",
			input:    "al",
			tabs:     1,
			wantText: "alice: ",
		},
		{
			name:     "complete mid-line appends space",
			input:    "hey al",
			tabs:     1,
			wantText: "hey alice ",
		},
		{
			name:     "cycle through matches",
			input:    "b",
			tabs:     1,
			wantText: "bob: ",
		},
		{
			name:     "no match leaves input unchanged",
			input:    "z",
			tabs:     1,
			wantText: "z",
		},
		{
			name:     "empty prefix does nothing",
			input:    "",
			tabs:     1,
			wantText: "> ",
		},
		{
			name:     "case insensitive match",
			input:    "AL",
			tabs:     1,
			wantText: "alice: ",
		},
		{
			name:     "tab cycles through multiple matches",
			input:    "c",
			tabs:     1,
			wantText: "charlie: ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := inputBarWithNicks(nicks)

			m = typeText(t, m, tt.input)

			for range tt.tabs {
				m, _ = m.Update(tabKey())
			}

			require.Contains(t, viewText(m), tt.wantText)
		})
	}
}

func TestInputBar_nick_completion_cycles(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex", "bob"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "al")

	// First Tab: alex.
	m, _ = m.Update(tabKey())
	require.Contains(t, viewText(m), "alex: ")

	// Second Tab: alice.
	m, _ = m.Update(tabKey())
	require.Contains(t, viewText(m), "alice: ")

	// Third Tab: wraps back to alex.
	m, _ = m.Update(tabKey())
	require.Contains(t, viewText(m), "alex: ")
}

func TestInputBar_nick_completion_resets_on_other_key(t *testing.T) {
	nicks := []domain.Nick{"alice", "alex"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "al")

	m, _ = m.Update(tabKey())
	require.Contains(t, viewText(m), "alex: ")

	// Typing resets completion state.
	m = typeText(t, m, "h")
	require.Contains(t, viewText(m), "alex: h")

	// Tab again starts a new completion on "h" (no match).
	m, _ = m.Update(tabKey())
	require.Contains(t, viewText(m), "alex: h")
}

func TestInputBar_nick_completion_skipped_in_command_mode(t *testing.T) {
	nicks := []domain.Nick{"alice"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "/join al")

	m, _ = m.Update(tabKey())

	// Tab in command mode should not perform nick completion.
	require.NotContains(t, viewText(m), "alice: ")
}

func TestInputBar_nick_completion_no_nicks(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m = typeText(t, m, "al")
	m, _ = m.Update(tabKey())

	v := viewText(m)
	require.Contains(t, v, "al")
	require.NotContains(t, v, "alice")
}

func TestInputBar_nick_completion_mid_line_with_trailing_text(t *testing.T) {
	nicks := []domain.Nick{"alice"}
	m := inputBarWithNicks(nicks)

	m = typeText(t, m, "hey al please")

	// Move cursor to just after "al" (position 6).
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	for range 6 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	}

	m, _ = m.Update(tabKey())

	require.Contains(t, viewText(m), "hey alice please")
}

func TestInputBar_UserNickMsg_updates_nick_in_view(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	m, _ = m.Update(components.UserNickMsg{Nick: "oldnick"})
	require.Contains(t, viewText(m), "oldnick")

	m, _ = m.Update(components.UserNickMsg{Nick: "newnick"})

	v := viewText(m)
	require.Contains(t, v, "newnick")
	require.NotContains(t, v, "oldnick")
}

func TestInputBar_NickListUpdatedMsg_enables_nick_completion(t *testing.T) {
	var m ui.Model = components.NewInputBar("")

	members := domain.NewMemberList()
	members.Add("alice")
	members.Add("bob")

	m, _ = m.Update(components.NickListUpdatedMsg{Members: members})
	m = typeText(t, m, "al")
	m, _ = m.Update(tabKey())

	require.Contains(t, viewText(m), "alice: ")
}

// inputBarKind is a minimal KindProvider for InputBar popover tests.
type inputBarKind domain.ChannelKind

func (k inputBarKind) ChannelKind() domain.ChannelKind { return domain.ChannelKind(k) }

const inputBarKindChannel = inputBarKind(domain.KindChannel)

func inputBarWithPopover(nodes []*command.Node) ui.Model {
	var m ui.Model = components.NewInputBar("testuser")

	m, _ = m.Update(components.CommandStateMsg{
		Commands:  nodes,
		Completer: command.CompletionSet[inputBarKind]{Set: command.Set{Commands: nodes}, Ctx: inputBarKindChannel},
	})
	m, _ = m.Update(ui.BoundsMsg{Rect: ui.Rect{X: 0, Y: 0, Width: 60, Height: 24}})

	return m
}

func TestInputBar_popover_shows_completions(t *testing.T) {
	nodes := []*command.Node{
		{Name: "join", Help: "Join a channel"},
		{Name: "part", Help: "Leave a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/")

	v := ansi.Strip(m.View(60, 3))
	require.Contains(t, v, "/join")
	require.Contains(t, v, "Join a channel")
	require.Contains(t, v, "/part")
}

func TestInputBar_popover_tab_accepts(t *testing.T) {
	nodes := []*command.Node{
		{Name: "join", Help: "Join a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/jo")

	var cmd tea.Cmd
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyTab})

	require.NotNil(t, cmd, "Tab should produce a cmd")
	m, _ = m.Update(cmd())

	m = typeText(t, m, "#general")
	_, cmd = enter(t, m)

	require.NotNil(t, cmd)
	sub := cmd().(components.CommandSubmitMsg)
	require.Equal(t, "/join #general", sub.Raw)
}

func TestInputBar_popover_dismiss_on_esc(t *testing.T) {
	nodes := []*command.Node{
		{Name: "join", Help: "Join a channel"},
		{Name: "part", Help: "Leave a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/")

	// Popover should be showing completions.
	v := ansi.Strip(m.View(60, 3))
	require.Contains(t, v, "/join")

	// Esc should dismiss the popover.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	v = ansi.Strip(m.View(60, 3))
	require.NotContains(t, v, "Join a channel")
}

func TestInputBar_keybindings_include_popover_when_visible(t *testing.T) {
	nodes := []*command.Node{
		{Name: "join", Help: "Join a channel"},
	}
	m := inputBarWithPopover(nodes)

	m = typeText(t, m, "/")

	bar := m.(components.InputBar)
	bindings := bar.KeyBindings()

	var helpTexts []string
	for _, b := range bindings {
		helpTexts = append(helpTexts, b.Help().Desc)
	}

	require.Contains(t, helpTexts, "accept")
	require.Contains(t, helpTexts, "dismiss")
}

func TestInputBar_keybindings_include_history_when_popover_hidden(t *testing.T) {
	var m ui.Model = components.NewInputBar("testuser")

	// Submit a message to populate history.
	m = typeText(t, m, "something")
	m, _ = enter(t, m)

	bar := m.(components.InputBar)
	bindings := bar.KeyBindings()

	var helpTexts []string
	for _, b := range bindings {
		helpTexts = append(helpTexts, b.Help().Desc)
	}

	require.Contains(t, helpTexts, "history")
	require.NotContains(t, helpTexts, "accept")
	require.NotContains(t, helpTexts, "dismiss")
}

func TestInputBar_view_does_not_show_plain_indicator(t *testing.T) {
	b := components.NewInputBar("user")
	v := viewText(b)

	require.NotContains(t, v, "[plain]")
}

func TestInputBar_active_formats(t *testing.T) {
	tests := []struct {
		name   string
		toggle rune
		want   components.ActiveFormats
	}{
		{
			name:   "no formatting active by default",
			toggle: 0,
			want:   components.ActiveFormats{},
		},
		{
			name:   "bold active after toggle",
			toggle: 'b',
			want:   components.ActiveFormats{Bold: true},
		},
		{
			name:   "italic active after toggle",
			toggle: 'i',
			want:   components.ActiveFormats{Italic: true},
		},
		{
			name:   "underline active after toggle",
			toggle: 'u',
			want:   components.ActiveFormats{Underline: true},
		},
		{
			name:   "reverse active after toggle",
			toggle: 'r',
			want:   components.ActiveFormats{Reverse: true},
		},
		{
			name:   "strikethrough active after toggle",
			toggle: 's',
			want:   components.ActiveFormats{Strike: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m ui.Model = components.NewInputBar()

			if tt.toggle != 0 {
				m, _ = m.Update(tea.KeyMsg{
					Type:  tea.KeyRunes,
					Runes: []rune{tt.toggle},
					Alt:   true,
				})
			}

			bar := m.(components.InputBar)
			require.Equal(t, tt.want, bar.ActiveFormats())
		})
	}
}

func TestInputBar_status_bar_renders_active_format_bold(t *testing.T) {
	// Force colour output so ANSI escapes are emitted.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI)
	t.Cleanup(func() { lipgloss.SetColorProfile(prev) })

	var m ui.Model = components.NewInputBar("user")

	// Toggle bold formatting.
	m, _ = m.Update(tea.KeyMsg{
		Type:  tea.KeyRunes,
		Runes: []rune{'b'},
		Alt:   true,
	})

	bar := m.(components.InputBar)
	bindings := bar.KeyBindings()

	// The status bar should render without [plain] and with the
	// bold binding rendered in bold (ANSI bold escape).
	rendered := components.RenderStatusBar(200, bindings, nil)

	// The bold binding should be rendered with ANSI bold (SGR 1)
	// applied directly to "M-B" and its description.
	require.Contains(t, rendered, "\x1b[1;90mM-B")
	require.Contains(t, rendered, "\x1b[1;90mbold")
}
