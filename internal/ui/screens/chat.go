package screens

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/modelmanager"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
	uitimestamp "github.com/laney/modeloff/internal/ui/timestamp"
	"github.com/laney/modeloff/internal/userclient"
)

// protocolEventMsg wraps a [protocol.Event] received from the
// user-client subscription's `Events()` channel. The protocol bus
// carries the wire-shaped events the chat-screen renders as IRC
// scrollback (joins, parts, messages, mode changes, etc.).
//
// `targets` carries the per-recipient channel list for
// actor-scoped events (Quit, NickChange) — the intersection
// [Session.fanOutProtocol] computed for this delivery, copied
// off the [protocol.Delivery] envelope so the handler can route
// the line into the user-client's open windows without consulting
// the wire payload. Nil for window-scoped events.
type protocolEventMsg struct {
	event   protocol.Event
	targets []domain.ChannelName
}

// NewProtocolEventForTest builds a [tea.Msg] that injects a
// protocol-bus delivery into the chat screen's update loop, mimicking
// the envelope shape the session's fan-out would have produced. The
// returned message is the only supported way to deliver an event into
// the chat screen from outside the `screens` package; tests should
// reach for it via [screenstest.SendProtocolEvent] rather than
// constructing wire events directly.
func NewProtocolEventForTest(evt protocol.Event, targets []domain.ChannelName) tea.Msg {
	return protocolEventMsg{event: evt, targets: targets}
}

// deliverNextPacedMsg triggers delivery of the next queued paced
// message for a specific channel. Per-channel scheduling means a
// burst of incoming messages on one channel cannot block another
// channel's messages behind its pacing delay.
type deliverNextPacedMsg struct {
	Channel domain.ChannelName
}

type liveModelsLoadedMsg struct {
	models []chatcmd.ModelOption
}

// liveModelsLoadFailedMsg is dispatched when `ListModels` fails. It
// carries the underlying error; the handler empties the live-model
// cache to degrade tab-completion gracefully, treats
// `modelclient.ErrNoAPIKey` as a silent no-op, and surfaces other
// failures as a `SystemNotice`.
type liveModelsLoadFailedMsg struct {
	err error
}

// UIStateStore persists client-side UX state across restarts. The
// chat screen depends only on this narrow surface so a test or
// embedded harness can pass `nil` to opt out of persistence
// without faking the whole store interface.
type UIStateStore interface {
	GetLastChannel(ctx context.Context) (domain.ChannelName, error)
	SetLastChannel(ctx context.Context, name domain.ChannelName) error
}

type logsUpdatedMsg struct{}

// dmWindowResolvedMsg carries the counterpart handle a held-back DM
// window was waiting on. `window` is the window key the events were
// held under, and `at` the time of the first of them, which the
// window takes as its creation time. A nil `counterpart` means the
// lookup failed and the held events are dropped: without the
// instance there is no window to render them in.
type dmWindowResolvedMsg struct {
	window      domain.ChannelName
	counterpart *domain.Instance
	at          time.Time
}

// dmWindowsRestoredMsg carries the counterparts of the DM windows
// the user had open when the process last ran, resolved from the
// client-owned record the user-client keeps. `landing` is the window
// the user left open, read in the same command so the Update
// goroutine waits on no store read; it names one of these windows
// when that is where the user left off, and something else when the
// user left off in a channel.
type dmWindowsRestoredMsg struct {
	counterparts []*domain.Instance
	landing      domain.ChannelName
}

// unreadCountedMsg carries the result of an unread-count query back
// to the Update goroutine. `visits` is the target window's visit
// count when the query was made; [ChatScreen.deliverUnreadCount]
// compares it with the window's current one before the badge is
// updated.
type unreadCountedMsg struct {
	channel domain.ChannelName
	count   int
	mention bool
	visits  int
}

// SessionReader is the read-only slice of the session the chat-screen
// depends on: backend state it renders (unread counts, the live
// instance set, the connect time) and the lookups the command context
// needs (window, nick, clock). The concrete `*session.Session`
// satisfies it; holding the interface keeps the frontend off the
// concrete backend type.
type SessionReader interface {
	GetWindow(ctx context.Context, name domain.ChannelName) (domain.Window, error)
	ResolveNick(ctx context.Context, nick domain.Nick) (*domain.Instance, error)

	// ResolveInstanceByID answers with the canonical handle for an
	// instance id. A DM window is addressed by its counterpart's id,
	// so this is what turns a window key back into the instance the
	// window is built around: for a DM arriving from someone the user
	// has no window for, and for the windows reopened at bootstrap.
	ResolveInstanceByID(ctx context.Context, id domain.InstanceID) (*domain.Instance, error)
	Now() time.Time
	UnreadCount(ctx context.Context, ch domain.ChannelName) (int, error)
	Instances(ctx context.Context) iter.Seq[*domain.Instance]
	ConnectedAt() time.Time
}

