package components

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	uvlayout "github.com/charmbracelet/ultraviolet/layout"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ircfmt"
	"github.com/laney/modeloff/internal/richtext"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/clipboard"
	"github.com/laney/modeloff/internal/ui/theme"
)

// ActiveFormats reports which formatting styles are currently active
// at the cursor position in the input bar.
type ActiveFormats struct {
	Bold      bool
	Italic    bool
	Underline bool
	Reverse   bool
	Strike    bool
}

// MessageSubmitMsg is emitted when the user presses Enter with a
// non-command message.
type MessageSubmitMsg struct {
	Text string
}

// CommandSubmitMsg is emitted when the user presses Enter with a
// message starting with "/".
type CommandSubmitMsg struct {
	Raw string
}

// InputLockedMsg toggles the input bar's locked state. While locked,
// keyboard input and submissions are ignored. Used while the client
// is shutting down to prevent the user typing into a UI that is
// about to disappear.
type InputLockedMsg struct {
	Locked bool
}

// historySize is the maximum number of entries kept in the input
// history ring buffer.
const historySize = 50

// nickCompletion holds the transient state for Tab-cycling through
// nick matches.
type nickCompletion struct {
	active  bool
	prefix  string
	start   int
	end     int
	matches []domain.Nick
	index   int
}

// InputBar wraps the rich composer with command completion, nick
// completion, and input history.
type InputBar struct {
	input    RichTextarea
	keyMap   InputBarKeyMap
	userNick domain.Nick
	bounds   uv.Rectangle

	history         []string
	histPos         int // -1 = editing new input, 0..len(history)-1 = browsing
	histDraft       string
	histDraftCursor int

	nicks            []domain.Nick
	nickComp         nickCompletion
	nickListRevision uint64

	locked bool

	// blurred hides the cursor without the badge `locked` renders. A
	// modal layer over this window is taking the keys, so nothing the
	// operator types reaches the bar and a cursor on it would say
	// otherwise.
	blurred bool

	// secretChecker reports whether a raw line carries a credential,
	// so pushHistory can keep it out of ↑ recall. Set once from
	// SecretCheckerMsg; nil until then, which pushHistory reads as
	// "nothing to exclude yet" rather than refusing every line.
	secretChecker command.SecretChecker

	// pasteFlattened is set when the most recent key was a paste that
	// contained a newline, which SingleLine flattens to one line. It
	// is cleared by any other key, so the hint reflects only the
	// paste that just happened.
	pasteFlattened bool
}

type inputBarLayout struct {
	palette uv.Rectangle
	note    uv.Rectangle
	input   uv.Rectangle
	editor  uv.Rectangle
}

// NewInputBar creates an input bar with an optional user nick. When
// called with no arguments, the nick is left empty (set later via
// UserNickMsg). A single argument sets the initial nick.
func NewInputBar(nick ...domain.Nick) InputBar {
	editor := NewRichTextarea(RichTextareaConfig{
		SingleLine:      true,
		Wrap:            false,
		AllowFormatting: true,
	})

	b := InputBar{
		input:   editor,
		keyMap:  DefaultInputBarKeyMap,
		histPos: -1,
	}

	if len(nick) > 0 {
		b.userNick = nick[0]
	}

	return b
}

// Init implements ui.Component.
func (b InputBar) Init() tea.Cmd {
	return b.input.Init()
}

