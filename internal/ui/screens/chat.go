package screens

import (
	"context"
	"slices"
	"strings"

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

// channelSwitchedMsg is sent after a channel switch completes,
// carrying the new channel's messages.
type channelSwitchedMsg struct {
	channel  domain.ChannelName
	topic    string
	channels []domain.Channel
	messages []domain.Message
	unread   map[domain.ChannelName]int
	members  []domain.Member
}

// messageSentMsg is sent after a message is saved, carrying the
// updated message list.
type messageSentMsg struct {
	channel  domain.ChannelName
	messages []domain.Message
}

// commandResultMsg carries the result of a slash command that
// modified session state.
type commandResultMsg struct {
	channels  []domain.Channel
	instances []domain.ModelInstance
	active    domain.ChannelName
	topic     string
	messages  []domain.Message
	unread    map[domain.ChannelName]int
	members   []domain.Member
	events    []components.ChatLine
}

// systemEventMsg carries typed events to display in the chat view
// without changing channel/sidebar state.
type systemEventMsg struct {
	events []components.ChatLine
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
	scope    command.Scope

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
	chatView.WithCommandState(command.Scope{}, command.CompletionContext{})
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

	case channelSwitchedMsg:
		return s.handleChannelSwitched(msg)

	case messageSentMsg:
		return s.handleMessageSent(msg)

	case commandResultMsg:
		return s.handleCommandResult(msg)

	case systemEventMsg:
		return s.handleSystemEvent(msg)

	case apiKeyActivatedMsg:
		model, _ := s.handleSystemEvent(systemEventMsg{
			events: []components.ChatLine{components.APIKeySaved{}},
		})
		s = model.(*ChatScreen)

		return s, s.loadLiveModels()

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

func (s *ChatScreen) handleChannelSwitched(msg channelSwitchedMsg) (ui.Model, tea.Cmd) {
	s.channels = msg.channels
	s.active = msg.channel
	s.topic = msg.topic
	s.channelCount = len(msg.channels)

	s.updateSidebar(msg.channels, msg.channel, msg.unread)
	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.channel, msg.topic, components.MessagesToLines(msg.messages))
	s.updateNickList(msg.members)
	s.applyCommandState()

	return s, nil
}

func (s *ChatScreen) handleMessageSent(msg messageSentMsg) (ui.Model, tea.Cmd) {
	s.chatView.SetLines(components.MessagesToLines(msg.messages))
	s.chatView.WithCommandState(s.CommandScope(), s.commandContext())
	s.forwardToLayout(components.PendingResponseMsg{Pending: false})

	return s, nil
}

func (s *ChatScreen) handleCommandResult(msg commandResultMsg) (ui.Model, tea.Cmd) {
	s.channels = msg.channels
	s.instances = msg.instances
	s.active = msg.active
	s.topic = msg.topic
	s.channelCount = len(msg.channels)

	lines := components.MessagesToLines(msg.messages)
	lines = append(lines, msg.events...)

	s.updateSidebar(msg.channels, msg.active, msg.unread)
	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.active, msg.topic, lines)
	s.updateNickList(msg.members)
	s.chatView.WithCommandState(s.CommandScope(), s.commandContext())
	s.forwardToLayout(components.PendingResponseMsg{Pending: false})
	s.applyLayoutBounds()

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
	s.chatView.WithCommandState(s.CommandScope(), s.commandContext())
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

// CommandScope implements ui.CommandScoper.
func (s *ChatScreen) CommandScope() command.Scope {
	if len(s.scope.Commands) > 0 {
		return s.scope
	}

	s.scope = command.Scope{
		Commands: []command.Spec{
			command.Handle("join", "Switch to a channel or create it if needed.", "/join <channel>",
				map[string]command.SuggestionSource{"channel": command.ChannelsSource()},
				func(cmd command.JoinCommand) tea.Cmd {
					return s.joinChannel(cmd.Channel)
				},
			),
			command.Handle("leave", "Leave the current channel.", "/leave",
				nil,
				func(_ command.LeaveCommand) tea.Cmd {
					if s.active == "" {
						return noChannelCmd()
					}

					return s.leaveChannel()
				},
			),
			command.Handle("list", "List all known channels.", "/list",
				nil,
				func(_ command.ListCommand) tea.Cmd {
					return s.listChannels()
				},
			),
			{
				Name:  "invite",
				Help:  "Invite a model or reusable instance into the current channel.",
				Usage: "/invite <model-or-nick> [--persona <text>]",
				Args: []command.ArgSpec{
					{
						Name: "model-or-nick",
						Help: "Choose a reusable instance or a live model ID.",
						Source: command.ComposeSources(
							command.ReusableInstancesSource(),
							command.LiveModelsSource(),
						),
					},
					{
						Name:     "--persona",
						Help:     "Optional persona flag.",
						Optional: true,
						Source: command.LiteralSource(command.Suggestion{
							Value:  "--persona",
							Label:  "--persona",
							Detail: "Attach a persona to the invited model.",
						}),
					},
					{
						Name:     "persona",
						Help:     "Persona text is free-form.",
						Optional: true,
						FreeForm: true,
					},
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					if s.active == "" {
						return noChannelCmd()
					}

					cmd := inv.Parsed.(command.InviteCommand)
					if cmd.Model == "" {
						return usageCmd("invite")
					}

					return s.inviteModel(domain.ModelID(cmd.Model), cmd.Persona)
				},
			},
			command.Handle("kick", "Remove a model instance from the current channel.", "/kick <nick>",
				map[string]command.SuggestionSource{"nick": command.ActiveMembersSource()},
				func(cmd command.KickCommand) tea.Cmd {
					if s.active == "" {
						return noChannelCmd()
					}

					return s.kickModel(domain.Nick(cmd.Nick))
				},
			),
			command.Handle("msg", "Open a direct message and optionally send text.", "/msg <nick> [message]",
				map[string]command.SuggestionSource{"nick": command.InstancesSource()},
				func(cmd command.MsgCommand) tea.Cmd {
					return s.directMessage(domain.Nick(cmd.Nick), strings.Join(cmd.Body, " "))
				},
			),
			command.Handle("nick", "Change your nickname.", "/nick <new-nick>",
				nil,
				func(cmd command.NickCommand) tea.Cmd {
					return s.changeNick(domain.Nick(cmd.Nick))
				},
			),
			command.Handle("topic", "Set or clear the current channel topic.", "/topic [text]",
				nil,
				func(cmd command.TopicCommand) tea.Cmd {
					if s.active == "" {
						return noChannelCmd()
					}

					return s.setTopic(strings.Join(cmd.Topic, " "))
				},
			),
			command.Handle("whois", "Show details about a model instance.", "/whois <nick>",
				map[string]command.SuggestionSource{"nick": command.InstancesSource()},
				func(cmd command.WhoisCommand) tea.Cmd {
					return s.whois(domain.Nick(cmd.Nick))
				},
			),
			{
				Name:  "config",
				Help:  "Update runtime configuration.",
				Usage: "/config <api-key|nick-model|poke-interval> [value]",
				Args: []command.ArgSpec{
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
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					return s.configure(inv.Parsed.(command.ConfigCommand))
				},
			},
			command.Handle("help", "Show available commands.", "/help",
				nil,
				func(_ command.HelpCommand) tea.Cmd {
					return s.showHelp()
				},
			),
			command.Handle("quit", "Exit modeloff.", "/quit",
				nil,
				func(_ command.QuitCommand) tea.Cmd {
					return tea.Quit
				},
			),
		},
	}

	return s.scope
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
	s.chatView.WithCommandState(s.CommandScope(), s.commandContext())
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
		ctx := s.ctx

		_, _ = s.sess.Join(ctx, string(ch))

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, ch)

		var topic string
		var members []domain.Member

		if channel, err := s.sess.GetChannel(ctx, ch); err == nil {
			topic = channel.Topic
			members = s.sortedMembers(channel.Members)
		}

		return channelSwitchedMsg{
			channel:  ch,
			topic:    topic,
			channels: channels,
			messages: messages,
			unread:   s.unreadCounts(ctx, channels),
			members:  members,
		}
	}
}

func (s *ChatScreen) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		_, _ = s.sess.SendMessage(ctx, s.active, text)

		messages, _ := s.sess.Messages(ctx, s.active)

		return messageSentMsg{
			channel:  s.active,
			messages: messages,
		}
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