// ChatScreen is the main screen that composes Sidebar, ChatView, and
// MainLayout. It reads backend state through the narrow
// [SessionReader]. The `baseContext` supplier mirrors
// [net/http.Server.BaseContext]: each backend call asks the supplier
// for the current application context rather than capturing a
// snapshot at construction.
type ChatScreen struct {
	baseContext func() context.Context
	sess        SessionReader
	mgr         *modelmanager.Manager
	user        *userclient.UserClient
	client      protocol.Client
	cfgStore    config.Store
	uiState     UIStateStore
	layout      components.MainLayout
	keyMap      components.ChatScreenKeyMap

	channels        *set.Sorted[*Window]
	liveModels      []chatcmd.ModelOption
	liveModelsState command.SuggestionState
	parser          chatcmd.Parser
	// pacedQueue holds queued non-user incoming messages keyed by
	// channel. Each channel drains at its own paced cadence
	// (pacedInterval) independently, so a burst of messages in one
	// channel does not delay a message in another. A map value is
	// never stored empty — deliverNextPaced deletes the key when
	// the last entry is popped — so len(pacedQueue) is the count
	// of channels with pending work.
	pacedQueue map[domain.ChannelName][]domain.Message

	// pendingDM holds the events of a DM window the chat-screen has
	// no entry for yet, keyed by the counterpart's id. A DM names its
	// counterpart by id and the window is built around the instance
	// handle, which is a store read, so the first line of a
	// conversation arrives before there is anywhere to put it. It
	// waits here, in arrival order, until
	// [ChatScreen.handleDMWindowResolved] moves the whole queue into
	// the new window's scrollback. A map value is never stored empty:
	// a queue that was empty is what schedules the resolve, so one
	// lookup runs however many lines arrive behind it.
	pendingDM map[domain.ChannelName][]domain.Event

	// dispatching tracks the model instances currently in a turn.
	// Membership is per-instance so the nick list's thinking
	// indicator stays on for every concurrently-dispatching model
	// until each one completes. The map's lifetime matches
	// `ChatScreen`'s; mutations from value-receiver Update
	// handlers are visible to subsequent calls because maps are
	// reference types.
	dispatching map[*domain.Instance]bool

	width  int
	height int

	// active names the window the user is looking at. It is a plain
	// value: an `Update` arm that needs the window a command was
	// issued against reads it before building the `tea.Cmd`, so the
	// command carries the window the user typed in even if the user
	// switches away before Bubble Tea runs it.
	active domain.ChannelName

	// visible is the render-side handle to the same window. The
	// message list is built once, in [NewChatScreen], and reads the
	// visible scrollback through a closure bound then, so the name
	// it resolves has to outlive the screen value that closure was
	// built from. [ChatScreen.focus] writes it and `active`
	// together and is the only writer of either.
	visible *visibleWindow

	obs       *observability.Runtime
	summary   components.MetricsSummaryModel
	checklist WelcomeChecklist

	// logsBehind is true when a log record arrived while the
	// observability drawer was closed, so the drawer's copy of the
	// log buffer is older than the buffer itself. The next message
	// that finds the drawer open hands it the current contents.
	logsBehind bool

	// quitting is true between QuitRequestedMsg and QuitCompleteMsg
	// so subsequent quit signals are ignored and input remains
	// locked.
	quitting bool

	// highlightWords is the cached highlight-words slice read from
	// the config store at construction and refreshed when
	// [chatcmd.HighlightWordsSetResult] flows through Update.
	// Per-message and per-nick-change highlight checks read this
	// directly instead of doing a blocking SQLite load on the Tea
	// goroutine.
	highlightWords []string
}

// NewChatScreen creates a chat screen reading backend state through
// the given [SessionReader]. `baseContext` is the supplier the screen
// calls to obtain the application context for each backend operation;
// it must return ctxs that share a cancellation source so
// chat-screen-spawned goroutines wake on app shutdown.
//
// initialKind is the channel kind the chat view renders against
// until the first channel is focused. `&modeloff` is the default
// first view at app boot, so `domain.KindStatus` is the right value
// for application startup. Tests that immediately focus a real
// channel before the first frame pass `domain.KindStatus` too —
// `SetChannelMsg` supplies the real kind atomically on the first
// focus event.
func NewChatScreen(baseContext func() context.Context, sess SessionReader, mgr *modelmanager.Manager, user *userclient.UserClient, cfgStore config.Store, uiState UIStateStore, initialKind domain.ChannelKind) (ChatScreen, error) {
	channels := set.NewSorted[*Window]()
	visible := &visibleWindow{channels: channels}

	sidebar := components.NewChannelSidebar()
	chatView := components.NewChatView[chatcmd.CompletionContext](visible.content, "", initialKind, user.Nick(), "")
	layout := components.NewMainLayout(sidebar, chatView)
	layout.NickList = components.NewNickList(domain.NewMemberList())

	cs := ChatScreen{
		baseContext:     baseContext,
		sess:            sess,
		mgr:             mgr,
		user:            user,
		client:          user,
		cfgStore:        cfgStore,
		uiState:         uiState,
		channels:        channels,
		visible:         visible,
		liveModelsState: command.SuggestionStateReady,
		layout:          layout,
		keyMap:          components.DefaultChatScreenKeyMap,
		checklist:       NewWelcomeChecklist(user.Nick(), mgr.HasAPIKey()),
		pacedQueue:      map[domain.ChannelName][]domain.Message{},
		pendingDM:       map[domain.ChannelName][]domain.Event{},
		dispatching:     map[*domain.Instance]bool{},
	}

	parser, err := chatcmd.NewParser()
	if err != nil {
		return ChatScreen{}, err
	}

	cs.parser = parser

	cfg, err := cs.loadConfig()
	if err != nil {
		return ChatScreen{}, err
	}

	cs.highlightWords = cfg.HighlightWords

	return cs, nil
}

