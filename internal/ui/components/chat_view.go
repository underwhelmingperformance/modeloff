package components

import (
	"image"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	uvlayout "github.com/charmbracelet/ultraviolet/layout"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui"
	"github.com/laney/modeloff/internal/ui/theme"
)

// SetChannelMsg updates the stable window identity, presentation name,
// topic and kind for a window switch. The message list re-reads its events
// through the injected getter; no explicit history payload is carried.
type SetChannelMsg struct {
	Channel     domain.ChannelName
	DisplayName string
	Topic       string
	Kind        domain.ChannelKind
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

// CompleterMsg sets the completer the window's completion popover
// consults.
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
	channel     domain.ChannelName
	displayName string

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

	// layers holds the command-completion popover, which is drawn over
	// the transcript. The stack is the only place it is kept, so
	// drawing it, offering it a key and routing a message to it all
	// read the same value.
	layers ui.LayerStack

	bounds uv.Rectangle
}

// popoverLayerID names the completion popover on the window's stack.
const popoverLayerID ui.LayerID = "completion-popover"

type chatViewLayout struct {
	InputRect   uv.Rectangle
	MessageRect uv.Rectangle

	// PopoverRect is where the completions float, over the transcript,
	// so it takes no rows from MessageRect.
	PopoverRect uv.Rectangle
}

// NewChatView creates a chat view for the given channel. The
// caller passes the real initial kind — the view renders against it
// from the first frame, rather than accepting a default and waiting
// for a later SetChannelMsg to correct it. Subsequent SetChannelMsg
// messages update the kind atomically when the user switches
// channels.
//
// `content` is the closure the embedded [MessageList] consults on
// every draw for the window in view and its scrollback. The chat
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

// Init implements ui.Component.
func (c ChatView[C]) Init() tea.Cmd {
	return c.input.Init()
}

// KeyBindings implements ui.Keybinding.
func (c ChatView[C]) KeyBindings() []ui.KeyBinding {
	// A locked window takes no keys at all, so it has none to name.
	// Root's own keys are still described, and they still work.
	if c.input.locked {
		return nil
	}

	scrollable := c.messages.Len() > 0

	var bindings []ui.KeyBinding
	for _, binding := range c.keyMap.Bindings() {
		bindings = append(bindings, ui.WithBindingEnabled(binding, scrollable))
	}

	// The popover is in front of the input bar and takes a key before
	// it, so its keys are named first and the bar's only when the
	// popover leaves them reachable. Up and Down are the example:
	// while there are suggestions to cycle, they do not reach input
	// history, and naming history there would offer the operator
	// something the key will not do.
	popover := c.popover()
	claimed := popover.claimedKeys()

	bindings = append(bindings, popover.KeyBindings()...)

	for _, binding := range ui.CollectKeyBindings(c.input) {
		if !shadowed(binding, claimed) {
			bindings = append(bindings, binding)
		}
	}

	return bindings
}

// Update implements ui.Component.
func (c ChatView[C]) Update(msg tea.Msg) (ui.Component, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.BoundsMsg:
		c.bounds = msg.Rect
		c, syncCmd := c.syncChildBounds()
		return c, syncCmd

	case SetChannelMsg:
		c.channel = msg.Channel
		c.displayName = msg.DisplayName
		c.kind = msg.Kind
		c.topic = msg.Topic

		c, msgCmd := c.updateMessages(msg)
		c, syncCmd := c.syncChildBounds()

		return c, tea.Batch(msgCmd, syncCmd)

	case TopicUpdatedMsg:
		c.topic = msg.Topic
		c, syncCmd := c.syncChildBounds()
		return c, syncCmd

	case UserNickMsg:
		c.userNick = msg.Nick
		c, inputCmd := c.updateInput(msg)
		c, syncCmd := c.syncChildBounds()

		return c, tea.Batch(inputCmd, syncCmd)

	case NickListUpdatedMsg:
		c, inputCmd := c.updateInput(msg)
		c, syncCmd := c.syncChildBounds()

		return c, tea.Batch(inputCmd, syncCmd)

	case SetPlaceholderMsg, HighlightWordsMsg, TimestampFormatMsg:
		c, msgCmd := c.updateMessages(msg)
		c, syncCmd := c.syncChildBounds()

		return c, tea.Batch(msgCmd, syncCmd)

	case CommandsMsg[C]:
		c, msgCmd := c.updateMessages(msg)
		c, syncCmd := c.syncChildBounds()

		return c, tea.Batch(msgCmd, syncCmd)

	case CompleterMsg:
		return c.updatePopover(PopoverApplyMsg{
			Completer: msg.Completer,
			Raw:       c.input.Value(),
			Cursor:    c.input.Cursor(),
		})

	case SecretCheckerMsg:
		c, inputCmd := c.updateInput(msg)
		c, syncCmd := c.syncChildBounds()

		return c, tea.Batch(inputCmd, syncCmd)

	case PopoverRefreshMsg, PopoverDismissMsg:
		return c.updatePopover(msg)

	case tea.MouseMsg:
		if updated, handled, cmd := c.handleMouse(msg); handled {
			updated, syncCmd := updated.syncChildBounds()
			return updated, tea.Batch(cmd, syncCmd)
		}

		return c, nil

	case tea.KeyPressMsg:
		// A locked bar ignores keys while the client shuts down, and
		// the completions in front of it have to ignore them too: a
		// layer offered the key first would go on accepting suggestions
		// into a line nothing will send.
		if c.input.locked {
			return c, nil
		}

		// The layers are offered the key before this window's own
		// children, and pass on what they do not want.
		layers, handled, cmd := c.layers.HandleKey(msg)
		c.layers = layers
		if handled {
			c, syncCmd := c.syncChildBounds()

			return c, tea.Batch(cmd, syncCmd)
		}
	}

	// Forward to message list for viewport navigation, then input bar.
	c, mlCmd := c.updateMessages(msg)

	c, inputCmd := c.updateInput(msg)
	c, syncCmd := c.syncChildBounds()

	return c, tea.Batch(mlCmd, inputCmd, syncCmd)
}

