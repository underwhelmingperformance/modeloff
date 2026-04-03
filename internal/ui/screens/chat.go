package screens

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/session"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/components"
)

// chatLoadedMsg carries the initial data needed to render the chat
// screen after loading from the session.
type chatLoadedMsg struct {
	channels []domain.Channel
	active   domain.ChannelName
	title    string
	messages []domain.Message
}

// channelSwitchedMsg is sent after a channel switch completes,
// carrying the new channel's messages.
type channelSwitchedMsg struct {
	channel  domain.ChannelName
	title    string
	channels []domain.Channel
	messages []domain.Message
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
	channels     []domain.Channel
	active       domain.ChannelName
	title        string
	messages     []domain.Message
	systemEvents []string
}

// systemEventMsg carries system event text to display in the chat
// view without changing channel/sidebar state.
type systemEventMsg struct {
	lines []string
}

// PokeTickMsg triggers a background poke cycle for model instances.
type PokeTickMsg struct{}

// ChatScreen is the main screen that composes Sidebar, ChatView, and
// MainLayout. It holds a reference to the session for backend
// operations.
type ChatScreen struct {
	sess   *session.Session
	layout components.MainLayout

	active domain.ChannelName
	title  string
}

// NewChatScreen creates a chat screen backed by the given session.
func NewChatScreen(sess *session.Session) ChatScreen {
	sidebar := components.NewSidebar(nil, "")
	chatView := components.NewChatView("", sess.UserNick(), "", nil)
	layout := components.NewMainLayout(sidebar, chatView)

	return ChatScreen{
		sess:   sess,
		layout: layout,
	}
}

// Init implements ui.Model.
func (s ChatScreen) Init() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		channels, err := s.sess.ListChannels(ctx)
		if err != nil {
			channels = nil
		}

		active, err := s.sess.LastChannel(ctx)
		if err != nil {
			active = ""
		}

		var messages []domain.Message
		var title string

		if active != "" {
			messages, _ = s.sess.Messages(ctx, active)

			if ch, err := s.sess.GetChannel(ctx, active); err == nil {
				title = ch.Title
			}
		}

		return chatLoadedMsg{
			channels: channels,
			active:   active,
			title:    title,
			messages: messages,
		}
	}
}

// Update implements ui.Model.
func (s ChatScreen) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
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

	case PokeTickMsg:
		return s, s.handlePoke()

	case components.ChannelSelectedMsg:
		return s, s.switchChannel(msg.Channel)

	case components.MessageSubmitMsg:
		return s, s.sendMessage(msg.Text)

	case components.CommandSubmitMsg:
		return s, s.handleCommand(msg)
	}

	updated, cmd := s.layout.Update(msg)
	s.layout = updated.(components.MainLayout)

	return s, cmd
}

