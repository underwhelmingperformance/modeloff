package uitest

import (
	"strconv"
	"strings"
)

// virtualScreen is a minimal terminal emulator just rich enough to
// reconstruct the visible state of a Bubble Tea program from the raw
// teatest output stream. Bubble Tea's standard renderer skips
// unchanged lines between flushes, so a naive "tail of the buffer"
// snapshot is a diff, not a screen. virtualScreen replays cursor
// movement and erase sequences into a row buffer so callers can
// inspect what the user would actually see right now.
//
// The emulator implements the subset of CSI sequences the renderer
// uses: CursorUp / CursorDown, CursorPosition / CursorHomePosition,
// EraseLine variants, EraseScreen variants, plus carriage return,
// line feed and SGR pass-through. Unknown sequences are consumed
// without effect, which is sufficient for non-alt-screen rendering
// in the modeloff TUI.
type virtualScreen struct {
	rows [][]rune
	row  int
	col  int
}

// newVirtualScreen creates an empty screen with no rows. Rows are
// added on demand as the cursor moves below the current bottom.
func newVirtualScreen() *virtualScreen {
	return &virtualScreen{}
}

// ensureRow grows the row buffer so that index r is addressable.
func (s *virtualScreen) ensureRow(r int) {
	for len(s.rows) <= r {
		s.rows = append(s.rows, nil)
	}
}

// writeRune places a rune at the cursor and advances the column.
func (s *virtualScreen) writeRune(r rune) {
	s.ensureRow(s.row)
	row := s.rows[s.row]
	for len(row) <= s.col {
		row = append(row, ' ')
	}
	row[s.col] = r
	s.rows[s.row] = row
	s.col++
}

// eraseLineRight clears from the cursor to the end of the current row.
func (s *virtualScreen) eraseLineRight() {
	s.ensureRow(s.row)
	row := s.rows[s.row]
	if s.col >= len(row) {
		return
	}
	s.rows[s.row] = row[:s.col]
}

// eraseLineLeft clears from the start of the row to the cursor.
func (s *virtualScreen) eraseLineLeft() {
	s.ensureRow(s.row)
	row := s.rows[s.row]
	for i := 0; i <= s.col && i < len(row); i++ {
		row[i] = ' '
	}
}

// eraseLineAll clears the entire current row.
func (s *virtualScreen) eraseLineAll() {
	s.ensureRow(s.row)
	s.rows[s.row] = nil
}

// eraseScreenBelow clears from the cursor to the end of the screen.
func (s *virtualScreen) eraseScreenBelow() {
	s.eraseLineRight()
	if s.row+1 < len(s.rows) {
		s.rows = s.rows[:s.row+1]
	}
}

// view returns the screen contents as a single string with rows
// joined by newlines. Empty trailing cells are not padded.
func (s *virtualScreen) view() string {
	out := make([]string, len(s.rows))
	for i, row := range s.rows {
		out[i] = string(row)
	}
	return strings.Join(out, "\n")
}

// feed replays the given bytes into the screen. It is safe to call
// repeatedly with cumulative output; the state evolves accordingly.
func (s *virtualScreen) feed(data []byte) {
	runes := []rune(string(data))

	for i := 0; i < len(runes); i++ {
		c := runes[i]

		if c == '\x1b' && i+1 < len(runes) && runes[i+1] == '[' {
			i = s.applyCSI(runes, i+2)
			continue
		}

		switch c {
		case '\r':
			s.col = 0
		case '\n':
			s.row++
			s.ensureRow(s.row)
		case '\b':
			if s.col > 0 {
				s.col--
			}
		default:
			if c < 0x20 {
				continue
			}
			s.writeRune(c)
		}
	}
}

// applyCSI handles a CSI sequence that begins at runes[start] (i.e.
// just after the `\x1b[` prefix). It returns the index of the final
// byte of the sequence so the caller can resume iteration.
func (s *virtualScreen) applyCSI(runes []rune, start int) int {
	end := start
	for end < len(runes) {
		c := runes[end]
		if c >= 0x40 && c <= 0x7e {
			break
		}
		end++
	}
	if end >= len(runes) {
		return end - 1
	}

	params := string(runes[start:end])
	final := runes[end]

	switch final {
	case 'A':
		s.row -= csiNumber(params, 1)
		if s.row < 0 {
			s.row = 0
		}
	case 'B':
		s.row += csiNumber(params, 1)
		s.ensureRow(s.row)
	case 'C':
		s.col += csiNumber(params, 1)
	case 'D':
		s.col -= csiNumber(params, 1)
		if s.col < 0 {
			s.col = 0
		}
	case 'H', 'f':
		row, col := csiTwoNumbers(params, 1, 1)
		s.row = max(row-1, 0)
		s.col = max(col-1, 0)
		s.ensureRow(s.row)
	case 'J':
		switch csiNumber(params, 0) {
		case 0:
			s.eraseScreenBelow()
		case 2, 3:
			s.rows = nil
			s.row = 0
			s.col = 0
		}
	case 'K':
		switch csiNumber(params, 0) {
		case 0:
			s.eraseLineRight()
		case 1:
			s.eraseLineLeft()
		case 2:
			s.eraseLineAll()
		}
	}

	return end
}

// csiNumber parses the first numeric parameter from a CSI param
// string, returning fallback when the parameter is absent or
// invalid.
func csiNumber(params string, fallback int) int {
	if params == "" {
		return fallback
	}
	first, _, _ := strings.Cut(params, ";")
	if first == "" {
		return fallback
	}
	n, err := strconv.Atoi(first)
	if err != nil {
		return fallback
	}
	return n
}

// csiTwoNumbers parses two semicolon-separated parameters, returning
// fallback values when they are absent or invalid.
func csiTwoNumbers(params string, fa, fb int) (int, int) {
	a, b, ok := strings.Cut(params, ";")
	if !ok {
		return csiNumber(params, fa), fb
	}
	na, err := strconv.Atoi(a)
	if err != nil || a == "" {
		na = fa
	}
	nb, err := strconv.Atoi(b)
	if err != nil || b == "" {
		nb = fb
	}
	return na, nb
}
