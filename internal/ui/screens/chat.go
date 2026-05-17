package screens

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/config"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
	uitimestamp "github.com/laney/modeloff/internal/ui/timestamp"
)

// sessionEventMsg wraps a domain.Event received from the
// session's non-protocol UI bus (`Session.Events()`): config
// changes, focus changes, model-reply rendering, error events,
// system notices, and other live-only events that don't satisfy
// [domain.ProtocolEvent]. Using a dedicated wrapper prevents the
// events channel listener from being re-invoked when the same
// underlying types are sent directly as tea.Msg.
type sessionEventMsg struct {
	event domain.Event
}

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

// actorChannelsForDirectSend returns the actor's full channel
// list as a stand-in for the per-recipient
// [protocol.Delivery.Targets] envelope when an actor-scoped
// event reaches the chat-screen's [tea.Model.Update] without
// having gone through [Session.fanOutProtocol] — the test
// `tm.Send(domain.Quit{...})` shortcut. The user-client always
// receives the actor's full channel list anyway (no intersection
// is applied for it; see `intersectActorTargets`), so this
// matches the production-shaped envelope that path would
// produce.
func actorChannelsForDirectSend(actor *domain.Instance) []domain.ChannelName {
	if actor == nil {
		return nil
	}

	channels := actor.Channels()
	if channels == nil {
		return nil
	}

	out := make([]domain.ChannelName, 0, channels.Len())
	for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
		out = append(out, pair.Key)
	}

	return out
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
// carries the underlying error; the handler empties `*s.liveModels`
// to degrade tab-completion gracefully, treats `session.ErrNoAPIKey`
// as a silent no-op, and surfaces other failures as a
// `SystemNotice`.
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

// PokeTickMsg triggers a background poke cycle for model instances.
type PokeTickMsg struct{}

// ChatScreen is the main screen that composes Sidebar, ChatView, and
// MainLayout. It holds a reference to the session for backend
// operations.
type ChatScreen struct {
	ctx      context.Context
	sess     *session.Session
	client   protocol.Client
	cfgStore config.Store
	uiState  UIStateStore
	layout   components.MainLayout
	keyMap   components.ChatScreenKeyMap

	channels        *set.Sorted[*Window]
	liveModels      *[]chatcmd.ModelOption
	liveModelsState *command.SuggestionState
	parser          chatcmd.Parser
	completer       command.Completable
	// pacedQueue holds queued non-user incoming messages keyed by
	// channel. Each channel drains at its own paced cadence
	// (pacedInterval) independently, so a burst of messages in one
	// channel does not delay a message in another. A map value is
	// never stored empty — deliverNextPaced deletes the key when
	// the last entry is popped — so len(pacedQueue) is the count
	// of channels with pending work.
	pacedQueue map[domain.ChannelName][]domain.Message

	// dispatching tracks the model instances currently in a turn.
	// Membership is per-instance rather than per-channel so two
	// models running concurrently in the same window survive each
	// other's `ModelDispatchDone` — the pending spinner stays on
	// until the last instance completes. The map's lifetime
	// matches `ChatScreen`'s; mutations from value-receiver Update
	// handlers are visible to subsequent calls because maps are
	// reference types.
	dispatching map[*domain.Instance]bool

	// scrollbackMu guards reads of [Window.Scrollback] from
	// goroutines other than Update — message-list rendering on a
	// teardown frame may overlap with a final append from a
	// background Cmd. Writes happen from the Update goroutine
	// via [appendToScrollback] (live event-bus traffic) and from
	// the `logAndShowOn` Cmd goroutine (chat-screen-authored
	// events). The mutex pointer is shared across value-receiver
	// copies of `ChatScreen`.
	scrollbackMu *sync.RWMutex

	width     int
	height    int
	active    *domain.ChannelName
	obs       *observability.Runtime
	summary   components.MetricsSummaryModel
	checklist WelcomeChecklist

	// quitting is true between QuitRequestedMsg and QuitCompleteMsg
	// so subsequent quit signals are ignored and input remains
	// locked.
	quitting bool
}

