package domain

import (
	"encoding/json"
	"iter"

	"github.com/laney/modeloff/internal/set"
)

// Member records the identity and privileges displayed for one
// channel member.
//
// Nick is a snapshot of the instance's nick at the time of the last
// Add/RenameTo; it stays consistent within a single render frame
// even as the underlying instance renames. The snapshot is kept in
// sync by MemberList.RenameTo, which is called from
// handleNickChangeEvent. Any code path that mutates Instance.nick
// without emitting NickChangeEvent for every channel the instance
// is in will leave this field stale.
type Member struct {
	InstanceID InstanceID
	Nick       Nick
	Modes      MemberModes
}

func (m Member) String() string {
	return m.Modes.Rank().String() + string(m.Nick)
}

// Less defines the display order for members: higher ranks first
// (op > voice > none), then alphabetically by nick within each
// rank. The final tiebreaker on `Instance.ID()` keeps distinct
// instances with the same rank-and-nick pair from colliding inside
// the sorted set.
func (m Member) Less(other Member) bool {
	rank, otherRank := m.Modes.Rank(), other.Modes.Rank()
	if rank != otherRank {
		return rank > otherRank
	}

	if m.Nick != other.Nick {
		return m.Nick < other.Nick
	}

	return m.InstanceID < other.InstanceID
}

// MemberList is a sorted set of channel members ordered by mode
// then nick. The sort is maintained at insertion time so iteration
// and positional access are always free of re-sorting. A parallel
// map keyed by stable instance ID backs O(1) identity lookups.
type MemberList struct {
	members *set.Sorted[Member]
	byID    map[InstanceID]Member
}

// NewMemberList creates an empty member list.
func NewMemberList() MemberList {
	return MemberList{
		members: set.NewSorted[Member](),
		byID:    make(map[InstanceID]Member),
	}
}

// ensureInit lazily initialises the underlying storage so that the
// zero value of MemberList remains usable.
func (ml *MemberList) ensureInit() {
	if ml.members == nil {
		ml.members = set.NewSorted[Member]()
	}

	if ml.byID == nil {
		ml.byID = make(map[InstanceID]Member)
	}
}

// Add inserts an instance as a regular (unprivileged) member. The
// snapshot nick in the resulting Member is captured from the
// instance at call time; subsequent renames propagate through
// `RenameTo`. Adding an instance that is already a member updates
// its snapshot nick while preserving the privileges it holds.
func (ml *MemberList) Add(inst *Instance) {
	if inst == nil {
		return
	}

	ml.AddIdentity(inst.ID(), inst.Nick())
}

// AddIdentity inserts an unprivileged member from protocol identity
// values.
func (ml *MemberList) AddIdentity(id InstanceID, nick Nick) {
	ml.ensureInit()

	m := Member{InstanceID: id, Nick: nick}

	if cur, ok := ml.byID[id]; ok {
		ml.members.Remove(cur)
		m.Modes = cur.Modes
	}

	ml.members.Insert(m)
	ml.byID[id] = m
}

// Remove deletes the given member. Identity is taken from
// `m.InstanceID`; the Modes and Nick on the argument are ignored so
// that callers holding a stale privilege set can still remove a
// member cleanly.
func (ml *MemberList) Remove(m Member) {
	if ml.members == nil {
		return
	}

	id := m.InstanceID
	cur, ok := ml.byID[id]
	if !ok {
		return
	}

	ml.members.Remove(cur)
	delete(ml.byID, id)
}

// RemoveInstance is a convenience for callers that hold the handle
// but not a full Member. It removes the member whose stable ID matches
// the instance.
func (ml *MemberList) RemoveInstance(inst *Instance) {
	if inst != nil {
		ml.RemoveID(inst.ID())
	}
}

