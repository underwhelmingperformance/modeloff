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
	// ChannelAddedMsg tells the sidebar a new channel has been
	// opened.
	ChannelAddedMsg struct {
		Channel domain.Channel
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
	ChannelUnreadMsg struct {
		Channel domain.ChannelName
		Count   int
	}
)
