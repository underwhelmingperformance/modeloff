package domain

import "time"

// DMWindow is a one-to-one stream between the user and a single
// model instance. The counterpart is held by canonical pointer so
// renames propagate without the DM needing its own snapshot. There
// is no member list, no topic, and no modes: a DM only ever
// receives messages plus the user-scoped lifecycle events (`Quit`,
// `NickChange`) routed via [Window]'s addressable name.
type DMWindow struct {
	name        ChannelName
	created     time.Time
	Counterpart *Instance
}

// NewDMWindow constructs a DM window targeting the given
// counterpart. The window's [Name] is the counterpart's nick at
// construction; later renames flow via the propagation rule and
// do not mutate the window's address (the store row is keyed on
// the bare nick at open time, which is the IRC-convention cue
// that DMs are addressed by nick rather than by `#`-channel).
func NewDMWindow(counterpart *Instance, created time.Time) *DMWindow {
	return &DMWindow{
		name:        ChannelName(counterpart.Nick()),
		created:     created,
		Counterpart: counterpart,
	}
}

// Name returns the bare counterpart nick used to address the DM.
func (w *DMWindow) Name() ChannelName { return w.name }

// Created returns the time the DM was first opened.
func (w *DMWindow) Created() time.Time { return w.created }

// Kind reports [KindDM].
func (*DMWindow) Kind() ChannelKind { return KindDM }

// DisplayName prefixes the counterpart's *current* nick with
// `@` to mark the window as a DM rather than a channel in the
// sidebar. The label is derived live from `Counterpart.Nick()`
// so a counterpart rename redraws the sidebar entry without a
// separate update path. The fallback to the stored `name` is
// only for placeholder windows produced by [WindowKey] (used as
// sorted-set lookup keys), which carry no counterpart.
func (w *DMWindow) DisplayName() string {
	if w.Counterpart != nil {
		return "@" + string(w.Counterpart.Nick())
	}

	return "@" + string(w.name)
}
