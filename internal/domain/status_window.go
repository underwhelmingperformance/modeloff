package domain

import "time"

// StatusWindow is the per-session "server window" — the leaf for
// server-narrated notices only. There are no members, no topic,
// no modes, and no inbound chat events; the session emits
// [SystemNotice] into it for connection-state lines, autojoin
// progress, and dispatch errors.
type StatusWindow struct {
	created time.Time
}

// NewStatusWindow constructs the singleton status window for a
// session. The reserved [StatusChannelName] is the only valid name
// so callers do not pass one.
func NewStatusWindow(created time.Time) *StatusWindow {
	return &StatusWindow{created: created}
}

// Name returns the reserved [StatusChannelName].
func (*StatusWindow) Name() ChannelName { return StatusChannelName }

// Created returns the time the window was opened.
func (w *StatusWindow) Created() time.Time { return w.created }

// Kind reports [KindStatus].
func (*StatusWindow) Kind() ChannelKind { return KindStatus }

// DisplayName renders the reserved name as-is so the sidebar keeps
// the IRC-convention cue that this is not a user-created room.
func (*StatusWindow) DisplayName() string { return string(StatusChannelName) }

// Less implements [Window].
func (w *StatusWindow) Less(other Window) bool { return windowLess(w, other) }