// focus moves the user to `ch` and returns the updated screen
// alongside the completer rebind that keeps tab-completion in step —
// the completion context resolves the active window and its kind, so
// suggestions bind to the window in view. An empty `ch` leaves the
// user with no window, which is the state the welcome checklist
// renders against.
//
// This is the only writer of the focused window: the `active` value
// every handler reads and every command closure captures, and the
// render-side handle the message list resolves, move together. It is
// therefore also where the read cursor moves. Both ends of the switch
// are marked read: what the window being left received while it was
// in view, and what the window being entered was already holding when
// the user arrived. Between them, the badge counts what landed in a
// window while the user was somewhere else.
func (s ChatScreen) focus(ch domain.ChannelName) (ChatScreen, tea.Cmd) {
	leaving := s.active

	s.active = ch
	s.visible.name = ch

	if w, ok := s.windowByName(ch); ok {
		w.Visits++
	}

	cmds := []tea.Cmd{s.rebindCompleter(), s.markReadCmd(ch)}
	if leaving != ch {
		cmds = append(cmds, s.markReadCmd(leaving))
	}

	return s, tea.Batch(cmds...)
}

// markReadCmd persists the user's read position in `ch`. The write
// runs off the Update goroutine; the badge the sidebar shows is
// cleared by the focus change itself, so nothing on screen waits for
// it. A failure costs a badge that over-counts until the window is
// next read, which is worth a log line and nothing more.
func (s ChatScreen) markReadCmd(ch domain.ChannelName) tea.Cmd {
	if ch == "" {
		return nil
	}

	return func() tea.Msg {
		ctx := s.baseContext()

		if err := s.user.MarkRead(ctx, ch); err != nil {
			slog.Default().WarnContext(ctx, "mark channel read",
				"component", "ui",
				"screen", "chat",
				"channel", ch,
				"error", err,
			)
		}

		return nil
	}
}

// setChannelCmd tells the chat view which window it is rendering,
// with that window's topic and kind. Every switch goes through here,
// after [ChatScreen.focus] has moved the user.
func (s ChatScreen) setChannelCmd() tea.Cmd {
	return msgCmd(components.SetChannelMsg{
		Channel: s.active,
		Topic:   s.activeTopic(),
		Kind:    s.activeKind(),
	})
}

// deliverUnreadCount hands a store-read unread count to the sidebar,
// unless the user has visited the window since the count was
// requested. Focusing a window clears its badge and moves the read
// cursor to the end of the channel, so a count read before that
// visit describes a state the user has already left behind, and
// applying it would put the badge back on a window they are reading.
func (s ChatScreen) deliverUnreadCount(msg unreadCountedMsg) (ChatScreen, tea.Cmd) {
	w, ok := s.windowByName(msg.channel)
	if !ok || w.Visits != msg.visits {
		return s, nil
	}

	return s, msgCmd(components.ChannelUnreadMsg{
		Channel: msg.channel,
		Count:   msg.count,
		Mention: msg.mention,
	})
}

// setLiveModels replaces the cached OpenRouter model catalogue and
// the suggestion state derived from the load that produced it,
// returning the completer rebind that publishes both to the input
// bar's popover.
func (s ChatScreen) setLiveModels(models []chatcmd.ModelOption, state command.SuggestionState) (ChatScreen, tea.Cmd) {
	s.liveModels = models
	s.liveModelsState = state
	s.checklist.modelCount = len(models)

	return s, s.rebindCompleter()
}

// rebindCompleter republishes the completion set to the input bar.
// [chatcmd.CompletionContext] reads the chat-screen through accessor
// closures; the ones that read a plain value field — the active
// window, its kind, the live-model cache — freeze that value when the
// closure is built, so a fresh set has to be published whenever one
// of them moves.
func (s ChatScreen) rebindCompleter() tea.Cmd {
	return msgCmd(components.CompleterMsg{Completer: s.completionSet()})
}

// realChannelCount returns the number of sidebar entries that are
// not the local `&modeloff` server view. The chat-screen owns
// `&modeloff` for the whole session, so it does not count against
// the "the user has joined nothing yet" check that drives the
// welcome checklist.
func (s ChatScreen) realChannelCount() int {
	n := s.channels.Len()
	if _, ok := s.windowByName(domain.StatusChannelName); ok {
		n--
	}

	return n
}

// firstRealChannel returns the first non-`&modeloff` window in
// sidebar order, used by post-part focus fallback. When no real
// channel remains, the caller falls through to the "no channels"
// branch which renders the welcome checklist.
func (s ChatScreen) firstRealChannel() (*Window, bool) {
	for i := range s.channels.Len() {
		w, ok := s.channels.GetAt(i)
		if !ok {
			continue
		}

		if w.Name() == domain.StatusChannelName {
			continue
		}

		return w, true
	}

	return nil, false
}

func (s ChatScreen) loadConfig() (config.Config, error) {
	if s.cfgStore == nil {
		return config.Config{
			HighlightWords: slices.Clone(config.DefaultHighlightWords),
		}, nil
	}

	return s.cfgStore.Load(s.baseContext())
}

// WithObservability wires local observability into the chat screen.
func (s ChatScreen) WithObservability(obs *observability.Runtime) ChatScreen {
	s.obs = obs
	s.summary = components.NewMetricsSummaryModel(s.baseContext, obs)

	chatView, ok := s.layout.Content.(components.ChatView[chatcmd.CompletionContext])
	if !ok {
		return s
	}

	workspace := components.NewChatWorkspace(chatView).
		WithMetrics(components.NewMetricsPane(s.baseContext, obs)).
		SetLogEntries(obs.LogBuffer().Entries())
	s.layout.Content = workspace

	return s
}

