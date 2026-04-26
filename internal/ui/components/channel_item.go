package components

import (
	"github.com/laney/modeloff/internal/domain"
)

// ChannelSelectedMsg is emitted when the user selects a channel in
// the sidebar, either by pressing ctrl-o or clicking on it.
type ChannelSelectedMsg struct {
	Channel domain.ChannelName
}

// Incremental sidebar messages. The chat screen sends these to
// update the channel list.
type (
	// ChannelAddedMsg tells the sidebar a new window has been
	// opened. The carried `Window` is the typed entry the
	// sidebar's sorted list stores; the sidebar reads
	// `DisplayName()` (which lives off the typed handle, so DM
	// renames redraw automatically) for rendering.
	ChannelAddedMsg struct {
		Channel domain.Window
		Unread  int
	}

	// ChannelRemovedMsg tells the sidebar a channel has been closed.
	ChannelRemovedMsg struct {
		Channel domain.ChannelName
	}

	// ChannelActiveMsg tells the sidebar which channel is now
	// active.
	ChannelActiveMsg struct {
		Channel domain.ChannelName
	}

	// ChannelUnreadMsg updates the unread count for a channel.
	// Items receive this through the tree and match on Channel.
	// Mention is true when at least one unread message contains a
	// highlight word (e.g. the user's nick).
	ChannelUnreadMsg struct {
		Channel domain.ChannelName
		Count   int
		Mention bool
	}
)