func (c ChatView[C]) handleMouse(msg tea.MouseMsg) (ChatView[C], bool, tea.Cmd) {
	if _, released := msg.(tea.MouseReleaseMsg); released {
		updated, cmd := c.input.Update(msg)
		c.input = updated.(InputBar)

		return c, true, cmd
	}

	if c.bounds.Dx() == 0 || c.bounds.Dy() == 0 {
		return c, false, nil
	}

	layout := c.layoutRects()
	mouse := msg.Mouse()

	// The popover is in front of the transcript, so it is offered a
	// pointer event before the window's own children.
	if layer, ok := c.layers.MouseLayer(image.Pt(mouse.X, mouse.Y), c.bounds); ok {
		layers, cmd := c.layers.UpdateID(layer.ID(), msg)
		c.layers = layers

		return c, true, cmd
	}

	// The mouse event missed the popover, so a left click closes the
	// suggestion list. A click that lands in the input moves the
	// cursor, and the refresh that follows computes the suggestions for
	// the cursor's new position: the operator is still composing that
	// line, and the list opens again on the next keystroke.
	var dismissCmd tea.Cmd

	_, dismissed := msg.(tea.MouseClickMsg)
	dismissed = dismissed && mouse.Button == tea.MouseLeft
	if dismissed {
		c, dismissCmd = c.updatePopover(PopoverDismissMsg{Raw: c.input.Value()})
	}

	if contains(layout.InputRect, mouse.X, mouse.Y) {
		c, inputCmd := c.updateInput(msg)

		return c, true, tea.Batch(dismissCmd, inputCmd)
	}

	if _, ok := msg.(tea.MouseWheelMsg); ok && contains(layout.MessageRect, mouse.X, mouse.Y) {
		switch mouse.Button {
		case tea.MouseWheelUp, tea.MouseWheelDown:
			c, mlCmd := c.updateMessages(msg)
			c, syncCmd := c.syncChildBounds()

			return c, true, tea.Batch(mlCmd, syncCmd)
		}
	}

	if dismissed {
		updated, cmd := c.input.Update(msg)
		c.input = updated.(InputBar)

		return c, true, tea.Batch(dismissCmd, cmd)
	}

	return c, false, nil
}

func (c ChatView[C]) layoutRects() chatViewLayout {
	return c.layoutRectsFor(c.bounds)
}

// layoutRectsFor stacks the header, the transcript and the input bar
// down the assigned rectangle. The header and the input bar each ask
// for the rows they need and the transcript fills what is left, so a
// window too short for all three gives up transcript rows first. The
// bar clamps its own bands to whatever rows it ends up with, keeping
// the row the operator types at.
func (c ChatView[C]) layoutRectsFor(bounds uv.Rectangle) chatViewLayout {
	width := bounds.Dx()
	if width <= 0 {
		return chatViewLayout{}
	}

	headerRows := 0
	if headerView := c.renderHeader(width); headerView != "" {
		headerRows = lipgloss.Height(headerView)
	}

	// The two fixed bands take their rows before the split, in this
	// window's own order, because the solver ranks constraint kinds and
	// not two constraints of one kind. Left to it, the header could take
	// a short window's only row, leaving no row for the input bar to
	// draw its prompt on.
	inputRows := min(c.input.Height(), bounds.Dy())
	headerRows = min(headerRows, max(bounds.Dy()-inputRows, 0))

	var header, message, input uv.Rectangle
	uvlayout.Vertical(
		uvlayout.Len(headerRows),
		uvlayout.Fill(1),
		uvlayout.Len(inputRows),
	).Split(bounds).Assign(&header, &message, &input)

	popover := c.popoverRect(message, input)

	return chatViewLayout{
		PopoverRect: popover,
		InputRect:   input,
		MessageRect: message,
	}
}

