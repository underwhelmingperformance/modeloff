package domain

import "time"

// Window is the addressable-by-name behaviour shared by every kind
// of chat target the user can switch into: the per-session status
// window, real IRC channels, and DM streams. The set of
// implementations is fixed (`*StatusWindow`, `*ChannelWindow`,
// `*DMWindow`) and lives in this package; per-kind state lives on
// the matching concrete type so invariants like "modes don't apply
// to status" and "DMs don't have a member list" are compile-time
// facts rather than runtime kind-checks.
//
// Code that addresses a window only by name (rendering, scrollback,
// `last_read`, focus) operates against this interface. Code that
// updates per-kind state (member list, topic, modes) downcasts to
// the matching concrete type at the receiving handler — by which
// point session-side propagation has already guaranteed the cast
// will succeed.
type Window interface {
	// Name returns the addressable name of the window. For
	// channels this is the `#`-prefixed name; for DMs it is the
	// counterpart's nick; for the status window it is the
	// reserved [StatusChannelName].
	Name() ChannelName

	// Created returns the time the window was first opened.
	Created() time.Time

	// Kind reports which leaf concrete type this window is. The
	// sidebar uses this for its pin-status-first sort and the
	// system-notice render branch keys off it; the rest of the
	// codebase prefers a typed downcast.
	Kind() ChannelKind

	// DisplayName returns the window name formatted for display.
	// Channels keep their `#` prefix; DMs prefix the nick with
	// `@`; the status window renders as its reserved name.
	DisplayName() string
}

// ChannelDirectoryEntry is one row in the result of `/list`. It
// holds just the fields a `ListReply` needs to be assembled
// from; clients construct the persistable event around their
// own `At` timestamp.
type ChannelDirectoryEntry struct {
	Channel ChannelName
	Members int
	Topic   string
}

// WindowKey builds a placeholder `Window` suitable only for
// keyed lookup in a sorted set whose comparator reads
// `Name()` and `Kind()`. The returned value carries no per-kind
// state — it must not be used as a real window. The DM case
// returns a `*DMWindow` whose `Counterpart` is nil; that is safe
// for lookup because the comparator does not touch it, and a
// caller that mistakes a key for a stored value will fail fast
// when it tries to read the counterpart.
func WindowKey(name ChannelName) Window {
	switch InferChannelKind(name) {
	case KindStatus:
		return &StatusWindow{}
	case KindChannel:
		return &ChannelWindow{name: name}
	case KindDM:
		return &DMWindow{name: name}
	}

	return nil
}

// WindowFromChannel projects a `Channel` to the matching concrete
// `Window` based on its `Kind`. The DM case requires a resolver
// that turns the stored `InstanceID` (a DM row's `Name` is the
// counterpart's stable InstanceID, not their nick) into the
// canonical counterpart `*Instance`; pass nil when DMs are not
// expected (e.g. status / channel-only paths). Returns an error
// for unknown kinds or when the resolver returns nil for a DM.
//
// This is the single bridge from the legacy `Channel` storage
// shape to the new `Window` types and lives here so the store
// and the session see the same projection rules.
func WindowFromChannel(ch Channel, resolveCounterpart func(InstanceID) *Instance) (Window, error) {
	switch ch.Kind {
	case KindStatus:
		return &StatusWindow{created: ch.Created}, nil

	case KindChannel:
		return &ChannelWindow{
			name:       ch.Name,
			created:    ch.Created,
			Topic:      ch.Topic,
			TopicSetBy: ch.TopicSetBy,
			TopicSetAt: ch.TopicSetAt,
			Members:    ch.Members,
		}, nil

	case KindDM:
		var counterpart *Instance
		if resolveCounterpart != nil {
			counterpart = resolveCounterpart(InstanceID(ch.Name))
		}

		if counterpart == nil {
			return nil, MissingDMCounterpartError{InstanceID: InstanceID(ch.Name)}
		}

		return &DMWindow{
			name:        ch.Name,
			created:     ch.Created,
			Counterpart: counterpart,
		}, nil

	default:
		return nil, UnknownChannelKindError{Kind: ch.Kind}
	}
}

// ChannelFromWindow projects a `Window` back to the legacy
// `Channel` storage shape. Per-kind state that doesn't apply to
// the projected shape is left zero-valued: a `StatusWindow`
// produces a `Channel` with no `Members` and no `Topic`; a
// `DMWindow` produces a `Channel` whose name is the counterpart
// nick and whose member list is empty.
//
// The bridge is one-way symmetric with `WindowFromChannel`: a
// round-trip Window→Channel→Window preserves the original concrete
// type and addressing. Tests pin the round-trip.
func ChannelFromWindow(w Window) Channel {
	ch := Channel{
		Name:    w.Name(),
		Kind:    w.Kind(),
		Created: w.Created(),
	}

	if cw, ok := w.(*ChannelWindow); ok {
		ch.Topic = cw.Topic
		ch.TopicSetBy = cw.TopicSetBy
		ch.TopicSetAt = cw.TopicSetAt
		ch.Members = cw.Members
	}

	return ch
}
