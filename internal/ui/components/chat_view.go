package components

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// SetChannelMsg updates the channel identity and topic for a channel
// switch. The message list re-reads its events through the injected
// getter; no explicit history payload is carried.
type SetChannelMsg struct {
	Channel domain.ChannelName
	Topic   string
	Kind    domain.ChannelKind
}

// ScrollbackClearedMsg tells the message list that a window's
// history is gone, either because `/clear` emptied it or because the
// user left the window. The list drops the reader's place in that
// window, so the next thing to arrive in it is new.
type ScrollbackClearedMsg struct {
	Channel domain.ChannelName
}

// ScrollbackUpdatedMsg signals that the scrollback for the named
// channel has been appended to and the active view should re-evaluate
// — picking up new content for the active window and arming the
// new-messages divider if the user is scrolled up. The chat-screen
// emits it after every event it commits to a window's scrollback.
type ScrollbackUpdatedMsg struct {
	Channel domain.ChannelName
}

// SetLinesMsg replaces the displayed lines, preserving divider logic.
type SetLinesMsg struct {
	Lines []tea.Msg
}

// SetPlaceholderMsg sets text to show when there are no messages.
type SetPlaceholderMsg struct {
	Text string
}

// TopicUpdatedMsg updates the topic displayed in the chat view
// header without replacing the channel or message lines.
type TopicUpdatedMsg struct {
	Topic string
}

// CompleterMsg sets the completer used by the input bar's popover.
type CompleterMsg struct {
	Completer command.Completable
}

// SecretCheckerMsg sets the checker the input bar's history uses to
// keep a secret-bearing command line out of ↑ recall. The grammar is
// static, so unlike CompleterMsg — which rebinds whenever chat-screen
// state the completion context closes over moves — this is sent once,
// at startup.
type SecretCheckerMsg struct {
	Checker command.SecretChecker
}

// CommandsMsg sets the command tree walked by `/help` rendering.
type CommandsMsg[C command.KindProvider] struct {
	Commands []*command.Node[C]
}

// UserNickMsg updates the user's nick in the chat view and input bar.
type UserNickMsg struct {
	Nick domain.Nick
}

// ChatView displays messages for a single channel with an input bar
// at the bottom. C is the grammar's completion-context type; the
// view carries it so its MessageList stores a typed command tree for
// `/help` rendering and its popover dispatches typed completions.
type ChatView[C command.KindProvider] struct {
	channel domain.ChannelName
	// kind governs kind-sensitive decisions: the glyph/style used for
	// system notices in the message list, and which of headerText's
	// two shapes the window header renders (the channel name plus
	// its topic for a channel, or a DM's "@nick").
	kind     domain.ChannelKind
	topic    string
	userNick domain.Nick
	messages MessageList[C]
	input    InputBar
	keyMap   ChatViewKeyMap

	bounds ui.Rect
}

type chatViewLayout struct {
	InputRect   ui.Rect
	PaletteRect ui.Rect
	MessageRect ui.Rect
}

// NewChatView creates a chat view for the given channel. The
// caller passes the real initial kind — the view renders against it
// from the first frame, rather than accepting a default and waiting
// for a later SetChannelMsg to correct it. Subsequent SetChannelMsg
// messages update the kind atomically when the user switches
// channels.
//
// `content` is the closure the embedded [MessageList] consults on
// every `View` for the window in view and its scrollback. The chat
// screen owns the storage; this view is a pure read over it.
func NewChatView[C command.KindProvider](
	content func() WindowContent,
	ch domain.ChannelName,
	kind domain.ChannelKind,
	userNick domain.Nick,
	topic string,
) ChatView[C] {
	keyMap := DefaultChatViewKeyMap

	ml := NewMessageList[C](content, kind).SetKeyMap(keyMap)

	return ChatView[C]{
		channel:  ch,
		kind:     kind,
		topic:    topic,
		userNick: userNick,
		messages: ml,
		input:    NewInputBar(userNick),
		keyMap:   keyMap,
	}
}

// Init implements ui.Model.
func (c ChatView[C]) Init() tea.Cmd {
	return c.input.Init()
}

