package components

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
)

func showSelectorState(t *testing.T, state PersonaSelectorState) PersonaSelector {
	t.Helper()

	updated, _ := NewPersonaSelector().Update(PersonaSelectorMsg{Revision: 1, State: &state})

	return updated.(PersonaSelector)
}

// pressSelectorKey sends one key and returns the selector afterwards
// alongside every message it produced.
func pressSelectorKey(t *testing.T, p PersonaSelector, key string) (PersonaSelector, []tea.Msg) {
	t.Helper()

	updated, handled, cmd := p.HandleKey(selectorKey(key))
	require.True(t, handled, "the selector is modal and takes every key it is offered")

	return updated.(PersonaSelector), collectSelectorMsgs(cmd)
}

// selectorKey builds the key press a name stands for: a named key by
// its code, and anything else as the character it types.
func selectorKey(name string) tea.KeyPressMsg {
	switch name {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}

	return tea.KeyPressMsg{Code: rune(name[0]), Text: name}
}

func collectSelectorMsgs(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, child := range batch {
			msgs = append(msgs, collectSelectorMsgs(child)...)
		}

		return msgs
	}

	return []tea.Msg{msg}
}

// TestPersonaSelector_renders_the_candidate_over_the_transcript covers
// the property the whole arrangement rests on: the selector paints over
// the transcript's bottom rows and the rows above it keep what they
// held.
func TestPersonaSelector_renders_the_candidate_over_the_transcript(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"a terse reviewer"},
	})

	screen := uv.NewScreenBuffer(60, 12)
	drawString(screen, screen.Bounds(), strings.Repeat("scrollback\n", 12))

	area := uv.Rect(0, 5, 60, 7)
	selector.Draw(screen, area)

	rows := strings.Split(screen.Render(), "\n")

	require.Equal(t, []string{
		"scrollback", "scrollback", "scrollback", "scrollback", "scrollback",
	}, trimRows(rows[:5]))
	require.Contains(t, rows[6], "persona for vendor/model")
	require.Contains(t, strings.Join(rows[5:], "\n"), "a terse reviewer")
}

func trimRows(rows []string) []string {
	trimmed := make([]string, 0, len(rows))
	for _, row := range rows {
		trimmed = append(trimmed, strings.TrimRight(row, " "))
	}

	return trimmed
}

// TestPersonaSelector_enter_accepts_or_rerolls pins what Enter means at
// each moment: with an empty adjustment it accepts the highlighted
// description, and with one typed it asks for another, sending what
// was typed as the reason.
func TestPersonaSelector_enter_accepts_or_rerolls(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"a terse reviewer"},
	})

	_, accepted := pressSelectorKey(t, selector, "enter")
	require.Equal(t, []tea.Msg{PersonaAcceptMsg{Index: 0}}, accepted)

	typed := selector
	for _, key := range []string{"t", "e", "r", "s", "e", "r"} {
		typed, _ = pressSelectorKey(t, typed, key)
	}

	_, rerolled := pressSelectorKey(t, typed, "enter")
	require.Equal(t, []tea.Msg{PersonaRerollMsg{Adjustment: "terser"}}, rerolled)
}

// TestPersonaSelector_emits_one_decision_per_state covers the keys an
// operator can press faster than the chat-screen replies. Each decision
// returns a command and the runtime runs commands concurrently, so a
// second one would arrive in no fixed order against the first. The
// first key of each pair is the one that takes effect.
//
// The state the chat-screen replies with is what admits the next
// decision. TestPersonaSelector_esc_takes_effect_against_the_next_state
// covers Esc against that state.
func TestPersonaSelector_emits_one_decision_per_state(t *testing.T) {
	for _, tc := range []struct {
		name string
		keys []string
		want []tea.Msg
	}{
		{
			name: "enter then tab",
			keys: []string{"enter", "tab"},
			want: []tea.Msg{PersonaAcceptMsg{Index: 0}},
		},
		{
			name: "tab then enter",
			keys: []string{"tab", "enter"},
			want: []tea.Msg{PersonaRerollMsg{}},
		},
		{
			name: "enter then esc",
			keys: []string{"enter", "esc"},
			want: []tea.Msg{PersonaAcceptMsg{Index: 0}},
		},
		{
			name: "esc then enter",
			keys: []string{"esc", "enter"},
			want: []tea.Msg{PersonaCancelMsg{}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := showSelectorState(t, PersonaSelectorState{
				Model:      "vendor/model",
				Candidates: []string{"a terse reviewer"},
			})

			var emitted []tea.Msg
			for _, key := range tc.keys {
				var msgs []tea.Msg
				selector, msgs = pressSelectorKey(t, selector, key)
				emitted = append(emitted, msgs...)
			}

			require.Equal(t, tc.want, emitted)
		})
	}
}

