package components

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// PersonaSelectorState is the review as the chat-screen holds it: the
// model, the descriptions written for it, whether a description is
// being written and how long that has taken so far, whether an
// `ADDMODEL` is in flight, and the error the most recent generation or
// `ADDMODEL` failed with. What the operator is doing with it is the
// selector's own: which description they are looking at, how far they
// have scrolled it, and the adjustment they are typing.
type PersonaSelectorState struct {
	// Review identifies the review this state describes. A state naming
	// a review the selector is not already showing opens a new
	// interaction, and the adjustment, highlight and scroll offset the
	// operator had built up belong to the one it replaces.
	Review uint64

	// Model is the model being added, named in the title.
	Model domain.ModelID

	// SmallModel is the model writing the descriptions, named while a
	// request is in flight so the operator knows what they are waiting
	// for.
	SmallModel domain.ModelID

	// Candidates holds each description the review was given, in the
	// order they arrived. Which one is highlighted is the selector's
	// own: it moves under the operator's keys, and the accept and
	// re-roll messages name its index, so a state arriving
	// mid-navigation cannot move it back.
	Candidates []string

	// Generating reports a request in flight, and Elapsed how long it
	// has been running. The chat-screen re-sends the state each second
	// so the count moves; the selector reads no clock.
	Generating bool
	Elapsed    time.Duration

	// Err is the last failure, from the generation request or from the
	// `ADDMODEL` an accepted description was sent as. Accepting reports
	// that `ADDMODEL` is in flight.
	Err       error
	Accepting bool
}

// PersonaSelectorMsg gives the selector the state it is to show, and
// closes it when State is nil.
//
// Revision orders these against each other. The chat-screen dispatches
// the state alongside the work that changes it, and Bubble Tea runs a
// batch's commands concurrently, so a message describing the review
// before a change can arrive after one describing it afterwards.
// [ChatView] drops any message whose revision it has already passed.
type PersonaSelectorMsg struct {
	Revision uint64
	State    *PersonaSelectorState
}

// PersonaAcceptMsg reports that the operator accepted the highlighted
// candidate of the review it names.
type PersonaAcceptMsg struct {
	Review uint64
	Index  int
}

// PersonaRerollMsg asks for another description. Adjustment is what the
// operator typed to steer it, empty when they typed nothing, and Index
// names the candidate they are asking to move on from, which is the one
// the chat-screen records as passed over.
//
// The selector holds the highlight and puts its index in the message,
// so the decision names the candidate that produced it. Reporting each
// move as its own message would leave the chat-screen applying the move
// and the decision in whichever order the two arrived.
type PersonaRerollMsg struct {
	Review     uint64
	Adjustment string
	Index      int
}

// PersonaCancelMsg reports that the operator abandoned the review it
// names.
type PersonaCancelMsg struct{ Review uint64 }

// PersonaSelector shows the descriptions written for a model the
// operator is adding, and reports which one they accept, whether they
// want another, and whether they have given up. It is pushed as a modal
// layer on the window's stack, so it is offered every key the window
// receives and the input bar underneath it receives none.
//
// It draws over the bottom of the transcript. Its rectangle is taken
// from the transcript's area without shrinking it, so the message list
// keeps the geometry it had before the selector opened.
type PersonaSelector struct {
	state      PersonaSelectorState
	adjustment RichTextarea
	keyMap     PersonaSelectorKeyMap

	// selected is the candidate the operator is looking at. scroll is
	// the first line of that candidate to draw, for one whose wrapped
	// text is taller than the rows left for it.
	selected int
	scroll   int

	// decided is set once a decision has been emitted for the state in
	// hand, and cleared by the next one. The chat-screen drops the
	// second of two decisions, so without this which one it drops would
	// come down to the order the two commands finished in.
	decided bool

	bounds uv.Rectangle
}

// personaSelectorChrome is how many rows the full form uses for
// something other than the candidate text: the border's two, its title,
// the adjustment field and the footer hint.
const personaSelectorChrome = 5

// personaSelectorMaxRows caps how tall the selector grows, so a long
// candidate in a tall terminal still leaves the transcript readable
// behind it.
const personaSelectorMaxRows = 14