// Init implements ui.Model.
//
// The chat screen does not load channel state from storage.
// Sidebar entries, active channel, member lists, topics and
// scrollback all arrive via ordinary session events. Init starts
// the event drain, inserts the local `&modeloff` server view,
// and restores focus to the user's prior landing channel.
func (s ChatScreen) Init() tea.Cmd {
	cfg, _ := s.loadConfig()

	statusWindow := newWindow(domain.NewStatusWindow(s.sess.ConnectedAt()))
	s.channels.Insert(statusWindow)

	cmds := []tea.Cmd{
		s.listenForProtocolEvents(),
		msgCmd(components.ChannelAddedMsg{Channel: statusWindow.Window}),
		msgCmd(components.CommandsMsg[chatcmd.CompletionContext]{
			Commands: command.VisibleCommands(s.parser.Set(), s.client.Caps()),
		}),
		s.rebindCompleter(),
		msgCmd(components.HighlightWordsMsg{
			Words:    s.highlightWords,
			UserNick: s.user.Nick(),
		}),
		msgCmd(components.TimestampFormatMsg{
			Format: cfg.TimestampFormat,
			Locale: uitimestamp.CurrentLocale(),
		}),
		msgCmd(components.SetPlaceholderMsg{Text: s.checklist.Render()}),
	}

	// Bootstrap from session state. Direct constructions
	// (chat-screen as Root's initial screen, in tests) start
	// with the user already a member of any seeded channels;
	// the chat-screen pre-creates the matching `*Window`s here
	// stamped with their session-recorded join times, so the
	// arbiter in `handleChannelFocus` has somewhere to compare
	// against before the protocol bus has caught up with the
	// listener.
	cmds = append(cmds, s.bootstrapFromSession()...)
	cmds = append(cmds, s.restoreDMWindows())

	if s.obs != nil {
		cmds = append(cmds, s.summary.Init(), s.waitForLogUpdateCmd())
	}

	return tea.Batch(cmds...)
}

// bootstrapFromSession pre-seeds the channel cache and emits a focus
// event for the window the user should land in. The Window's
// `UserTime` is the session's recorded join time, so a focus event
// arriving later from the protocol bus with the same timestamp
// neither steals the focus nor loses it — the user's most recent
// deliberate channel wins.
func (s ChatScreen) bootstrapFromSession() []tea.Cmd {
	channels := s.user.Instance().Channels()
	if channels == nil || channels.Len() == 0 {
		return nil
	}

	var (
		cmds       []tea.Cmd
		joined     = map[domain.ChannelName]time.Time{}
		newestName domain.ChannelName
		newestTime time.Time
	)

	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		cw := domain.NewChannelWindow(pair.Key, pair.Value)
		w := newWindow(cw)
		w.UserTime = pair.Value
		s.channels.Insert(w)
		cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: cw}))

		joined[pair.Key] = pair.Value

		if pair.Value.After(newestTime) {
			newestTime = pair.Value
			newestName = pair.Key
		}
	}

	// The window the user left open last session is their standing
	// preference, so it beats the join times: it is stamped `now`,
	// which outranks every join-time-stamped proposal the autojoin
	// NAMES replies will make as the protocol bus drains. Anything
	// the user is not currently in falls back to the freshest join —
	// a first run with nothing recorded, or a channel since parted.
	// The fallback carries its own join time, so the matching NAMES
	// reply neither steals the focus nor loses it. A DM window is
	// not among the channels read here at all;
	// [ChatScreen.handleDMWindowsRestored] lands on one when that is
	// where the user left off.
	landing, at := newestName, newestTime

	if last, ok := s.restoredChannel(); ok {
		if _, open := joined[last]; open {
			landing, at = last, time.Now()
		}
	}

	if landing != "" {
		cmds = append(cmds, msgCmd(chatcmd.ChannelFocusMsg{Channel: landing, At: at}))
	}

	return cmds
}

// restoredChannel reads the window the user had open when they last
// quit. A screen built without a [UIStateStore] has no preference to
// restore, and a read failure is reported and treated the same way:
// the caller falls back to the freshest join.
func (s ChatScreen) restoredChannel() (domain.ChannelName, bool) {
	if s.uiState == nil {
		return "", false
	}

	last, err := s.uiState.GetLastChannel(s.baseContext())
	if err != nil {
		slog.Default().WarnContext(s.baseContext(), "read last channel",
			"component", "ui",
			"screen", "chat",
			"error", err,
		)

		return "", false
	}

	return last, last != ""
}

// listenForProtocolEvents reads the next delivery from the
// user-client subscription's protocol channel and wraps its event
// in a protocolEventMsg. The chat-screen does not consume the
// span context the delivery carries — that is for model-client
// dispatch goroutines to link their turn spans to the originating
// handler. After each delivery, this should be re-invoked so the
// channel is continuously drained.
func (s ChatScreen) listenForProtocolEvents() tea.Cmd {
	ch := s.client.Events()

	return func() tea.Msg {
		delivery, ok := <-ch
		if !ok {
			return nil
		}

		return protocolEventMsg{event: delivery.Event, targets: delivery.Targets}
	}
}

// Update implements ui.Model. It adapts the concrete screen the
// message handling produces to the [ui.Model] the router stores.
func (s ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	next, cmd := s.update(msg)

	return next, cmd
}

