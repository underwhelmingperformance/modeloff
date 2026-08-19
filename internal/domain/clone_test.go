package domain

import (
	"testing"
	"time"

	orderedmap "github.com/wk8/go-ordered-map/v2"

	"github.com/stretchr/testify/require"
)

var cloneTestTime = time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)

func TestChannelWindow_Clone_is_independent(t *testing.T) {
	alice := NewModelInstance("inst-alice", "alice", "test/model", "", nil)
	bob := NewModelInstance("inst-bob", "bob", "test/model", "", nil)

	original := NewChannelWindow("#dev", cloneTestTime)
	original.Topic = "shipping"
	original.TopicSetBy = "alice"
	original.TopicSetAt = cloneTestTime
	original.Modes = ChannelModes{TopicLock: true, UserLimit: 4}
	original.Members.Add(alice)
	original.Members.SetModes(alice, MemberModes{Operator: true})
	original.InvitedNicks.Add("carol")

	clone := original.Clone()

	require.Equal(t, original.Name(), clone.Name())
	require.Equal(t, original.Created(), clone.Created())
	require.Equal(t, original.Topic, clone.Topic)
	require.Equal(t, original.TopicSetBy, clone.TopicSetBy)
	require.Equal(t, original.TopicSetAt, clone.TopicSetAt)
	require.Equal(t, original.Modes, clone.Modes)
	require.Equal(t, []Member{{Instance: alice, Nick: "alice", Modes: MemberModes{Operator: true}}}, membersOf(clone))
	require.True(t, clone.InvitedNicks.Contains("carol"))

	clone.Topic = "rewritten"
	clone.Modes.TopicLock = false
	clone.Members.Add(bob)
	clone.Members.RemoveInstance(alice)
	clone.InvitedNicks.Add("dave")
	clone.InvitedNicks.Remove("carol")

	require.Equal(t, "shipping", original.Topic)
	require.Equal(t, ChannelModes{TopicLock: true, UserLimit: 4}, original.Modes)
	require.Equal(t, []Member{{Instance: alice, Nick: "alice", Modes: MemberModes{Operator: true}}}, membersOf(original))
	require.True(t, original.InvitedNicks.Contains("carol"))
	require.False(t, original.InvitedNicks.Contains("dave"))
}

// TestMemberList_Clone_preserves_nick_snapshots pins that cloning
// copies each member verbatim. The nick on a [Member] is a snapshot
// taken when the member was added or renamed; re-reading it from
// the live instance would make a clone render differently from the
// list it was copied from.
func TestMemberList_Clone_preserves_nick_snapshots(t *testing.T) {
	alice := NewModelInstance("inst-alice", "alice", "test/model", "", nil)

	original := NewMemberList()
	original.Add(alice)
	original.SetModes(alice, MemberModes{Voice: true})

	alice.SetNick("renamed-behind-the-list")

	require.Equal(t,
		[]Member{{Instance: alice, Nick: "alice", Modes: MemberModes{Voice: true}}},
		memberEntries(original.Clone()))
}

func TestMemberList_Clone_of_the_zero_value_is_usable(t *testing.T) {
	var zero MemberList

	clone := zero.Clone()
	clone.Add(NewModelInstance("inst-alice", "alice", "test/model", "", nil))

	require.Equal(t, 1, clone.Len())
	require.Equal(t, 0, zero.Len())
}

func TestInvitedNicks_Clone(t *testing.T) {
	tests := []struct {
		name string
		in   InvitedNicks
	}{
		{name: "nil set clones to nil", in: nil},
		{name: "empty set clones empty", in: InvitedNicks{}},
		{name: "populated set clones its entries", in: InvitedNicks{"alice": {}, "bob": {}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clone := tt.in.Clone()
			require.Equal(t, tt.in, clone)

			clone.Add("carol")
			require.False(t, tt.in.Contains("carol"))
		})
	}
}

func TestInstance_InChannel(t *testing.T) {
	channels := orderedmap.New[ChannelName, time.Time]()
	channels.Set("#dev", cloneTestTime)

	tests := []struct {
		name string
		inst *Instance
		ch   ChannelName
		want bool
	}{
		{name: "nil instance is in nothing", inst: nil, ch: "#dev", want: false},
		{
			name: "instance with no channels",
			inst: NewModelInstance("inst-alice", "alice", "test/model", "", nil),
			ch:   "#dev",
			want: false,
		},
		{
			name: "joined channel",
			inst: NewModelInstance("inst-bob", "bob", "test/model", "", channels),
			ch:   "#dev",
			want: true,
		},
		{
			name: "channel not joined",
			inst: NewModelInstance("inst-carol", "carol", "test/model", "", channels),
			ch:   "#ops",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.inst.InChannel(tt.ch))
		})
	}
}

// membersOf returns a window's members in display order.
func membersOf(w *ChannelWindow) []Member {
	return memberEntries(w.Members)
}

// memberEntries returns a member list's entries in display order.
func memberEntries(ml MemberList) []Member {
	var out []Member
	for m := range ml.All() {
		out = append(out, m)
	}

	return out
}