// NewChatScreen creates a chat screen backed by the given session.
// The provided context is used for all backend operations, allowing
// them to be cancelled on shutdown.
//
// initialKind is the channel kind the chat view renders against
// until the first channel is focused. `&modeloff` is the default
// first view at app boot, so `domain.KindStatus` is the right value
// for application startup. Tests that immediately focus a real
// channel before the first frame pass `domain.KindStatus` too —
// `SetChannelMsg` supplies the real kind atomically on the first
// focus event.
func NewChatScreen(ctx context.Context, sess *session.Session, cfgStore config.Store, uiState UIStateStore, initialKind domain.ChannelKind) (ChatScreen, error) {
	active := domain.ChannelName("")
	channels := set.NewSorted[*Window]()
	scrollbackMu := &sync.RWMutex{}

	events := func() []domain.StoredEvent {
		scrollbackMu.RLock()
		defer scrollbackMu.RUnlock()

		w, ok := channels.Get(windowKey(active))
		if !ok {
			return nil
		}

		return w.Scrollback
	}

	sidebar := components.NewChannelSidebar()
	chatView := components.NewChatView[chatcmd.CompletionContext](events, "", initialKind, sess.UserNick(), "")
	layout := components.NewMainLayout(sidebar, chatView)
	layout.NickList = components.NewNickList(domain.NewMemberList())

	liveModels := []chatcmd.ModelOption(nil)
	liveModelsState := command.SuggestionStateReady

	cs := ChatScreen{
		ctx:             ctx,
		sess:            sess,
		client:          sess.User(),
		cfgStore:        cfgStore,
		uiState:         uiState,
		channels:        channels,
		active:          &active,
		liveModels:      &liveModels,
		liveModelsState: &liveModelsState,
		layout:          layout,
		keyMap:          components.DefaultChatScreenKeyMap,
		checklist:       NewWelcomeChecklist(sess.UserNick(), sess.HasAPIKey()),
		pacedQueue:      map[domain.ChannelName][]domain.Message{},
		dispatching:     map[*domain.Instance]bool{},
		scrollbackMu:    scrollbackMu,
	}

	parser, err := chatcmd.NewParser()
	if err != nil {
		return ChatScreen{}, err
	}

	cs.parser = parser
	cs.completer = cs.completionSet()

	return cs, nil
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
			HighlightWords: config.DefaultHighlightWords,
		}, nil
	}

	return s.cfgStore.Load(s.ctx)
}

// WithObservability wires local observability into the chat screen.
func (s ChatScreen) WithObservability(obs *observability.Runtime) ChatScreen {
	s.obs = obs
	s.summary = components.NewMetricsSummaryModel(s.ctx, obs)

	chatView, ok := s.layout.Content.(components.ChatView[chatcmd.CompletionContext])
	if !ok {
		return s
	}

	workspace := components.NewChatWorkspace(chatView).
		WithMetrics(components.NewMetricsPane(s.ctx, obs)).
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
		s.listenForEvents(),
		s.listenForProtocolEvents(),
		msgCmd(components.ChannelAddedMsg{Channel: statusWindow.Window}),
		msgCmd(components.CommandsMsg[chatcmd.CompletionContext]{
			Commands: s.parser.Set().Commands,
		}),
		msgCmd(components.CompleterMsg{Completer: s.completer}),
		msgCmd(components.HighlightWordsMsg{
			Words:    cfg.HighlightWords,
			UserNick: s.sess.UserNick(),
		}),
		msgCmd(components.TimestampFormatMsg{
			Format: cfg.TimestampFormat,
			Locale: uitimestamp.CurrentLocale(),
		}),
		msgCmd(components.SetPlaceholderMsg{Text: s.checklist.Render()}),
	}

	// Seed the sidebar with the user's already-joined channels
	// (from session state) before `restoreFocus` fires, so that
	// `ChannelFocusMsg`'s sidebar marker lands on a sidebar that
	// already knows about the target. The Join events arriving on
	// the protocol bus shortly afterwards become idempotent
	// re-inserts into the sorted set.
	if channels := s.sess.UserInstance().Channels(); channels != nil {
		for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
			cw := domain.NewChannelWindow(pair.Key, time.Time{})
			s.channels.Insert(newWindow(cw))
			cmds = append(cmds, msgCmd(components.ChannelAddedMsg{Channel: cw}))
		}
	}

	cmds = append(cmds, s.restoreFocus())

	if s.obs != nil {
		cmds = append(cmds, s.summary.Init(), s.waitForLogUpdateCmd())
	}

	return tea.Batch(cmds...)
}

