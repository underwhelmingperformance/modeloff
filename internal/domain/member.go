package domain

import (
	"encoding/json"
	"iter"

	"github.com/laney/modeloff/internal/set"
)

// Member pairs an Instance with its channel mode for display in the
// nick list.
//
// Nick is a snapshot of the instance's nick at the time of the last
// Add/RenameTo; it stays consistent within a single render frame
// even as the underlying instance renames. The snapshot is kept in
// sync by MemberList.RenameTo, which is called from
// handleNickChangeEvent. Any code path that mutates Instance.nick
// without emitting NickChangeEvent for every channel the instance
// is in will leave this field stale.
type Member struct {
	Instance *Instance
	Nick     Nick
	Mode     NickMode
}

func (m Member) String() string {
	return m.Mode.String() + string(m.Nick)
}

// Less defines the display order for members: higher modes
// first (op > voice > none), then alphabetically by nick within
// each mode. The final tiebreaker on `Instance.ID()` keeps
// distinct instances with the same mode-and-nick pair from
// colliding inside the sorted set.
func (m Member) Less(other Member) bool {
	if m.Mode != other.Mode {
		return m.Mode > other.Mode
	}

	if m.Nick != other.Nick {
		return m.Nick < other.Nick
	}

	return m.Instance.ID() < other.Instance.ID()
}

// MemberList is a sorted set of channel members ordered by mode
// then nick. The sort is maintained at insertion time so iteration
// and positional access are always free of re-sorting. A parallel
// map keyed by `*Instance` pointer backs O(1) identity lookups.
type MemberList struct {
	members    *set.Sorted[Member]
	byInstance map[*Instance]Member
}

// NewMemberList creates an empty member list.
func NewMemberList() MemberList {
	return MemberList{
		members:    set.NewSorted[Member](),
		byInstance: make(map[*Instance]Member),
	}
}

// ensureInit lazily initialises the underlying storage so that the
// zero value of MemberList remains usable.
func (ml *MemberList) ensureInit() {
	if ml.members == nil {
		ml.members = set.NewSorted[Member]()
	}

	if ml.byInstance == nil {
		ml.byInstance = make(map[*Instance]Member)
	}
}

// Add inserts an instance as a regular (unprivileged) member. The
// snapshot nick in the resulting Member is captured from the
// instance at call time; subsequent renames propagate through
// `RenameTo`. Adding an instance that is already a member updates
// its snapshot nick while preserving the current mode.
func (ml *MemberList) Add(inst *Instance) {
	ml.ensureInit()

	m := Member{Instance: inst, Nick: inst.Nick(), Mode: ModeNone}

	if cur, ok := ml.byInstance[inst]; ok {
		ml.members.Remove(cur)
		m.Mode = cur.Mode
	}

	ml.members.Insert(m)
	ml.byInstance[inst] = m
}

// Remove deletes the given member. Identity is taken from
// `m.Instance`; the Mode and Nick on the argument are ignored so
// that callers holding a stale mode can still remove a member
// cleanly.
func (ml *MemberList) Remove(m Member) {
	if ml.members == nil {
		return
	}

	cur, ok := ml.byInstance[m.Instance]
	if !ok {
		return
	}

	ml.members.Remove(cur)
	delete(ml.byInstance, m.Instance)
}

// RemoveInstance is a convenience for callers that hold the handle
// but not a full Member. It is equivalent to `Remove(Member{Instance:
// inst})`.
func (ml *MemberList) RemoveInstance(inst *Instance) {
	ml.Remove(Member{Instance: inst})
}

// SetMode changes a member's privilege level. This removes and
// re-inserts the member since mode is part of the sort key. Setting
// the mode of an unknown instance is a no-op.
func (ml *MemberList) SetMode(inst *Instance, mode NickMode) {
	if ml.members == nil {
		return
	}

	cur, ok := ml.byInstance[inst]
	if !ok {
		return
	}

	ml.members.Remove(cur)

	updated := Member{Instance: inst, Nick: cur.Nick, Mode: mode}
	ml.members.Insert(updated)
	ml.byInstance[inst] = updated
}

// SetModeByNick is a display-layer convenience that forwards to
// SetMode after resolving the nick to its instance handle. Wire
// code that only has a nick in hand (e.g. ChanServ-style commands)
// uses this. It is a no-op if the nick is unknown.
func (ml *MemberList) SetModeByNick(nick Nick, mode NickMode) {
	if ml.members == nil {
		return
	}

	m, ok := ml.GetByNick(nick)
	if !ok {
		return
	}

	ml.SetMode(m.Instance, mode)
}

// RenameTo updates the snapshot nick for the given instance handle,
// preserving the existing mode. The underlying sorted set is
// re-keyed in place (remove + insert) because Nick participates in
// the sort order. It is a no-op if the instance is not currently a
// member.
//
// RenameTo only updates this MemberList's snapshot — the caller is
// responsible for also calling `inst.SetNick(newNick)` and for
// re-calling `RenameTo` on every other channel the instance is in.
// The session's nick-change path handles this fan-out.
func (ml *MemberList) RenameTo(inst *Instance, newNick Nick) {
	if ml.members == nil {
		return
	}

	cur, ok := ml.byInstance[inst]
	if !ok {
		return
	}

	ml.members.Remove(cur)

	updated := Member{Instance: inst, Nick: newNick, Mode: cur.Mode}
	ml.members.Insert(updated)
	ml.byInstance[inst] = updated
}

