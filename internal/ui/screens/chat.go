package screens

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/theme"
)

// chatLoadedMsg carries the initial data needed to render the chat
// screen after loading from the session.
type chatLoadedMsg struct {
	channels  []domain.Channel
	instances []domain.ModelInstance
	active    domain.ChannelName
	topic     string
	messages  []domain.Message
	unread    map[domain.ChannelName]int
	members   []domain.Member
}

// systemEventMsg carries typed events to display in the chat view
// without changing channel/sidebar state.
type systemEventMsg struct {
	events []components.ChatLine
}

// eventBatchMsg carries multiple domain events from a single command.
// Update unpacks and dispatches each event sequentially.
type eventBatchMsg struct {
	events []domain.Event
}

type apiKeyActivatedMsg struct{}

type liveModelsLoadedMsg struct {
	models []command.ModelOption
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
	commands command.Set

	channels     []domain.Channel
	instances    []domain.ModelInstance
	liveModels   []command.ModelOption
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
	chatView.WithCommandState(command.Set{}, command.CompletionContext{})
	layout := components.NewMainLayout(sidebar, chatView)
	layout.SetNickList(components.NewNickList(nil))

	return &ChatScreen{
		ctx:      ctx,
		sess:     sess,
		layout:   layout,
		chatView: chatView,
		keyMap:   components.DefaultChatScreenKeyMap,
	}
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

		return chatLoadedMsg{
			channels:  channels,
			instances: instances,
			active:    active,
			topic:     topic,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   members,
		}
	}

	return tea.Batch(loadInitial, s.loadLiveModels())
}

// Update implements ui.Model.
func (s *ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	forwardedMsg := msg

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.height = msg.Height
		forwardedMsg = tea.WindowSizeMsg{Width: msg.Width, Height: s.layoutHeight()}

	case chatLoadedMsg:
		return s.handleLoaded(msg)

	case systemEventMsg:
		return s.handleSystemEvent(msg)

	case eventBatchMsg:
		return s.handleEventBatch(msg)

	case apiKeyActivatedMsg:
		model, _ := s.handleSystemEvent(systemEventMsg{
			events: []components.ChatLine{components.APIKeySaved{}},
		})
		s = model.(*ChatScreen)

		return s, s.loadLiveModels()

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

	case PokeTickMsg:
		s.forwardToLayout(components.PendingResponseMsg{Pending: true})

		return s, s.handlePoke()

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case components.MessageSubmitMsg:
		if s.active == "" {
			return s, func() tea.Msg {
				return systemEventMsg{events: []components.ChatLine{components.NoChannel{}}}
			}
		}

		s.forwardToLayout(components.PendingResponseMsg{Pending: true})

		return s, s.sendMessage(msg.Text)

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg)

	case tea.KeyMsg:
		if key.Matches(msg, s.keyMap.ToggleNickList) {
			s.forwardToLayout(components.NickListToggleMsg{})
			return s, nil
		}
	}

	updated, cmd := s.layout.Update(forwardedMsg)
	s.layout = updated.(components.MainLayout)

	return s, cmd
}

func (s *ChatScreen) handleLoaded(msg chatLoadedMsg) (ui.Model, tea.Cmd) {
	s.channels = msg.channels
	s.instances = msg.instances
	s.active = msg.active
	s.topic = msg.topic
	s.channelCount = len(msg.channels)

	s.updateSidebar(msg.channels, msg.active, msg.unread)
	s.chatView.SetChannel(msg.active, msg.topic, components.MessagesToLines(msg.messages))
	s.updateNickList(msg.members)

	if s.channelCount == 0 {
		s.chatView.SetPlaceholder(welcomeText(s.sess.UserNick()))
	} else {
		s.chatView.SetPlaceholder("")
	}

	s.applyCommandState()

	return s, nil
}

func (s *ChatScreen) handleSystemEvent(msg systemEventMsg) (ui.Model, tea.Cmd) {
	var lines []components.ChatLine

	if s.active != "" {
		messages, _ := s.sess.Messages(s.ctx, s.active)
		lines = components.MessagesToLines(messages)
	}

	lines = append(lines, msg.events...)
	s.chatView.SetLines(lines)
	s.chatView.WithCommandState(s.Commands(), s.commandContext())
	s.forwardToLayout(components.PendingResponseMsg{Pending: false})

	return s, nil
}

