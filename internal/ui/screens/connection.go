package screens

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

// stepDelay is the time between each connection step appearing.
const stepDelay = 400 * time.Millisecond

// statusPaneMaxRows caps the height of the status pane below the
// welcome animation.
const statusPaneMaxRows = 8

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

// connectionReadyMsg is sent when Session.Connected() closes,
// indicating the backend handshake is complete.
type connectionReadyMsg struct{ err error }

// joinAutojoinDoneMsg is sent when JoinAutojoinChannels (and the
// follow-up FocusChannel) returns.
type joinAutojoinDoneMsg struct{ err error }

// statusRefreshMsg refreshes the status pane from the persisted
// status log.
type statusRefreshMsg struct{}

// ConnectionConfig holds the inputs the connection screen needs to
// determine what to show.
type ConnectionConfig struct {
	HasAPIKey    bool
	ChannelCount int
	Nick         string
	Next         ui.Model

	// Session is the backend handle the screen drives during the
	// connection handshake. When nil the screen runs in animation-only
	// mode (used by tests that only care about the visual sequence).
	Session *session.Session

	// Ctx is the application context used for backend calls. Defaults
	// to context.Background when zero, but production callers should
	// supply the application context so cancellation propagates.
	Ctx context.Context
}

// ConnectionScreen shows the IRC-style startup animation and a
// scrolling status pane below it. The screen orchestrates the
// backend handshake (Session.Connect) and the autojoin sequence
// (Session.JoinAutojoinChannels followed by Session.FocusChannel),
// transitioning to the chat screen when both have completed.
type ConnectionScreen struct {
	cfg   ConnectionConfig
	steps []connectionStep
	cur   int
	done  bool

	connected bool

	// autojoinKicked is true once runAutojoin has been started.
	// Subsequent ticks on the "Joining channels" step must not
	// re-launch the autojoin Cmd while the first invocation is
	// still in flight, otherwise JoinAutojoinChannels runs multiple
	// times in parallel, emits duplicate status notices, and
	// produces a LastChannel race that leaves the wrong channel
	// focused when the screen transitions.
	autojoinKicked bool
	autojoinDone   bool

	// paneCursor tracks the highest StoredEvent.ID appended to the
	// pane. refreshPane appends only events with a strictly greater
	// ID, so periodic refreshes do not replay the whole log and
	// accumulate duplicates. The ID-gap check relies on store IDs
	// being strictly increasing by insertion order — guaranteed by
	// SQLite's INTEGER PRIMARY KEY on the events table (see
	// store/sqlite.go). Each per-channel read is therefore
	// monotonic by insertion order regardless of how many writers
	// target the channel.
	paneCursor int64

	// quitting is true between QuitRequestedMsg and QuitCompleteMsg
	// so a second Ctrl-C can short-circuit a stuck Session.Quit and
	// the status bar can surface "Disconnecting…" feedback.
	quitting bool

	pane components.MessageList
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
			connectionStep{label: "Joining channels"},
			connectionStep{label: fmt.Sprintf("Welcome, %s", cfg.Nick)},
		)
	}

	s := ConnectionScreen{
		cfg:   cfg,
		steps: steps,
		pane:  components.NewMessageList(domain.StatusChannelName, domain.KindStatus),
	}

	// Animation-only mode (no Session): pretend the async signals
	// have already arrived so the tick advances every step without
	// gating.
	if cfg.Session == nil {
		s.connected = true
		s.autojoinDone = true
	}

	return s
}

func (s ConnectionScreen) ctx() context.Context {
	if s.cfg.Ctx != nil {
		return s.cfg.Ctx
	}

	return context.Background()
}

// Init implements ui.Model.
func (s ConnectionScreen) Init() tea.Cmd {
	cmds := []tea.Cmd{
		tea.Tick(stepDelay, func(time.Time) tea.Msg { return ConnectionTickMsg{} }),
	}

	if s.cfg.Session != nil {
		cmds = append(cmds, s.runConnect(), s.waitForConnected())
	}

	return tea.Batch(cmds...)
}

// runConnect runs Session.Connect and returns connectionReadyMsg.
// Connect is fast enough to run inline (no need for a separate
// goroutine), but wrapping it in a tea.Cmd lets the framework drain
// the resulting events through Update naturally.
func (s ConnectionScreen) runConnect() tea.Cmd {
	sess := s.cfg.Session

	return func() tea.Msg {
		return connectionReadyMsg{err: sess.Connect(s.ctx())}
	}
}