// NewPersonaSelector creates a selector showing nothing. The
// chat-screen sends it a [PersonaSelectorMsg] to open it.
func NewPersonaSelector() PersonaSelector {
	return PersonaSelector{
		keyMap: DefaultPersonaSelectorKeyMap,
		adjustment: NewRichTextarea(RichTextareaConfig{
			SingleLine: true,
			Wrap:       false,
		}),
	}
}

// Init implements ui.Component.
func (p PersonaSelector) Init() tea.Cmd {
	return nil
}

// HandleKey implements ui.KeyHandler. Every key the selector is offered
// is one it takes: the ones its keymap names act on the review, and the
// rest edit the adjustment field, which the full form draws with the
// window's only live cursor in it.
func (p PersonaSelector) HandleKey(msg tea.KeyPressMsg) (ui.KeyHandler, bool, tea.Cmd) {
	adjustment := strings.TrimSpace(p.adjustment.Value())

	switch {
	case ui.Matches(msg, p.keyMap.Cancel):
		return p.decide(PersonaCancelMsg{Review: p.state.Review})

	case ui.Matches(msg, p.keyMap.Accept):
		if adjustment != "" {
			return p.reroll(adjustment)
		}

		if len(p.state.Candidates) == 0 || p.state.Generating || p.state.Accepting {
			return p, true, nil
		}

		return p.decide(PersonaAcceptMsg{Review: p.state.Review, Index: p.selected})

	case ui.Matches(msg, p.keyMap.Reroll):
		return p.reroll(adjustment)

	case ui.Matches(msg, p.keyMap.Navigate):
		return p.navigate(msg), true, nil

	case ui.Matches(msg, p.keyMap.Scroll):
		return p.scrollBy(msg), true, nil
	}

	updated, cmd := p.adjustment.Update(msg)
	p.adjustment = updated.(RichTextarea)

	return p, true, cmd
}

// UpdateKeys implements ui.KeyHandler.
func (p PersonaSelector) UpdateKeys(msg tea.Msg) (ui.KeyHandler, tea.Cmd) {
	updated, cmd := p.Update(msg)

	return updated.(PersonaSelector), cmd
}

// Update implements ui.Component. Keys arrive through HandleKey.
func (p PersonaSelector) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		p.bounds = msg.Rect

		// A window grown until the candidate fits has nowhere left to
		// scroll to, and an offset taken while it did not would hide
		// the description's opening lines.
		p.scroll = min(p.scroll, max(
			len(p.candidateLines(p.contentWidth(p.bounds.Dx())))-p.selectedCapacity(), 0,
		))

		return p, nil

	case tea.PasteMsg:
		updated, cmd := p.adjustment.Update(msg)
		p.adjustment = updated.(RichTextarea)

		return p, cmd

	case PersonaSelectorMsg:
		if msg.State == nil {
			return p, nil
		}

		// A state naming another review opens another interaction. The
		// adjustment, highlight and scroll offset belong to the review
		// being replaced, so they go with it.
		if msg.State.Review != p.state.Review {
			p.adjustment = p.adjustment.SetPlainText("")
			p.selected = 0
			p.scroll = 0
		}

		// A description that has just arrived is the one to look at.
		// Otherwise the highlight stays where the operator left it, and
		// only follows the candidates away when there are fewer of them.
		if len(msg.State.Candidates) > len(p.state.Candidates) {
			p.selected = len(msg.State.Candidates) - 1
			p.scroll = 0
		}

		p.state = *msg.State
		p.selected = min(max(p.selected, 0), max(len(p.state.Candidates)-1, 0))
		p.decided = false

		return p, nil
	}

	return p, nil
}