// SetModes replaces the privileges a member holds. This removes and
// re-inserts the member, because the rank derived from those
// privileges is part of the sort key. Setting the privileges of an
// unknown instance does nothing.
func (ml *MemberList) SetModes(inst *Instance, modes MemberModes) {
	if ml.members == nil {
		return
	}

	id := inst.ID()
	cur, ok := ml.byID[id]
	if !ok {
		return
	}

	ml.members.Remove(cur)

	updated := Member{InstanceID: cur.InstanceID, Nick: cur.Nick, Modes: modes}
	ml.members.Insert(updated)
	ml.byID[id] = updated
}

// ApplyMode grants or revokes a single privilege, leaving the
// member's other privileges as they are (RFC 2811 §4.1). It does
// nothing if the flag is not a per-member mode, or if the instance
// is not a member.
func (ml *MemberList) ApplyMode(inst *Instance, flag Mode, add bool) {
	cur, ok := ml.GetByInstance(inst)
	if !ok {
		return
	}

	ml.SetModes(inst, cur.Modes.With(flag, add))
}

// SetModesByNick resolves `nick` to its instance handle and forwards
// to SetModes. It does nothing if the nick is not a member.
func (ml *MemberList) SetModesByNick(nick Nick, modes MemberModes) {
	m, ok := ml.GetByNick(nick)
	if !ok {
		return
	}

	ml.SetModesID(m.InstanceID, modes)
}

// RenameTo updates the snapshot nick for the given instance handle,
// preserving the privileges it holds. The underlying sorted set is
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

	id := inst.ID()
	cur, ok := ml.byID[id]
	if !ok {
		return
	}

	ml.members.Remove(cur)

	updated := Member{InstanceID: cur.InstanceID, Nick: newNick, Modes: cur.Modes}
	ml.members.Insert(updated)
	ml.byID[id] = updated
}

// GetByInstance returns the member with the given instance's stable
// identity.
func (ml MemberList) GetByInstance(inst *Instance) (Member, bool) {
	if inst == nil {
		return Member{}, false
	}

	return ml.GetByID(inst.ID())
}

// GetByID returns the member with the given stable identity.
func (ml MemberList) GetByID(id InstanceID) (Member, bool) {
	if ml.byID == nil {
		return Member{}, false
	}

	m, ok := ml.byID[id]

	return m, ok
}

// HasInstance reports whether the given instance handle is a
// member.
func (ml MemberList) HasInstance(inst *Instance) bool {
	_, ok := ml.GetByInstance(inst)

	return ok
}

// HasID reports whether a member has the given stable identity.
func (ml MemberList) HasID(id InstanceID) bool {
	_, ok := ml.GetByID(id)
	return ok
}

// RemoveID removes the member with the given stable identity.
func (ml *MemberList) RemoveID(id InstanceID) {
	member, ok := ml.GetByID(id)
	if ok {
		ml.Remove(member)
	}
}

// RemoveNick removes the member whose current nick matches under the
// server casemapping.
func (ml *MemberList) RemoveNick(nick Nick) {
	member, ok := ml.GetByNick(nick)
	if ok {
		ml.RemoveID(member.InstanceID)
	}
}

// SetModesID replaces the privileges for the member with the given
// stable identity.
func (ml *MemberList) SetModesID(id InstanceID, modes MemberModes) {
	if ml.members == nil {
		return
	}

	cur, ok := ml.byID[id]
	if !ok {
		return
	}

	ml.members.Remove(cur)
	cur.Modes = modes
	ml.members.Insert(cur)
	ml.byID[id] = cur
}

// ApplyModeID grants or revokes a privilege for the member with the
// given stable identity.
func (ml *MemberList) ApplyModeID(id InstanceID, flag Mode, add bool) {
	member, ok := ml.GetByID(id)
	if ok {
		ml.SetModesID(id, member.Modes.With(flag, add))
	}
}

// ApplyModeNick grants or revokes a privilege for the member whose
// current nick matches under the server casemapping.
func (ml *MemberList) ApplyModeNick(nick Nick, flag Mode, add bool) {
	member, ok := ml.GetByNick(nick)
	if ok {
		ml.SetModesID(member.InstanceID, member.Modes.With(flag, add))
	}
}