func (s *ChatScreen) handleEventBatch(msg eventBatchMsg) (ui.Model, tea.Cmd) {
	var cmds []tea.Cmd

	for _, evt := range msg.events {
		var cmd tea.Cmd

		switch e := evt.(type) {
		case domain.JoinEvent:
			_, cmd = s.handleJoinEvent(e)
		case domain.PartEvent:
			_, cmd = s.handlePartEvent(e)
		case domain.TopicChangeEvent:
			_, cmd = s.handleTopicChangeEvent(e)
		case domain.NickChangeEvent:
			_, cmd = s.handleNickChangeEvent(e)
		case domain.ModelInvitedEvent:
			_, cmd = s.handleModelInvitedEvent(e)
		case domain.ModelKickedEvent:
			_, cmd = s.handleModelKickedEvent(e)
		case domain.MessageEvent:
			_, cmd = s.handleMessageEvent(e)
		case domain.ModelReplyEvent:
			_, cmd = s.handleModelReplyEvent(e)
		case domain.DMOpenedEvent:
			_, cmd = s.handleDMOpenedEvent(e)
		case domain.ConfigChangedEvent:
			_, cmd = s.handleConfigChangedEvent(e)
		case domain.ErrorEvent:
			_, cmd = s.handleErrorEvent(e)
		}

		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return s, tea.Batch(cmds...)
}

func (s *ChatScreen) handleJoinEvent(msg domain.JoinEvent) (ui.Model, tea.Cmd) {
	s.active = msg.Channel

	channels, _ := s.sess.ListChannels(s.ctx)
	s.channels = channels
	s.channelCount = len(channels)

	messages, _ := s.sess.Messages(s.ctx, msg.Channel)

	var topic string
	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, msg.Channel); err == nil {
		topic = ch.Topic
		members = s.sortedMembers(ch.Members)
	}

	s.topic = topic

	lines := components.MessagesToLines(messages)
	lines = append(lines, components.Join{JoinEvent: msg})
	unread := s.unreadCounts(s.ctx, channels)

	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.Channel, topic, lines)
	s.updateSidebar(channels, msg.Channel, unread)
	s.updateNickList(members)
	s.applyCommandState()

	return s, nil
}

func (s *ChatScreen) handlePartEvent(msg domain.PartEvent) (ui.Model, tea.Cmd) {
	channels, _ := s.sess.ListChannels(s.ctx)
	s.channels = channels
	s.channelCount = len(channels)

	leavingActive := s.active == msg.Channel

	if leavingActive {
		if len(channels) > 0 {
			s.active = channels[0].Name
			s.topic = channels[0].Topic
		} else {
			s.active = ""
			s.topic = ""
		}

		var lines []components.ChatLine

		if s.active != "" {
			messages, _ := s.sess.Messages(s.ctx, s.active)
			lines = components.MessagesToLines(messages)
		}

		s.chatView.SetChannel(s.active, s.topic, lines)
	}

	unread := s.unreadCounts(s.ctx, channels)

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	s.updateSidebar(channels, s.active, unread)
	s.updateNickList(members)
	s.applyCommandState()

	if !leavingActive && s.active == msg.Channel {
		s.forwardToLayout(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.Part{PartEvent: msg},
			},
		})
	}

	return s, nil
}

func (s *ChatScreen) handleTopicChangeEvent(msg domain.TopicChangeEvent) (ui.Model, tea.Cmd) {
	if msg.Channel == s.active {
		s.topic = msg.Topic
	}

	s.applyCommandState()

	if s.active == msg.Channel {
		s.forwardToLayout(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.TopicChange{TopicChangeEvent: msg},
			},
		})
	}

	return s, nil
}

func (s *ChatScreen) handleNickChangeEvent(msg domain.NickChangeEvent) (ui.Model, tea.Cmd) {
	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	s.updateNickList(members)
	s.applyCommandState()

	if s.active != "" {
		s.forwardToLayout(components.AppendLinesMsg{
			Channel: s.active,
			Lines: []components.ChatLine{
				components.NickChange{NickChangeEvent: msg},
			},
		})
	}

	return s, nil
}

