// Package theme provides the design system for the modeloff TUI.
// All styles use ANSI colours so the user's terminal theme is
// respected.
package theme

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

// ANSI colour indices used throughout the application.
const (
	colourBlack   = lipgloss.ANSIColor(0)
	colourRed     = lipgloss.ANSIColor(1)
	colourGreen   = lipgloss.ANSIColor(2)
	colourYellow  = lipgloss.ANSIColor(3)
	colourBlue    = lipgloss.ANSIColor(4)
	colourMagenta = lipgloss.ANSIColor(5)
	colourCyan    = lipgloss.ANSIColor(6)
	colourWhite   = lipgloss.ANSIColor(7)

	colourBrightBlack = lipgloss.ANSIColor(8)
)

// Text styles.
var (
	// Bold is used for emphasis.
	Bold = lipgloss.NewStyle().Bold(true)

	// Dim is used for secondary or less important text.
	Dim = lipgloss.NewStyle().Foreground(colourBrightBlack)

	// Error is used for error messages.
	Error = lipgloss.NewStyle().Foreground(colourRed)

	// Success is used for success indicators.
	Success = lipgloss.NewStyle().Foreground(colourGreen)

	// Warning is used for warnings and notices.
	Warning = lipgloss.NewStyle().Foreground(colourYellow)

	// Info is used for informational messages.
	Info = lipgloss.NewStyle().Foreground(colourCyan)
)

// Nick styles — used to colour nicknames in chat.
var (
	// UserNick is used for the user's nick in the input prompt area.
	UserNick = lipgloss.NewStyle().Foreground(colourGreen).Bold(true)
)

// nickColours are the ANSI colours used for nick hashing. Black,
// white, and bright-black are excluded as they blend with common
// terminal backgrounds.
var nickColours = [...]lipgloss.ANSIColor{
	colourRed,
	colourGreen,
	colourYellow,
	colourBlue,
	colourMagenta,
	colourCyan,
}

// NickStyle returns a bold style with a colour determined by hashing
// the nick string. The same nick always produces the same colour.
func NickStyle(nick string) lipgloss.Style {
	var h uint32

	for _, r := range nick {
		h = h*31 + uint32(r)
	}

	return lipgloss.NewStyle().
		Foreground(nickColours[h%uint32(len(nickColours))]).
		Bold(true)
}

// Channel styles.
var (
	ChannelName     = lipgloss.NewStyle().Foreground(colourCyan).Bold(true)
	DMName          = lipgloss.NewStyle().Foreground(colourMagenta).Bold(true)
	ChannelTitle    = lipgloss.NewStyle().Foreground(colourYellow).Italic(true)
	ActiveChannel   = lipgloss.NewStyle().Foreground(colourWhite).Bold(true)
	InactiveChannel = lipgloss.NewStyle().Foreground(colourBrightBlack)
	UnreadChannel   = lipgloss.NewStyle().Foreground(colourWhite).Bold(true)
)

// System message styles — for join/part/topic events.
var (
	SystemEvent = lipgloss.NewStyle().Foreground(colourBrightBlack).Italic(true)
)

// Input area styles.
var (
	Prompt = lipgloss.NewStyle().Foreground(colourGreen).Bold(true)
)

// Layout constants.
const (
	// MinTerminalWidth is the narrowest terminal width the app can
	// render. Below this, screens show a fallback message.
	MinTerminalWidth = 80
)

// NarrowTerminalView returns a centred fallback message prompting the
// user to widen their terminal. Use this as an early return in View
// methods when width < MinTerminalWidth.
func NarrowTerminalView(width, height int) string {
	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		Warning.Render(fmt.Sprintf("Resize terminal to %d+ columns", MinTerminalWidth)))
}

// Sidebar styles.
var (
	SidebarBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderRight(true).
			BorderForeground(colourBrightBlack)

	NickListBorder = lipgloss.NewStyle().
			BorderStyle(lipgloss.NormalBorder()).
			BorderLeft(true).
			BorderForeground(colourBrightBlack)
)
