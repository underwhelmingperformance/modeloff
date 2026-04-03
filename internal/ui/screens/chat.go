package screens

import (
	"context"
	"slices"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/set"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// chatLoadedMsg carries the initial data needed to render the chat
// screen after loading from the session.
type chatLoadedMsg struct {
	channels  []domain.Channel
	instances []domain.ModelInstance
	active    domain.ChannelName
	title     string
	messages  []domain.Message
	unread    map[domain.ChannelName]int
	members   []domain.Member
}

// channelSwitchedMsg is sent after a channel switch completes,
// carrying the new channel's messages.
type channelSwitchedMsg struct {
	channel  domain.ChannelName
	title    string
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
	title     string
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

	channels     []domain.Channel
	instances    []domain.ModelInstance
	liveModels   []command.ModelOption
	width        int
	height       int
	active       domain.ChannelName
	title        string
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
		var title string
		var members []domain.Member

		if active != "" {
			messages, _ = s.sess.Messages(ctx, active)

			if ch, err := s.sess.GetChannel(ctx, active); err == nil {
				title = ch.Title
				members = s.sortedMembers(ch.Members)
			}
		}

		return chatLoadedMsg{
			channels:  channels,
			instances: instances,
			active:    active,
			title:     title,
			messages:  messages,
			unread:    s.unreadCounts(ctx, channels),
			members:   members,
		}
	}

	return tea.Batch(loadInitial, s.loadLiveModels())
}

// Update implements ui.Model.
func (s *ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		s.width = msg.Width
		s.height = msg.Height

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

	updated, cmd := s.layout.Update(msg)
	s.layout = updated.(components.MainLayout)

	return s, cmd
}

