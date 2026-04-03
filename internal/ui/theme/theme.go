// Package theme provides the design system for the modeloff TUI.
// All styles use ANSI colours so the user's terminal theme is
// respected.
package theme

import "github.com/charmbracelet/lipgloss"

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
	UserNick  = lipgloss.NewStyle().Foreground(colourGreen).Bold(true)
	ModelNick = lipgloss.NewStyle().Foreground(colourMagenta).Bold(true)
)

// Room styles.
var (
	RoomName     = lipgloss.NewStyle().Foreground(colourCyan).Bold(true)
	RoomTitle    = lipgloss.NewStyle().Foreground(colourYellow).Italic(true)
	ActiveRoom   = lipgloss.NewStyle().Foreground(colourWhite).Bold(true)
	InactiveRoom = lipgloss.NewStyle().Foreground(colourBrightBlack)
)

// System message styles — for join/part/topic events.
var (
	SystemEvent = lipgloss.NewStyle().Foreground(colourBrightBlack).Italic(true)
)

// Input area styles.
var (
	Prompt = lipgloss.NewStyle().Foreground(colourGreen).Bold(true)
)

// Sidebar styles.
var (
	SidebarBorder = lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderRight(true).
		BorderForeground(colourBrightBlack)
)