// update routes a message to its handler and returns the updated
// screen. Every arm and every handler returns the concrete
// `ChatScreen` by value, so a state change reaches the caller as a
// snapshot taken the moment the handler ran. A command built from
// that snapshot keeps the values its arm read there, even once Bubble
// Tea runs it later: it cannot observe whatever the screen holds by
// then, because it never held a pointer into the screen to begin
// with. The window records, the paced and pending-DM queues and the
// dispatching set stay shared: they are per-window and per-instance
// state with a lifetime longer than one message, and only this
// goroutine touches them.
func (s ChatScreen) update(msg tea.Msg) (ChatScreen, tea.Cmd) {
	forwardedMsg := msg
	summary, summaryCmd := s.summary.Update(msg)
	s.summary = summary

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.height = msg.Height
		forwardedMsg = tea.WindowSizeMsg{Width: msg.Width, Height: s.layoutHeight()}

	case protocolEventMsg:
		return s.handleProtocolEvent(msg)

	case ui.QuitRequestedMsg:
		return s.handleQuitRequested(msg)

	case ui.QuitCompleteMsg:
		return s, tea.Quit

	case chatcmd.HelpResult:
		return s, s.logAndShow(domain.Help{Target: s.active, At: time.Now()})

	case chatcmd.ClearResult:
		w, ok := s.windowByName(s.active)
		if !ok {
			return s, nil
		}

		w.Scrollback.Clear()

		return s, msgCmd(components.ScrollbackClearedMsg{Channel: s.active})

	case chatcmd.PokeRequested:
		return s, s.handlePoke()

	case chatcmd.TopicInfoResult:
		return s, s.logAndShow(domain.TopicInfo{
			Target:     msg.Window.Name(),
			Topic:      msg.Window.Topic,
			TopicSetBy: msg.Window.TopicSetBy,
			TopicSetAt: msg.Window.TopicSetAt,
			At:         time.Now(),
		})

	case chatcmd.ReplyEvents:
		return s, s.deliverReplyEvents(msg)

	case chatcmd.UsageError:
		return s, s.logAndShow(domain.UsageHint{
			Target: s.active, Command: msg.Command, Usage: msg.Usage, At: time.Now(),
		})

	case chatcmd.NoChannelError:
		usage := "join a channel first"
		if msg.Command == "part" {
			usage = "no channel to part from"
		}

		return s, s.logAndShow(domain.UsageHint{
			Command: msg.Command, Usage: usage, At: time.Now(),
		})

	case chatcmd.APIKeySetResult:
		text := "OpenRouter API key saved and activated."
		if msg.Reset {
			text = "OpenRouter API key cleared."
		}

		s.checklist.hasAPIKey = !msg.Reset

		var rebind tea.Cmd
		s, rebind = s.setLiveModels(nil, command.SuggestionStateReady)

		if s.realChannelCount() == 0 {
			return s, tea.Batch(
				rebind,
				s.loadLiveModels(),
				s.ensurePersonas(),
				msgCmd(components.SetPlaceholderMsg{
					Text: s.checklist.Render(),
				}),
			)
		}

		return s, tea.Batch(
			rebind,
			s.logAndShow(domain.SystemNotice{
				Target: s.active, Text: text, At: time.Now(),
			}),
			s.loadLiveModels(),
			s.ensurePersonas(),
		)

	case chatcmd.PokeIntervalSetResult:
		text := fmt.Sprintf("Poke interval set to %s.", humanDuration(msg.Interval))
		if msg.Reset {
			text = fmt.Sprintf("Poke interval reset to %s.", humanDuration(msg.Interval))
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.DrainTimeoutSetResult:
		text := fmt.Sprintf("Drain timeout set to %s.", humanDuration(msg.Timeout))
		if msg.Reset {
			text = fmt.Sprintf("Drain timeout reset to %s.", humanDuration(msg.Timeout))
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.SmallModelSetResult:
		text := fmt.Sprintf("Small model set to %s.", msg.ModelID)
		if msg.Reset {
			text = fmt.Sprintf("Small model reset to %s.", msg.ModelID)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.HighlightWordsSetResult:
		s.highlightWords = msg.Words

		text := fmt.Sprintf("Highlight words set to: %s.", humanWordList(msg.Words))
		if msg.Reset {
			text = fmt.Sprintf("Highlight words reset to: %s.", humanWordList(msg.Words))
		}

		return s, tea.Batch(
			s.logAndShow(domain.SystemNotice{
				Target: s.active,
				Text:   text,
				At:     time.Now(),
			}),
			msgCmd(components.HighlightWordsMsg{
				Words:    msg.Words,
				UserNick: s.user.Nick(),
			}),
		)

	case chatcmd.BaseURLSetResult:
		text := fmt.Sprintf("Base URL set to %s.", msg.URL)
		if msg.Reset {
			text = fmt.Sprintf("Base URL reset to %s.", msg.URL)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.EmbeddingModelSetResult:
		text := fmt.Sprintf("Embedding model set to %s.", msg.ModelID)
		if msg.Reset {
			text = fmt.Sprintf("Embedding model reset to %s.", msg.ModelID)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.PersonasListResult:
		personasList := domain.PersonasList{
			Personas: msg,
			At:       time.Now(),
		}

		return s, tea.Batch(
			s.logAndShow(personasList),
			s.recordReply(personasList),
		)

	case chatcmd.PersonasRegeneratedResult:
		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   fmt.Sprintf("Generated %d personas.", msg.Count),
			At:     time.Now(),
		})

	case chatcmd.PersonaSetResult:
		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   fmt.Sprintf("Persona %s saved.", msg.ID),
			At:     time.Now(),
		})

	case chatcmd.PersonaResetResult:
		return s, s.logAndShow(domain.SystemNotice{
			Target: s.active,
			Text:   fmt.Sprintf("Removed %d user-defined persona(s).", msg.Count),
			At:     time.Now(),
		})

	case chatcmd.TimestampFormatSetResult:
		var text string

		switch {
		case msg.Reset:
			text = "Timestamp format reset to the default 24-hour clock."
		case msg.Format != nil && *msg.Format != "":
			text = fmt.Sprintf("Timestamp format set to %s.", *msg.Format)
		default:
			text = "Timestamps disabled."
		}

		return s, tea.Batch(
			s.logAndShow(domain.SystemNotice{
				Target: s.active,
				Text:   text,
				At:     time.Now(),
			}),
			msgCmd(components.TimestampFormatMsg{
				Format: msg.Format,
				Locale: uitimestamp.CurrentLocale(),
			}),
		)

	case domain.Invited:
		// Echo path for the inviter's own `/invite` result. `session.handleInvite`
		// returns the resulting `Invited` in `protocol.Response.Events`,
		// which `chatcmd.sendCommand` delivers to the chat-screen via
		// `chatcmd.ReplyEvents`. The session bus does not deliver this event
		// back to the inviter, so this is the only way the inviter sees the
		// RPL_INVITING-equivalent line in scrollback. An invitation confers
		// no membership (RFC 2812 §3.2.7): the nick list gains the invitee
		// only when the JOIN arrives, if it ever does.
		return s, s.logAndShowOn(msg.Target, msg)

	case domain.SystemNotice:
		// Command-reply feedback path for the issuing client. A handler
		// such as `session.handleInvite` returns a `SystemNotice` (for a
		// failed `/invite`, "no such nick: <target>") in
		// `protocol.Response.Events`, which `chatcmd.sendCommand` delivers
		// via `chatcmd.ReplyEvents`. The session bus does not deliver this
		// notice back over the protocol feed, so this arm renders it on the
		// notice's own target channel.
		return s, s.logAndShowOn(msg.Target, msg)

	case domain.Whois:
		// Command-reply feedback path for the issuing client's `/whois`.
		// `session.handleWhois` returns the identity snapshot in
		// `protocol.Response.Events`; `chatcmd.sendCommand` delivers it via
		// `chatcmd.ReplyEvents`. The dispatcher stamps the snapshot's
		// `Target` with the window the command was issued from, so this arm
		// renders it there. A whois issued with no active window carries an
		// empty target; `logAndShow` routes it to `&modeloff`, matching the
		// other numeric replies.
		if msg.Target == "" {
			return s, s.logAndShow(msg)
		}
		return s, s.logAndShowOn(msg.Target, msg)

	case domain.ListReply:
		// Command-reply feedback path for the issuing client's `/list`.
		// `session.handleList` returns one `ListReply` per channel followed
		// by a closing `ListEnd` in `protocol.Response.Events`;
		// `chatcmd.sendCommand` delivers them in order via
		// `chatcmd.ReplyEvents`. Each renders on the active channel through
		// the generic bus-event path.
		return s, s.logAndShow(msg)

	case domain.ListEnd:
		return s, s.logAndShow(msg)

	case chatcmd.DMOpenedMsg:
		return s.handleDMOpenedMsg(msg)

	case chatcmd.DMClosedMsg:
		return s.handleDMClosedMsg(msg)

	case dmWindowResolvedMsg:
		return s.handleDMWindowResolved(msg)

	case dmWindowsRestoredMsg:
		return s.handleDMWindowsRestored(msg)

	case domain.ErrorEvent:
		return s.handleErrorEvent(msg)

	case chatcmd.ChannelFocusMsg:
		return s.handleChannelFocus(msg)

	case liveModelsLoadedMsg:
		return s.handleLiveModelsLoaded(msg)

	case liveModelsLoadFailedMsg:
		return s.handleLiveModelsLoadFailed(msg)

	case logsUpdatedMsg:
		s = s.updateLogEntries()
		return s, tea.Batch(summaryCmd, s.waitForLogUpdateCmd())

	case deliverNextPacedMsg:
		return s.deliverNextPaced(msg)

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case unreadCountedMsg:
		return s.deliverUnreadCount(msg)

	case components.MessageSubmitMsg:
		return s.handleMessageSubmit(msg)

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg)

	case tea.KeyMsg:
		if ui.Matches(msg, s.keyMap.ToggleNickList) {
			slog.Default().InfoContext(s.baseContext(), "keybind triggered",
				"component", "ui",
				"action", "toggle_nick_list",
				"key", msg.String(),
			)

			return s, msgCmd(components.NickListToggleMsg{})
		}
	}

	updated, cmd := s.layout.Update(forwardedMsg)
	s.layout = updated.(components.MainLayout)

	// The drawer's toggle key arrives here, so this is where a drawer
	// that was closed while records were arriving is caught up.
	if s.logsBehind {
		s = s.updateLogEntries()
	}

	return s, tea.Batch(summaryCmd, cmd)
}