func (s *ChatScreen) handleLoaded(msg chatLoadedMsg) (ui.Model, tea.Cmd) {
	s.channels = msg.channels
	s.instances = msg.instances
	s.active = msg.active
	s.title = msg.title
	s.channelCount = len(msg.channels)

	s.updateSidebar(msg.channels, msg.active, msg.unread)
	s.chatView.SetChannel(msg.active, msg.title, components.MessagesToLines(msg.messages))
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
	s.title = msg.title
	s.channelCount = len(msg.channels)

	s.updateSidebar(msg.channels, msg.channel, msg.unread)
	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.channel, msg.title, components.MessagesToLines(msg.messages))
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
	s.title = msg.title
	s.channelCount = len(msg.channels)

	lines := components.MessagesToLines(msg.messages)
	lines = append(lines, msg.events...)

	s.updateSidebar(msg.channels, msg.active, msg.unread)
	s.chatView.SetPlaceholder("")
	s.chatView.SetChannel(msg.active, msg.title, lines)
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
	return command.Scope{
		Commands: []command.Spec{
			{
				Name:  "join",
				Help:  "Switch to a channel or create it if needed.",
				Usage: "/join <channel>",
				Args: []command.ArgSpec{
					{Name: "channel", Help: "Select an existing channel or type a new one.", Source: command.ChannelsSource()},
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					return s.joinChannel(parsed.(command.JoinCommand).Channel)
				},
			},
			{
				Name:  "leave",
				Help:  "Leave the current channel.",
				Usage: "/leave",
				Handler: func(command.Invocation) tea.Cmd {
					if s.active == "" {
						return noChannelCmd()
					}

					return s.leaveChannel()
				},
			},
			{
				Name:  "list",
				Help:  "List all known channels.",
				Usage: "/list",
				Handler: func(command.Invocation) tea.Cmd {
					return s.listChannels()
				},
			},
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

					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					cmd := parsed.(command.InviteCommand)
					if cmd.Model == "" {
						return usageCmd("invite")
					}

					return s.inviteModel(domain.ModelID(cmd.Model), cmd.Persona)
				},
			},
			{
				Name:  "kick",
				Help:  "Remove a model instance from the current channel.",
				Usage: "/kick <nick>",
				Args: []command.ArgSpec{
					{Name: "nick", Help: "Choose a member of the current channel.", Source: command.ActiveMembersSource()},
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					if s.active == "" {
						return noChannelCmd()
					}

					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					return s.kickModel(domain.Nick(parsed.(command.KickCommand).Nick))
				},
			},
			{
				Name:  "msg",
				Help:  "Open a direct message and optionally send text.",
				Usage: "/msg <nick> [message]",
				Args: []command.ArgSpec{
					{Name: "nick", Help: "Choose a known instance nick.", Source: command.InstancesSource()},
					{Name: "message", Help: "Message text is free-form.", Optional: true, FreeForm: true},
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					cmd := parsed.(command.MsgCommand)
					return s.directMessage(domain.Nick(cmd.Nick), cmd.Body)
				},
			},
			{
				Name:  "nick",
				Help:  "Change your nickname.",
				Usage: "/nick <new-nick>",
				Args: []command.ArgSpec{
					{Name: "new-nick", Help: "Nicknames are free-form.", FreeForm: true},
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					return s.changeNick(domain.Nick(parsed.(command.NickCommand).Nick))
				},
			},
			{
				Name:  "title",
				Help:  "Set or clear the current channel title.",
				Usage: "/title [text]",
				Args: []command.ArgSpec{
					{Name: "text", Help: "Title text is free-form.", Optional: true, FreeForm: true},
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					if s.active == "" {
						return noChannelCmd()
					}

					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					return s.setTitle(parsed.(command.TitleCommand).Title)
				},
			},
			{
				Name:  "whois",
				Help:  "Show details about a model instance.",
				Usage: "/whois <nick>",
				Args: []command.ArgSpec{
					{Name: "nick", Help: "Choose a known instance nick.", Source: command.InstancesSource()},
				},
				Handler: func(inv command.Invocation) tea.Cmd {
					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					return s.whois(domain.Nick(parsed.(command.WhoisCommand).Nick))
				},
			},
			{
				Name:  "config",
				Help:  "Update runtime configuration.",
				Usage: "/config <api-key|poke-interval> [value]",
				Args: []command.ArgSpec{
					{
						Name: "key",
						Help: "Choose a config key.",
						Source: command.LiteralSource(
							command.Suggestion{Value: "api-key", Label: "api-key", Detail: "Activate OpenRouter immediately."},
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
					parsed, err := command.Parse(inv.Raw)
					if err != nil {
						return errorCmd(err)
					}

					return s.configure(parsed.(command.ConfigCommand))
				},
			},
			{
				Name:  "help",
				Help:  "Show available commands.",
				Usage: "/help",
				Handler: func(command.Invocation) tea.Cmd {
					return s.showHelp()
				},
			},
			{
				Name:  "quit",
				Help:  "Exit modeloff.",
				Usage: "/quit",
				Handler: func(command.Invocation) tea.Cmd {
					return tea.Quit
				},
			},
		},
	}
}

func errorCmd(err error) tea.Cmd {
	return func() tea.Msg {
		return systemEventMsg{events: []components.ChatLine{components.CommandError{Err: err}}}
	}
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
	s.layout = components.NewMainLayout(s.layout.Sidebar, s.chatView)
	s.layout.SetNickList(s.layout.NickList)
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

	updated, _ := s.layout.Update(tea.WindowSizeMsg{Width: s.width, Height: s.height})
	s.layout = updated.(components.MainLayout)
}

func (s *ChatScreen) switchChannel(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg {
		ctx := s.ctx

		_, _ = s.sess.Join(ctx, string(ch))

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, ch)

		var title string
		var members []domain.Member

		if channel, err := s.sess.GetChannel(ctx, ch); err == nil {
			title = channel.Title
			members = s.sortedMembers(channel.Members)
		}

		return channelSwitchedMsg{
			channel:  ch,
			title:    title,
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

// View implements ui.Model.
func (s *ChatScreen) View(width, height int) string {
	return s.layout.View(width, height)
}