// Update implements ui.Component.
func (b InputBar) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	switch msg := msg.(type) {
	case InputLockedMsg:
		b.locked = msg.Locked
		return b, nil

	case UserNickMsg:
		b.userNick = msg.Nick
		return b, nil

	case NickListUpdatedMsg:
		if msg.Revision < b.nickListRevision {
			return b, nil
		}

		b.nickListRevision = msg.Revision
		b.nicks = slices.Collect(msg.Members.Nicks())
		return b, nil

	case SecretCheckerMsg:
		b.secretChecker = msg.Checker
		return b, nil

	case PopoverAcceptMsg:
		return b.ReplaceRange(msg.ReplaceStart, msg.ReplaceEnd, msg.Replacement), nil

	case ui.BoundsMsg:
		b.bounds = msg.Rect
		return b.updateChildBounds()

	case tea.MouseMsg:
		if updated, handled, cmd := b.handleMouse(msg); handled {
			return updated, cmd
		}

		return b, nil

	case tea.PasteMsg:
		if b.locked {
			return b, nil
		}

		b.pasteFlattened = containsNewline(msg.Content)
		updated, cmd := b.input.Update(msg)
		b.input = updated.(RichTextarea)

		return b, cmd

	case tea.KeyPressMsg:
		if b.locked {
			return b, nil
		}

		return b.handleKey(msg)
	}

	if b.locked {
		return b, nil
	}

	var cmd tea.Cmd
	updated, cmd := b.input.Update(msg)
	b.input = updated.(RichTextarea)

	return b, cmd
}

func (b InputBar) handleKey(msg tea.KeyPressMsg) (ui.Component, tea.Cmd) {
	// Any ordinary key clears the paste notice, so it describes only
	// the most recent input event.
	b.pasteFlattened = false

	// When the colour palette is open, the rich textarea owns
	// Esc/Tab/Left/Right/Enter and digit jumps. Forward the key
	// straight through so the input bar's Submit, history, and
	// nick-completion bindings don't swallow them.
	if b.input.PaletteVisible() {
		b.nickComp.active = false
		updated, cmd := b.input.Update(msg)
		b.input = updated.(RichTextarea)
		return b, cmd
	}

	switch {
	case ui.Matches(msg, b.keyMap.Submit):
		return b.submit()

	case ui.Matches(msg, b.keyMap.CopySelection):
		return b, clipboard.CopyCmd(b.input.SelectedText())

	case ui.Matches(msg, b.keyMap.HistoryUp):
		return b.historyUp(), nil

	case ui.Matches(msg, b.keyMap.HistoryDn):
		return b.historyDown(), nil

	case ui.Matches(msg, b.keyMap.KillLineStart):
		return b.killToLineStart(), nil

	case ui.Matches(msg, b.keyMap.DeleteChar):
		return b.deleteCharForward(), nil

	case msg.Code == tea.KeyTab && !msg.Mod.Contains(tea.ModShift):
		if !strings.HasPrefix(b.input.Value(), "/") {
			return b.completeNick(false), nil
		}

	case msg.Code == tea.KeyTab && msg.Mod.Contains(tea.ModShift):
		if !strings.HasPrefix(b.input.Value(), "/") {
			return b.completeNick(true), nil
		}
	}

	b.nickComp.active = false

	b.input = b.input.SetAllowFormatting(!strings.HasPrefix(b.input.Value(), "/"))
	updated, cmd := b.input.Update(msg)
	b.input = updated.(RichTextarea)
	b.input = b.input.SetAllowFormatting(!strings.HasPrefix(b.input.Value(), "/"))

	return b, cmd
}

// killToLineStart deletes from the cursor back to the start of the
// line (Unix readline's unix-line-discard), recording the removed
// text on the rich textarea's kill ring so ctrl+y can yank it back.
func (b InputBar) killToLineStart() InputBar {
	cursor := b.input.Cursor()
	if cursor == 0 {
		return b
	}

	value := []rune(b.input.Value())
	b.input.recordKill(string(value[:cursor]))

	return b.ReplaceRange(0, cursor, "")
}

// deleteCharForward deletes the character at the cursor (Emacs's
// delete-char), or the selection if one is active. Unlike the kill
// commands, it does not touch the kill ring.
func (b InputBar) deleteCharForward() InputBar {
	if !b.input.selection.Collapsed() {
		b.input.deleteSelection()
		return b
	}

	end := b.input.document.MoveRight(b.input.position)
	b.input.position = b.input.document.Delete(richtext.Selection{Anchor: b.input.position, Head: end})
	b.input.selection = richtext.Selection{Anchor: b.input.position, Head: b.input.position}
	b.input = b.input.ensureViewport()

	return b
}

