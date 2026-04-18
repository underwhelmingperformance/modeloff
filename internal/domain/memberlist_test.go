package domain_test

import (
	"encoding/json"
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
	require.Equal(t, domain.Member{Instance: alice, Nick: "alice", Mode: domain.ModeNone}, got)

	got, ok = ml.GetByNick("bob")
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: bob, Nick: "bob", Mode: domain.ModeNone}, got)

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
	ml.SetMode(zara, domain.ModeVoice)
	ml.Add(alice)
	ml.SetMode(alice, domain.ModeOp)
	ml.Add(bob)

	expected := []domain.Member{
		{Instance: alice, Nick: "alice", Mode: domain.ModeOp},
		{Instance: zara, Nick: "zara", Mode: domain.ModeVoice},
		{Instance: bob, Nick: "bob", Mode: domain.ModeNone},
	}

	require.Equal(t, expected, ml.Slice())
}

func TestMemberList_SetMode_by_instance(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	ml.SetMode(bob, domain.ModeOp)

	expected := []domain.Member{
		{Instance: bob, Nick: "bob", Mode: domain.ModeOp},
		{Instance: alice, Nick: "alice", Mode: domain.ModeNone},
	}

	require.Equal(t, expected, ml.Slice())
}

func TestMemberList_SetMode_unknown_instance_is_noop(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	ghost := newModel("inst-ghost", "ghost")

	ml := domain.NewMemberList()
	ml.Add(alice)

	ml.SetMode(ghost, domain.ModeOp)

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_SetModeByNick_forwards_to_handle(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.Add(bob)

	ml.SetModeByNick("bob", domain.ModeOp)

	got, ok := ml.GetByInstance(bob)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: bob, Nick: "bob", Mode: domain.ModeOp}, got)

	// Unknown nick is a no-op.
	ml.SetModeByNick("ghost", domain.ModeOp)

	require.Equal(t, []domain.Member{
		{Instance: bob, Nick: "bob", Mode: domain.ModeOp},
		{Instance: alice, Nick: "alice", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_RenameTo_preserves_identity_and_mode(t *testing.T) {
	alice := newModel("inst-alice", "alice")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.SetMode(alice, domain.ModeOp)

	ml.RenameTo(alice, "alice2")

	got, ok := ml.GetByInstance(alice)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: alice, Nick: "alice2", Mode: domain.ModeOp}, got)

	require.False(t, ml.HasNick("alice"))
	require.True(t, ml.HasNick("alice2"))

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice2", Mode: domain.ModeOp},
	}, ml.Slice())
}

func TestMemberList_RenameTo_unknown_instance_is_noop(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	ghost := newModel("inst-ghost", "ghost")

	ml := domain.NewMemberList()
	ml.Add(alice)

	ml.RenameTo(ghost, "ghost2")

	require.Equal(t, []domain.Member{
		{Instance: alice, Nick: "alice", Mode: domain.ModeNone},
	}, ml.Slice())
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
		{Instance: bob, Nick: "bob", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_Add_existing_instance_updates_snapshot_nick(t *testing.T) {
	alice := newModel("inst-alice", "alice")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.SetMode(alice, domain.ModeOp)

	// Renaming the handle and re-adding picks up the new nick while
	// preserving the existing mode.
	alice.SetNick("alice_renamed")
	ml.Add(alice)

	got, ok := ml.GetByInstance(alice)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: alice, Nick: "alice_renamed", Mode: domain.ModeOp}, got)
}

func TestMemberList_user_instance_is_a_regular_member(t *testing.T) {
	user := domain.NewUserInstance("testuser")

	ml := domain.NewMemberList()
	ml.Add(user)
	ml.SetMode(user, domain.ModeOp)

	require.True(t, ml.HasInstance(user))
	require.Equal(t, []domain.Member{
		{Instance: user, Nick: "testuser", Mode: domain.ModeOp},
	}, ml.Slice())

	// The user's handle supports rename in place, just like any
	// other member.
	ml.RenameTo(user, "renamed")

	got, ok := ml.GetByInstance(user)
	require.True(t, ok)
	require.Equal(t, domain.Member{Instance: user, Nick: "renamed", Mode: domain.ModeOp}, got)
}

func TestMemberList_JSON_round_trip_requires_resolver(t *testing.T) {
	alice := newModel("inst-alice", "alice")
	bob := newModel("inst-bob", "bob")

	ml := domain.NewMemberList()
	ml.Add(alice)
	ml.SetMode(alice, domain.ModeOp)
	ml.Add(bob)
	ml.SetMode(bob, domain.ModeVoice)

	data, err := json.Marshal(ml)
	require.NoError(t, err)

	var ml2 domain.MemberList
	err = json.Unmarshal(data, &ml2)
	require.NoError(t, err)

	// Before ResolveInstances is called, the unmarshal produces stub
	// Instance handles — the slice content is consistent but the
	// handles are not the canonical ones.
	require.Equal(t, ml.Len(), ml2.Len())

	// Rewriting the stubs via a resolver that returns the original
	// handles reproduces the input exactly.
	canonical := map[domain.InstanceID]*domain.Instance{
		alice.ID(): alice,
		bob.ID():   bob,
	}
	ml2.ResolveInstances(func(id domain.InstanceID) *domain.Instance {
		return canonical[id]
	})

	require.Equal(t, ml.Slice(), ml2.Slice())
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
		{Instance: alice, Nick: "alice", Mode: domain.ModeNone},
	}, ml2.Slice())
}

func TestMemberList_zero_value_is_safe(t *testing.T) {
	alice := newModel("inst-alice", "alice")

	var ml domain.MemberList

	require.Equal(t, 0, ml.Len())
	require.False(t, ml.HasInstance(alice))
	require.False(t, ml.HasNick("alice"))
	require.Empty(t, ml.Slice())

	_, ok := ml.GetByInstance(alice)
	require.False(t, ok)

	_, ok = ml.GetByNick("alice")
	require.False(t, ok)
}
