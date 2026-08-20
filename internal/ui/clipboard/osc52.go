// Package clipboard emits terminal-clipboard control sequences via
// Bubble Tea commands. Today only OSC 52 copy is supported. Paste is
// left to the terminal's own bracketed-paste path.
package clipboard

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// writer is the destination for OSC 52 sequences. Production writes
// to stderr so the escape lands on the user's controlling terminal
// regardless of where stdout is connected. Tests override it.
//
// Bubble Tea runs each Cmd it returns on its own goroutine, so a
// CopyCmd invocation and a concurrent SetWriter (or another CopyCmd)
// can race on this package global; writerMu serialises access to it.
var (
	writerMu sync.RWMutex
	writer   io.Writer = os.Stderr
)

// CopyCmd returns a tea.Cmd that copies text to the host terminal's
// clipboard via an OSC 52 sequence. Empty input is a no-op (returns
// a nil Cmd that bubbletea will skip).
//
// Terminal support is opt-in for some hosts: tmux requires
// `set -g set-clipboard on` and many SSH session setups pass the
// sequence through transparently. Where it works, the user can paste
// into any other app with the system keystroke.
func CopyCmd(text string) tea.Cmd {
	if text == "" {
		return nil
	}

	return func() tea.Msg {
		writerMu.RLock()
		w := writer
		writerMu.RUnlock()

		_, _ = fmt.Fprintf(w, "\x1b]52;c;%s\x07", base64.StdEncoding.EncodeToString([]byte(text)))
		return nil
	}
}

// SetWriter overrides the destination for the OSC 52 sequence. Used
// in tests to capture the emitted bytes.
func SetWriter(w io.Writer) (restore func()) {
	writerMu.Lock()
	prev := writer
	writer = w
	writerMu.Unlock()

	return func() {
		writerMu.Lock()
		writer = prev
		writerMu.Unlock()
	}
}