// containsNewline reports whether text contains a carriage return or
// line feed, which the single-line input will flatten.
func containsNewline(text string) bool {
	return strings.ContainsAny(text, "\r\n")
}

func (b InputBar) handleMouse(msg tea.MouseMsg) (InputBar, bool, tea.Cmd) {
	if _, released := msg.(tea.MouseReleaseMsg); released && b.input.mouseSelecting {
		updated, cmd := b.input.Update(msg)
		b.input = updated.(RichTextarea)

		return b, true, cmd
	}

	if b.bounds.Dx() == 0 || b.bounds.Dy() == 0 {
		return b, false, nil
	}

	layout := b.layout(b.bounds)
	mouse := msg.Mouse()

	if contains(layout.palette, mouse.X, mouse.Y) {
		localX, localY := localPoint(layout.palette, mouse.X, mouse.Y)
		updated, handled := b.input.handlePaletteMouse(mouseAt(msg, localX, localY))
		if handled {
			b.input = updated
			return b, true, nil
		}
	}

	if contains(layout.input, mouse.X, mouse.Y) {
		switch msg.(type) {
		case tea.MouseClickMsg:
			if mouse.Button == tea.MouseLeft {
				inputMsg := mouseAt(msg, max(mouse.X, layout.editor.Min.X), mouse.Y)
				updated, inputCmd := b.input.Update(inputMsg)
				b.input = updated.(RichTextarea)

				return b, true, inputCmd
			}
		case tea.MouseMotionMsg:
			if b.input.mouseSelecting {
				inputMsg := mouseAt(msg, max(mouse.X, layout.editor.Min.X), mouse.Y)
				updated, cmd := b.input.Update(inputMsg)
				b.input = updated.(RichTextarea)

				return b, true, cmd
			}

			return b, true, nil
		}
	}

	return b, false, nil
}

func (b InputBar) submit() (ui.Component, tea.Cmd) {
	text := strings.TrimSpace(b.input.Value())
	if text == "" {
		return b, nil
	}

	raw := b.rawValue()
	b = b.pushHistory(raw)
	b.input = NewRichTextarea(RichTextareaConfig{
		SingleLine:      true,
		Wrap:            false,
		AllowFormatting: true,
	})
	b.histPos = -1
	b.histDraft = ""

	if strings.HasPrefix(text, "/") {
		return b, func() tea.Msg {
			return CommandSubmitMsg{Raw: text}
		}
	}

	return b, func() tea.Msg {
		return MessageSubmitMsg{Text: raw}
	}
}

// pushHistory appends text to the history ring, unless it repeats the
// last entry or secretChecker reports it carries a credential — the
// chatcmd grammar's `secret:""` marker is what decides that, so a
// later /config api-key run can't be recalled with ↑ and re-submitted
// (or just read off the screen) by anyone who gets a look at this
// session afterwards.
func (b InputBar) pushHistory(text string) InputBar {
	if b.secretChecker != nil && b.secretChecker.IsSecret(text) {
		return b
	}

	if len(b.history) > 0 && b.history[len(b.history)-1] == text {
		return b
	}

	b.history = append(b.history, text)

	if len(b.history) > historySize {
		b.history = b.history[len(b.history)-historySize:]
	}

	return b
}

func (b InputBar) historyUp() InputBar {
	if len(b.history) == 0 {
		return b
	}

	if b.histPos == -1 {
		b.histDraft = b.rawValue()
		b.histDraftCursor = b.input.Cursor()
		b.histPos = len(b.history) - 1
	} else if b.histPos > 0 {
		b.histPos--
	} else {
		return b
	}

	b = b.setRawValue(b.history[b.histPos])

	return b
}

func (b InputBar) historyDown() InputBar {
	if b.histPos == -1 {
		return b
	}

	if b.histPos < len(b.history)-1 {
		b.histPos++
		b = b.setRawValue(b.history[b.histPos])
		return b
	}

	b.histPos = -1
	b = b.setRawValue(b.histDraft)
	b.input = b.input.SetCursorFromRuneIndex(b.histDraftCursor)
	b.histDraft = ""
	b.histDraftCursor = 0

	return b
}

