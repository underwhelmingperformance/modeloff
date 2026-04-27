package domain

import "time"

// DMWindow is a one-to-one stream between the user and a single
// model instance. The window is addressed by the counterpart's
// `InstanceID`; the counterpart pointer is held canonically so
// the live nick reaches the renderer through [DisplayName].
type DMWindow struct {
	name        ChannelName
	created     time.Time
	Counterpart *Instance
}

// NewDMWindow constructs a DM window targeting the given
// counterpart.
func NewDMWindow(counterpart *Instance, created time.Time) *DMWindow {
	return &DMWindow{
		name:        ChannelName(counterpart.ID()),
		created:     created,
		Counterpart: counterpart,
	}
}

// Name returns the counterpart's `InstanceID` as a
// `ChannelName`. Instance IDs are 16-char hex, so they don't
// collide with `#`-prefixed channels or the `&`-prefixed status
// name.
func (w *DMWindow) Name() ChannelName { return w.name }

// Created returns the time the DM was first opened.
func (w *DMWindow) Created() time.Time { return w.created }

// Kind reports [KindDM].
func (*DMWindow) Kind() ChannelKind { return KindDM }

// DisplayName returns the counterpart's current nick. Lookup-
// key DMWindows produced by [WindowKey] carry no counterpart
// and fall back to the stored name; key windows never reach
// the sidebar.
func (w *DMWindow) DisplayName() string {
	if w.Counterpart != nil {
		return string(w.Counterpart.Nick())
	}

	return string(w.name)
}