// listenForEvents reads the next event from the session's
// non-protocol UI channel and wraps it in a sessionEventMsg. After
// each event, it should be re-invoked so the channel is
// continuously drained.
func (s ChatScreen) listenForEvents() tea.Cmd {
	ch := s.sess.Events()

	return func() tea.Msg {
		evt, ok := <-ch
		if !ok {
			return nil
		}

		return sessionEventMsg{event: evt}
	}
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

// Update implements ui.Model.
func (s ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	forwardedMsg := msg
	summary, summaryCmd := s.summary.Update(msg)
	s.summary = summary

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.height = msg.Height
		forwardedMsg = tea.WindowSizeMsg{Width: msg.Width, Height: s.layoutHeight()}

	case sessionEventMsg:
		return s.handleSessionEvent(msg)

	case protocolEventMsg:
		return s.handleProtocolEvent(msg)

	case joinAutojoinDoneMsg:
		// Forwarded from the connection screen once autojoin
		// settles. The chat-screen's initial `restoreFocus`
		// returns a no-op when no channels are joined yet (the
		// common case at decorator-Init time), so re-run it now
		// that membership is populated.
		return s, s.restoreFocus()

	case ui.QuitRequestedMsg:
		return s.handleQuitRequested(msg)

	case ui.QuitCompleteMsg:
		return s, tea.Quit

	case chatcmd.HelpResult:
		return s, s.logAndShow(domain.Help{Target: *s.active, At: time.Now()})

	case chatcmd.ClearResult:
		if w, ok := s.windowByName(*s.active); ok {
			s.scrollbackMu.Lock()
			w.Scrollback = nil
			s.scrollbackMu.Unlock()
		}
		return s, nil

	case chatcmd.TopicInfoResult:
		return s, s.logAndShow(domain.TopicInfo{
			Target:     msg.Window.Name(),
			Topic:      msg.Window.Topic,
			TopicSetBy: msg.Window.TopicSetBy,
			TopicSetAt: msg.Window.TopicSetAt,
			At:         time.Now(),
		})

	case chatcmd.WhoisResult:
		return s.handleWhoisResult(msg)

	case chatcmd.ListResult:
		now := time.Now()
		cmds := make([]tea.Cmd, 0, len(msg.Entries)+1)

		for _, entry := range msg.Entries {
			cmds = append(cmds, s.logAndShow(domain.ListReply{
				Channel: entry.Channel,
				Members: entry.Members,
				Topic:   entry.Topic,
				At:      now,
			}))
		}

		cmds = append(cmds, s.logAndShow(domain.ListEnd{At: now}))

		return s, tea.Sequence(cmds...)

	case chatcmd.UsageError:
		return s, s.logAndShow(domain.UsageHint{
			Target: *s.active, Command: msg.Command, Usage: msg.Usage, At: time.Now(),
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
		*s.liveModels = nil
		*s.liveModelsState = command.SuggestionStateReady

		if s.realChannelCount() == 0 {
			return s, tea.Batch(
				s.loadLiveModels(),
				msgCmd(components.SetPlaceholderMsg{
					Text: s.checklist.Render(),
				}),
			)
		}

		return s, tea.Batch(
			s.logAndShow(domain.SystemNotice{
				Target: *s.active, Text: text, At: time.Now(),
			}),
			s.loadLiveModels(),
		)

	case chatcmd.PokeIntervalSetResult:
		text := fmt.Sprintf("Poke interval set to %s.", msg.Interval)
		if msg.Reset {
			text = fmt.Sprintf("Poke interval reset to %s.", msg.Interval)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.DrainTimeoutSetResult:
		text := fmt.Sprintf("Drain timeout set to %s.", msg.Timeout)
		if msg.Reset {
			text = fmt.Sprintf("Drain timeout reset to %s.", msg.Timeout)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.SmallModelSetResult:
		text := fmt.Sprintf("Small model set to %s.", msg.ModelID)
		if msg.Reset {
			text = fmt.Sprintf("Small model reset to %s.", msg.ModelID)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.HighlightWordsSetResult:
		text := fmt.Sprintf("highlight words set to: %v", msg.Words)
		if msg.Reset {
			text = fmt.Sprintf("highlight words reset to: %v", msg.Words)
		}

		return s, tea.Batch(
			s.logAndShow(domain.SystemNotice{
				Target: *s.active,
				Text:   text,
				At:     time.Now(),
			}),
			msgCmd(components.HighlightWordsMsg{
				Words:    msg.Words,
				UserNick: s.sess.UserNick(),
			}),
		)

	case chatcmd.BaseURLSetResult:
		text := fmt.Sprintf("base URL set to %s", msg.URL)
		if msg.Reset {
			text = fmt.Sprintf("base URL reset to %s", msg.URL)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.EmbeddingModelSetResult:
		text := fmt.Sprintf("embedding model set to %s", msg.ModelID)
		if msg.Reset {
			text = fmt.Sprintf("embedding model reset to %s", msg.ModelID)
		}

		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   text,
			At:     time.Now(),
		})

	case chatcmd.PersonasListResult:
		return s, s.logAndShow(domain.PersonasList{
			Personas: msg.Personas,
			At:       time.Now(),
		})

	case chatcmd.PersonasRegeneratedResult:
		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   fmt.Sprintf("Generated %d personas.", msg.Count),
			At:     time.Now(),
		})

	case chatcmd.PersonaSetResult:
		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   fmt.Sprintf("Persona %s saved.", msg.ID),
			At:     time.Now(),
		})

	case chatcmd.PersonaResetResult:
		return s, s.logAndShow(domain.SystemNotice{
			Target: *s.active,
			Text:   fmt.Sprintf("Removed %d user-defined persona(s).", msg.Count),
			At:     time.Now(),
		})

	case chatcmd.TimestampFormatSetResult:
		var text string

		switch {
		case msg.Reset:
			text = "Timestamp format reset to locale default."
		case msg.Format != nil && *msg.Format != "":
			text = fmt.Sprintf("timestamp format set to %s", *msg.Format)
		default:
			text = "timestamps disabled"
		}

		return s, tea.Batch(
			s.logAndShow(domain.SystemNotice{
				Target: *s.active,
				Text:   text,
				At:     time.Now(),
			}),
			msgCmd(components.TimestampFormatMsg{
				Format: msg.Format,
				Locale: uitimestamp.CurrentLocale(),
			}),
		)

	case domain.Join:
		s.bufferEvent(msg)
		return s.handleJoinEvent(msg)

	case domain.Part:
		s.bufferEvent(msg)
		return s.handlePartEvent(msg)

	case domain.Quit:
		targets := actorChannelsForDirectSend(msg.Instance)
		s.bufferActorEvent(targets, msg.Instance, domain.StoredEvent{Event: msg})
		return s.handleQuitEvent(msg, targets)

	case domain.TopicChange:
		s.bufferEvent(msg)
		return s.handleTopicChangeEvent(msg)

	case domain.NickChange:
		targets := actorChannelsForDirectSend(msg.Instance)
		s.bufferActorEvent(targets, msg.Instance, domain.StoredEvent{Event: msg})
		return s.handleNickChangeEvent(msg, targets)

	case domain.ModelInvited:
		s.bufferEvent(msg)
		return s.handleModelInvitedEvent(msg)

	case domain.ModelKicked:
		s.bufferEvent(msg)
		return s.handleModelKickedEvent(msg)

	case domain.Message:
		// User-side outgoing arrives here directly from the
		// send-cmd path; incoming model traffic comes via
		// `protocolEventMsg` and is buffered there.
		s.bufferEvent(msg)
		return s.handleMessageEvent(msg)

	case chatcmd.DMOpenedMsg:
		return s.handleDMOpenedMsg(msg)

	case domain.ConfigChangedEvent:
		return s.handleConfigChangedEvent(msg)

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

	case PokeTickMsg:
		return s, s.handlePoke()

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case components.MessageSubmitMsg:
		if *s.active == "" {
			return s, s.logAndShow(domain.UsageHint{
				Usage: "join a channel first", At: time.Now(),
			})
		}

		return s, s.sendMessage(msg.Text)

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg)

	case tea.KeyMsg:
		if ui.Matches(msg, s.keyMap.ToggleNickList) {
			slog.Default().InfoContext(s.ctx, "keybind triggered",
				"component", "ui",
				"action", "toggle_nick_list",
				"key", msg.String(),
			)

			return s, msgCmd(components.NickListToggleMsg{})
		}
	}

	updated, cmd := s.layout.Update(forwardedMsg)
	s.layout = updated.(components.MainLayout)

	return s, tea.Batch(summaryCmd, cmd)
}

