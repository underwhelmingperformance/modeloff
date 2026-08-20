package screens

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
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

// UIStateStore persists client-side UX state across restarts. The
// chat screen depends only on this narrow surface so a test or
// embedded harness can pass `nil` to opt out of persistence
// without faking the whole store interface.
type UIStateStore interface {
	GetLastChannel(ctx context.Context) (domain.ChannelName, error)
	SetLastChannel(ctx context.Context, name domain.ChannelName) error
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

	// DirectoryChannels answers with the channels `issuer` may be
	// told exist, the same [Session.channelVisibleTo] predicate
	// `/list` answers under. `completionSet` calls it with the
	// user's own instance so `/join` completion can offer a channel
	// the user has not joined without bypassing that predicate; the
	// user-client holds `+o`, so in practice it sees every channel.
	DirectoryChannels(ctx context.Context, issuer *domain.Instance) ([]domain.ChannelDirectoryEntry, error)
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

	// apiKeyMissing mirrors whether the persisted config currently
	// carries an API key, independent of channel state: the welcome
	// checklist only renders while no channel is open, so it alone
	// would let a still-unconfigured key go unnoticed once the user
	// joins something. StatusItems appends noAPIKeyStatusItem while
	// this is true, so the prompt to run /config stays visible
	// however many channels are open. NewChatScreen subscribes to
	// cfgStore's OnChange so any config write keeps this current, not
	// only the /config api-key command path; listenForAPIKeyChanges
	// is what reads those updates off apiKeyChanges, and unsubscribes
	// via unsubscribeAPIKeyChanges once baseContext is done.
	apiKeyMissing            bool
	apiKeyChanges            <-chan bool
	unsubscribeAPIKeyChanges config.UnsubscribeFunc

	// highlightWords is the cached highlight-words slice read from
	// the config store at construction and refreshed when
	// [chatcmd.HighlightWordsSetResult] flows through Update.
	// Per-message and per-nick-change highlight checks read this
	// directly instead of doing a blocking SQLite load on the Tea
	// goroutine.
	highlightWords []string

	// configRecoveryBackup is the backup path main.go's loadConfig
	// moved an unreadable config.json to, set via
	// WithConfigRecoveryNotice. Empty when config.json loaded
	// normally. The recovery happens before the session or the chat
	// screen exists, so there is nothing to emit a protocol event
	// through it; Init renders it into `&modeloff` directly, once,
	// the same way Welcome and Reconnected would if they had already
	// been on the bus by the time this screen started listening.
	configRecoveryBackup string
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

	apiKeyChanges := make(chan bool, 1)

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
		apiKeyMissing:   !mgr.HasAPIKey(),
		apiKeyChanges:   apiKeyChanges,
		pacedQueue:      map[domain.ChannelName][]domain.Message{},
		pendingDM:       map[domain.ChannelName][]domain.Event{},
		dispatching:     map[*domain.Instance]bool{},
	}

	cs.unsubscribeAPIKeyChanges = watchAPIKey(cfgStore, apiKeyChanges)

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

func (s ChatScreen) loadConfig() (config.Config, error) {
	if s.cfgStore == nil {
		return config.Config{
			HighlightWords: slices.Clone(config.DefaultHighlightWords),
		}, nil
	}

	return s.cfgStore.Load(s.baseContext())
}

