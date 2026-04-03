package components

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// MessagesUpdatedMsg tells the chat view to refresh its message list.
type MessagesUpdatedMsg struct {
	Room     domain.RoomName
	Messages []domain.Message
}

// ChatView displays messages for a single room with an input bar at
// the bottom.
type ChatView struct {
	room     domain.RoomName
	userNick domain.Nick
	messages []domain.Message
	input    InputBar
	scroll   int
}

// NewChatView creates a chat view for the given room.
func NewChatView(room domain.RoomName, userNick domain.Nick, messages []domain.Message) ChatView {
	return ChatView{
		room:     room,
		userNick: userNick,
		messages: messages,
		input:    NewInputBar(),
	}
}

// Init implements ui.Model.
func (c ChatView) Init() tea.Cmd {
	return c.input.Init()
}

// Update implements ui.Model.
func (c ChatView) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case MessagesUpdatedMsg:
		if msg.Room != c.room {
			return c, nil
		}

		c.messages = msg.Messages
		c.scroll = 0

		return c, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyPgUp {
			c.scroll++
			return c, nil
		}

		if msg.Type == tea.KeyPgDown {
			if c.scroll > 0 {
				c.scroll--
			}
			return c, nil
		}
	}

	updated, cmd := c.input.Update(msg)
	c.input = updated.(InputBar)

	return c, cmd
}

// View implements ui.Model.
func (c ChatView) View(width, height int) string {
	inputView := c.input.View(width, 1)
	inputHeight := lipgloss.Height(inputView)

	listHeight := height - inputHeight
	if listHeight < 0 {
		listHeight = 0
	}

	messageView := c.renderMessages(width, listHeight)

	return lipgloss.JoinVertical(lipgloss.Left, messageView, inputView)
}

func (c ChatView) renderMessages(width, height int) string {
	if len(c.messages) == 0 {
		return lipgloss.Place(width, height,
			lipgloss.Center, lipgloss.Center,
			theme.Dim.Render("No messages yet"))
	}

	lines := make([]string, 0, len(c.messages))

	for _, msg := range c.messages {
		lines = append(lines, c.renderMessage(msg))
	}

	// Apply scroll offset from the bottom.
	end := len(lines) - c.scroll
	if end < 0 {
		end = 0
	}

	start := end - height
	if start < 0 {
		start = 0
	}

	visible := lines[start:end]

	content := strings.Join(visible, "\n")

	// Pad to fill the available height so the input bar stays at the
	// bottom.
	rendered := lipgloss.Height(content)
	if rendered < height {
		padding := strings.Repeat("\n", height-rendered-1)
		content = padding + content
	}

	return content
}

func (c ChatView) renderMessage(msg domain.Message) string {
	style := theme.ModelNick
	if msg.From == c.userNick {
		style = theme.UserNick
	}

	nick := style.Render(fmt.Sprintf("<%s>", string(msg.From)))

	return fmt.Sprintf("%s %s", nick, msg.Body)
}

// RenderSystemEvent formats a system event (join, part, topic change)
// in the SystemEvent style.
func RenderSystemEvent(text string) string {
	return theme.SystemEvent.Render("*** " + text)
}
