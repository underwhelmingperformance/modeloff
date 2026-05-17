package screens

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/theme"
)

// stepDelay is the time between each connection step appearing.
const stepDelay = 400 * time.Millisecond

// stepGate identifies the async signal a step waits on before the
// animation tick allows it to advance. Steps with [gateNone] are
// pure visual placeholders that progress on every tick.
type stepGate int

const (
	gateNone stepGate = iota
	gateConnect
	gateLoadModels
	gateAutojoin
)

// connectionStep describes one line in the connection sequence.
// `gate` is the data-driven version of what used to be a
// label-matched switch inside `advanceTick`: adding a fourth
// gated phase is now a new `stepGate` value plus a step entry,
// no control-flow change.
type connectionStep struct {
	label  string
	status stepStatus
	gate   stepGate
}

type stepStatus int

const (
	stepPending stepStatus = iota
	stepDone
	stepError
)

// ConnectionTickMsg advances the connection sequence by one step.
type ConnectionTickMsg struct{}

// connectionReadyMsg is sent when `Session.Connect` returns.
type connectionReadyMsg struct{ err error }

// joinAutojoinDoneMsg is sent when `Session.JoinAutojoinChannels`
// returns.
type joinAutojoinDoneMsg struct{ err error }

// loadModelsDoneMsg carries the result of the connect-time
// `sess.ListModels` call. `models` is nil when no API key is
// configured (a silent no-op rather than an error).
type loadModelsDoneMsg struct {
	models []chatcmd.ModelOption
	err    error
}

// ConnectionConfig holds the inputs the connection screen needs to
// determine what to show.
type ConnectionConfig struct {
	HasAPIKey    bool
	ChannelCount int
	Nick         string

	// Session is the backend handle the screen drives during the
	// connection handshake. When nil the screen runs in animation-only
	// mode (used by tests that only care about the visual sequence).
	Session *session.Session

	// Ctx is the application context used for backend calls. Defaults
	// to context.Background when zero, but production callers should
	// supply the application context so cancellation propagates.
	Ctx context.Context
}

// ConnectionScreen runs the IRC-style startup animation while the
// real chat-screen quietly accumulates state behind it. Every
// framework message is forwarded to `chatScreen` unconditionally
// — its protocol-bus and session-bus listeners subscribe from the
// start, so by the time the animation finishes and the connection
// screen emits [ui.ScreenMsg] to swap itself out, the chat-screen
// already holds the full handshake result (sidebar populated,
// `&modeloff` carrying the Welcome notice and any typed errors,
// paced replies queued, and so on).
//
// The async pipeline runs at its natural pace: `Init` fires
// `Session.Connect` and `Session.ListModels` in parallel; the
// `connectionReadyMsg` handler arms `JoinAutojoinChannels` (the
// only sequenced edge in the graph, since unclean-recovery must
// clear stale memberships before autojoin re-adds them). The
// animation walks `s.steps` at `stepDelay` cadence, and each step
// that carries a [stepGate] holds the cur cursor until the matching
// async signal has arrived.
type ConnectionScreen struct {
	cfg        ConnectionConfig
	chatScreen ui.Model
	steps      []connectionStep
	cur        int
	done       bool

	connected      bool
	autojoinDone   bool
	loadModelsDone bool
	loadedModels   []chatcmd.ModelOption
	loadModelsErr  error
}

// NewConnectionScreen creates a connection screen that wraps the
// supplied chat-screen during the handshake animation. The
// chat-screen is initialised alongside the connection screen and
// receives every message until the animation completes, at which
// point Root swaps it in as the active screen.
func NewConnectionScreen(cfg ConnectionConfig, chatScreen ui.Model) ConnectionScreen {
	steps := []connectionStep{
		{label: "Connecting to modeloff", gate: gateConnect},
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
			connectionStep{label: "Loading models", gate: gateLoadModels},
			connectionStep{label: "Joining channels", gate: gateAutojoin},
			connectionStep{label: fmt.Sprintf("Welcome, %s", cfg.Nick)},
		)
	}

	s := ConnectionScreen{
		cfg:        cfg,
		chatScreen: chatScreen,
		steps:      steps,
	}

	// Animation-only mode (no Session): pretend the async signals
	// have already arrived so each gated step advances on its tick.
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