// waitForConnected blocks on Session.Connected() and is the redundant
// signal-from-the-other-side. runConnect produces connectionReadyMsg
// directly; this exists so the screen can also notice readiness when
// Connect was kicked off elsewhere (e.g. tests). In normal flow
// runConnect's return arrives first and this is a no-op.
func (s ConnectionScreen) waitForConnected() tea.Cmd {
	sess := s.cfg.Session

	return func() tea.Msg {
		<-sess.Connected()
		return statusRefreshMsg{}
	}
}

// runAutojoin issues JoinAutojoinChannels followed by FocusChannel
// for the saved last channel (or the status channel as a fallback).
func (s ConnectionScreen) runAutojoin() tea.Cmd {
	sess := s.cfg.Session

	return func() tea.Msg {
		if err := sess.JoinAutojoinChannels(s.ctx()); err != nil {
			return joinAutojoinDoneMsg{err: err}
		}

		focus, err := sess.LastChannel(s.ctx())
		if err != nil || focus == "" {
			focus = domain.StatusChannelName
		}

		if err := sess.FocusChannel(s.ctx(), focus); err != nil {
			return joinAutojoinDoneMsg{err: err}
		}

		return joinAutojoinDoneMsg{}
	}
}

// Update implements ui.Model.
func (s ConnectionScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ConnectionTickMsg:
		return s.advanceTick()

	case connectionReadyMsg:
		s.connected = true

		if msg.err != nil {
			s.markCurrentStepError(msg.err.Error())
		}

		return s, statusRefreshCmd(s.cfg.Session, s.ctx())

	case joinAutojoinDoneMsg:
		s.autojoinDone = true

		if msg.err != nil {
			s.markCurrentStepError(msg.err.Error())
		}

		return s, statusRefreshCmd(s.cfg.Session, s.ctx())

	case statusRefreshMsg:
		s.refreshPane()
		return s, nil

	case ui.QuitRequestedMsg:
		return s.handleQuitRequested(msg)

	case ui.QuitCompleteMsg:
		return s, tea.Quit
	}

	return s, nil
}

// advanceTick moves the visible animation forward, gating the
// "Connecting" and "Joining" steps on their corresponding async
// signals. When a gate is not yet satisfied, the tick re-arms
// without advancing.
func (s ConnectionScreen) advanceTick() (ui.Model, tea.Cmd) {
	if s.cur >= len(s.steps) {
		return s, s.transitionCmd()
	}

	current := s.steps[s.cur]

	switch current.label {
	case "Connecting to modeloff":
		if !s.connected && s.cfg.Session != nil {
			return s, s.tickCmd()
		}

	case "Joining channels":
		// If autojoinDone has already fired by the time the
		// animation reaches this step, we fall through to the
		// ordinary pending→done transition below: the step still
		// holds for at least one stepDelay because tickCmd() paces
		// the next ConnectionTickMsg. The animation does not stutter.
		if !s.autojoinDone {
			if !s.autojoinKicked {
				// Kick off the autojoin once we reach this step,
				// then keep ticking until it completes. The
				// autojoinKicked guard prevents subsequent ticks
				// from launching parallel runAutojoin goroutines
				// while the first one is still in flight.
				return s.kickAutojoin()
			}

			return s, s.tickCmd()
		}
	}

	if current.status == stepPending {
		s.steps[s.cur].status = stepDone
	}

	s.cur++

	if s.cur >= len(s.steps) {
		s.done = true
		s.refreshPane()

		return s, s.transitionCmd()
	}

	s.refreshPane()

	return s, s.tickCmd()
}

// kickAutojoin starts the autojoin Cmd and re-arms the animation
// tick. Callers are expected to gate on s.autojoinKicked so this is
// only invoked once per session; kickAutojoin itself flips the flag
// so the gate stays correct even if a caller forgets.
func (s ConnectionScreen) kickAutojoin() (ui.Model, tea.Cmd) {
	if s.cfg.Session == nil {
		s.autojoinDone = true
		s.autojoinKicked = true
		return s, s.tickCmd()
	}

	s.autojoinKicked = true

	return s, tea.Batch(s.runAutojoin(), s.tickCmd())
}

func (s ConnectionScreen) tickCmd() tea.Cmd {
	return tea.Tick(stepDelay, func(time.Time) tea.Msg { return ConnectionTickMsg{} })
}