func (s *ChatScreen) handleModelInvitedEvent(msg domain.ModelInvitedEvent) (ui.Model, tea.Cmd) {
	instances, _ := s.sess.ListInstances(s.ctx)
	s.instances = instances

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	s.updateNickList(members)
	s.applyCommandState()

	if s.active == msg.Channel {
		s.forwardToLayout(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.ModelInvited{ModelInvitedEvent: msg},
			},
		})
	}

	return s, nil
}

func (s *ChatScreen) handleModelKickedEvent(msg domain.ModelKickedEvent) (ui.Model, tea.Cmd) {
	instances, _ := s.sess.ListInstances(s.ctx)
	s.instances = instances

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, s.active); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	s.updateNickList(members)
	s.applyCommandState()

	if s.active == msg.Channel {
		s.forwardToLayout(components.AppendLinesMsg{
			Channel: msg.Channel,
			Lines: []components.ChatLine{
				components.ModelKicked{ModelKickedEvent: msg},
			},
		})
	}

	return s, nil
}

func (s *ChatScreen) handleMessageEvent(msg domain.MessageEvent) (ui.Model, tea.Cmd) {
	return s.handleNewMessage(msg.Message.Channel)
}

func (s *ChatScreen) handleModelReplyEvent(msg domain.ModelReplyEvent) (ui.Model, tea.Cmd) {
	return s.handleNewMessage(msg.Message.Channel)
}

func (s *ChatScreen) handleNewMessage(channel domain.ChannelName) (ui.Model, tea.Cmd) {
	s.forwardToLayout(components.PendingResponseMsg{Pending: false})

	if channel == s.active {
		messages, _ := s.sess.Messages(s.ctx, channel)
		lines := components.MessagesToLines(messages)

		s.chatView.SetLines(lines)
	} else {
		channels, _ := s.sess.ListChannels(s.ctx)
		s.channels = channels
		unread := s.unreadCounts(s.ctx, channels)

		s.updateSidebar(channels, s.active, unread)
	}

	return s, nil
}

func (s *ChatScreen) handleDMOpenedEvent(msg domain.DMOpenedEvent) (ui.Model, tea.Cmd) {
	s.active = msg.Channel.Name

	channels, _ := s.sess.ListChannels(s.ctx)
	s.channels = channels
	s.channelCount = len(channels)

	messages, _ := s.sess.Messages(s.ctx, msg.Channel.Name)
	s.topic = msg.Channel.Topic

	lines := components.MessagesToLines(messages)
	lines = append(lines, components.DMOpened{Nick: msg.Nick})
	unread := s.unreadCounts(s.ctx, channels)

	var members []domain.Member

	if ch, err := s.sess.GetChannel(s.ctx, msg.Channel.Name); err == nil {
		members = s.sortedMembers(ch.Members)
	}

	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.Channel.Name, msg.Channel.Topic, lines)
	s.updateSidebar(channels, msg.Channel.Name, unread)
	s.updateNickList(members)
	s.applyCommandState()

	return s, nil
}

func (s *ChatScreen) handleConfigChangedEvent(msg domain.ConfigChangedEvent) (ui.Model, tea.Cmd) {
	if s.active == "" {
		return s, nil
	}

	s.forwardToLayout(components.AppendLinesMsg{
		Channel: s.active,
		Lines: []components.ChatLine{
			components.ConfigChanged{Operation: msg.Operation},
		},
	})

	return s, nil
}

func (s *ChatScreen) handleErrorEvent(msg domain.ErrorEvent) (ui.Model, tea.Cmd) {
	s.forwardToLayout(components.AppendLinesMsg{
		Channel: s.active,
		Lines: []components.ChatLine{
			components.BackendError{
				Operation: msg.Operation,
				Err:       msg.Err,
			},
		},
	})
	s.forwardToLayout(components.PendingResponseMsg{Pending: false})

	return s, nil
}