// msgCmd wraps a message as a tea.Cmd so it flows through the Bubble
// Tea runtime rather than bypassing it with a direct Update call.
func msgCmd(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
}

func (s ChatScreen) completionSet() command.CompletionSet[chatcmd.CompletionContext] {
	return command.CompletionSet[chatcmd.CompletionContext]{
		Set: s.parser.Set(),
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
			Instances:      func() iter.Seq[*domain.Instance] { return s.sess.Instances(s.ctx) },
			ChannelMembers: s.activeChannelInstances,
			ActiveMembers:  func() iter.Seq[domain.Nick] { return s.activeMemberNicks() },
			ActiveChannel:  func() domain.ChannelName { return *s.active },
			UserNick:       func() domain.Nick { return s.sess.UserNick() },
			LiveModels: func() iter.Seq[chatcmd.ModelOption] {
				return slices.Values(*s.liveModels)
			},
			LiveModelsState: func() command.SuggestionState {
				return *s.liveModelsState
			},
			Personas: func() iter.Seq[domain.Persona] {
				personas, _ := s.sess.ListPersonas(s.ctx)
				return slices.Values(personas)
			},
			Kind: func() domain.ChannelKind { return s.activeKind() },
		},
	}
}

// restoreFocus picks an Init-time landing channel. It prefers the
// persisted `last_channel` entry when it matches a current
// membership; otherwise it falls back to the most-recently-joined
// channel. Membership is read from the session's in-memory
// snapshot so the cmd does not depend on bus events. There is a
// race between this `ChannelFocusMsg` and the bus-event drain;
// fixing it properly is γ.2's pure-view MessageList redesign.
func (s ChatScreen) restoreFocus() tea.Cmd {
	return func() tea.Msg {
		channels := s.sess.UserInstance().Channels()
		if channels == nil || channels.Len() == 0 {
			return nil
		}

		var target domain.ChannelName

		if s.uiState != nil {
			if last, err := s.uiState.GetLastChannel(s.ctx); err == nil && last != "" {
				if _, ok := channels.Get(last); ok {
					target = last
				}
			}
		}

		if target == "" {
			newest := channels.Newest()
			if newest == nil {
				return nil
			}
			target = newest.Key
		}

		return chatcmd.ChannelFocusMsg{Channel: target}
	}
}