// WithConfigRecoveryNotice records that the persisted config.json was
// unreadable and got moved aside to backupPath before this screen was
// constructed (see main.go's loadConfig), so Init can surface it as a
// boot-time `&modeloff` notice. An empty backupPath is a no-op —
// config.json loaded normally, so there is nothing to report.
func (s ChatScreen) WithConfigRecoveryNotice(backupPath string) ChatScreen {
	s.configRecoveryBackup = backupPath

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

	if s.configRecoveryBackup != "" {
		s.appendStatusNotice(time.Now(), fmt.Sprintf(
			"your config file could not be read; it was backed up to %s and defaults were used",
			s.configRecoveryBackup,
		))
	}

	cmds := []tea.Cmd{
		s.listenForProtocolEvents(),
		s.listenForAPIKeyChanges(),
		msgCmd(components.ChannelAddedMsg{Channel: statusWindow.Window}),
		msgCmd(components.CommandsMsg[chatcmd.CompletionContext]{
			Commands: command.VisibleCommands(s.parser.Set(), s.client.Caps()),
		}),
		msgCmd(components.SecretCheckerMsg{Checker: s.parser}),
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

// Update implements ui.Model. It adapts the concrete screen the
// message handling produces to the [ui.Model] the router stores.
func (s ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	next, cmd := s.update(msg)

	return next, cmd
}

// update gives every message to the metrics summary, then to the
// router, and returns the two commands together. The summary reads
// every message the screen sees and answers a refresh with the command
// that schedules the next one, so that command has to survive whatever
// the router decides to do with the same message.
//
// Every arm and every handler returns the concrete `ChatScreen` by
// value, so a state change reaches the caller as a snapshot taken the
// moment the handler ran. A command built from that snapshot keeps the
// values its arm read there, even once Bubble Tea runs it later: it
// cannot observe whatever the screen holds by then, because it never
// held a pointer into the screen to begin with. The window records,
// the paced and pending-DM queues and the dispatching set stay shared:
// they are per-window and per-instance state with a lifetime longer
// than one message, and only this goroutine touches them.
func (s ChatScreen) update(msg tea.Msg) (ChatScreen, tea.Cmd) {
	summary, summaryCmd := s.summary.Update(msg)
	s.summary = summary

	next, cmd := s.route(msg)

	return next, tea.Batch(summaryCmd, cmd)
}

// route offers a message to each group of handlers in turn and stops
// at the one that claims it. The groups are the concerns the chat
// screen answers for, each in its own file:
//
//   - routeSessionEvents: what the server sent (chat_events.go);
//   - routeWindows: which window the user is in (chat_focus.go,
//     chat_dm.go);
//   - routeInput: what the user typed (chat_commands.go);
//   - routeReplies: what a command answered (chat_replies.go);
//   - routeConfigResults: what a setting changed to (chat_config.go);
//   - routeCatalogue: the model list (chat_catalogue.go);
//   - routeLifecycle: quitting and the API key (chat_lifecycle.go);
//   - routeObservability: the log drawer (chat_observability.go).
//
// No message belongs to two groups, so the order of the checks decides
// nothing: it is there for someone reading the file top to bottom.
// Anything unclaimed belongs to the layout below.
func (s ChatScreen) route(msg tea.Msg) (ChatScreen, tea.Cmd) {
	if next, cmd, ok := s.routeSessionEvents(msg); ok {
		return next, cmd
	}

	if next, cmd, ok := s.routeWindows(msg); ok {
		return next, cmd
	}

	if next, cmd, ok := s.routeInput(msg); ok {
		return next, cmd
	}

	if next, cmd, ok := s.routeReplies(msg); ok {
		return next, cmd
	}

	if next, cmd, ok := s.routeConfigResults(msg); ok {
		return next, cmd
	}

	if next, cmd, ok := s.routeCatalogue(msg); ok {
		return next, cmd
	}

	if next, cmd, ok := s.routeLifecycle(msg); ok {
		return next, cmd
	}

	if next, cmd, ok := s.routeObservability(msg); ok {
		return next, cmd
	}

	return s.forwardToLayout(msg)
}

// forwardToLayout gives a message the chat screen does not answer
// itself to the layout below it. A resize is recorded on the way
// through and the layout is given the height left over once the status
// bar this screen draws has taken its row.
func (s ChatScreen) forwardToLayout(msg tea.Msg) (ChatScreen, tea.Cmd) {
	if size, ok := msg.(tea.WindowSizeMsg); ok {
		s.width = size.Width
		s.height = size.Height
		msg = tea.WindowSizeMsg{Width: size.Width, Height: s.layoutHeight()}
	}

	updated, cmd := s.layout.Update(msg)
	s.layout = updated.(components.MainLayout)

	// The drawer's toggle key arrives here, so this is where a drawer
	// that was closed while records were arriving is caught up.
	if s.logsBehind {
		s = s.updateLogEntries()
	}

	return s, cmd
}

// msgCmd wraps a message as a tea.Cmd so it flows through the Bubble
// Tea runtime rather than bypassing it with a direct Update call.
func msgCmd(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
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
			Instances:      s.otherInstances,
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
			Directory: func() iter.Seq[domain.ChannelDirectoryEntry] {
				entries, err := s.sess.DirectoryChannels(s.baseContext(), s.user.Instance())
				if err != nil {
					slog.Default().ErrorContext(s.baseContext(), "directory channels for completion",
						"component", "ui",
						"screen", "chat",
						"error", err,
					)
					return func(func(domain.ChannelDirectoryEntry) bool) {}
				}

				return slices.Values(entries)
			},
		},
	}
}

func (s ChatScreen) layoutHeight() int {
	if s.width < theme.MinTerminalWidth {
		return s.height
	}

	return max(s.height-lipgloss.Height(components.RenderStatusBar(s.width, s.KeyBindings(), s.StatusItems())), 0)
}

// KeyBindings implements ui.Keybinding.
func (s ChatScreen) KeyBindings() []ui.KeyBinding {
	bindings := ui.CollectKeyBindings(s.layout)
	bindings = append(bindings, s.keyMap.ToggleNickList, ui.DefaultAppKeyMap.Quit)

	return bindings
}

// StatusItems implements ui.StatusProvider.
func (s ChatScreen) StatusItems() []ui.StatusItem {
	items := ui.CollectStatusItems(s.layout, s.summary)

	if s.apiKeyMissing {
		items = append(items, noAPIKeyStatusItem)
	}

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