// msgCmd wraps a message as a tea.Cmd so it flows through the Bubble
// Tea runtime rather than bypassing it with a direct Update call.
func msgCmd(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
}

// humanDuration renders d for a `/config` confirmation without Go's
// trailing zero-value units: time.Duration.String() would print
// "1h0m0s" for an hour, where a person reads "1h". Only the
// hour/minute/second components that matter for the durations
// `/config` accepts (poke-interval, drain-timeout) are considered;
// a sub-second remainder falls back to Duration's own String.
func humanDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}

	sign := ""
	whole := d
	if d < 0 {
		sign = "-"
		whole = -d
	}

	hours := whole / time.Hour
	remainder := whole - hours*time.Hour
	minutes := remainder / time.Minute
	remainder -= minutes * time.Minute
	seconds := remainder / time.Second
	remainder -= seconds * time.Second

	if remainder != 0 {
		return d.String()
	}

	var parts []string
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}

	return sign + strings.Join(parts, "")
}

// humanWordList renders a word list for a `/config` confirmation as
// a plain comma-separated sentence, not Go's `%v` slice rendering
// (e.g. "[alice bob $nick]").
func humanWordList(words []string) string {
	if len(words) == 0 {
		return "(none)"
	}

	return strings.Join(words, ", ")
}