// Init implements ui.Model. The connection screen's own async
// pipeline fires immediately, alongside the chat-screen's `Init`.
// The chat-screen subscribes to the session and protocol buses
// here, so the events the handshake produces accumulate into its
// state instead of piling up unconsumed.
func (s ConnectionScreen) Init() tea.Cmd {
	cmds := []tea.Cmd{s.tickCmd()}

	if s.cfg.Session != nil {
		cmds = append(cmds, s.runConnect(), s.runLoadModels())
	}

	if s.chatScreen != nil {
		cmds = append(cmds, s.chatScreen.Init())
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

// Update implements ui.Model. Every message is forwarded to the
// chat-screen unconditionally — the connection screen has no
// opinion on what the chat-screen wants to see, and the chat-
// screen is the one that decides how to react to wire-shape
// events, app-wide signals, framework messages, and anything
// else. The connection-screen-internal messages (ticks and the
// handshake-completion signals) also drive the animation state
// here, in parallel.
func (s ConnectionScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	var ownCmd tea.Cmd

	switch m := msg.(type) {
	case ConnectionTickMsg:
		s, ownCmd = s.advanceTick(m)

	case connectionReadyMsg:
		s.connected = true
		if m.err != nil {
			s.markCurrentStepError(m.err.Error())
		} else if s.cfg.Session != nil {
			// Connect → autojoin is the only sequenced edge in
			// the async graph: unclean-recovery clears stale
			// memberships from a prior session before autojoin
			// re-adds them. Arm the autojoin Cmd here rather than
			// gating it on the animation reaching the
			// corresponding step.
			ownCmd = s.runAutojoin()
		}

	case joinAutojoinDoneMsg:
		s.autojoinDone = true
		if m.err != nil {
			s.markCurrentStepError(m.err.Error())
		}

	case loadModelsDoneMsg:
		s.loadModelsDone = true
		s.loadedModels = m.models
		s.loadModelsErr = m.err
		if m.err != nil {
			s.markCurrentStepError(fmt.Sprintf("Loading models: %s", m.err))
		}
	}

	if s.chatScreen != nil {
		child, childCmd := s.chatScreen.Update(msg)
		s.chatScreen = child

		return s, tea.Batch(ownCmd, childCmd)
	}

	return s, ownCmd
}

// advanceTick moves the visible animation forward one step. When
// the current step carries a gate that hasn't closed yet, the tick
// re-arms without advancing. Once `cur` reaches the end the screen
// transitions — which by construction means every gated signal has
// already landed.
func (s ConnectionScreen) advanceTick(_ ConnectionTickMsg) (ConnectionScreen, tea.Cmd) {
	if s.cur >= len(s.steps) {
		return s, s.transitionCmd()
	}

	current := s.steps[s.cur]

	if !s.gateSatisfied(current.gate) {
		return s, s.tickCmd()
	}

	if current.status == stepPending {
		s.steps[s.cur].status = stepDone
	}

	s.cur++

	if s.cur >= len(s.steps) {
		s.done = true

		return s, s.transitionCmd()
	}

	return s, s.tickCmd()
}

// gateSatisfied reports whether the async signal a step waits on
// has arrived. Steps with [gateNone] always advance.
func (s ConnectionScreen) gateSatisfied(g stepGate) bool {
	switch g {
	case gateConnect:
		return s.connected
	case gateLoadModels:
		return s.loadModelsDone
	case gateAutojoin:
		return s.autojoinDone
	}

	return true
}

func (s ConnectionScreen) tickCmd() tea.Cmd {
	return tea.Tick(stepDelay, func(time.Time) tea.Msg { return ConnectionTickMsg{} })
}

// transitionCmd hands control to the chat-screen. The chat-screen
// has been receiving forwarded messages throughout the animation
// and holds the full handshake state already; Root just swaps
// which model owns the visible area. The live-model load result
// is delivered after the transition so the chat screen can
// populate its tab-completion cache from the welcomed state.
func (s ConnectionScreen) transitionCmd() tea.Cmd {
	if s.chatScreen == nil {
		return nil
	}

	next := s.chatScreen
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

// View implements ui.Model. The animation owns the visible area
// for the connection screen's whole lifetime; the wrapped chat-
// screen only becomes visible after [ui.ScreenMsg] swaps it in.
func (s ConnectionScreen) View(width, height int) string {
	if width < theme.MinTerminalWidth {
		return theme.NarrowTerminalView(width, height)
	}

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, s.renderAnimation())
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