func (s ChatScreen) handleLoaded(msg chatLoadedMsg) (ui.Model, tea.Cmd) {
	s.active = msg.active
	s.title = msg.title

	sidebar := components.NewSidebar(msg.channels, msg.active)
	chatView := components.NewChatView(msg.active, s.sess.UserNick(), msg.title, msg.messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleChannelSwitched(msg channelSwitchedMsg) (ui.Model, tea.Cmd) {
	s.active = msg.channel
	s.title = msg.title

	sidebar := components.NewSidebar(msg.channels, msg.channel)
	chatView := components.NewChatView(msg.channel, s.sess.UserNick(), msg.title, msg.messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleMessageSent(msg messageSentMsg) (ui.Model, tea.Cmd) {
	chatView := components.NewChatView(msg.channel, s.sess.UserNick(), s.title, msg.messages)
	s.layout = components.NewMainLayout(s.layout.Sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleCommandResult(msg commandResultMsg) (ui.Model, tea.Cmd) {
	s.active = msg.active
	s.title = msg.title

	messages := msg.messages
	messages = appendSystemEvents(messages, s.active, msg.systemEvents)

	sidebar := components.NewSidebar(msg.channels, msg.active)
	chatView := components.NewChatView(msg.active, s.sess.UserNick(), msg.title, messages)
	s.layout = components.NewMainLayout(sidebar, chatView)

	return s, nil
}

func (s ChatScreen) handleSystemEvent(msg systemEventMsg) (ui.Model, tea.Cmd) {
	ctx := context.Background()

	messages, _ := s.sess.Messages(ctx, s.active)
	messages = appendSystemEvents(messages, s.active, msg.lines)

	chatView := components.NewChatView(s.active, s.sess.UserNick(), s.title, messages)
	s.layout = components.NewMainLayout(s.layout.Sidebar, chatView)

	return s, nil
}

func appendSystemEvents(messages []domain.Message, ch domain.ChannelName, events []string) []domain.Message {
	for _, line := range events {
		messages = append(messages, domain.Message{
			Channel: ch,
			From:    "***",
			Body:    line,
		})
	}

	return messages
}

func (s ChatScreen) switchChannel(ch domain.ChannelName) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, _ = s.sess.Join(ctx, string(ch))

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, ch)

		var title string
		if channel, err := s.sess.GetChannel(ctx, ch); err == nil {
			title = channel.Title
		}

		return channelSwitchedMsg{
			channel:  ch,
			title:    title,
			channels: channels,
			messages: messages,
		}
	}
}

func (s ChatScreen) sendMessage(text string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, _ = s.sess.SendMessage(ctx, s.active, text)

		messages, _ := s.sess.Messages(ctx, s.active)

		return messageSentMsg{
			channel:  s.active,
			messages: messages,
		}
	}
}

func (s ChatScreen) handleCommand(msg components.CommandSubmitMsg) tea.Cmd {
	raw := "/" + msg.Name
	if msg.Args != "" {
		raw += " " + msg.Args
	}

	parsed, err := command.Parse(raw)
	if err != nil {
		return func() tea.Msg {
			return systemEventMsg{lines: []string{err.Error()}}
		}
	}

	switch cmd := parsed.(type) {
	case command.JoinCommand:
		return s.joinChannel(cmd.Channel)

	case command.LeaveCommand:
		return s.leaveChannel()

	case command.NickCommand:
		return s.changeNick(domain.Nick(cmd.Nick))

	case command.TitleCommand:
		return s.setTitle(cmd.Title)

	case command.WhoisCommand:
		return s.whois(domain.Nick(cmd.Nick))

	case command.ListCommand:
		return s.listChannels()

	case command.InviteCommand:
		if cmd.Model == "" {
			return func() tea.Msg {
				return systemEventMsg{lines: []string{"usage: /invite <model-id> [--persona <text>]"}}
			}
		}

		return s.inviteModel(domain.ModelID(cmd.Model), cmd.Persona)

	case command.KickCommand:
		return s.kickModel(domain.Nick(cmd.Nick))

	case command.ConfigCommand:
		return s.configure(cmd)

	case command.MsgCommand:
		return s.directMessage(domain.Nick(cmd.Nick), cmd.Body)

	default:
		return nil
	}
}

func (s ChatScreen) configure(cmd command.ConfigCommand) tea.Cmd {
	return func() tea.Msg {
		const usage = "usage: /config api-key <value> | /config poke-interval <duration>"

		ctx := context.Background()

		switch cmd.Key {
		case "":
			return systemEventMsg{lines: []string{usage}}

		case "api-key":
			if strings.TrimSpace(cmd.Value) == "" {
				return systemEventMsg{lines: []string{"usage: /config api-key <value>"}}
			}

			if _, err := s.sess.SetAPIKey(ctx, strings.TrimSpace(cmd.Value)); err != nil {
				return systemEventMsg{lines: []string{err.Error()}}
			}

			return systemEventMsg{lines: []string{
				"OpenRouter API key saved. Restart modeloff to use it.",
			}}

		case "poke-interval":
			if strings.TrimSpace(cmd.Value) == "" {
				return systemEventMsg{lines: []string{"usage: /config poke-interval <duration>"}}
			}

			interval, err := time.ParseDuration(strings.TrimSpace(cmd.Value))
			if err != nil {
				return systemEventMsg{lines: []string{
					fmt.Sprintf("invalid duration %q: %v", cmd.Value, err),
				}}
			}

			if _, err := s.sess.SetPokeInterval(ctx, interval); err != nil {
				return systemEventMsg{lines: []string{err.Error()}}
			}

			return systemEventMsg{lines: []string{
				fmt.Sprintf("Poke interval set to %s.", interval),
			}}

		default:
			return systemEventMsg{lines: []string{
				fmt.Sprintf("unknown config key: %s", cmd.Key),
				usage,
			}}
		}
	}
}

func (s ChatScreen) directMessage(nick domain.Nick, body string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		ch, created, err := s.sess.OpenDM(ctx, nick)
		if err != nil {
			return systemEventMsg{lines: []string{
				fmt.Sprintf("no such nick: %s", nick),
			}}
		}

		if strings.TrimSpace(body) != "" {
			if _, err := s.sess.SendMessage(ctx, ch.Name, body); err != nil {
				return systemEventMsg{lines: []string{err.Error()}}
			}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, ch.Name)

		var systemEvents []string
		if created {
			systemEvents = []string{
				fmt.Sprintf("Opened direct message with %s", nick),
			}
		}

		return commandResultMsg{
			channels:     channels,
			active:       ch.Name,
			title:        "",
			messages:     messages,
			systemEvents: systemEvents,
		}
	}
}

func (s ChatScreen) handlePoke() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		if err := s.sess.Poke(ctx); err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
		}
	}
}

