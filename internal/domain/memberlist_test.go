package domain_test

import (
	"encoding/json"
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func newModel(id domain.InstanceID, nick domain.Nick) *domain.Instance {
	return domain.NewModelInstance(id, nick, "test/model", "", nil)
}

func TestMemberList_Add_and_query_helpers(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")
	ghost := newModel("inst-ghost", "ghost")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	require.True(t, ml.HasInstance(alice))
	require.True(t, ml.HasInstance(bob))
	require.False(t, ml.HasInstance(ghost))

	require.True(t, ml.HasNick("alice"))
	require.True(t, ml.HasNick("bob"))
	require.False(t, ml.HasNick("charlie"))

	got, ok := ml.GetByInstance(alice)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: alice, Nick: "alice", Modes: domain.MemberModes{}}, got)

	got, ok = ml.GetByNick("bob")
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: bob, Nick: "bob", Modes: domain.MemberModes{}}, got)

	_, ok = ml.GetByInstance(ghost)
	require.False(t, ok)

	_, ok = ml.GetByNick("charlie")
	require.False(t, ok)
}

func TestMemberList_sort_order_by_mode_then_nick(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")
	zara := newModel("inst-zara", "zara")

	ml := domain.NewMemberList()
	ml.Add(zara)
	ml.SetModes(zara, domain.MemberModes{Voice: true})
	ml.Add(alice)
	ml.SetModes(alice, domain.MemberModes{Operator: true})
	ml.Add(bob)

	expected := []domain.Member{
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{Operator: true}},
		{Instance: zara, Nick: "zara", Modes: domain.MemberModes{Voice: true}},
		{Instance: bob, Nick: "bob", Modes: domain.MemberModes{}},
	}

	require.Equal(t, expected, slices.Collect(ml.All()))
}

func TestMemberList_SetMode_by_instance(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	ml.SetModes(bob, domain.MemberModes{Operator: true})

	expected := []domain.Member{
		{Instance: bob, Nick: "bob", Modes: domain.MemberModes{Operator: true}},
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{}},
	}

	require.Equal(t, expected, slices.Collect(ml.All()))
}

func TestMemberList_SetMode_unknown_instance_is_noop(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	ghost := newModel("inst-ghost", "ghost")

	ml := domain.NewMemberList()
	ml.Add(alice)

	ml.SetModes(ghost, domain.MemberModes{Operator: true})

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{}},
	}, slices.Collect(ml.All()))
}

func TestMemberList_SetModeByNick_forwards_to_handle(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	ml.SetModesByNick("bob", domain.MemberModes{Operator: true})

	got, ok := ml.GetByInstance(bob)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: bob, Nick: "bob", Modes: domain.MemberModes{Operator: true}}, got)

	// Unknown nick is a no-op.
	ml.SetModesByNick("ghost", domain.MemberModes{Operator: true})

	require.Equal(t, []domain.Member{
		{Instance: bob, Nick: "bob", Modes: domain.MemberModes{Operator: true}},
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{}},
	}, slices.Collect(ml.All()))
}

func TestMemberList_RenameTo_preserves_identity_and_mode(t *testing.T) {
	alice := newModel("inst-alice", "alice")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.SetModes(alice, domain.MemberModes{Operator: true})

	ml.RenameTo(alice, "alice2")

	got, ok := ml.GetByInstance(alice)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: alice, Nick: "alice2", Modes: domain.MemberModes{Operator: true}}, got)

	require.False(t, ml.HasNick("alice"))
	require.True(t, ml.HasNick("alice2"))

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice2", Modes: domain.MemberModes{Operator: true}},
	}, slices.Collect(ml.All()))
}

func TestMemberList_RenameTo_unknown_instance_is_noop(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	ghost := newModel("inst-ghost", "ghost")

	ml := domain.NewMemberList()
	ml.Add(alice)

	ml.RenameTo(ghost, "ghost2")

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{}},
	}, slices.Collect(ml.All()))
}