// KeyBindings implements ui.Keybinding.
func (c ChatView[C]) KeyBindings() []ui.KeyBinding {
	bindings := []ui.KeyBinding{
		ui.WithBindingEnabled(
			ui.Bind(key.NewBinding(
				key.WithKeys("pgup", "pgdown"),
				key.WithHelp("PgUp/Dn", "scroll"),
			)),
			c.messages.Len() > 0,
		),
		ui.WithBindingEnabled(
			ui.Bind(key.NewBinding(
				key.WithKeys("ctrl+up", "ctrl+down"),
				key.WithHelp("^↑/↓", "scroll"),
			)),
			c.messages.Len() > 0,
		),
	}

	bindings = append(bindings, ui.CollectKeyBindings(c.input)...)

	return bindings
}

// Update implements ui.Model.
func (c ChatView[C]) Update(msg tea.Msg) (ui.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		c.bounds = msg.Rect
		c, inputCmd := c.updateInput(msg)
		c, syncCmd := c.syncMessageViewport()
		return c, tea.Batch(inputCmd, syncCmd)

	case SetChannelMsg:
		c.channel = msg.Channel
		c.kind = msg.Kind
		c.topic = msg.Topic

		c, msgCmd := c.updateMessages(msg)
		c, syncCmd := c.syncMessageViewport()

		return c, tea.Batch(msgCmd, syncCmd)

	case TopicUpdatedMsg:
		c.topic = msg.Topic
		c, syncCmd := c.syncMessageViewport()
		return c, syncCmd

	case UserNickMsg:
		c.userNick = msg.Nick
		c, inputCmd := c.updateInput(msg)

		return c, inputCmd

	case NickListUpdatedMsg:
		c, inputCmd := c.updateInput(msg)

		return c, inputCmd

	case SetPlaceholderMsg, HighlightWordsMsg, TimestampFormatMsg:
		c, msgCmd := c.updateMessages(msg)
		c, syncCmd := c.syncMessageViewport()

		return c, tea.Batch(msgCmd, syncCmd)

	case CommandsMsg[C]:
		c, msgCmd := c.updateMessages(msg)
		c, syncCmd := c.syncMessageViewport()

		return c, tea.Batch(msgCmd, syncCmd)

	case CompleterMsg:
		c, inputCmd := c.updateInput(msg)

		return c, inputCmd

	case SecretCheckerMsg:
		c, inputCmd := c.updateInput(msg)

		return c, inputCmd

	case tea.MouseMsg:
		if updated, handled, cmd := c.handleMouse(msg); handled {
			return updated, cmd
		}
	}

	// Forward to message list for viewport navigation, then input bar.
	c, mlCmd := c.updateMessages(msg)

	c, inputCmd := c.updateInput(msg)
	c, syncCmd := c.syncMessageViewport()

	return c, tea.Batch(mlCmd, inputCmd, syncCmd)
}

func (c ChatView[C]) handleMouse(msg tea.MouseMsg) (ChatView[C], bool, tea.Cmd) {
	if c.bounds.Width == 0 || c.bounds.Height == 0 {
		return c, false, nil
	}

	layout := c.layoutRects()

	if layout.PaletteRect.Contains(msg.X, msg.Y) {
		localX, localY := layout.PaletteRect.Local(msg.X, msg.Y)
		local := msg
		local.X = localX
		local.Y = localY

		updated, handled, cmd := c.input.HandlePaletteMouse(local)
		if handled {
			c.input = updated
			return c, true, cmd
		}
	}

	if layout.InputRect.Contains(msg.X, msg.Y) {
		updated, cmd := c.input.Update(msg)
		c.input = updated.(InputBar)

		if cmd != nil {
			return c, true, cmd
		}

		return c, true, nil
	}

	if layout.MessageRect.Contains(msg.X, msg.Y) && msg.Action == tea.MouseActionPress {
		switch msg.Button {
		case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
			c, mlCmd := c.updateMessages(msg)
			c, syncCmd := c.syncMessageViewport()

			return c, true, tea.Batch(mlCmd, syncCmd)
		}
	}

	return c, false, nil
}