// KeyBindings is what the status bar and the F1 help list for the
// selector. Enter and Tab are offered while [PersonaSelector.HandleKey]
// acts on them: not while a request is in flight, and not once a
// decision has been taken for the state in hand. Enter is offered as
// accepting alone, so an adjustment in the field withdraws it; the
// footer is what tells the operator that Enter re-rolls instead. Esc is
// offered throughout, because giving up always works.
func (p PersonaSelector) KeyBindings() []ui.KeyBinding {
	idle := p.idle()
	accepts := idle &&
		len(p.state.Candidates) > 0 &&
		strings.TrimSpace(p.adjustment.Value()) == ""

	return []ui.KeyBinding{
		ui.WithBindingEnabled(p.keyMap.Accept, accepts),
		ui.WithBindingEnabled(p.keyMap.Reroll, idle),
		ui.WithBindingEnabled(p.keyMap.Navigate, len(p.state.Candidates) > 1),
		ui.WithBindingEnabled(p.keyMap.Scroll, p.scrollable()),
		p.keyMap.Cancel,
	}
}

// idle reports whether the selector will act on a decision: not while a
// request is in flight, and not once it has taken one for the state it
// is showing.
func (p PersonaSelector) idle() bool {
	return !p.state.Generating && !p.state.Accepting && !p.decided
}

// scrollable reports whether the highlighted candidate has more lines
// than the rows it was given. The condensed form cuts the candidate to
// the width of its one row, so there are no further lines to scroll to
// there.
func (p PersonaSelector) scrollable() bool {
	if p.bounds.Dy() < personaSelectorChrome+1 {
		return false
	}

	return len(p.candidateLines(p.contentWidth(p.bounds.Dx()))) > p.selectedCapacity()
}

// selectedCapacity is the most rows the highlighted candidate can be
// given: what is left after the footer, the status line and the
// adjustment field. [PersonaSelector.bodyRows] drops collapsed entries
// to reach it, so they are not subtracted here.
func (p PersonaSelector) selectedCapacity() int {
	rows := max(p.bounds.Dy()-personaSelectorBorderRows, 0)

	tail := 0
	if rows >= 3 {
		tail++
	}
	if p.statusLine() != "" && rows >= 4 {
		tail++
	}

	return max(rows-tail-1, 1)
}

// personaSelectorBorderRows is what the pane's border and its title row
// take before anything is laid out inside it.
const personaSelectorBorderRows = 3

// Height is how many rows the selector wants, given the width it will
// be drawn at and the rows available above the input bar. Anything less
// than one row more than the chrome asks for a single row, which
// [PersonaSelector.Draw] fills with the condensed form or the
// adjustment field. With no row at all it draws nothing, which is the
// state the transcript is in too.
func (p PersonaSelector) Height(width, available int) int {
	if available <= 0 || width <= 0 {
		return 0
	}

	if available < personaSelectorChrome+1 {
		return 1
	}

	rows := personaSelectorChrome
	if p.statusLine() != "" {
		rows++
	}

	rows += max(len(p.candidateLines(p.contentWidth(width))), 1)
	rows += p.collapsedCount()

	return min(rows, min(available, personaSelectorMaxRows))
}

// collapsedCount is how many candidates other than the highlighted one
// the full form lists, one row each.
func (p PersonaSelector) collapsedCount() int {
	return max(len(p.state.Candidates)-1, 0)
}

// reroll asks for another description, with whatever steering the
// operator typed, which [PersonaSelector.HandleKey] has already read
// out of the field. The field is emptied so the text steers this
// request alone.
//
// The guard is [PersonaSelector.idle] and not the pair of state flags,
// because it has to cover a decision already emitted as well. Emptying
// the field ahead of a request `decide` goes on to refuse would throw
// away what the operator typed without sending it anywhere.
func (p PersonaSelector) reroll(adjustment string) (ui.KeyHandler, bool, tea.Cmd) {
	if !p.idle() {
		return p, true, nil
	}

	p.adjustment = p.adjustment.SetPlainText("")

	return p.decide(PersonaRerollMsg{
		Review: p.state.Review, Adjustment: adjustment, Index: p.selected,
	})
}

// decide emits one decision for the state in hand and refuses any
// second one until the chat-screen has replied with a new state. Each
// decision returns a command, and the runtime runs commands
// concurrently, so two emitted from one state would reach the
// chat-screen in no fixed order and the operator's keys would decide
// nothing.
//
// Esc goes through here too, so the accept or re-roll before it stands.
// The operator can still leave a review they have changed their mind
// about, because the state naming it as accepting or generating clears
// `decided` and an Esc against that state is emitted. The chat-screen
// returns that state from a command that only produces a message,
// while the `ADDMODEL` and the generation request each make a provider
// call, so the wait is a pass through the Update loop.
func (p PersonaSelector) decide(msg tea.Msg) (ui.KeyHandler, bool, tea.Cmd) {
	if p.decided {
		return p, true, nil
	}

	p.decided = true

	return p, true, msgCmd(msg)
}

