package screens

import (
	"context"
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

// eventBatchMsg carries multiple domain events from a single command.
// Update unpacks and dispatches each event sequentially.
type eventBatchMsg struct {
	events []any
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
	events   chatEventHandler

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
	layout := components.NewMainLayout(sidebar, chatView)
	layout.SetNickList(components.NewNickList(nil))

	cs := &ChatScreen{
		ctx:      ctx,
		sess:     sess,
		layout:   layout,
		chatView: chatView,
		keyMap:   components.DefaultChatScreenKeyMap,
	}
	cs.events = chatEventHandler{screen: cs}

	return cs
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

	case domain.InitialLoadEvent:
		return s.events.handleInitialLoad(msg)

	case eventBatchMsg:
		return s.events.handleEventBatch(msg)

	case apiKeyActivatedMsg:
		cmd := msgCmd(components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.APIKeySaved{}},
		})

		return s, tea.Batch(cmd, s.loadLiveModels())

	case domain.JoinEvent:
		return s.events.handleJoinEvent(msg)

	case domain.PartEvent:
		return s.events.handlePartEvent(msg)

	case domain.TopicChangeEvent:
		return s.events.handleTopicChangeEvent(msg)

	case domain.NickChangeEvent:
		return s.events.handleNickChangeEvent(msg)

	case domain.ModelInvitedEvent:
		return s.events.handleModelInvitedEvent(msg)

	case domain.ModelKickedEvent:
		return s.events.handleModelKickedEvent(msg)

	case domain.MessageEvent:
		return s.events.handleMessageEvent(msg)

	case domain.ModelReplyEvent:
		return s.events.handleModelReplyEvent(msg)

	case domain.DMOpenedEvent:
		return s.events.handleDMOpenedEvent(msg)

	case domain.ConfigChangedEvent:
		return s.events.handleConfigChangedEvent(msg)

	case domain.ErrorEvent:
		return s.events.handleErrorEvent(msg)

	case liveModelsLoadedMsg:
		return s.events.handleLiveModelsLoaded(msg)

	case PokeTickMsg:
		cmd := msgCmd(components.PendingResponseMsg{Pending: true})

		return s, tea.Batch(cmd, s.handlePoke())

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case components.MessageSubmitMsg:
		if s.active == "" {
			return s, s.noChannelCmd()
		}

		cmd := msgCmd(components.PendingResponseMsg{Pending: true})

		return s, tea.Batch(cmd, s.sendMessage(msg.Text))

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
			return s.noChannelCmd()
		}

		return s.leaveChannel()
	})

	command.Bind(cmds, "list", func(_ command.ListCommand) tea.Cmd {
		return s.listChannels()
	})

	command.Bind(cmds, "invite", func(cmd command.InviteCommand) tea.Cmd {
		if s.active == "" {
			return s.noChannelCmd()
		}

		if cmd.Model == "" {
			return s.usageCmd("invite")
		}

		return s.inviteModel(domain.ModelID(cmd.Model), strings.Join(cmd.Persona, " "))
	})

	command.Bind(cmds, "kick", func(cmd command.KickCommand) tea.Cmd {
		if s.active == "" {
			return s.noChannelCmd()
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
			return s.noChannelCmd()
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

func (s *ChatScreen) usageCmd(commandName string) tea.Cmd {
	return func() tea.Msg {
		return components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.UsageHint{Command: commandName}},
		}
	}
}

func (s *ChatScreen) noChannelCmd() tea.Cmd {
	return func() tea.Msg {
		return components.AppendLinesMsg{
			Channel: s.active,
			Lines:   []components.ChatLine{components.NoChannel{}},
		}
	}
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

		events := make([]any, 0, 1+len(replies))
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