func TestMemberList_Remove_by_instance(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	ml.Remove(domain.Member{Instance: alice})

	require.False(t, ml.HasInstance(alice))
	require.True(t, ml.HasInstance(bob))
	require.Equal(t, []domain.Member{
		{Instance: bob, Nick: "bob", Modes: domain.MemberModes{}},
	}, slices.Collect(ml.All()))
}

func TestMemberList_Add_existing_instance_updates_snapshot_nick(t *testing.T) {
	alice := newModel("inst-alice", "alice")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.SetModes(alice, domain.MemberModes{Operator: true})

	// Renaming the handle and re-adding picks up the new nick while
	// preserving the existing mode.
	alice.SetNick("alice_renamed")
	ml.Add(alice)

	got, ok := ml.GetByInstance(alice)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: alice, Nick: "alice_renamed", Modes: domain.MemberModes{Operator: true}}, got)
}

func TestMemberList_user_instance_is_a_regular_member(t *testing.T) {
	user := domain.NewUserInstance("testuser")

	ml := domain.NewMemberList()
	ml.Add(user)
	ml.SetModes(user, domain.MemberModes{Operator: true})

	require.True(t, ml.HasInstance(user))
	require.Equal(t, []domain.Member{
		{Instance: user, Nick: "testuser", Modes: domain.MemberModes{Operator: true}},
	}, slices.Collect(ml.All()))

	// The user's handle supports rename in place, just like any
	// other member.
	ml.RenameTo(user, "renamed")

	got, ok := ml.GetByInstance(user)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: user, Nick: "renamed", Modes: domain.MemberModes{Operator: true}}, got)
}

func TestMemberList_JSON_round_trip_requires_resolver(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.SetModes(alice, domain.MemberModes{Operator: true})
	ml.Add(bob)
	ml.SetModes(bob, domain.MemberModes{Voice: true})

	data, err := json.Marshal(ml)
	require.NoError(t, err)

	var ml2 domain.MemberList
	err = json.Unmarshal(data, &ml2)
	require.NoError(t, err)

	// Before ResolveInstances is called, the unmarshal produces stub
	// Instance handles — the (nick, mode) pairs survive the round-trip
	// even though the handles themselves are not yet canonical.
	type nickModes struct {
		Nick  domain.Nick
		Modes domain.MemberModes
	}

	pairs := func(members iter.Seq[domain.Member]) []nickModes {
		var out []nickModes
		for m := range members {
			out = append(out, nickModes{Nick: m.Nick, Modes: m.Modes})
		}

		return out
	}

	require.Equal(t, pairs(ml.All()), pairs(ml2.All()))

	// Rewriting the stubs via a resolver that returns the original
	// handles reproduces the input exactly.
	canonical := map[domain.InstanceID]*domain.Instance{
		alice.ID(): alice,
		bob.ID():   bob,
	}
	ml2.ResolveInstances(func(id domain.InstanceID) *domain.Instance {
		return canonical[id]
	})

	require.Equal(t, slices.Collect(ml.All()), slices.Collect(ml2.All()))
}

// TestMemberList_ApplyMode_leaves_other_privileges_alone pins that
// each `MODE` flag changes only its own privilege, and that the
// display order follows the rank those privileges produce.
func TestMemberList_ApplyMode_leaves_other_privileges_alone(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	ml.ApplyMode(alice, domain.ModeOperator, true)
	ml.ApplyMode(alice, domain.ModeChannelVoice, true)
	ml.ApplyMode(bob, domain.ModeChannelVoice, true)

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{Operator: true, Voice: true}},
		{Instance: bob, Nick: "bob", Modes: domain.MemberModes{Voice: true}},
	}, slices.Collect(ml.All()))

	// Taking alice's voice leaves her `@`, so the display order does
	// not change.
	ml.ApplyMode(alice, domain.ModeChannelVoice, false)

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{Operator: true}},
		{Instance: bob, Nick: "bob", Modes: domain.MemberModes{Voice: true}},
	}, slices.Collect(ml.All()))

	// Taking the `@` leaves alice with no privileges at all, so bob's
	// voice now outranks her.
	ml.ApplyMode(alice, domain.ModeOperator, false)

	require.Equal(t, []domain.Member{
		{Instance: bob, Nick: "bob", Modes: domain.MemberModes{Voice: true}},
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{}},
	}, slices.Collect(ml.All()))
}

