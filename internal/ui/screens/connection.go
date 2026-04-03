package screens

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// stepDelay is the time between each connection step appearing.
const stepDelay = 400 * time.Millisecond

// connectionStep describes one line in the connection sequence.
type connectionStep struct {
	label  string
	status stepStatus
}

type stepStatus int

const (
	stepPending stepStatus = iota
	stepDone
	stepError
)

// ConnectionTickMsg advances the connection sequence by one step.
type ConnectionTickMsg struct{}

// ConnectionDoneMsg is sent when the connection sequence finishes
// successfully and no Next screen was configured. When Next is set,
// a ui.ScreenMsg is emitted instead.
type ConnectionDoneMsg struct{}

// ConnectionConfig holds the inputs the connection screen needs to
// determine what to show.
type ConnectionConfig struct {
	HasAPIKey    bool
	ChannelCount int
	Nick         string
	Next         ui.Model
}

// ConnectionScreen shows an IRC-style startup animation.
type ConnectionScreen struct {
	cfg   ConnectionConfig
	steps []connectionStep
	cur   int
	done  bool
}

// NewConnectionScreen creates a connection screen with the given
// configuration.
func NewConnectionScreen(cfg ConnectionConfig) ConnectionScreen {
	steps := []connectionStep{
		{label: "Connecting to modeloff"},
		{label: "Checking configuration"},
	}

	if !cfg.HasAPIKey {
		steps = append(steps, connectionStep{
			label:  "No API key configured — use /config to set one",
			status: stepError,
		})
	} else {
		steps = append(steps,
			connectionStep{label: fmt.Sprintf("Loading channels (%d found)", cfg.ChannelCount)},
			connectionStep{label: fmt.Sprintf("Welcome, %s", cfg.Nick)},
		)
	}

	return ConnectionScreen{
		cfg:   cfg,
		steps: steps,
	}
}

// Init implements ui.Model.
func (s ConnectionScreen) Init() tea.Cmd {
	return tea.Tick(stepDelay, func(time.Time) tea.Msg {
		return ConnectionTickMsg{}
	})
}

// Update implements ui.Model.
func (s ConnectionScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	if _, ok := msg.(ConnectionTickMsg); !ok {
		return s, nil
	}

	if s.cur < len(s.steps) {
		if s.steps[s.cur].status == stepPending {
			s.steps[s.cur].status = stepDone
		}
		s.cur++
	}

	if s.cur >= len(s.steps) {
		s.done = true

		if s.cfg.HasAPIKey {
			if s.cfg.Next != nil {
				next := s.cfg.Next
				return s, func() tea.Msg {
					return ui.ScreenMsg{Screen: next}
				}
			}

			return s, func() tea.Msg { return ConnectionDoneMsg{} }
		}

		return s, nil
	}

	return s, tea.Tick(stepDelay, func(time.Time) tea.Msg {
		return ConnectionTickMsg{}
	})
}

// View implements ui.Model.
func (s ConnectionScreen) View(width, height int) string {
	var b strings.Builder

	for i := 0; i < s.cur && i < len(s.steps); i++ {
		step := s.steps[i]
		b.WriteString(renderStep(step))
		b.WriteByte('\n')
	}

	if !s.done && s.cur < len(s.steps) {
		b.WriteString(renderPending(s.steps[s.cur]))
		b.WriteByte('\n')
	}

	return lipgloss.Place(width, height,
		lipgloss.Center, lipgloss.Center,
		b.String())
}

func renderStep(step connectionStep) string {
	switch step.status {
	case stepDone:
		return theme.Success.Render("✓") + " " + step.label
	case stepError:
		return theme.Error.Render("✗") + " " + step.label
	default:
		return "  " + step.label
	}
}

func renderPending(step connectionStep) string {
	if step.status == stepError {
		return theme.Error.Render("✗") + " " + step.label
	}

	return theme.Dim.Render("…") + " " + theme.Dim.Render(step.label)
}