// RenameID updates the nick snapshot for the member with the given
// stable identity.
func (ml *MemberList) RenameID(id InstanceID, nick Nick) {
	member, ok := ml.GetByID(id)
	if ok {
		ml.members.Remove(member)
		member.Nick = nick
		ml.members.Insert(member)
		ml.byID[id] = member
	}
}

// RenameNick updates a member selected by its current nick.
func (ml *MemberList) RenameNick(oldNick, newNick Nick) {
	member, ok := ml.GetByNick(oldNick)
	if ok {
		ml.RenameID(member.InstanceID, newNick)
	}
}

// GetByNick finds a member by display nick, under the server's
// casemapping. It is intended for display-layer lookups (tab
// completion, resolving a typed command argument); identity-bearing
// code should prefer GetByInstance.
func (ml MemberList) GetByNick(nick Nick) (Member, bool) {
	if ml.members == nil {
		return Member{}, false
	}

	for m := range ml.members.All() {
		if EqualNick(m.Nick, nick) {
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

// Clone returns an independent member list holding the same
// members. Each [Member] is copied verbatim, including its nick
// snapshot, so the copy renders exactly as the original would.
func (ml MemberList) Clone() MemberList {
	dst := NewMemberList()

	if ml.members == nil {
		return dst
	}

	for m := range ml.All() {
		dst.members.Insert(m)
		dst.byID[m.InstanceID] = m
	}

	return dst
}

// AnonymousMembers returns the member list an anonymous (`+a`)
// channel answers a NAMES query with: one entry under
// [AnonymousNick], holding no privileges and no instance handle.
//
// RFC 2811 §4.2.1 masks every name on such a channel, channel
// operators included, so the reply carries neither who is there nor
// how many. A per-member masked entry would still carry the count,
// and one masked entry per rank would still say who holds `@`.
func AnonymousMembers() MemberList {
	ml := NewMemberList()
	ml.members.Insert(Member{Nick: AnonymousNick})

	return ml
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
//
// A member's privileges are written under `modes` as their mode
// letters. Records written before the two privileges became
// independent instead store a single display rank under `mode`, and
// `Rank` reads that field.
//
// `modes` decides whenever the record carries it, `mode` only when it
// does not. Both fields are pointers so that a record carrying
// `"modes":""` is distinguishable from one carrying no `modes` at
// all: the first states that the member holds no privileges and must
// read that way even beside a stale rank, while only the second
// consults `mode`. Marshalling always writes `modes` and never
// writes `mode`.
type memberJSON struct {
	InstanceID InstanceID   `json:"instance_id,omitempty"`
	Nick       Nick         `json:"nick"`
	Modes      *MemberModes `json:"modes"`
	Rank       *NickMode    `json:"mode,omitempty"`
}

// memberModesFrom returns the privileges a decoded member record
// holds, applying the `modes`-over-`mode` precedence [memberJSON]
// documents. A record carrying neither field holds no privileges.
func memberModesFrom(r memberJSON) MemberModes {
	if r.Modes != nil {
		return *r.Modes
	}

	if r.Rank != nil {
		return modesForRank(*r.Rank)
	}

	return MemberModes{}
}

// MarshalJSON encodes the member list as a JSON array of members
// keyed by InstanceID on the wire.
func (ml MemberList) MarshalJSON() ([]byte, error) {
	out := make([]memberJSON, 0, ml.Len())

	for m := range ml.All() {
		modes := m.Modes
		out = append(out, memberJSON{InstanceID: m.InstanceID, Nick: m.Nick, Modes: &modes})
	}

	return json.Marshal(out)
}

// UnmarshalJSON decodes a JSON array of member records into the
// list.
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
		m := Member{InstanceID: r.InstanceID, Nick: r.Nick, Modes: memberModesFrom(r)}

		ml.members.Insert(m)
		ml.byID[r.InstanceID] = m
	}

	return nil
}