func (c ChatView[C]) updateMessages(msg tea.Msg) (ChatView[C], tea.Cmd) {
	updated, cmd := c.messages.Update(msg)
	c.messages = updated.(MessageList[C])

	return c, cmd
}

func (c ChatView[C]) syncChildBounds() (ChatView[C], tea.Cmd) {
	layout := c.layoutRects()

	updatedInput, inputCmd := c.input.Update(ui.BoundsMsg{Rect: layout.InputRect})
	c.input = updatedInput.(InputBar)

	updated, cmd := c.messages.Update(ui.BoundsMsg{Rect: layout.MessageRect})
	c.messages = updated.(MessageList[C])

	c, layerCmd := c.syncPopoverLayer(layout)

	return c, tea.Batch(inputCmd, cmd, layerCmd)
}

// popoverRect is where the completions float: the rows immediately
// above the input, taken from the transcript's area without shrinking
// it. A popover taller than the transcript is cut at the top, which is
// the end furthest from the input it belongs to.
func (c ChatView[C]) popoverRect(message, input uv.Rectangle) uv.Rectangle {
	rows := min(c.popover().height(), message.Dy())
	if rows <= 0 {
		return uv.Rectangle{}
	}

	return uv.Rect(message.Min.X, input.Min.Y-rows, message.Dx(), rows)
}

// popover returns the popover on the window's stack, or a fresh empty
// one until the window has laid out and pushed the layer.
func (c ChatView[C]) popover() Popover {
	layer, ok := c.layers.Get(popoverLayerID)
	if !ok {
		return NewPopover()
	}

	popover, _ := layer.Content().(Popover)

	return popover
}

// syncPopoverLayer moves the popover's layer to where the layout says
// it goes, and creates it on the first call.
//
// A layer with nothing to show keeps its place with an empty
// rectangle. Removing it would discard the popover on it, and the
// next call would push a fresh one without the completer the window
// was given or the closed state a dismissal left.
func (c ChatView[C]) syncPopoverLayer(layout chatViewLayout) (ChatView[C], tea.Cmd) {
	if _, ok := c.layers.Get(popoverLayerID); !ok {
		layers, cmd := c.layers.Push(
			ui.NewKeyLayer(popoverLayerID, NewPopover(), layout.PopoverRect).WithOpaque(),
		)
		c.layers = layers

		return c, cmd
	}

	layers, cmd := c.layers.Move(popoverLayerID, layout.PopoverRect)
	c.layers = layers

	return c, cmd
}

// refreshPopover recomputes the completions for what is now typed.
// Recompute here and not through a returned command: a message
// travelling out to the runtime and back would leave the completions a
// frame behind the line they describe.
func (c ChatView[C]) refreshPopover() (ChatView[C], tea.Cmd) {
	return c.updatePopover(PopoverRefreshMsg{
		Raw:    c.input.Value(),
		Cursor: c.input.Cursor(),
	})
}

// updatePopover routes a message to the popover on the stack, then
// lays out again: the suggestions it holds afterwards decide how many
// rows its rectangle needs.
func (c ChatView[C]) updatePopover(msg tea.Msg) (ChatView[C], tea.Cmd) {
	c, ensureCmd := c.syncPopoverLayer(c.layoutRects())

	layers, cmd := c.layers.UpdateID(popoverLayerID, msg)
	c.layers = layers

	c, syncCmd := c.syncChildBounds()

	return c, tea.Batch(ensureCmd, cmd, syncCmd)
}

// updateInput forwards a message to the input bar and recomputes the
// completions when that message changed the text or the cursor
// position. The cursor counts as well as the text: what a completion
// offers depends on where in the line it is being asked about.
func (c ChatView[C]) updateInput(msg tea.Msg) (ChatView[C], tea.Cmd) {
	before, at := c.input.Value(), c.input.Cursor()

	updated, cmd := c.input.Update(msg)
	c.input = updated.(InputBar)

	if c.input.Value() == before && c.input.Cursor() == at {
		return c, cmd
	}

	c, refreshCmd := c.refreshPopover()

	return c, tea.Batch(cmd, refreshCmd)
}

// headerText names the window in view: the channel name plus its
// topic when one is set, or the counterpart's current nick for a DM.
//
// `channel` starts as "" at construction, so the view's first frame
// does not have to wait for the `SetChannelMsg` round trip that
// supplies the real window; there is nothing to name during that
// span, so headerText answers "".
func (c ChatView[C]) headerText() string {
	if c.kind == domain.KindDM {
		name := c.displayName
		if name == "" {
			name = string(c.channel)
		}
		if name == "" {
			return ""
		}

		return "@" + name
	}

	if c.channel == "" {
		return ""
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