// View implements ui.Model.
func (c ChatView[C]) View(width, height int) string {
	inputView := c.input.View(width, 1)
	inputHeight := lipgloss.Height(inputView)

	paletteView := c.input.PaletteView(width)
	paletteHeight := 0
	if paletteView != "" {
		paletteHeight = lipgloss.Height(paletteView)
	}

	headerView := c.renderHeader(width)
	headerHeight := 0
	if headerView != "" {
		headerHeight = lipgloss.Height(headerView)
	}

	messageListHeight := max(height-inputHeight-headerHeight-paletteHeight, 0)

	messageView := c.messages.View(width, messageListHeight)

	parts := make([]string, 0, 4)
	if headerView != "" {
		parts = append(parts, headerView)
	}

	parts = append(parts, messageView)

	if paletteView != "" {
		parts = append(parts, paletteView)
	}

	parts = append(parts, inputView)

	view := lipgloss.JoinVertical(lipgloss.Left, parts...)
	if lipgloss.Height(view) >= height {
		return view
	}

	return lipgloss.Place(width, height, lipgloss.Left, lipgloss.Bottom, view)
}

func (c ChatView[C]) layoutRects() chatViewLayout {
	width := c.bounds.Width
	if width <= 0 {
		return chatViewLayout{}
	}

	inputView := c.input.View(width, 1)
	inputHeight := lipgloss.Height(inputView)
	paletteHeight := c.input.PaletteHeight(width)

	inputRect := ui.Rect{
		X:      c.bounds.X,
		Y:      c.bounds.Y + c.bounds.Height - inputHeight,
		Width:  width,
		Height: inputHeight,
	}

	paletteRect := ui.Rect{
		X:      c.bounds.X,
		Y:      inputRect.Y - paletteHeight,
		Width:  width,
		Height: paletteHeight,
	}

	headerHeight := 0
	if headerView := c.renderHeader(width); headerView != "" {
		headerHeight = lipgloss.Height(headerView)
	}

	messageRect := ui.Rect{
		X:      c.bounds.X,
		Y:      c.bounds.Y + headerHeight,
		Width:  width,
		Height: c.bounds.Height - headerHeight - paletteHeight - inputHeight,
	}
	if messageRect.Height < 0 {
		messageRect.Height = 0
	}

	return chatViewLayout{
		InputRect:   inputRect,
		PaletteRect: paletteRect,
		MessageRect: messageRect,
	}
}

func (c ChatView[C]) updateMessages(msg tea.Msg) (ChatView[C], tea.Cmd) {
	updated, cmd := c.messages.Update(msg)
	c.messages = updated.(MessageList[C])

	return c, cmd
}

func (c ChatView[C]) syncMessageViewport() (ChatView[C], tea.Cmd) {
	layout := c.layoutRects()

	updated, cmd := c.messages.Update(ui.BoundsMsg{Rect: layout.MessageRect})
	c.messages = updated.(MessageList[C])

	return c, cmd
}

func (c ChatView[C]) updateInput(msg tea.Msg) (ChatView[C], tea.Cmd) {
	updated, cmd := c.input.Update(msg)
	c.input = updated.(InputBar)

	return c, cmd
}

// headerText names the window in view: the channel name plus its
// topic when one is set, or the counterpart's nick for a DM (a DM
// window's `Name()` is already the counterpart's nick, matching
// [domain.Window.DisplayName]'s "@nick" convention for DMs).
//
// `channel` starts as "" at construction, so the view's first frame
// does not have to wait for the `SetChannelMsg` round trip that
// supplies the real window; there is nothing to name during that
// span, so headerText answers "".
func (c ChatView[C]) headerText() string {
	if c.channel == "" {
		return ""
	}

	if c.kind == domain.KindDM {
		return "@" + string(c.channel)
	}

	text := string(c.channel)
	if c.topic != "" {
		text += ": " + c.topic
	}

	return text
}

// renderHeader draws the window header: every window in view gets
// one, so a topicless channel and a DM are identified the same way a
// topic-bearing channel is. While headerText is still "" (before the
// first SetChannelMsg), it renders nothing at all, not even the
// border rule, so that span costs no row and draws no stray line.
func (c ChatView[C]) renderHeader(width int) string {
	text := c.headerText()
	if text == "" {
		return ""
	}

	style := theme.PaneBorder.BorderBottom(true).Width(width)

	return style.Render(theme.ChannelTitle.Render(text))
}