// navigate moves the highlight. The selector holds it between one key
// and the next, and an accept or a re-roll names the index it was
// taken against, so the chat-screen never has to be told where the
// highlight went on its own.
func (p PersonaSelector) navigate(msg tea.KeyPressMsg) PersonaSelector {
	if len(p.state.Candidates) == 0 {
		return p
	}

	step := 1
	if msg.String() == "up" {
		step = -1
	}

	next := min(max(p.selected+step, 0), len(p.state.Candidates)-1)
	if next == p.selected {
		return p
	}

	p.selected = next
	p.scroll = 0

	return p
}

// scrollBy moves through a candidate too tall for the rows it was
// given. The offset stops where the candidate's last line reaches the
// bottom of those rows, so a candidate that already fits does not
// scroll at all and paging down at the end records no offset that a
// later page up would have to undo before the view moved.
func (p PersonaSelector) scrollBy(msg tea.KeyPressMsg) PersonaSelector {
	if !p.scrollable() {
		return p
	}

	step := 1
	if msg.String() == "pgup" {
		step = -1
	}

	lines := len(p.candidateLines(p.contentWidth(p.bounds.Dx())))

	p.scroll = min(max(p.scroll+step, 0), max(lines-p.selectedCapacity(), 0))

	return p
}

func (p PersonaSelector) selectedText() string {
	if p.selected < 0 || p.selected >= len(p.state.Candidates) {
		return ""
	}

	return p.state.Candidates[p.selected]
}

func (p PersonaSelector) statusLine() string {
	switch {
	case p.state.Err != nil:
		return theme.Error.Render("✗ " + p.state.Err.Error())

	case p.state.Accepting:
		return theme.Warning.Render("… adding " + string(p.state.Model))

	case p.state.Generating:
		return theme.Warning.Render(fmt.Sprintf(
			"… asking %s (%ds)", p.state.SmallModel, int(p.state.Elapsed.Seconds()),
		))
	}

	return ""
}

// footer labels the keys that decide the review. Enter and Tab appear
// only when [PersonaSelector.HandleKey] acts on them, so a request in
// flight leaves Esc alone in the footer. Moving between candidates,
// scrolling one and typing an adjustment go on working throughout.
func (p PersonaSelector) footer() string {
	if p.state.Generating {
		return theme.Dim.Render("Esc cancel")
	}

	if p.state.Accepting || p.decided {
		return theme.Dim.Render("Esc close")
	}

	if strings.TrimSpace(p.adjustment.Value()) != "" {
		return theme.Dim.Render("↵/Tab re-roll with adjustment · Esc cancel")
	}

	if len(p.state.Candidates) == 0 {
		return theme.Dim.Render("Tab re-roll · type to adjust · Esc cancel")
	}

	return theme.Dim.Render("↵ accept · Tab re-roll · type to adjust · Esc cancel")
}

func (p PersonaSelector) title(width int) string {
	name := string(p.state.Model)
	title := "persona for " + name
	if len(p.state.Candidates) > 1 {
		title = fmt.Sprintf("%s ── candidate %d of %d",
			title, p.selected+1, len(p.state.Candidates))
	}

	return truncateLine(title, width)
}

// candidateLines is the highlighted candidate wrapped to the columns a
// candidate row has, one entry per rendered row. `content` is the width
// inside the border, which is what [PersonaSelector.Draw] is given and
// what [PersonaSelector.contentWidth] derives for the callers that
// start from the selector's own width.
func (p PersonaSelector) candidateLines(content int) []string {
	text := p.selectedText()
	if text == "" {
		return nil
	}

	wrapped := lipgloss.NewStyle().
		Width(max(content-personaSelectorGutter, 1)).
		Render(text)

	return strings.Split(wrapped, "\n")
}