// TestPersonaSelector_esc_takes_effect_against_the_next_state is the other
// half of the rule above: once the chat-screen has replied that the
// `ADDMODEL` is in flight, Esc takes effect and the operator leaves.
// The `ADDMODEL` itself goes on to succeed or fail on its own.
func TestPersonaSelector_esc_takes_effect_against_the_next_state(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"a terse reviewer"},
	})

	selector, accepted := pressSelectorKey(t, selector, "enter")
	require.Equal(t, []tea.Msg{PersonaAcceptMsg{Index: 0}}, accepted)

	updated, _ := selector.Update(PersonaSelectorMsg{
		Revision: 2,
		State: &PersonaSelectorState{
			Model:      "vendor/model",
			Candidates: []string{"a terse reviewer"},
			Accepting:  true,
		},
	})

	_, cancelled := pressSelectorKey(t, updated.(PersonaSelector), "esc")
	require.Equal(t, []tea.Msg{PersonaCancelMsg{}}, cancelled)
}

// TestPersonaSelector_keeps_an_adjustment_it_did_not_send covers the
// operator typing into the field between asking for a description and
// the chat-screen replying. The second request is refused, so the text
// they typed has gone nowhere and is still theirs to send.
func TestPersonaSelector_keeps_an_adjustment_it_did_not_send(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"a terse reviewer"},
	})

	selector, rerolled := pressSelectorKey(t, selector, "tab")
	require.Equal(t, []tea.Msg{PersonaRerollMsg{}}, rerolled)

	for _, key := range []string{"t", "e", "r", "s", "e", "r"} {
		selector, _ = pressSelectorKey(t, selector, key)
	}

	selector, refused := pressSelectorKey(t, selector, "enter")

	require.Equal(t, struct {
		Emitted    []tea.Msg
		Adjustment string
	}{Adjustment: "terser"}, struct {
		Emitted    []tea.Msg
		Adjustment string
	}{
		Emitted:    refused,
		Adjustment: selector.adjustment.Value(),
	})
}

// TestPersonaSelector_footer_states_what_enter_does covers the footer,
// which is where the operator reads what Enter will do before pressing
// it.
func TestPersonaSelector_footer_states_what_enter_does(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"a terse reviewer"},
	})

	empty := selector.footer()

	typed, _ := pressSelectorKey(t, selector, "x")

	require.Equal(t, struct{ Empty, Typed string }{
		Empty: "↵ accept · Tab re-roll · type to adjust · Esc cancel",
		Typed: "↵/Tab re-roll with adjustment · Esc cancel",
	}, struct{ Empty, Typed string }{
		Empty: ansi.Strip(empty),
		Typed: ansi.Strip(typed.footer()),
	})
}

// TestPersonaSelector_reports_a_running_request covers the state the
// selector opens in, before any description has arrived: it holds no
// candidate, and the status line names the small model it is waiting
// on and how long it has been running.
func TestPersonaSelector_reports_a_running_request(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		SmallModel: "small/model",
		Generating: true,
		Elapsed:    8 * time.Second,
	})

	require.Contains(t, ansi.Strip(selector.statusLine()), "asking small/model (8s)")

	_, msgs := pressSelectorKey(t, selector, "enter")
	require.Equal(t, []tea.Msg(nil), msgs)
}

// TestPersonaSelector_reports_a_failure covers a request that ended
// with an error and no description. The descriptions written before it
// stay selectable, so the operator can accept one or ask for another.
func TestPersonaSelector_reports_a_failure(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"a terse reviewer"},
		Err:        errors.New("upstream unreachable"),
	})

	require.Contains(t, ansi.Strip(selector.statusLine()), "upstream unreachable")

	_, accepted := pressSelectorKey(t, selector, "enter")
	require.Equal(t, []tea.Msg{PersonaAcceptMsg{Index: 0}}, accepted)

	_, rerolled := pressSelectorKey(t, selector, "tab")
	require.Equal(t, []tea.Msg{PersonaRerollMsg{}}, rerolled)
}