func (s *ChatScreen) handleLiveModelsLoaded(msg liveModelsLoadedMsg) (ui.Model, tea.Cmd) {
	s.liveModels = msg.models
	s.applyCommandState()

	return s, nil
}

func (s *ChatScreen) updateSidebar(channels []domain.Channel, active domain.ChannelName, unread map[domain.ChannelName]int) {
	s.forwardToLayout(components.ChannelsUpdatedMsg{
		Channels: channels,
		Active:   active,
		Unread:   unread,
	})
}

func (s *ChatScreen) updateNickList(members []domain.Member) {
	s.forwardToLayout(components.NickListUpdatedMsg{Members: members})
}

func (s *ChatScreen) forwardToLayout(msg tea.Msg) {
	updated, _ := s.layout.Update(msg)
	s.layout = updated.(components.MainLayout)
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

// Commands implements ui.CommandSource.
func (s *ChatScreen) Commands() command.Set {
	if len(s.commands.Commands) > 0 {
		return s.commands
	}

	type grammar struct {
		Join   command.JoinCommand   `cmd:"" help:"Switch to a channel or create it if needed."`
		Leave  command.LeaveCommand  `cmd:"" help:"Leave the current channel."`
		List   command.ListCommand   `cmd:"" help:"List all known channels."`
		Invite command.InviteCommand `cmd:"" help:"Invite a model or reusable instance into the current channel."`
		Kick   command.KickCommand   `cmd:"" help:"Remove a model instance from the current channel."`
		Msg    command.MsgCommand    `cmd:"" help:"Open a direct message and optionally send text."`
		Nick   command.NickCommand   `cmd:"" help:"Change your nickname."`
		Topic  command.TopicCommand  `cmd:"" help:"Set or clear the current channel topic."`
		Whois  command.WhoisCommand  `cmd:"" help:"Show details about a model instance."`
		Config command.ConfigCommand `cmd:"" help:"Update runtime configuration."`
		Help   command.HelpCommand   `cmd:"" help:"Show available commands."`
		Quit   command.QuitCommand   `cmd:"" help:"Exit modeloff."`
	}

	cmds := command.Build(&grammar{})

	// Bind handlers.

	command.Bind(cmds, "join", func(cmd command.JoinCommand) tea.Cmd {
		return s.joinChannel(cmd.Channel.String())
	})

	command.Bind(cmds, "leave", func(_ command.LeaveCommand) tea.Cmd {
		if s.active == "" {
			return noChannelCmd()
		}

		return s.leaveChannel()
	})

	command.Bind(cmds, "list", func(_ command.ListCommand) tea.Cmd {
		return s.listChannels()
	})

	command.Bind(cmds, "invite", func(cmd command.InviteCommand) tea.Cmd {
		if s.active == "" {
			return noChannelCmd()
		}

		if cmd.Model == "" {
			return usageCmd("invite")
		}

		return s.inviteModel(domain.ModelID(cmd.Model), strings.Join(cmd.Persona, " "))
	})

	command.Bind(cmds, "kick", func(cmd command.KickCommand) tea.Cmd {
		if s.active == "" {
			return noChannelCmd()
		}

		return s.kickModel(domain.Nick(cmd.Nick))
	})

	command.Bind(cmds, "msg", func(cmd command.MsgCommand) tea.Cmd {
		return s.directMessage(domain.Nick(cmd.Nick), strings.Join(cmd.Body, " "))
	})

	command.Bind(cmds, "nick", func(cmd command.NickCommand) tea.Cmd {
		return s.changeNick(domain.Nick(cmd.Nick))
	})

	command.Bind(cmds, "topic", func(cmd command.TopicCommand) tea.Cmd {
		if s.active == "" {
			return noChannelCmd()
		}

		return s.setTopic(strings.Join(cmd.Topic, " "))
	})

	command.Bind(cmds, "whois", func(cmd command.WhoisCommand) tea.Cmd {
		return s.whois(domain.Nick(cmd.Nick))
	})

	command.Bind(cmds, "help", func(_ command.HelpCommand) tea.Cmd {
		return s.showHelp()
	})

	command.Bind(cmds, "quit", func(_ command.QuitCommand) tea.Cmd {
		return tea.Quit
	})

	// Bind suggestion sources.

	cmds.Find("join").SetSource("channel", command.ChannelsSource())

	invite := cmds.Find("invite")
	invite.SetSource("model", command.ComposeSources(
		command.ReusableInstancesSource(),
		command.LiveModelsSource(),
	))

	cmds.Find("kick").SetSource("nick", command.ActiveMembersSource())
	cmds.Find("msg").SetSource("nick", command.InstancesSource())
	cmds.Find("whois").SetSource("nick", command.InstancesSource())

	// Config has custom positionals with dynamic completion sources.
	configNode := cmds.Find("config")
	configNode.Positionals = []command.Positional{
		{
			Name: "key",
			Help: "Choose a config key.",
			Source: command.LiteralSource(
				command.Suggestion{Value: "api-key", Label: "api-key", Detail: "Activate OpenRouter immediately."},
				command.Suggestion{Value: "nick-model", Label: "nick-model", Detail: "Set the model used to generate nicknames."},
				command.Suggestion{Value: "poke-interval", Label: "poke-interval", Detail: "Set the background poke cadence."},
			),
		},
		{
			Name:     "value",
			Help:     "Values are free-form after the key.",
			Optional: true,
			Source: func(_ command.CompletionContext, state command.InvocationState) []command.Suggestion {
				if len(state.Args) == 0 || state.Args[0] != "poke-interval" {
					return nil
				}

				return []command.Suggestion{
					{Value: "5m", Label: "5m", Detail: "Fast poke cadence"},
					{Value: "10m", Label: "10m", Detail: "Balanced poke cadence"},
					{Value: "30m", Label: "30m", Detail: "Quiet channels"},
					{Value: "1h", Label: "1h", Detail: "Very low activity"},
				}
			},
		},
	}
	command.Bind(cmds, "config", func(cmd command.ConfigCommand) tea.Cmd {
		return s.configure(cmd)
	})

	s.commands = cmds

	return s.commands
}

func usageCmd(commandName string) tea.Cmd {
	return func() tea.Msg {
		return systemEventMsg{events: []components.ChatLine{components.UsageHint{Command: commandName}}}
	}
}

func noChannelCmd() tea.Cmd {
	return func() tea.Msg {
		return noChannelMsg()
	}
}

func (s *ChatScreen) commandContext() command.CompletionContext {
	return command.CompletionContext{
		Channels:      append([]domain.Channel(nil), s.channels...),
		Instances:     append([]domain.ModelInstance(nil), s.instances...),
		ActiveChannel: s.active,
		ActiveMembers: s.activeMembers(),
		UserNick:      s.sess.UserNick(),
		LiveModels:    append([]command.ModelOption(nil), s.liveModels...),
	}
}

func (s *ChatScreen) activeMembers() []domain.Nick {
	for _, ch := range s.channels {
		if ch.Name != s.active {
			continue
		}

		return slices.Collect(ch.Members.Sorted())
	}

	return nil
}

func (s *ChatScreen) applyCommandState() {
	s.chatView.WithCommandState(s.Commands(), s.commandContext())
	s.applyLayoutBounds()
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

		options := make([]command.ModelOption, 0, len(models))
		for _, model := range models {
			options = append(options, command.ModelOption{
				ID:          model.ID,
				Name:        model.Name,
				Description: model.Description,
			})
		}

		return liveModelsLoadedMsg{models: options}
	}
}

func (s *ChatScreen) applyLayoutBounds() {
	if s.width <= 0 || s.height <= 0 {
		return
	}

	updated, _ := s.layout.Update(tea.WindowSizeMsg{Width: s.width, Height: s.layoutHeight()})
	s.layout = updated.(components.MainLayout)
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
		evt, replies, err := s.sess.SendMessage(s.ctx, s.active, text)
		if err != nil {
			return domain.ErrorEvent{Operation: "send", Err: err, At: time.Now()}
		}

		events := make([]domain.Event, 0, 1+len(replies))
		events = append(events, evt)

		for _, r := range replies {
			events = append(events, r)
		}

		return eventBatchMsg{events: events}
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
