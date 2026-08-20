package components

import (
	"github.com/laney/modeloff/internal/richtext"
)

// killRingCap bounds the kill ring. Readline keeps an unbounded ring;
// a chat input bar has no use for more than the last few kills, and a
// long session would otherwise hold every line the user ever deleted.
const killRingCap = 16

// killRing holds the text of recent kill commands (ctrl+w, ctrl+k,
// alt+d and their selection forms), newest first, so ctrl+y can put
// the last one back.
type killRing struct {
	entries []string
}

// record prepends text to the ring, dropping the oldest entry once the
// ring is at capacity. An empty kill is not recorded: a kill command
// that removed nothing must leave the previous kill yankable.
func (k *killRing) record(text string) {
	if text == "" {
		return
	}

	ring := make([]string, 0, len(k.entries)+1)
	ring = append(ring, text)
	ring = append(ring, k.entries...)
	if len(ring) > killRingCap {
		ring = ring[:killRingCap]
	}

	k.entries = ring
}

// top returns the most recent kill, and false when nothing has been
// killed yet.
func (k killRing) top() (string, bool) {
	if len(k.entries) == 0 {
		return "", false
	}

	return k.entries[0], true
}

// canYank reports whether ctrl+y has anything to put back, which is
// what decides whether the yank binding is offered.
func (r RichTextarea) canYank() bool {
	_, ok := r.kills.top()

	return ok
}

// recordKill puts text on the kill ring without touching the
// document, for a caller that removed the text itself.
func (r *RichTextarea) recordKill(text string) {
	r.kills.record(text)
}

// killSelection records the current selection text on the kill ring
// and deletes it from the document.
func (r *RichTextarea) killSelection() {
	r.kills.record(r.selectionText(r.selection))
	r.deleteSelection()
}

// killRange records the selected range's text on the kill ring and
// removes it from the document.
func (r *RichTextarea) killRange(selection richtext.Selection) {
	r.kills.record(r.selectionText(selection))
	r.position = r.document.Delete(selection)
	r.selection = richtext.Selection{Anchor: r.position, Head: r.position}
	*r = r.ensureViewport()
}

// yank inserts the most recently killed text at the cursor. An empty
// ring is a no-op.
func (r RichTextarea) yank() RichTextarea {
	text, ok := r.kills.top()
	if !ok {
		return r
	}

	r.insertText(text)

	return r
}