// TestPersonaSelector_navigates_the_candidates covers Up and Down over
// the descriptions written so far. The selector holds the highlight
// itself and names it on the decision it emits, so a re-roll after
// moving asks to move on from the candidate the operator is looking at.
func TestPersonaSelector_navigates_the_candidates(t *testing.T) {
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"first", "second", "third"},
	})

	once, _ := pressSelectorKey(t, selector, "up")
	twice, _ := pressSelectorKey(t, once, "up")
	pastTheStart, _ := pressSelectorKey(t, twice, "up")
	back, _ := pressSelectorKey(t, pastTheStart, "down")

	_, rerolled := pressSelectorKey(t, twice, "tab")

	require.Equal(t, struct {
		Highlights []int
		Reroll     []tea.Msg
	}{
		Highlights: []int{1, 0, 0, 1},
		Reroll:     []tea.Msg{PersonaRerollMsg{Index: 0}},
	}, struct {
		Highlights []int
		Reroll     []tea.Msg
	}{
		Highlights: []int{
			once.selected, twice.selected,
			pastTheStart.selected, back.selected,
		},
		Reroll: rerolled,
	})
}

// TestPersonaSelector_keeps_the_highlight_when_a_state_arrives covers the
// state the chat-screen sends every second while a request runs. The
// operator can be reading an earlier description meanwhile, and the
// highlight is theirs: only a description that has just arrived takes
// it.
func TestPersonaSelector_keeps_the_highlight_when_a_state_arrives(t *testing.T) {
	held := []string{"first", "second"}

	selector := showSelectorState(t, PersonaSelectorState{Candidates: held})
	moved, _ := pressSelectorKey(t, selector, "up")

	ticked, _ := moved.Update(PersonaSelectorMsg{
		Revision: 2,
		State:    &PersonaSelectorState{Candidates: held, Generating: true},
	})

	arrived, _ := ticked.(PersonaSelector).Update(PersonaSelectorMsg{
		Revision: 3,
		State:    &PersonaSelectorState{Candidates: append(held, "third")},
	})

	require.Equal(t, []int{0, 0, 2}, []int{
		moved.selected,
		ticked.(PersonaSelector).selected,
		arrived.(PersonaSelector).selected,
	})
}

// TestPersonaSelector_shows_the_start_of_a_long_candidate covers a
// description too tall for the rows it was given. What it shows is the
// beginning, and Page Down moves towards the end.
func TestPersonaSelector_shows_the_start_of_a_long_candidate(t *testing.T) {
	long := "OPENING. " + strings.Repeat("a description that keeps going. ", 10) + "CLOSING."
	selector := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{long},
	})

	area := uv.Rect(0, 0, 60, 9)
	bounded, _ := selector.Update(ui.BoundsMsg{Rect: area})
	selector = bounded.(PersonaSelector)

	first := drawSelector(selector, area)

	paged := selector
	for range 6 {
		paged, _ = pressSelectorKey(t, paged, "pgdown")
	}

	require.Equal(t, []bool{true, false, false, true}, []bool{
		strings.Contains(first, "OPENING."),
		strings.Contains(first, "CLOSING."),
		strings.Contains(drawSelector(paged, area), "OPENING."),
		strings.Contains(drawSelector(paged, area), "CLOSING."),
	})
}

func drawSelector(p PersonaSelector, area uv.Rectangle) string {
	screen := uv.NewScreenBuffer(area.Dx(), area.Dy())
	p.Draw(screen, screen.Bounds())

	return ansi.Strip(screen.Render())
}

// TestPersonaSelector_condensed_form_fits_one_row covers a window with
// no room for the bordered pane. The review is still open, so the row
// says which model it is for and lists the keys available in the state
// it is in. Once the operator types, the adjustment takes the
// row: a row still showing the candidate would leave Enter asking for
// another description while the operator read it as accepting.
func TestPersonaSelector_condensed_form_fits_one_row(t *testing.T) {
	ready := showSelectorState(t, PersonaSelectorState{
		Model:      "vendor/model",
		Candidates: []string{"a methodical database consultant who distrusts benchmarks"},
	})
	waiting := showSelectorState(t, PersonaSelectorState{
		Model: "vendor/model", Generating: true,
	})
	typed := ready
	for key := range strings.SplitSeq("an adjustment long enough to fill the row and then some more", "") {
		typed, _ = pressSelectorKey(t, typed, key)
	}

	require.Equal(t, 1, ready.Height(60, 3))

	row := uv.Rect(0, 0, 70, 1)

	require.Equal(t, []string{
		"persona for vendor/model: a methodical database consultant … ↵/Tab/Esc",
		"persona for vendor/model: … Esc",
		"adjust: ment long enough to fill the row and then some more  ↵/Tab/Esc",
	}, []string{
		strings.TrimRight(drawSelector(ready, row), " "),
		strings.TrimRight(drawSelector(waiting, row), " "),
		strings.TrimRight(drawSelector(typed, row), " "),
	})
}

