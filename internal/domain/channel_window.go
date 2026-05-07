package domain

import "time"

// ChannelWindow is the full IRC-style channel: a multi-party room
// with a member list, an optional topic, and per-member privilege
// modes. It accepts every channel-lifecycle event (join, part,
// names-reply, mode change, topic change, model invite/kick) on
// top of the addressable-window behaviour shared via [Window].
type ChannelWindow struct {
	name       ChannelName
	created    time.Time
	Topic      string
	TopicSetBy Nick
	TopicSetAt time.Time
	Members    MemberList
}

// NewChannelWindow constructs a `#`-prefixed channel window with
// an empty member list. The name is normalised via
// [NormaliseChannelName] so callers may pass either bare or
// prefixed names.
func NewChannelWindow(name ChannelName, created time.Time) *ChannelWindow {
	return &ChannelWindow{
		name:    NormaliseChannelName(name),
		created: created,
		Members: NewMemberList(),
	}
}

// Name returns the `#`-prefixed channel name.
func (w *ChannelWindow) Name() ChannelName { return w.name }

// Created returns the time the channel was opened.
func (w *ChannelWindow) Created() time.Time { return w.created }

// Kind reports [KindChannel].
func (*ChannelWindow) Kind() ChannelKind { return KindChannel }

// DisplayName returns the `#`-prefixed channel name as displayed
// in the sidebar and chat header.
func (w *ChannelWindow) DisplayName() string { return string(w.name) }

// Less implements [Window].
func (w *ChannelWindow) Less(other Window) bool { return windowLess(w, other) }