// completeNick cycles through nick matches for the word before the
// cursor, forward on Tab and backward (reverse) on Shift+Tab.
func (b InputBar) completeNick(reverse bool) InputBar {
	if len(b.nicks) == 0 {
		return b
	}

	if b.nickComp.active {
		n := len(b.nickComp.matches)
		if reverse {
			b.nickComp.index = (b.nickComp.index - 1 + n) % n
		} else {
			b.nickComp.index = (b.nickComp.index + 1) % n
		}

		return b.applyNickCompletion()
	}

	value := []rune(b.input.Value())
	cursor := b.input.Cursor()

	// Find the word boundary before the cursor.
	start := cursor
	for start > 0 && value[start-1] != ' ' {
		start--
	}

	if start == cursor {
		return b
	}

	prefix := strings.ToLower(string(value[start:cursor]))
	matches := b.matchNicks(prefix)

	if len(matches) == 0 {
		return b
	}

	b.nickComp = nickCompletion{
		active:  true,
		prefix:  prefix,
		start:   start,
		end:     cursor,
		matches: matches,
		index:   0,
	}

	if reverse {
		b.nickComp.index = len(matches) - 1
	}

	return b.applyNickCompletion()
}

func (b InputBar) matchNicks(prefix string) []domain.Nick {
	var matches []domain.Nick

	for _, nick := range b.nicks {
		if strings.HasPrefix(strings.ToLower(string(nick)), prefix) {
			matches = append(matches, nick)
		}
	}

	slices.Sort(matches)

	return matches
}

func (b InputBar) applyNickCompletion() InputBar {
	nick := b.nickComp.matches[b.nickComp.index]
	value := []rune(b.input.Value())

	// At the start of the line, append ": ". Mid-line, append " "
	// unless the next character is already a space.
	suffix := " "
	if b.nickComp.start == 0 {
		suffix = ": "
	} else if b.nickComp.end < len(value) && value[b.nickComp.end] == ' ' {
		suffix = ""
	}

	replacement := string(nick) + suffix

	b.input = b.input.ReplaceRange(b.nickComp.start, b.nickComp.end, replacement)

	newEnd := b.nickComp.start + len([]rune(replacement))
	b.input = b.input.SetCursorFromRuneIndex(newEnd)
	b.nickComp.end = newEnd

	return b
}

// Value returns the current text in the input buffer.
func (b InputBar) Value() string {
	return b.input.Value()
}

// Cursor returns the cursor position in runes.
func (b InputBar) Cursor() int {
	return b.input.Cursor()
}

// ReplaceRange replaces the given rune range with the provided text.
func (b InputBar) ReplaceRange(start, end int, replacement string) InputBar {
	value := []rune(b.input.Value())
	start = clampInputIndex(start, len(value))
	end = clampInputIndex(end, len(value))
	end = max(end, start)
	b.input = b.input.ReplaceRange(start, end, replacement)

	return b
}

// SetCursorFromCell moves the cursor to the nearest cell within the input area.
func (b InputBar) SetCursorFromCell(x int) InputBar {
	x -= b.prefixWidth()

	if x <= 0 {
		b.input = b.input.SetCursorFromRuneIndex(0)
		return b
	}

	b.input = b.input.SetCursorFromCell(x)

	return b
}

