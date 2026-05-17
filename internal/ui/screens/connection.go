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
	"github.com/laney/modeloff/internal/ui/chatcmd"
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

// loadModelsDoneMsg carries the result of the connect-time
// `sess.ListModels` call. `models` is nil when no API key is
// configured (a silent no-op rather than an error).
type loadModelsDoneMsg struct {
	models []chatcmd.ModelOption
	err    error
}

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

	// loadModelsKicked guards `runLoadModels` against parallel
	// launches in the same way `autojoinKicked` guards the autojoin
	// Cmd.
	loadModelsKicked bool
	loadModelsDone   bool
	loadedModels     []chatcmd.ModelOption
	loadModelsErr    error

	// paneCursor tracks the highest StoredEvent.ID appended to
	// [paneEvents]. refreshPane appends only events with a
	// strictly greater ID, so periodic refreshes do not replay
	// the whole log and accumulate duplicates.
	paneCursor int64

	// paneEvents is the per-tick accumulator the status pane
	// renders. A pointer keeps the slice header stable across
	// the value-copy `tea.Model.Update` returns, so the closure
	// the message list captures continues to read the latest
	// append.
	paneEvents *[]domain.StoredEvent

	// quitting is true between QuitRequestedMsg and QuitCompleteMsg
	// so a second Ctrl-C can short-circuit a stuck Session.Quit and
	// the status bar can surface "Disconnecting…" feedback.
	quitting bool

	pane components.MessageList[chatcmd.CompletionContext]
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
			connectionStep{label: "Loading models"},
			connectionStep{label: "Joining channels"},
			connectionStep{label: fmt.Sprintf("Welcome, %s", cfg.Nick)},
		)
	}

	paneEvents := &[]domain.StoredEvent{}

	s := ConnectionScreen{
		cfg:        cfg,
		steps:      steps,
		paneEvents: paneEvents,
		pane: components.NewMessageList[chatcmd.CompletionContext](
			func() []domain.StoredEvent { return *paneEvents },
			domain.StatusChannelName,
			domain.KindStatus,
		),
	}

	// Animation-only mode (no Session): pretend the async signals
	// have already arrived so the tick advances every step without
	// gating.
	if cfg.Session == nil {
		s.connected = true
		s.autojoinDone = true
		s.loadModelsDone = true
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

// runLoadModels calls [session.Session.ListModels] and packages
// the result as a `loadModelsDoneMsg`. With no API key configured
// the load is a silent no-op, leaving the chat screen's
// suggestion state at its zero value (ready, empty).
func (s ConnectionScreen) runLoadModels() tea.Cmd {
	sess := s.cfg.Session

	return func() tea.Msg {
		if !sess.HasAPIKey() {
			return loadModelsDoneMsg{}
		}

		models, err := sess.ListModels(s.ctx())
		if err != nil {
			return loadModelsDoneMsg{err: err}
		}

		options := make([]chatcmd.ModelOption, 0, len(models))
		for _, model := range models {
			options = append(options, chatcmd.ModelOption{
				ID:          model.ID,
				Name:        model.Name,
				Description: model.Description,
			})
		}

		return loadModelsDoneMsg{models: options}
	}
}

// runAutojoin issues `JoinAutojoinChannels`. Focus restoration
// is the chat screen's concern; the connection screen just kicks
// off the joins and reports completion.
func (s ConnectionScreen) runAutojoin() tea.Cmd {
	sess := s.cfg.Session

	return func() tea.Msg {
		return joinAutojoinDoneMsg{err: sess.JoinAutojoinChannels(s.ctx())}
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

		return s, statusRefreshCmd(s.cfg.Session)

	case joinAutojoinDoneMsg:
		s.autojoinDone = true

		if msg.err != nil {
			s.markCurrentStepError(msg.err.Error())
		}

		return s, statusRefreshCmd(s.cfg.Session)

	case loadModelsDoneMsg:
		s.loadModelsDone = true
		s.loadedModels = msg.models
		s.loadModelsErr = msg.err

		if msg.err != nil {
			s.markCurrentStepError(fmt.Sprintf("Loading models: %s", msg.err))
		}

		return s, nil

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

	case "Loading models":
		if !s.loadModelsDone {
			if !s.loadModelsKicked {
				return s.kickLoadModels()
			}

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

// kickLoadModels starts the live-model load and re-arms the
// animation tick, mirroring [kickAutojoin]'s shape.
func (s ConnectionScreen) kickLoadModels() (ui.Model, tea.Cmd) {
	if s.cfg.Session == nil {
		s.loadModelsDone = true
		s.loadModelsKicked = true
		return s, s.tickCmd()
	}

	s.loadModelsKicked = true

	return s, tea.Batch(s.runLoadModels(), s.tickCmd())
}

func (s ConnectionScreen) tickCmd() tea.Cmd {
	return tea.Tick(stepDelay, func(time.Time) tea.Msg { return ConnectionTickMsg{} })
}

// transitionCmd hands control to the chat screen and, in the
// session-backed case, delivers the connect-time live-model load
// result so the chat screen can populate its tab-completion cache.
// The screen change must land first; the loaded-models message
// reaches the new screen via sequencing.
func (s ConnectionScreen) transitionCmd() tea.Cmd {
	if s.cfg.Next == nil {
		return nil
	}

	next := s.cfg.Next
	screenCmd := func() tea.Msg { return ui.ScreenMsg{Screen: next} }

	if s.cfg.Session == nil {
		return screenCmd
	}

	var deliverCmd tea.Cmd
	if s.loadModelsErr != nil {
		err := s.loadModelsErr
		deliverCmd = func() tea.Msg { return liveModelsLoadFailedMsg{err: err} }
	} else {
		models := s.loadedModels
		deliverCmd = func() tea.Msg { return liveModelsLoadedMsg{models: models} }
	}

	return tea.Sequence(screenCmd, deliverCmd)
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

	*s.paneEvents = append(*s.paneEvents, fresh...)
	s.paneCursor = fresh[len(fresh)-1].ID
}

func statusRefreshCmd(sess *session.Session) tea.Cmd {
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
