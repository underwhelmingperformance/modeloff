package screens

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
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

// PokeTickMsg triggers a background poke cycle for model instances.
type PokeTickMsg struct{}

// ChatScreen is the main screen that composes Sidebar, ChatView, and
// MainLayout. It holds a reference to the session for backend
// operations. The ChatView is held as a pointer so that viewport
// and input state survive across message and channel updates.
type ChatScreen struct {
	ctx      context.Context
	sess     *session.Session
	layout   components.MainLayout
	chatView *components.ChatView
	keyMap   components.ChatScreenKeyMap
	parser   chatcmd.Parser

	channels     []domain.Channel
	instances    []domain.ModelInstance
	liveModels   []chatcmd.ModelOption
	replyQueue   []domain.ModelReplyEvent
	width        int
	height       int
	active       domain.ChannelName
	topic        string
	channelCount int
}

// NewChatScreen creates a chat screen backed by the given session.
// The provided context is used for all backend operations, allowing
// them to be cancelled on shutdown.
func NewChatScreen(ctx context.Context, sess *session.Session) *ChatScreen {
	sidebar := components.NewSidebar(nil, "", nil)
	chatView := components.NewChatView("", sess.UserNick(), "", nil)
	layout := components.NewMainLayout(sidebar, chatView)
	layout.SetNickList(components.NewNickList(nil))

	s := &ChatScreen{
		ctx:      ctx,
		sess:     sess,
		layout:   layout,
		chatView: chatView,
		keyMap:   components.DefaultChatScreenKeyMap,
	}

	s.parser = chatcmd.BuildParser(chatcmd.Sources{
		Channels:      func() []domain.Channel { return s.channels },
		Instances:     func() []domain.ModelInstance { return s.instances },
		ActiveChannel: func() domain.ChannelName { return s.active },
		ActiveMembers: s.activeMembers,
		UserNick:      sess.UserNick,
		LiveModels:    func() []chatcmd.ModelOption { return s.liveModels },
	})

	return s
}

// Init implements ui.Model.
func (s *ChatScreen) Init() tea.Cmd {
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

		var messages []domain.Message
		var topic string
		var members []domain.Member

		if active != "" {
			messages, _ = s.sess.Messages(ctx, active)

			if ch, err := s.sess.GetChannel(ctx, active); err == nil {
				topic = ch.Topic
				members = s.sortedMembers(ch.Members)
			}
		}

		return domain.InitialLoadEvent{
			Channels:  channels,
			Instances: instances,
			Active:    active,
			Topic:     topic,
			Messages:  messages,
			Unread:    s.unreadCounts(ctx, channels),
			Members:   members,
			At:        time.Now(),
		}
	}

	return tea.Batch(loadInitial, s.loadLiveModels(), s.listenForEvents())
}

// listenForEvents reads the next event from the session's background
// event channel and wraps it in a sessionEventMsg. After each event,
// it should be re-invoked so the channel is continuously drained.
func (s *ChatScreen) listenForEvents() tea.Cmd {
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
func (s *ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	forwardedMsg := msg

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
		return s, msgCmd(components.Help{})

	case chatcmd.TopicInfoResult:
		return s, msgCmd(components.TopicInfo{Channel: msg.Channel})

	case chatcmd.WhoisResult:
		return s, msgCmd(components.Whois{ModelInstance: msg.Instance})

	case chatcmd.ListResult:
		return s, msgCmd(components.ChannelList{Channels: msg.Channels})

	case chatcmd.UsageError:
		return s, msgCmd(components.UsageHint{Command: msg.Command, Usage: msg.Usage})

	case chatcmd.NoChannelError:
		return s, msgCmd(components.NoChannel{})

	case chatcmd.APIKeySetResult:
		return s, tea.Batch(
			msgCmd(components.APIKeySaved{}),
			s.loadLiveModels(),
		)

	case chatcmd.PokeIntervalSetResult:
		return s, msgCmd(components.PokeIntervalSet{Interval: msg.Interval})

	case chatcmd.NickModelSetResult:
		return s, msgCmd(components.NickModelSet{ModelID: msg.ModelID})

	case chatcmd.HighlightWordsSetResult:
		return s, tea.Batch(
			msgCmd(components.ConfigChanged{Operation: fmt.Sprintf("highlight words set to: %v", msg.Words)}),
			msgCmd(components.HighlightWordsMsg{
				Words:    msg.Words,
				UserNick: s.sess.UserNick(),
			}),
		)

	case domain.JoinEvent:
		return s.handleJoinEvent(msg)

	case domain.PartEvent:
		return s.handlePartEvent(msg)

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

	case deliverNextReplyMsg:
		return s.deliverNextReply()

	case PokeTickMsg:
		return s, s.handlePoke()

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case components.MessageSubmitMsg:
		if s.active == "" {
			return s, msgCmd(components.NoChannel{})
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

	return s, cmd
}

// msgCmd wraps a message as a tea.Cmd so it flows through the Bubble
// Tea runtime rather than bypassing it with a direct Update call.
func msgCmd(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
}

func (s *ChatScreen) unreadCounts(ctx context.Context, channels []domain.Channel) map[domain.ChannelName]int {
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

func (s *ChatScreen) sortedMembers(members set.Ordered[domain.Nick]) []domain.Member {
	if members == nil {
		return nil
	}

	userNick := s.sess.UserNick()

	var result []domain.Member

	for nick := range members.Sorted() {
		mode := domain.ModeVoice

		if nick == userNick {
			mode = domain.ModeOp
		}

		result = append(result, domain.Member{Nick: nick, Mode: mode})
	}

	return result
}

func (s *ChatScreen) loadLiveModels() tea.Cmd {
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

func (s *ChatScreen) layoutHeight() int {
	if s.width < theme.MinTerminalWidth {
		return s.height
	}

	height := s.height - lipgloss.Height(components.RenderStatusBar(s.width, s.KeyBindings()))
	if height < 0 {
		return 0
	}

	return height
}

func (s *ChatScreen) switchChannel(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.Join(s.ctx, string(ch))
		if err != nil {
			return domain.ErrorEvent{Operation: "switch", Err: err, At: time.Now()}
		}

		return evt
	}
}

func (s *ChatScreen) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		evt, err := s.sess.SendMessage(s.ctx, s.active, text)
		if err != nil {
			return domain.ErrorEvent{Operation: "send", Err: err, At: time.Now()}
		}

		return evt
	}
}

// KeyBindings implements ui.Keybinding.
func (s *ChatScreen) KeyBindings() []key.Binding {
	bindings := ui.CollectKeyBindings(s.layout)
	bindings = append(bindings, s.keyMap.ToggleNickList, ui.DefaultAppKeyMap.Quit)

	return bindings
}

// View implements ui.Model.
func (s *ChatScreen) View(width, height int) string {
	if width < theme.MinTerminalWidth {
		return s.layout.View(width, height)
	}

	bar := components.RenderStatusBar(width, s.KeyBindings())
	layoutHeight := height - lipgloss.Height(bar)
	if layoutHeight < 0 {
		layoutHeight = 0
	}

	view := s.layout.View(width, layoutHeight)
	if bar == "" {
		return view
	}

	return lipgloss.JoinVertical(lipgloss.Left, view, bar)
}