// TestPersonaSelector_bindings_follow_what_the_key_does pins the
// advertised set against the state, in each of the states the selector
// passes through: waiting on the first description, holding one,
// steering the next, waiting on the `ADDMODEL` for an accepted one, and
// having just asked for something. A key the selector discards is not
// offered, and Esc is offered throughout, because giving up is the one
// thing that always works.
func TestPersonaSelector_bindings_follow_what_the_key_does(t *testing.T) {
	held := []string{"a terse reviewer"}

	generating := showSelectorState(t, PersonaSelectorState{
		Candidates: held, Generating: true,
	})
	accepting := showSelectorState(t, PersonaSelectorState{
		Candidates: held, Accepting: true,
	})
	ready := showSelectorState(t, PersonaSelectorState{Candidates: held})
	typed, _ := pressSelectorKey(t, ready, "x")
	decided, _ := pressSelectorKey(t, ready, "tab")

	require.Equal(t, []offeredBindings{
		{Cancel: true},
		{Cancel: true},
		{Accept: true, Reroll: true, Cancel: true},
		{Reroll: true, Cancel: true},
		{Cancel: true},
	}, []offeredBindings{
		bindingsOffered(generating),
		bindingsOffered(accepting),
		bindingsOffered(ready),
		bindingsOffered(typed),
		bindingsOffered(decided),
	})
}

type offeredBindings struct {
	Accept bool
	Reroll bool
	Scroll bool
	Cancel bool
}

func bindingsOffered(p PersonaSelector) offeredBindings {
	bindings := p.KeyBindings()

	return offeredBindings{
		Accept: enabledBinding(bindings, "accept persona"),
		Reroll: enabledBinding(bindings, "write another"),
		Scroll: enabledBinding(bindings, "scroll the candidate"),
		Cancel: enabledBinding(bindings, "close the review"),
	}
}

// TestPersonaSelector_offers_scrolling_only_where_it_moves covers the
// scroll binding against the candidate in view. Only the full form
// wraps a description, so only there can one run past the rows it was
// given, and Page Down is offered exactly then. A description short
// enough to fit those rows has nowhere to scroll to, and the condensed
// form cuts its candidate to one row, so there is nothing below it.
func TestPersonaSelector_offers_scrolling_only_where_it_moves(t *testing.T) {
	long := strings.Repeat("a description that keeps going. ", 10)

	fits := boundedSelector(t, PersonaSelectorState{Candidates: []string{"short"}},
		uv.Rect(0, 0, 60, 10))
	overflows := boundedSelector(t, PersonaSelectorState{Candidates: []string{long}},
		uv.Rect(0, 0, 60, 10))
	condensed := boundedSelector(t, PersonaSelectorState{Candidates: []string{long}},
		uv.Rect(0, 0, 60, 1))

	// A key that moves nothing must record no offset. Growing the window
	// to a size the candidate still overflows would otherwise draw it
	// from that offset, so the operator would find it already scrolled
	// past its first line.
	pagedCondensed, _ := pressSelectorKey(t, condensed, "pgdown")
	pagedOverflow, _ := pressSelectorKey(t, overflows, "pgdown")

	// An offset taken while the candidate overflowed is cleared when the
	// window grows enough to hold it.
	grown, _ := pagedOverflow.Update(ui.BoundsMsg{Rect: uv.Rect(0, 0, 60, 30)})

	require.Equal(t, struct {
		Offered []bool
		Offsets []int
	}{
		Offered: []bool{false, true, false},
		Offsets: []int{0, 1, 0},
	}, struct {
		Offered []bool
		Offsets []int
	}{
		Offered: []bool{
			enabledBinding(fits.KeyBindings(), "scroll the candidate"),
			enabledBinding(overflows.KeyBindings(), "scroll the candidate"),
			enabledBinding(condensed.KeyBindings(), "scroll the candidate"),
		},
		Offsets: []int{
			pagedCondensed.scroll,
			pagedOverflow.scroll,
			grown.(PersonaSelector).scroll,
		},
	})
}

func boundedSelector(
	t *testing.T, state PersonaSelectorState, area uv.Rectangle,
) PersonaSelector {
	t.Helper()

	bounded, _ := showSelectorState(t, state).Update(ui.BoundsMsg{Rect: area})

	return bounded.(PersonaSelector)
}

func enabledBinding(bindings []ui.KeyBinding, help string) bool {
	for _, binding := range bindings {
		if binding.Help().Desc == help {
			return binding.Enabled()
		}
	}

	return false
}