func (s ConnectionScreen) transitionCmd() tea.Cmd {
	if s.cfg.Next == nil {
		return nil
	}

	next := s.cfg.Next

	return func() tea.Msg {
		return ui.ScreenMsg{Screen: next}
	}
}

func (s *ConnectionScreen) markCurrentStepError(label string) {
	if s.cur >= len(s.steps) {
		return
	}

	s.steps[s.cur].label = label
	s.steps[s.cur].status = stepError
}

// refreshPane appends any status events that have landed since the
// previous refresh. It tracks the last-seen event ID as a cursor so
// each tick only feeds new events into the pane rather than replaying
// the entire per-session log and accumulating duplicate entries.
func (s *ConnectionScreen) refreshPane() {
	if s.cfg.Session == nil {
		return
	}

	events, err := s.cfg.Session.EventsAfter(s.ctx(), domain.StatusChannelName, s.cfg.Session.ConnectedAt())
	if err != nil {
		return
	}

	fresh := events[:0]
	for _, evt := range events {
		if evt.ID <= s.paneCursor {
			continue
		}

		fresh = append(fresh, evt)
	}

	if len(fresh) == 0 {
		return
	}

	s.pane = s.pane.Append(fresh...)
	s.paneCursor = fresh[len(fresh)-1].ID
}

func statusRefreshCmd(sess *session.Session, _ context.Context) tea.Cmd {
	if sess == nil {
		return nil
	}

	return func() tea.Msg { return statusRefreshMsg{} }
}

// handleQuitRequested wires Ctrl-C and other quit triggers during
// the connection phase. Behaviour mirrors ChatScreen: run the
// backend quit, then return tea.Quit on QuitCompleteMsg.
func (s ConnectionScreen) handleQuitRequested(msg ui.QuitRequestedMsg) (ui.Model, tea.Cmd) {
	if s.quitting {
		// A second quit request while the first is in flight is an
		// escape hatch: the user pressed Ctrl+C again because the
		// disconnect looks stuck. Bypass Session.Quit and exit now.
		return s, tea.Quit
	}

	if s.cfg.Session == nil {
		return s, tea.Quit
	}

	s.quitting = true

	sess := s.cfg.Session
	message := msg.Message
	ctx := s.ctx()

	return s, func() tea.Msg {
		return ui.QuitCompleteMsg{Err: sess.Quit(ctx, message)}
	}
}

// StatusItems implements ui.StatusProvider. The connection screen
// only contributes the in-flight "Disconnecting…" indicator; the
// startup animation already conveys the rest of the connection state.
func (s ConnectionScreen) StatusItems() []ui.StatusItem {
	if !s.quitting {
		return nil
	}

	return []ui.StatusItem{disconnectingStatusItem}
}

// View implements ui.Model.
func (s ConnectionScreen) View(width, height int) string {
	if width < theme.MinTerminalWidth {
		return theme.NarrowTerminalView(width, height)
	}

	// The connection screen only contributes to the status bar while
	// quitting; idle renders skip the bar entirely so the animation
	// owns the full vertical space instead of sitting above a blank
	// trailing row.
	bar := components.RenderStatusBar(width, ui.CollectKeyBindings(s), s.StatusItems())
	if bar == "" {
		return s.renderContent(width, height)
	}

	contentHeight := max(height-lipgloss.Height(bar), 0)

	return lipgloss.JoinVertical(lipgloss.Left, s.renderContent(width, contentHeight), bar)
}

// renderContent renders the animation and status pane within the
// supplied vertical budget, leaving the status bar (if any) to be
// joined separately by View.
func (s ConnectionScreen) renderContent(width, height int) string {
	animation := s.renderAnimation()

	paneHeight := s.paneHeight(height)
	if paneHeight <= 0 {
		return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, animation)
	}

	animationHeight := max(height-paneHeight, 0)
	animationView := lipgloss.Place(width, animationHeight, lipgloss.Center, lipgloss.Center, animation)

	paneView := s.pane.View(width, paneHeight)

	return lipgloss.JoinVertical(lipgloss.Left, animationView, paneView)
}

// paneHeight returns the desired status-pane height. The pane
// collapses on very short terminals so the animation always fits.
func (s ConnectionScreen) paneHeight(total int) int {
	if s.cfg.Session == nil {
		return 0
	}

	desired := min(statusPaneMaxRows, total/3)

	if total-desired < len(s.steps)+2 {
		return 0
	}

	return desired
}

func (s ConnectionScreen) renderAnimation() string {
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

	return b.String()
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