// GetByInstance returns the member for the given instance handle.
func (ml MemberList) GetByInstance(inst *Instance) (Member, bool) {
	if ml.byInstance == nil {
		return Member{}, false
	}

	m, ok := ml.byInstance[inst]

	return m, ok
}

// HasInstance reports whether the given instance handle is a
// member.
func (ml MemberList) HasInstance(inst *Instance) bool {
	_, ok := ml.GetByInstance(inst)

	return ok
}

// GetByNick finds a member by display nick. It is intended for
// display-layer lookups (tab completion, resolving a typed command
// argument); identity-bearing code should prefer GetByInstance.
func (ml MemberList) GetByNick(nick Nick) (Member, bool) {
	if ml.members == nil {
		return Member{}, false
	}

	for m := range ml.members.All() {
		if m.Nick == nick {
			return m, true
		}
	}

	return Member{}, false
}

// HasNick reports whether any member currently displays the given
// nick.
func (ml MemberList) HasNick(nick Nick) bool {
	_, ok := ml.GetByNick(nick)

	return ok
}

// Len returns the total number of members.
func (ml MemberList) Len() int {
	return ml.members.Len()
}

// GetAt returns the member at the given display position.
func (ml MemberList) GetAt(index int) (Member, bool) {
	return ml.members.GetAt(index)
}

// SortedSet returns the underlying sorted set. This exposes the
// btree directly for use by the generic sidebar.
func (ml MemberList) SortedSet() *set.Sorted[Member] {
	return ml.members
}

// All yields every member in display order.
func (ml MemberList) All() iter.Seq[Member] {
	return ml.members.All()
}

// Nicks returns an iterator over just the nicks in display order.
func (ml MemberList) Nicks() iter.Seq[Nick] {
	return func(yield func(Nick) bool) {
		for m := range ml.All() {
			if !yield(m.Nick) {
				return
			}
		}
	}
}

// memberJSON is the wire format for a single Member. It records the
// instance id on the wire so the channel can round-trip before the
// store has resolved ids back to canonical `*Instance` handles; the
// store layer provides an `InstanceResolver` when loading to rewrite
// the id back to a pointer.
type memberJSON struct {
	InstanceID InstanceID `json:"instance_id,omitempty"`
	Nick       Nick       `json:"nick"`
	Mode       NickMode   `json:"mode"`
}

// MarshalJSON encodes the member list as a JSON array of members
// keyed by InstanceID on the wire.
func (ml MemberList) MarshalJSON() ([]byte, error) {
	out := make([]memberJSON, 0, ml.Len())

	for m := range ml.All() {
		var id InstanceID
		if m.Instance != nil {
			id = m.Instance.ID()
		}

		out = append(out, memberJSON{InstanceID: id, Nick: m.Nick, Mode: m.Mode})
	}

	return json.Marshal(out)
}

// UnmarshalJSON decodes a JSON array of member records into the
// list. Each record is stored as a stub `*Instance` carrying only
// the serialised id; callers that need canonical handles (the
// session on channel load) rewrite the stubs via
// `MemberList.ResolveInstances`.
func (ml *MemberList) UnmarshalJSON(data []byte) error {
	var records []memberJSON
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}

	*ml = NewMemberList()

	if len(records) == 0 {
		return nil
	}

	for _, r := range records {
		stub := &Instance{instanceID: r.InstanceID}
		m := Member{Instance: stub, Nick: r.Nick, Mode: r.Mode}

		ml.members.Insert(m)
		ml.byInstance[stub] = m
	}

	return nil
}

// InstanceResolver turns a serialised InstanceID back into the
// canonical `*Instance` handle produced by the store. Returning nil
// for a not-found id indicates "drop this member" — currently used
// only by the store layer when a member row references an instance
// row that has been deleted.
type InstanceResolver func(InstanceID) *Instance

// ResolveInstances rewrites each member's stub `*Instance` (set by
// UnmarshalJSON to carry only the serialised id) to the canonical
// handle returned by resolve. A stub whose id resolves to nil is
// dropped from the list.
//
// This is intended for the store's channel-deserialisation path
// only: the store reads a channel's member-list records from disk,
// then calls ResolveInstances to rewrite the stubs to the canonical
// pointers it owns. Session and UI code never call this directly —
// by the time a Channel surfaces to session the MemberList already
// carries canonical handles.
func (ml *MemberList) ResolveInstances(resolve InstanceResolver) {
	if ml.members == nil {
		return
	}

	rebuilt := set.NewSorted[Member]()
	byInstance := make(map[*Instance]Member, ml.members.Len())

	for m := range ml.All() {
		id := m.Instance.ID()

		canonical := resolve(id)
		if canonical == nil {
			continue
		}

		updated := Member{Instance: canonical, Nick: m.Nick, Mode: m.Mode}
		rebuilt.Insert(updated)
		byInstance[canonical] = updated
	}

	ml.members = rebuilt
	ml.byInstance = byInstance
}