// deliverReplyEvents re-delivers each confirmation event from a
// command's `protocol.Response.Events` as its own message, in
// dispatcher order, so each lands on its per-event render arm. The
// [tea.Sequence] preserves ordering — for `/list` the
// `domain.ListReply` rows render before the closing
// `domain.ListEnd`.
func (s ChatScreen) deliverReplyEvents(events chatcmd.ReplyEvents) tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(events))
	for _, event := range events {
		// The user-client's own chat traffic returns over the bus
		// (echo-message) and renders there. The reply path carries the
		// point-to-point numerics (Whois, ListReply, …).
		if _, ok := event.(domain.Message); ok {
			continue
		}

		cmds = append(cmds, msgCmd(event))
	}

	return tea.Sequence(cmds...)
}

// completionSet binds the grammar to the chat-screen's current state.
// The accessor closures capture this receiver, so the ones reading a
// plain value field — the visible window, its kind, the live-model
// cache — answer with the values held when the set was built.
// [ChatScreen.rebindCompleter] republishes the set when one moves.
func (s ChatScreen) completionSet() command.CompletionSet[chatcmd.CompletionContext] {
	return command.CompletionSet[chatcmd.CompletionContext]{
		Set:  s.parser.Set(),
		Caps: s.client.Caps(),
		Ctx: chatcmd.CompletionContext{
			Channels: func() iter.Seq[domain.Window] {
				return func(yield func(domain.Window) bool) {
					for w := range s.channels.All() {
						if !yield(w.Window) {
							return
						}
					}
				}
			},
			Instances:      func() iter.Seq[*domain.Instance] { return s.sess.Instances(s.baseContext()) },
			ChannelMembers: s.activeChannelInstances,
			ActiveMembers:  func() iter.Seq[domain.Nick] { return s.activeMemberNicks() },
			ActiveChannel:  func() domain.ChannelName { return s.active },
			UserNick:       func() domain.Nick { return s.user.Nick() },
			LiveModels: func() iter.Seq[chatcmd.ModelOption] {
				return slices.Values(s.liveModels)
			},
			LiveModelsState: func() command.SuggestionState {
				return s.liveModelsState
			},
			Personas: func() iter.Seq[domain.Persona] {
				personas, _ := s.mgr.ListPersonas(s.baseContext())
				return slices.Values(personas)
			},
			Kind: func() domain.ChannelKind { return s.activeKind() },
		},
	}
}

func (s ChatScreen) loadLiveModels() tea.Cmd {
	if !s.mgr.HasAPIKey() {
		return nil
	}

	return func() tea.Msg {
		models, err := s.mgr.ListModels(s.baseContext())
		if err != nil {
			return liveModelsLoadFailedMsg{err: err}
		}

		options := make([]chatcmd.ModelOption, 0, len(models))
		for _, model := range models {
			options = append(options, chatcmd.ModelOption{
				ID:          model.ID,
				Name:        model.Name,
				Description: model.Description,
			})
		}

		return liveModelsLoadedMsg{models: options}
	}
}

// ensurePersonas seeds the persona pool in the background so the
// next `--persona` tab-completion has something to offer. Best-
// effort: a failure is logged and discarded, since the persona pool
// is not required for any user-visible flow except completion.
func (s ChatScreen) ensurePersonas() tea.Cmd {
	if !s.mgr.HasAPIKey() {
		return nil
	}

	return func() tea.Msg {
		ctx := s.baseContext()

		if err := s.mgr.EnsurePersonas(ctx); err != nil {
			slog.Default().WarnContext(ctx, "ensure personas",
				"component", "ui",
				"screen", "chat",
				"error", err,
			)
		}

		return nil
	}
}

func (s ChatScreen) layoutHeight() int {
	if s.width < theme.MinTerminalWidth {
		return s.height
	}

	return max(s.height-lipgloss.Height(components.RenderStatusBar(s.width, s.KeyBindings(), s.StatusItems())), 0)
}

// logAndShow renders a numeric or UI-feedback event in the active
// window's in-memory scrollback. These are the issuing client's
// command replies and UI notices; they are transient by design and
// never reach the shared channel log, which holds only genuine
// channel activity that a model later loads.
//
// When no channel is active the user is on the welcome screen with
// no channels. The output is routed to `&modeloff` and a trailing
// `ChannelFocusMsg` brings that window into focus so the user sees
// the response — the focus handler is the one place that moves the
// user, so the routing decision here stays a pure read.
func (s ChatScreen) logAndShow(event domain.Event) tea.Cmd {
	if s.active != "" {
		return s.logAndShowOn(s.active, event)
	}

	return tea.Batch(
		s.logAndShowOn(domain.StatusChannelName, event),
		msgCmd(chatcmd.ChannelFocusMsg{Channel: domain.StatusChannelName, At: time.Now()}),
	)
}

// logAndShowOn renders a numeric or UI-feedback event in the
// scrollback of the explicit target window. Callers use this when the
// event's home is not the currently-focused window — for example a
// notice carrying its own target channel, or a `/whois` reply the
// dispatcher stamped with the window it was issued from. The append
// happens on the Update goroutine (the single writer of chat-screen
// state); the returned `ScrollbackUpdatedMsg` nudges the message list
// to re-evaluate the active window's scrollback.
//
// An empty `ch` carries no window to render in, so the event is
// dropped.
func (s ChatScreen) logAndShowOn(ch domain.ChannelName, event domain.Event) tea.Cmd {
	if ch == "" {
		return nil
	}

	s.appendToScrollback(ch, event)

	return msgCmd(components.ScrollbackUpdatedMsg{Channel: ch})
}

