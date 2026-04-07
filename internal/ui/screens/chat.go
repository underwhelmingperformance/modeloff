package screens

import (
	"context"
	"fmt"
	"iter"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

// sessionEventMsg wraps a domain.SessionEvent received from the
// session's background event channel. Using a dedicated wrapper
// prevents the events channel listener from being re-invoked when
// the same underlying types are sent directly as tea.Msg.
type sessionEventMsg struct {
	event domain.SessionEvent
}

// deliverNextReplyMsg triggers delivery of the next queued reply.
type deliverNextReplyMsg struct{}

type liveModelsLoadedMsg struct {
	models []chatcmd.ModelOption
}

type logsUpdatedMsg struct{}

// PokeTickMsg triggers a background poke cycle for model instances.
type PokeTickMsg struct{}

// ChatScreen is the main screen that composes Sidebar, ChatView, and
// MainLayout. It holds a reference to the session for backend
// operations.
type ChatScreen struct {
	ctx    context.Context
	sess   *session.Session
	layout components.MainLayout
	keyMap components.ChatScreenKeyMap

	channels   *set.Sorted[domain.Channel]
	instances  *set.Sorted[domain.Instance]
	liveModels *[]chatcmd.ModelOption
	parser     chatcmd.Parser
	replyQueue []domain.ModelReplyEvent
	width      int
	height     int
	active     *domain.ChannelName
	obs        *observability.Runtime
	summary    components.MetricsSummaryModel
}

// NewChatScreen creates a chat screen backed by the given session.
// The provided context is used for all backend operations, allowing
// them to be cancelled on shutdown.
func NewChatScreen(ctx context.Context, sess *session.Session) ChatScreen {
	sidebar := components.NewChannelSidebar()
	chatView := components.NewChatView("", sess.UserNick(), "")
	layout := components.NewMainLayout(sidebar, chatView)
	layout.NickList = components.NewNickList(domain.NewMemberList())

	active := domain.ChannelName("")
	liveModels := []chatcmd.ModelOption(nil)

	cs := ChatScreen{
		ctx:  ctx,
		sess: sess,
		channels: set.NewSorted(func(a, b domain.Channel) bool {
			return a.Name < b.Name
		}),
		instances: set.NewSorted(func(a, b domain.Instance) bool {
			return a.Nick < b.Nick
		}),
		active:     &active,
		liveModels: &liveModels,
		layout:     layout,
		keyMap:     components.DefaultChatScreenKeyMap,
	}

	cs.parser = cs.buildParser()

	return cs
}

// WithObservability wires local observability into the chat screen.
func (s ChatScreen) WithObservability(obs *observability.Runtime) ChatScreen {
	s.obs = obs
	s.summary = components.NewMetricsSummaryModel(s.ctx, obs)

	chatView, ok := s.layout.Content.(components.ChatView)
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
func (s ChatScreen) Init() tea.Cmd {
	loadInitial := func() tea.Msg {
		ctx := s.ctx

		channels, err := s.sess.ListChannels(ctx)
		if err != nil {
			channels = nil
		}

		instances, err := s.sess.ListInstances(ctx)
		if err != nil {
			instances = nil
		}

		active, err := s.sess.LastChannel(ctx)
		if err != nil {
			active = ""
		}

		var topic string
		var members domain.MemberList

		if active != "" {
			if ch, err := s.sess.GetChannel(ctx, active); err == nil {
				topic = ch.Topic
				members = ch.Members
			}
		}

		return domain.InitialLoadEvent{
			Channels:  channels,
			Instances: instances,
			Active:    active,
			Topic:     topic,
			Unread:    s.unreadCounts(ctx, channels),
			Members:   members,
			At:        time.Now(),
		}
	}

	cmds := []tea.Cmd{loadInitial, s.processPendingQuit(), s.loadLiveModels(), s.listenForEvents()}

	if s.obs != nil {
		cmds = append(cmds, s.summary.Init(), s.waitForLogUpdateCmd())
	}

	return tea.Batch(cmds...)
}

// listenForEvents reads the next event from the session's background
// event channel and wraps it in a sessionEventMsg. After each event,
// it should be re-invoked so the channel is continuously drained.
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

	case domain.InitialLoadEvent:
		return s.handleInitialLoad(msg)

	case sessionEventMsg:
		return s.handleSessionEvent(msg)

	case chatcmd.HelpResult:
		return s, s.logAndShow(domain.ChannelHelp{Channel: *s.active, At: time.Now()})

	case chatcmd.TopicInfoResult:
		return s, s.logAndShow(domain.ChannelTopicInfo{
			Channel:    msg.Channel.Name,
			Topic:      msg.Channel.Topic,
			TopicSetBy: msg.Channel.TopicSetBy,
			TopicSetAt: msg.Channel.TopicSetAt,
			At:         time.Now(),
		})

	case chatcmd.WhoisResult:
		return s, s.logAndShow(domain.ChannelWhois{
			Channel: *s.active, Instance: msg.Instance, At: time.Now(),
		})

	case chatcmd.ListResult:
		return s, s.logAndShow(domain.ChannelListOutput{
			Channels: msg.Channels, At: time.Now(),
		})

	case chatcmd.UsageError:
		return s, s.logAndShow(domain.ChannelUsageHint{
			Channel: *s.active, Command: msg.Command, Usage: msg.Usage, At: time.Now(),
		})

	case chatcmd.NoChannelError:
		return s, s.logAndShow(domain.ChannelUsageHint{
			Command: "", Usage: "join a channel first", At: time.Now(),
		})

	case chatcmd.APIKeySetResult:
		return s, tea.Batch(
			s.logAndShow(domain.ChannelSystemNotice{
				Channel: *s.active, Text: "OpenRouter API key saved and activated.", At: time.Now(),
			}),
			s.loadLiveModels(),
		)

	case chatcmd.PokeIntervalSetResult:
		return s, s.logAndShow(domain.ChannelSystemNotice{
			Channel: *s.active,
			Text:    fmt.Sprintf("Poke interval set to %s.", msg.Interval),
			At:      time.Now(),
		})

	case chatcmd.NickModelSetResult:
		return s, s.logAndShow(domain.ChannelSystemNotice{
			Channel: *s.active,
			Text:    fmt.Sprintf("Nick generation model set to %s.", msg.ModelID),
			At:      time.Now(),
		})

	case chatcmd.HighlightWordsSetResult:
		return s, tea.Batch(
			s.logAndShow(domain.ChannelSystemNotice{
				Channel: *s.active,
				Text:    fmt.Sprintf("highlight words set to: %v", msg.Words),
				At:      time.Now(),
			}),
			msgCmd(components.HighlightWordsMsg{
				Words:    msg.Words,
				UserNick: s.sess.UserNick(),
			}),
		)

	case chatcmd.BaseURLSetResult:
		return s, s.logAndShow(domain.ChannelSystemNotice{
			Channel: *s.active,
			Text:    fmt.Sprintf("base URL set to %s", msg.URL),
			At:      time.Now(),
		})

	case chatcmd.EmbeddingModelSetResult:
		return s, s.logAndShow(domain.ChannelSystemNotice{
			Channel: *s.active,
			Text:    fmt.Sprintf("embedding model set to %s", msg.ModelID),
			At:      time.Now(),
		})

	case domain.JoinEvent:
		return s.handleJoinEvent(msg)

	case domain.PartEvent:
		return s.handlePartEvent(msg)

	case domain.QuitEvent:
		return s.handleQuitEvent(msg)

	case domain.TopicChangeEvent:
		return s.handleTopicChangeEvent(msg)

	case domain.NickChangeEvent:
		return s.handleNickChangeEvent(msg)

	case domain.ModelInvitedEvent:
		return s.handleModelInvitedEvent(msg)

	case domain.ModelKickedEvent:
		return s.handleModelKickedEvent(msg)

	case domain.MessageEvent:
		return s.handleMessageEvent(msg)

	case domain.ModelReplyEvent:
		return s.handleModelReplyEvent(msg)

	case domain.DMOpenedEvent:
		return s.handleDMOpenedEvent(msg)

	case domain.ConfigChangedEvent:
		return s.handleConfigChangedEvent(msg)

	case domain.ErrorEvent:
		return s.handleErrorEvent(msg)

	case liveModelsLoadedMsg:
		return s.handleLiveModelsLoaded(msg)

	case logsUpdatedMsg:
		s = s.updateLogEntries()
		return s, tea.Batch(summaryCmd, s.waitForLogUpdateCmd())

	case deliverNextReplyMsg:
		return s.deliverNextReply()

	case PokeTickMsg:
		return s, s.handlePoke()

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case components.MessageSubmitMsg:
		if *s.active == "" {
			return s, s.logAndShow(domain.ChannelUsageHint{
				Usage: "join a channel first", At: time.Now(),
			})
		}

		return s, s.sendMessage(msg.Text)

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg)

	case tea.KeyMsg:
		if key.Matches(msg, s.keyMap.ToggleNickList) {
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

func (s ChatScreen) buildParser() chatcmd.Parser {
	return chatcmd.BuildParser(chatcmd.Sources{
		Channels:      func() iter.Seq[domain.Channel] { return s.channels.All() },
		Instances:     func() iter.Seq[domain.Instance] { return s.instances.All() },
		ActiveChannel: func() domain.ChannelName { return *s.active },
		ActiveMembers: func() iter.Seq[domain.Nick] { return s.activeMemberNicks() },
		UserNick:      func() domain.Nick { return s.sess.UserNick() },
		LiveModels:    func() []chatcmd.ModelOption { return *s.liveModels },
	})
}

func (s ChatScreen) unreadCounts(ctx context.Context, channels []domain.Channel) map[domain.ChannelName]int {
	counts := make(map[domain.ChannelName]int, len(channels))

	for _, ch := range channels {
		n, err := s.sess.UnreadCount(ctx, ch.Name)
		if err != nil {
			continue
		}

		if n > 0 {
			counts[ch.Name] = n
		}
	}

	return counts
}

func (s ChatScreen) loadLiveModels() tea.Cmd {
	if !s.sess.HasAPIKey() {
		return nil
	}

	return func() tea.Msg {
		models, err := s.sess.ListModels(s.ctx)
		if err != nil {
			return liveModelsLoadedMsg{}
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

// logAndShow persists a channel event to the event log and returns
// a command that sends the StoredEvent to the message list. When no
// channel is active the event is still sent for rendering but is not
// persisted to the store.
func (s ChatScreen) logAndShow(event domain.ChannelEvent) tea.Cmd {
	ch := *s.active
	if ch == "" {
		return msgCmd(domain.StoredEvent{Event: event})
	}

	stored, err := s.sess.LogEvent(s.ctx, ch, event)
	if err != nil {
		return nil
	}

	return msgCmd(stored)
}

// fetchHistory returns a Cmd that loads the latest events for a
// channel from the event log and sends them as a HistoryLoadedMsg.
// The number of events fetched is based on the viewport height.
func (s ChatScreen) fetchHistory(ch domain.ChannelName) tea.Cmd {
	return s.fetchHistoryAfter(ch, time.Time{})
}

func (s ChatScreen) fetchHistoryAfter(ch domain.ChannelName, after time.Time) tea.Cmd {
	if ch == "" {
		return nil
	}

	n := max(s.height, 50)

	return func() tea.Msg {
		events, err := s.sess.EventsBefore(s.ctx, ch, nil, n)
		if err != nil {
			return nil
		}

		if !after.IsZero() {
			filtered := events[:0]
			for _, evt := range events {
				if !domain.ChannelEventTime(evt.Event).Before(after) {
					filtered = append(filtered, evt)
				}
			}

			events = filtered
		}

		return components.HistoryLoadedMsg{Events: events}
	}
}

func (s ChatScreen) processPendingQuit() tea.Cmd {
	return func() tea.Msg {
		if err := s.sess.ProcessPendingQuit(s.ctx); err != nil {
			return domain.ErrorEvent{
				Operation: "pending quit",
				Err:       err,
				At:        time.Now(),
			}
		}

		return nil
	}
}

func (s ChatScreen) switchChannel(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg {
		if err := s.sess.Join(s.ctx, string(ch)); err != nil {
			return domain.ErrorEvent{Operation: "switch", Err: err, At: time.Now()}
		}

		return nil
	}
}

func (s ChatScreen) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		if err := s.sess.SendMessage(s.ctx, *s.active, text); err != nil {
			return domain.ErrorEvent{Operation: "send", Err: err, At: time.Now()}
		}

		return nil
	}
}

// KeyBindings implements ui.Keybinding.
func (s ChatScreen) KeyBindings() []key.Binding {
	bindings := ui.CollectKeyBindings(s.layout)
	bindings = append(bindings, s.keyMap.ToggleNickList, ui.DefaultAppKeyMap.Quit)

	return bindings
}

// StatusItems implements ui.StatusProvider.
func (s ChatScreen) StatusItems() []ui.StatusItem {
	return ui.CollectStatusItems(s.layout, s.summary)
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

	workspace, ok := s.layout.Content.(components.ChatWorkspace)
	if !ok {
		return s
	}

	s.layout.Content = workspace.SetLogEntries(s.obs.LogBuffer().Entries())

	return s
}