func (s ChatScreen) joinChannel(name string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		evt, err := s.sess.Join(ctx, name)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		active := domain.ChannelName(name)
		messages, _ := s.sess.Messages(ctx, active)

		var title string
		if ch, err := s.sess.GetChannel(ctx, active); err == nil {
			title = ch.Title
		}

		event := fmt.Sprintf("Switched to %s", active)
		if evt.Created {
			event = fmt.Sprintf("Created channel %s", active)
		}

		return commandResultMsg{
			channels:     channels,
			active:       active,
			title:        title,
			messages:     messages,
			systemEvents: []string{event},
		}
	}
}

func (s ChatScreen) leaveChannel() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, _ = s.sess.Leave(ctx, s.active)

		channels, _ := s.sess.ListChannels(ctx)

		var active domain.ChannelName
		var title string
		var messages []domain.Message

		if len(channels) > 0 {
			active = channels[0].Name
			title = channels[0].Title
			messages, _ = s.sess.Messages(ctx, active)
		}

		return commandResultMsg{
			channels: channels,
			active:   active,
			title:    title,
			messages: messages,
		}
	}
}

func (s ChatScreen) changeNick(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		evt := s.sess.ChangeNick(nick)

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
			systemEvents: []string{
				fmt.Sprintf("%s is now known as %s", evt.OldNick, evt.NewNick),
			},
		}
	}
}

func (s ChatScreen) setTitle(title string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		_, err := s.sess.SetTitle(ctx, s.active, title)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		event := fmt.Sprintf("topic for %s set to: %s", s.active, title)
		if title == "" {
			event = fmt.Sprintf("topic for %s cleared", s.active)
		}

		return commandResultMsg{
			channels:     channels,
			active:       s.active,
			title:        title,
			messages:     messages,
			systemEvents: []string{event},
		}
	}
}

func (s ChatScreen) whois(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		inst, err := s.sess.Whois(ctx, nick)
		if err != nil {
			return systemEventMsg{lines: []string{
				fmt.Sprintf("no such nick: %s", nick),
			}}
		}

		lines := []string{
			fmt.Sprintf("%s is %s", inst.Nick, inst.ModelID),
		}

		if inst.Persona != "" {
			lines = append(lines, fmt.Sprintf("  persona: %s", inst.Persona))
		}

		if len(inst.Channels) > 0 {
			var chStrs []string
			for ch := range inst.Channels.Sorted() {
				chStrs = append(chStrs, string(ch))
			}

			lines = append(lines, fmt.Sprintf("  channels: %s", strings.Join(chStrs, ", ")))
		}

		return systemEventMsg{lines: lines}
	}
}

func (s ChatScreen) inviteModel(modelID domain.ModelID, persona string) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		evt, err := s.sess.Invite(ctx, s.active, modelID, persona)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		event := fmt.Sprintf("%s (%s) has joined %s", evt.Instance.Nick, evt.Instance.ModelID, evt.Channel)
		if evt.Instance.Persona != "" {
			event = fmt.Sprintf("%s with persona %q", event, evt.Instance.Persona)
		}

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
			systemEvents: []string{event},
		}
	}
}

func (s ChatScreen) kickModel(nick domain.Nick) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		evt, err := s.sess.Kick(ctx, s.active, nick)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		channels, _ := s.sess.ListChannels(ctx)
		messages, _ := s.sess.Messages(ctx, s.active)

		return commandResultMsg{
			channels: channels,
			active:   s.active,
			title:    s.title,
			messages: messages,
			systemEvents: []string{
				fmt.Sprintf("%s has been kicked from %s", evt.Nick, evt.Channel),
			},
		}
	}
}

func (s ChatScreen) listChannels() tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()

		channels, err := s.sess.ListChannels(ctx)
		if err != nil {
			return systemEventMsg{lines: []string{err.Error()}}
		}

		if len(channels) == 0 {
			return systemEventMsg{lines: []string{"no channels"}}
		}

		lines := make([]string, len(channels))
		for i, ch := range channels {
			line := string(ch.Name)
			if ch.Title != "" {
				line += " — " + ch.Title
			}

			lines[i] = line
		}

		return systemEventMsg{lines: lines}
	}
}

// View implements ui.Model.
func (s ChatScreen) View(width, height int) string {
	return s.layout.View(width, height)
}