// TestMemberList_UnmarshalJSON_privilege_precedence covers which
// field decides a member's privileges. `modes` decides whenever the
// record carries it, including when it carries it empty; `mode`, the
// single display rank a record persisted before the two privileges
// became independent stores, decides only when `modes` is absent.
//
// The empty-and-present case is the one worth pinning: a record that
// says the member holds nothing, sitting beside a stale rank, must
// read as nothing rather than as an operator.
func TestMemberList_UnmarshalJSON_privilege_precedence(t *testing.T) {
	tests := []struct {
		name string
		data string
		want domain.MemberModes
	}{
		{name: "rank 0 is a plain member", data: `[{"instance_id":"inst-alice","nick":"alice","mode":0}]`, want: domain.MemberModes{}},
		{name: "rank 1 is voice", data: `[{"instance_id":"inst-alice","nick":"alice","mode":1}]`, want: domain.MemberModes{Voice: true}},
		{name: "rank 2 is operator", data: `[{"instance_id":"inst-alice","nick":"alice","mode":2}]`, want: domain.MemberModes{Operator: true}},
		{name: "neither field is a plain member", data: `[{"instance_id":"inst-alice","nick":"alice"}]`, want: domain.MemberModes{}},
		{name: "modes wins over a zero rank", data: `[{"instance_id":"inst-alice","nick":"alice","mode":0,"modes":"ov"}]`, want: domain.MemberModes{Operator: true, Voice: true}},
		{name: "modes wins over an operator rank", data: `[{"instance_id":"inst-alice","nick":"alice","mode":2,"modes":"v"}]`, want: domain.MemberModes{Voice: true}},
		{name: "an empty modes wins over an operator rank", data: `[{"instance_id":"inst-alice","nick":"alice","mode":2,"modes":""}]`, want: domain.MemberModes{}},
		{name: "an empty modes alone is a plain member", data: `[{"instance_id":"inst-alice","nick":"alice","modes":""}]`, want: domain.MemberModes{}},
		{name: "a letter this build does not know is dropped", data: `[{"instance_id":"inst-alice","nick":"alice","modes":"ovz"}]`, want: domain.MemberModes{Operator: true, Voice: true}},
		{name: "only unknown letters leaves a plain member", data: `[{"instance_id":"inst-alice","nick":"alice","modes":"z"}]`, want: domain.MemberModes{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ml domain.MemberList
			require.NoError(t, json.Unmarshal([]byte(tc.data), &ml))

			alice := newModel("inst-alice", "alice")
			ml.ResolveInstances(func(domain.InstanceID) *domain.Instance { return alice })

			require.Equal(t, []domain.Member{
				{Instance: alice, Nick: "alice", Modes: tc.want},
			}, slices.Collect(ml.All()))
		})
	}
}

func TestMemberList_ResolveInstances_drops_nil_resolved(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	data, err := json.Marshal(ml)
	require.NoError(t, err)

	var ml2 domain.MemberList
	require.NoError(t, json.Unmarshal(data, &ml2))

	// Resolver only knows about alice; bob's stub resolves to nil
	// and must be dropped.
	ml2.ResolveInstances(func(id domain.InstanceID) *domain.Instance {
		if id == alice.ID() {
			return alice
		}

		return nil
	})

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice", Modes: domain.MemberModes{}},
	}, slices.Collect(ml2.All()))
}

func TestMemberList_zero_value_is_safe(t *testing.T) {
	alice := newModel("inst-alice", "alice")

	var ml domain.MemberList

	require.Equal(t, 0, ml.Len())
	require.False(t, ml.HasInstance(alice))
	require.False(t, ml.HasNick("alice"))
	require.Empty(t, slices.Collect(ml.All()))

	_, ok := ml.GetByInstance(alice)
	require.False(t, ok)

	_, ok = ml.GetByNick("alice")
	require.False(t, ok)
}