// handleQuitRequested locks the UI, shows a "Disconnecting…"
// indication, and runs the backend quit asynchronously. The result
// arrives as a QuitCompleteMsg, which the screen turns into
// tea.Quit.
func (s ChatScreen) handleQuitRequested(msg ui.QuitRequestedMsg) (ChatScreen, tea.Cmd) {
	if s.quitting {
		// A second quit request while the first is in flight is an
		// escape hatch: the user pressed Ctrl+C again because the
		// disconnect looks stuck. Bypass Session.Quit and exit now.
		return s, tea.Quit
	}

	s.quitting = true

	message := msg.Message

	// The "Disconnecting…" feedback comes from the status item that
	// StatusItems appends when s.quitting is true; the status bar is
	// always rendered when the terminal is wide enough, so no
	// placeholder fallback is needed.
	cmds := []tea.Cmd{
		msgCmd(components.InputLockedMsg{Locked: true}),
		func() tea.Msg {
			resp, err := s.user.Send(s.baseContext(), protocol.Quit{Reason: message})
			if err == nil {
				err = resp.Err
			}
			return ui.QuitCompleteMsg{Err: err}
		},
	}

	return s, tea.Batch(cmds...)
}

func (s ChatScreen) switchChannel(ch domain.ChannelName) tea.Cmd {
	_, exists := s.windowByName(ch)

	return func() tea.Msg {
		// Existing channels: pure frontend state transition. The
		// session call is needed only to create/join a brand-new
		// channel; for ones already in our local cache, switching
		// view is a buffer swap, not a backend round-trip.
		if !exists {
			if err := s.user.Join(s.baseContext(), ch); err != nil {
				return domain.ErrorEvent{Operation: "switch", Err: err, At: time.Now()}
			}
		}

		return chatcmd.ChannelFocusMsg{Channel: ch, At: time.Now()}
	}
}

// handleMessageSubmit dispatches a user-typed chat line. With no
// active channel the user is on the welcome screen; the hint
// directs them to join one. `&modeloff` is server-narrated only
// and refuses chat with a hint that points at the right command.
// Everything else flows through to the user-client's
// [userclient.UserClient.SendMessage].
//
// The target window is read here, on the Update goroutine, and handed
// to [ChatScreen.sendMessageCmd] as a value, so a channel switch
// between the submit and Bubble Tea running the command cannot
// redirect the line the user typed.
func (s ChatScreen) handleMessageSubmit(msg components.MessageSubmitMsg) (ChatScreen, tea.Cmd) {
	if s.active == "" {
		return s, s.logAndShow(domain.UsageHint{
			Usage: "join a channel first", At: time.Now(),
		})
	}

	if s.active == domain.StatusChannelName {
		return s, s.logAndShow(domain.UsageHint{
			Command: "send",
			Usage:   "the status channel doesn't take messages — try /msg <nick-or-#channel> instead",
			At:      time.Now(),
		})
	}

	return s, s.sendMessageCmd("send", s.active, msg.Text)
}

// KeyBindings implements ui.Keybinding.
func (s ChatScreen) KeyBindings() []ui.KeyBinding {
	bindings := ui.CollectKeyBindings(s.layout)
	bindings = append(bindings, s.keyMap.ToggleNickList, ui.DefaultAppKeyMap.Quit)

	return bindings
}

// disconnectingStatusItem is the always-visible feedback the chat and
// connection screens emit while a quit is in flight, so the user
// sees something happening even if Session.Quit takes a moment.
var disconnectingStatusItem = ui.StatusItem{
	ID:       "disconnecting",
	Side:     ui.StatusSideRight,
	Priority: 100,
	Full:     "Disconnecting…",
	Compact:  "off…",
}

// StatusItems implements ui.StatusProvider.
func (s ChatScreen) StatusItems() []ui.StatusItem {
	items := ui.CollectStatusItems(s.layout, s.summary)

	if s.quitting {
		items = append(items, disconnectingStatusItem)
	}

	return items
}

// View implements ui.Model.
func (s ChatScreen) View(width, height int) string {
	if width < theme.MinTerminalWidth {
		return s.layout.View(width, height)
	}

	bar := components.RenderStatusBar(width, s.KeyBindings(), s.StatusItems())
	layoutHeight := height - lipgloss.Height(bar)
	view := s.layout.View(width, max(layoutHeight, 0))
	if bar == "" {
		return view
	}

	return lipgloss.JoinVertical(lipgloss.Left, view, bar)
}

func (s ChatScreen) waitForLogUpdateCmd() tea.Cmd {
	if s.obs == nil {
		return nil
	}

	ch := s.obs.LogBuffer().Updates()

	return func() tea.Msg {
		_, ok := <-ch
		if !ok {
			return nil
		}

		return logsUpdatedMsg{}
	}
}

// updateLogEntries hands the log buffer's current contents to the
// observability drawer, which renders every entry it is given.
//
// A closed drawer takes nothing and is noted as behind instead. The
// application logs several records per command and the drawer is
// closed almost all the time, so rendering every entry into a pane
// nobody can see would be the largest cost of writing a log line.
// [ChatScreen.update] catches the drawer up on the message that opens
// it.
func (s ChatScreen) updateLogEntries() ChatScreen {
	if s.obs == nil {
		return s
	}

	workspace, ok := s.layout.Content.(components.ChatWorkspace[chatcmd.CompletionContext])
	if !ok {
		return s
	}

	if !workspace.Open {
		s.logsBehind = true

		return s
	}

	s.logsBehind = false
	s.layout.Content = workspace.SetLogEntries(s.obs.LogBuffer().Entries())

	return s
}