// KeyBindings implements ui.Keybinding.
func (b InputBar) KeyBindings() []ui.KeyBinding {
	if b.input.PaletteVisible() {
		return b.input.paletteKeyMap.Bindings()
	}

	bindings := []ui.KeyBinding{
		b.keyMap.Submit,
		ui.WithBindingEnabled(b.keyMap.HistoryUp, len(b.history) > 0),
		ui.WithBindingEnabled(b.keyMap.HistoryDn, len(b.history) > 0),
		b.keyMap.KillLineStart,
		b.keyMap.DeleteChar,
		ui.WithBindingEnabled(b.keyMap.CopySelection, !b.input.selection.Collapsed()),
	}

	commandMode := strings.HasPrefix(b.input.Value(), "/")
	attrs := b.input.activeAttrs()
	editor := b.input.keyMap
	claimed := b.claimedKeys()

	for _, binding := range b.input.Bindings() {
		// The bar matches its own bindings before the editor sees a
		// key, so an editor binding whose every key the bar already
		// matches can never be reached from the bar. Leave it
		// unadvertised.
		if shadowed(binding, claimed) {
			continue
		}

		switch binding.Help().Key {
		case editor.Yank.Help().Key:
			binding = ui.WithBindingEnabled(binding, b.input.canYank())
		case editor.ToggleBold.Help().Key:
			binding = b.fmtBinding(binding, attrs.Bold)
		case editor.ToggleItalic.Help().Key:
			binding = b.fmtBinding(binding, attrs.Italic)
		case editor.ToggleUnderline.Help().Key:
			binding = b.fmtBinding(binding, attrs.Underline)
		case editor.ToggleReverse.Help().Key:
			binding = b.fmtBinding(binding, attrs.Reverse)
		case editor.ToggleStrike.Help().Key:
			binding = b.fmtBinding(binding, attrs.Strike)
		case editor.OpenPalette.Help().Key:
			binding = b.fmtBinding(binding, attrs.FG != nil || attrs.BG != nil)
		case editor.ResetFormat.Help().Key:
			binding = b.fmtBinding(binding, attrs != (richtext.Attrs{}))
		}

		// A line starting with "/" is a command and takes no
		// formatting, so its toggles are not offered.
		if commandMode && binding.HelpGroup == ui.KeyHelpFormatting {
			continue
		}

		bindings = append(bindings, binding)
	}

	return bindings
}

// claimedKeys collects the keys in the bar's own keymap. Tab and the
// palette's keys are matched outside that keymap, so they are not in
// the set.
func (b InputBar) claimedKeys() map[string]bool {
	claimed := map[string]bool{}
	for _, binding := range b.keyMap.Bindings() {
		for _, k := range binding.Keys() {
			claimed[k] = true
		}
	}

	return claimed
}

// shadowed reports whether claimed already contains every key the
// binding matches.
func shadowed(binding ui.KeyBinding, claimed map[string]bool) bool {
	for _, k := range binding.Keys() {
		if !claimed[k] {
			return false
		}
	}

	return true
}

func (b InputBar) fmtBinding(binding ui.KeyBinding, active bool) ui.KeyBinding {
	return ui.WithBindingActive(binding, active)
}

func clampInputIndex(index, length int) int {
	if index < 0 {
		return 0
	}

	if index > length {
		return length
	}

	return index
}

func (b InputBar) prefixWidth() int {
	nickLabel, lockBadge, prompt := b.inputPrefix()

	return lipgloss.Width(nickLabel) + lipgloss.Width(lockBadge) + lipgloss.Width(prompt)
}

// bandRows returns the rows the input bar's three bands want, from the
// top down: the colour palette, the paste note, and the input row
// itself. Height and layout both read it, so the rows the bar asks its
// parent for are the rows it sets out to place.
func (b InputBar) bandRows() (palette, aux, input int) {
	if b.input.PaletteVisible() {
		palette = 1
	}

	if b.pasteFlattened {
		aux = 1
	}

	return palette, aux, 1
}

// bandRowsWithin returns the rows the bands take in an area of the
// given height. An area too short for all three keeps the input row
// first, then the paste note, and drops the palette soonest: the input
// row is where the operator is typing, and a bar that painted a colour
// swatch over that row would take the prompt and the text off the
// screen.
func (b InputBar) bandRowsWithin(height int) (palette, aux, input int) {
	palette, aux, input = b.bandRows()

	input = min(input, max(height, 0))
	aux = min(aux, max(height-input, 0))
	palette = min(palette, max(height-input-aux, 0))

	return palette, aux, input
}

// Height returns the rows the bar needs, which is what ChatView
// reserves for it at the bottom of the window.
func (b InputBar) Height() int {
	palette, aux, input := b.bandRows()

	return palette + aux + input
}