func (p PersonaSelector) contentWidth(width int) int {
	border := theme.PaneBorder.GetBorderLeftSize() + theme.PaneBorder.GetBorderRightSize()

	return max(width-border, 0)
}

// personaSelectorGutter is how many columns sit before a candidate's
// text, holding the highlight marker and the candidate's number. The
// highlighted row and a collapsed row reserve the same four, so their
// text starts in the same column. A hundredth candidate's number needs
// a fifth column and takes it from the text, which is cut to the row's
// width either way.
const personaSelectorGutter = 4

// condensed is the one-row form, for a window with no room for the
// bordered one, and names the keys that decide the review in the state
// it is in. [PersonaSelector.Draw] renders the adjustment field in its
// place when the field's trimmed value is not empty: a row still showing
// the candidate while an adjustment sat unseen in the field would have
// Enter asking for another description with the operator reading it as
// accepting.
//
// The candidate is cut to the row's width, so there is nothing below
// it to scroll to, and [PersonaSelector.KeyBindings] disables the
// scroll binding here.
func (p PersonaSelector) condensed(width int) string {
	text := p.selectedText()
	if text == "" {
		text = "…"
	}

	keys := p.condensedKeys()
	head := "persona for " + string(p.state.Model) + ": "

	return theme.Dim.Render(truncateLine(head+text, max(width-lipgloss.Width(keys), 1)) + keys)
}

func (p PersonaSelector) condensedKeys() string {
	switch {
	case !p.idle():
		return " Esc"
	case len(p.state.Candidates) == 0 && strings.TrimSpace(p.adjustment.Value()) == "":
		return " Tab/Esc"
	}

	return " ↵/Tab/Esc"
}

// bodyRows lays the content out into the rows it was given. Given
// three rows or more, the footer takes the last; given four or more, a
// status line takes the row above it. The adjustment field takes the
// row above those, and the candidates take what is left, the
// highlighted one last.
//
// The second result is the field's row, counted from the top of the
// content area, which [PersonaSelector.Draw] needs to place the editor
// and let it render its own cursor.
func (p PersonaSelector) bodyRows(width, rows int) ([]string, int) {
	if rows <= 0 {
		return nil, -1
	}

	var tail []string
	if rows >= 3 {
		tail = append(tail, p.footer())
	}

	if status := p.statusLine(); status != "" && rows >= 4 {
		tail = append([]string{status}, tail...)
	}

	field := max(rows-len(tail)-1, 0)
	body := make([]string, 0, rows)

	for i, text := range p.state.Candidates {
		if i == p.selected {
			continue
		}

		body = append(body, theme.Dim.Render(fmt.Sprintf(
			" %2d %s", i+1, truncateLine(text, max(width-personaSelectorGutter, 1)),
		)))
	}

	// The collapsed entries go first: the highlighted candidate is what
	// the operator is judging.
	selected := p.selectedRows(width)
	if len(body) > max(field-len(selected), 0) {
		body = body[len(body)-max(field-len(selected), 0):]
	}

	// What is left of the highlighted candidate is cut at the bottom, so
	// its first visible row is the one `scroll` names and Page Down
	// reaches the rest.
	body = append(body, selected[:min(len(selected), max(field-len(body), 0))]...)

	for len(body) < field {
		body = append(body, "")
	}

	// The field's own row is blank here. Draw writes the label and the
	// editor over it, which is what puts the cursor there.
	body = append(body, "")

	return append(body, tail...), field
}

// selectedRows renders the highlighted candidate, wrapped and scrolled.
// The index marker sits on the first row of the description, so a
// candidate scrolled past its start shows none.
func (p PersonaSelector) selectedRows(width int) []string {
	lines := p.candidateLines(width)
	if len(lines) == 0 {
		return nil
	}

	scroll := min(p.scroll, len(lines)-1)
	lines = lines[scroll:]

	rows := make([]string, 0, len(lines))
	for i, line := range lines {
		marker := "    "
		if i == 0 && scroll == 0 {
			marker = fmt.Sprintf("▌%2d ", p.selected+1)
		}

		rows = append(rows, theme.PopoverSelection.Render(marker+line))
	}

	return rows
}

func msgCmd(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
}