func (s ChatScreen) loadLiveModels() tea.Cmd {
	if !s.sess.HasAPIKey() {
		return nil
	}

	return func() tea.Msg {
		models, err := s.sess.ListModels(s.ctx)
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

func (s ChatScreen) layoutHeight() int {
	if s.width < theme.MinTerminalWidth {
		return s.height
	}

	return max(s.height-lipgloss.Height(components.RenderStatusBar(s.width, s.KeyBindings(), s.StatusItems())), 0)
}

// logAndShow persists a channel event under the active channel and
// returns a command that sends the StoredEvent to the message list.
// When no channel is active the event is still sent for rendering but
// is not persisted to the store.
func (s ChatScreen) logAndShow(event domain.PersistableEvent) tea.Cmd {
	// Empty active means the user is on the welcome screen with no
	// channels. Route transient output to `&modeloff` and bring it
	// into focus so the user sees the response. The active pointer
	// is set inline (the call site is inside Update, which owns
	// the writer side of `*s.active`) so the `logAndShowOn`
	// closure observes the new target and returns its StoredEvent
	// — necessary for the message-list render trigger and for
	// callers that inspect the returned cmd. A trailing
	// `ChannelFocusMsg` runs the rest of the focus pipeline
	// (sidebar marker, placeholder clear, last-channel persist)
	// without re-touching `*s.active`.
	if *s.active == "" {
		*s.active = domain.StatusChannelName

		return tea.Batch(
			s.logAndStoreCmd(domain.StatusChannelName, event),
			func() tea.Msg {
				return chatcmd.ChannelFocusMsg{Channel: domain.StatusChannelName}
			},
		)
	}

	return s.logAndShowOn(*s.active, event)
}

// logAndStoreCmd persists `event` under `ch` and appends to the
// matching scrollback, returning the StoredEvent unconditionally so
// it can act as the message-list render trigger for the freshly-
// focused window. Unlike [ChatScreen.logAndShowOn] it does not read
// `*s.active` from the Cmd goroutine, which would race against an
// in-flight focus mutation on the Update goroutine.
func (s ChatScreen) logAndStoreCmd(ch domain.ChannelName, event domain.PersistableEvent) tea.Cmd {
	return func() tea.Msg {
		stored, err := s.sess.LogEvent(s.ctx, ch, event)
		if err != nil {
			return nil
		}

		s.appendToScrollback(ch, stored)

		return stored
	}
}

// persistOnStatus records a channel event on the per-session status
// channel without forwarding it to the active window. The store
// call runs inside the returned Cmd; failures log via slog and the
// `#10` persistence-failure path inside the session, so callers
// can fire-and-forget. Returns nil if the persistence step fails to
// schedule, since dropping the trailing message is acceptable for
// an audit-trail copy.
func (s ChatScreen) persistOnStatus(event domain.PersistableEvent) tea.Cmd {
	return func() tea.Msg {
		if _, err := s.sess.LogEvent(s.ctx, domain.StatusChannelName, event); err != nil {
			slog.Default().ErrorContext(s.ctx, "persist on status channel", "error", err)
		}

		return nil
	}
}

// logAndShowOn persists a channel event under the explicit target
// channel and returns a command that sends the StoredEvent to the
// message list. Callers use this when the event's home is not the
// currently-focused channel — for example, routing a notice to the
// status channel when no user-visible channel is active. The caller
// is responsible for setting event.Channel consistently with ch;
// this helper does not rewrite it.
//
// The store call happens inside the returned Cmd, not in the
// caller's goroutine, so Update remains the single writer of
// chat-screen state — the session mutation is fenced off the Tea
// program's main loop until its result lands as a tea.Msg.
//
// The Cmd appends the persisted event to `s.scrollback[ch]` under
// `scrollbackMu` so a subsequent focus into `ch` re-renders the
// line via [ChatScreen.scrollbackCmd]. Without this, a focus
// change racing with the Cmd would replace the message list with
// the channel's scrollback (which would not contain the freshly-
// logged event) and wipe the line off the screen.
//
// `*s.active` is read from the Cmd goroutine but the chat-screen
// is the single writer of `*s.active` on the Update goroutine,
// and this Cmd was scheduled from Update. The active-channel
// branch returns `stored` for live append; the off-channel branch
// returns `nil` and lets `scrollbackCmd` own the next render.
//
// A narrow residual race remains in the active-channel branch:
// if focus settles to `ch` during the persist's lifetime,
// [ChatScreen.handleChannelFocus] will have queued a
// `scrollbackCmd(ch)` at the tail of its [tea.Sequence], and if
// our `appendToScrollback` wins against that queued closure's
// `RLock`, the focus-driven `HistoryLoadedMsg` carries a snapshot
// containing the line and the subsequent live `stored` append
// doubles it. The window is bounded by the focus sequence's
// `persistLastChannel` step (an SQLite write) and is rare in
// practice — 400-iter `-race` runs at the test sites are clean.
// A structurally airtight fix would move the scrollback append
// back onto the Update goroutine after the persist resolves, at
// the cost of an extra round-trip; the duplicate-line failure
// mode is visually less severe than the original wipe, so we
// accept the residual here.
func (s ChatScreen) logAndShowOn(ch domain.ChannelName, event domain.PersistableEvent) tea.Cmd {
	if ch == "" {
		return msgCmd(domain.StoredEvent{Event: event})
	}

	return func() tea.Msg {
		stored, err := s.sess.LogEvent(s.ctx, ch, event)
		if err != nil {
			return nil
		}

		s.appendToScrollback(ch, stored)

		if ch == *s.active {
			return stored
		}

		return nil
	}
}

// handleQuitRequested locks the UI, shows a "Disconnecting…"
// indication, and runs the backend quit asynchronously. The result
// arrives as a QuitCompleteMsg, which the screen turns into
// tea.Quit.
func (s ChatScreen) handleQuitRequested(msg ui.QuitRequestedMsg) (ui.Model, tea.Cmd) {
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
			err := s.sess.Quit(s.ctx, message)
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
			if err := s.sess.Join(s.ctx, string(ch)); err != nil {
				return domain.ErrorEvent{Operation: "switch", Err: err, At: time.Now()}
			}
		}

		return chatcmd.ChannelFocusMsg{Channel: ch}
	}
}