func (b InputBar) inputPrefix() (nickLabel, lockBadge, prompt string) {
	nickLabel = theme.UserNick.Render(string(b.userNick)) + " "
	prompt = theme.Prompt.Render("> ")

	if b.locked {
		lockBadge = theme.Dim.Render("(locked) ")
		nickLabel = theme.Dim.Render(string(b.userNick)) + " "
		prompt = theme.Dim.Render("> ")
	}

	return nickLabel, lockBadge, prompt
}

func (b InputBar) layout(area uv.Rectangle) inputBarLayout {
	if area.Empty() {
		return inputBarLayout{}
	}

	// The bands take their rows before the split, because the solver
	// ranks constraint kinds and not two constraints of one kind, and
	// which band survives a short area is a decision this bar makes.
	// The leading fill is what holds the rest against the bottom of
	// whatever room the bar is given.
	paletteRows, auxRows, inputRows := b.bandRowsWithin(area.Dy())

	var surplus, palette, aux, input uv.Rectangle
	uvlayout.Vertical(
		uvlayout.Fill(1),
		uvlayout.Len(paletteRows),
		uvlayout.Len(auxRows),
		uvlayout.Len(inputRows),
	).Split(area).Assign(&surplus, &palette, &aux, &input)

	nickLabel, lockBadge, prompt := b.inputPrefix()
	prefixWidth := min(
		lipgloss.Width(nickLabel)+lipgloss.Width(lockBadge)+lipgloss.Width(prompt),
		input.Dx(),
	)

	var prefix, editor uv.Rectangle
	uvlayout.Horizontal(
		uvlayout.Len(prefixWidth),
		uvlayout.Fill(1),
	).Split(input).Assign(&prefix, &editor)

	return inputBarLayout{
		input:   input,
		editor:  editor,
		palette: palette,
		note:    aux,
	}
}

func (b InputBar) updateChildBounds() (ui.Component, tea.Cmd) {
	layout := b.layout(b.bounds)

	updatedInput, inputCmd := b.input.Update(ui.BoundsMsg{Rect: layout.editor})
	b.input = updatedInput.(RichTextarea)

	return b, inputCmd
}

// ActiveFormats returns the formatting state at the current cursor
// position. In command mode all formats are reported as inactive.
func (b InputBar) ActiveFormats() ActiveFormats {
	if strings.HasPrefix(b.input.Value(), "/") {
		return ActiveFormats{}
	}

	attrs := b.input.activeAttrs()

	return ActiveFormats{
		Bold:      attrs.Bold,
		Italic:    attrs.Italic,
		Underline: attrs.Underline,
		Reverse:   attrs.Reverse,
		Strike:    attrs.Strike,
	}
}

// PaletteVisible reports whether the colour palette is open.
func (b InputBar) PaletteVisible() bool {
	return b.input.PaletteVisible()
}

// PaletteTarget reports which colour slot the palette is editing.
// The result is meaningful only when PaletteVisible reports true.
func (b InputBar) PaletteTarget() PaletteTarget {
	return b.input.PaletteTarget()
}

// PaletteIndex returns the active swatch index within the palette.
func (b InputBar) PaletteIndex() int {
	return b.input.PaletteIndex()
}

func (b InputBar) rawValue() string {
	plain := b.input.Value()
	if strings.HasPrefix(plain, "/") {
		return plain
	}

	return ircfmt.Encode(b.input.Document())
}

func (b InputBar) setRawValue(raw string) InputBar {
	if strings.HasPrefix(raw, "/") {
		b.input = b.input.SetPlainText(raw)
		b.input = b.input.SetAllowFormatting(false)
		b.input = b.input.SetCursorFromRuneIndex(len([]rune(b.input.Value())))
		return b
	}

	b.input = b.input.SetDocument(ircfmt.Parse(raw))
	b.input = b.input.SetAllowFormatting(true)
	b.input = b.input.SetCursorFromRuneIndex(len([]rune(b.input.Value())))

	return b
}