func (s ChatScreen) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		msg, err := s.sess.SendMessage(s.ctx, *s.active, text)
		if err != nil {
			return domain.ErrorEvent{Operation: "send", Err: err, At: time.Now()}
		}

		return msg
	}
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

func (s ChatScreen) updateLogEntries() ChatScreen {
	if s.obs == nil {
		return s
	}

	workspace, ok := s.layout.Content.(components.ChatWorkspace[chatcmd.CompletionContext])
	if !ok {
		return s
	}

	s.layout.Content = workspace.SetLogEntries(s.obs.LogBuffer().Entries())

	return s
}

// handleWhoisResult routes a `/whois` response. When the active
// window already is `&modeloff`, a single persisted entry serves both
// roles; otherwise the response shows ephemerally on the active
// window (in-memory scrollback append, no persistence) and an audit
// copy is persisted under `&modeloff` so the IRC-style server log
// records every `/whois` the user ran.
func (s ChatScreen) handleWhoisResult(msg chatcmd.WhoisResult) (ui.Model, tea.Cmd) {
	now := time.Now()

	if *s.active == domain.StatusChannelName {
		return s, s.logAndShow(snapshotWhois(*s.active, msg.Instance, now))
	}

	statusEvent := snapshotWhois(domain.StatusChannelName, msg.Instance, now)

	s.appendToScrollback(*s.active, domain.StoredEvent{
		Event: snapshotWhois(*s.active, msg.Instance, now),
	})

	return s, tea.Batch(
		msgCmd(components.ScrollbackUpdatedMsg{Channel: *s.active}),
		s.persistOnStatus(statusEvent),
	)
}

// snapshotWhois freezes an instance's mutable identity surface
// (`Nick`, `Persona`, `Channels`) into a `Whois` event at the
// moment `/whois` is issued. The renderer reads from these fields,
// not the live pointer, so subsequent renames or channel changes do
// not retro-edit the historical line. `ModelID` is captured even
// though it is immutable so that future commits can drop the
// `Instance` pointer entirely once legacy stored events have aged
// out.
func snapshotWhois(channel domain.ChannelName, inst *domain.Instance, at time.Time) domain.Whois {
	whois := domain.Whois{
		Target:  channel,
		Nick:    inst.Nick(),
		ModelID: inst.ModelID,
		Persona: inst.Persona(),
		At:      at,
	}

	if channels := inst.Channels(); channels != nil {
		whois.Channels = make([]domain.ChannelName, 0, channels.Len())
		for pair := channels.Oldest(); pair != nil; pair = pair.Next() {
			whois.Channels = append(whois.Channels, pair.Key)
		}
	}

	return whois
}
