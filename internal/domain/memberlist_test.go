package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

const (
	aliceID   domain.InstanceID = "inst-alice"
	bobID     domain.InstanceID = "inst-bob"
	zaraID    domain.InstanceID = "inst-zara"
	humanID   domain.InstanceID = ""
	missingID domain.InstanceID = "inst-missing"
)

func TestMemberList_Add_and_query_helpers(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")
	ml.Add(bobID, "bob")

	require.True(t, ml.HasID(aliceID))
	require.True(t, ml.HasID(bobID))
	require.False(t, ml.HasID(missingID))

	require.True(t, ml.HasNick("alice"))
	require.True(t, ml.HasNick("bob"))
	require.False(t, ml.HasNick("charlie"))

	got, ok := ml.GetByID(aliceID)
	require.True(t, ok)
	require.Equal(t, domain.Member{InstanceID: aliceID, Nick: "alice", Mode: domain.ModeNone}, got)

	got, ok = ml.GetByNick("bob")
	require.True(t, ok)
	require.Equal(t, domain.Member{InstanceID: bobID, Nick: "bob", Mode: domain.ModeNone}, got)

	_, ok = ml.GetByID(missingID)
	require.False(t, ok)

	_, ok = ml.GetByNick("charlie")
	require.False(t, ok)
}

func TestMemberList_sort_order_by_mode_then_nick(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(zaraID, "zara")
	ml.SetMode(zaraID, domain.ModeVoice)
	ml.Add(aliceID, "alice")
	ml.SetMode(aliceID, domain.ModeOp)
	ml.Add(bobID, "bob")

	expected := []domain.Member{
		{InstanceID: aliceID, Nick: "alice", Mode: domain.ModeOp},
		{InstanceID: zaraID, Nick: "zara", Mode: domain.ModeVoice},
		{InstanceID: bobID, Nick: "bob", Mode: domain.ModeNone},
	}

	require.Equal(t, expected, ml.Slice())
}

func TestMemberList_SetMode_by_id(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")
	ml.Add(bobID, "bob")

	ml.SetMode(bobID, domain.ModeOp)

	expected := []domain.Member{
		{InstanceID: bobID, Nick: "bob", Mode: domain.ModeOp},
		{InstanceID: aliceID, Nick: "alice", Mode: domain.ModeNone},
	}

	require.Equal(t, expected, ml.Slice())
}

func TestMemberList_SetMode_unknown_id_is_noop(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")

	ml.SetMode(missingID, domain.ModeOp)

	require.Equal(t, []domain.Member{
		{InstanceID: aliceID, Nick: "alice", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_SetModeByNick_forwards_to_id(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")
	ml.Add(bobID, "bob")

	ml.SetModeByNick("bob", domain.ModeOp)

	got, ok := ml.GetByID(bobID)
	require.True(t, ok)
	require.Equal(t, domain.Member{InstanceID: bobID, Nick: "bob", Mode: domain.ModeOp}, got)

	// Unknown nick is a no-op.
	ml.SetModeByNick("ghost", domain.ModeOp)

	require.Equal(t, []domain.Member{
		{InstanceID: bobID, Nick: "bob", Mode: domain.ModeOp},
		{InstanceID: aliceID, Nick: "alice", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_RenameTo_preserves_identity_and_mode(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")
	ml.SetMode(aliceID, domain.ModeOp)

	ml.RenameTo(aliceID, "alice2")

	got, ok := ml.GetByID(aliceID)
	require.True(t, ok)
	require.Equal(t, domain.Member{InstanceID: aliceID, Nick: "alice2", Mode: domain.ModeOp}, got)

	require.False(t, ml.HasNick("alice"))
	require.True(t, ml.HasNick("alice2"))

	require.Equal(t, []domain.Member{
		{InstanceID: aliceID, Nick: "alice2", Mode: domain.ModeOp},
	}, ml.Slice())
}

func TestMemberList_RenameTo_unknown_id_is_noop(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")

	ml.RenameTo(missingID, "ghost")

	require.Equal(t, []domain.Member{
		{InstanceID: aliceID, Nick: "alice", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_Remove_by_instance_id(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")
	ml.Add(bobID, "bob")

	ml.Remove(domain.Member{InstanceID: aliceID})

	require.False(t, ml.HasID(aliceID))
	require.True(t, ml.HasID(bobID))
	require.Equal(t, []domain.Member{
		{InstanceID: bobID, Nick: "bob", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_Add_existing_id_preserves_mode_and_updates_nick(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")
	ml.SetMode(aliceID, domain.ModeOp)

	ml.Add(aliceID, "alice_renamed")

	got, ok := ml.GetByID(aliceID)
	require.True(t, ok)
	require.Equal(t, domain.Member{InstanceID: aliceID, Nick: "alice_renamed", Mode: domain.ModeOp}, got)
}

func TestMemberList_empty_instance_id_is_structurally_unique(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(humanID, "testuser")
	ml.Add(humanID, "other")

	// Re-adding with the same id (including the empty human id) is
	// idempotent at the storage level: the nick is updated, mode is
	// preserved, and the list stays at a single entry.
	require.Equal(t, []domain.Member{
		{InstanceID: humanID, Nick: "other", Mode: domain.ModeNone},
	}, ml.Slice())
}

func TestMemberList_empty_instance_id_can_be_renamed_in_place(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(humanID, "testuser")

	ml.RenameTo(humanID, "renamed")

	got, ok := ml.GetByID(humanID)
	require.True(t, ok)
	require.Equal(t, domain.Member{InstanceID: humanID, Nick: "renamed", Mode: domain.ModeNone}, got)
}

func TestMemberList_JSON_round_trip_includes_instance_id(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add(aliceID, "alice")
	ml.SetMode(aliceID, domain.ModeOp)
	ml.Add(bobID, "bob")
	ml.SetMode(bobID, domain.ModeVoice)

	data, err := json.Marshal(ml)
	require.NoError(t, err)

	var ml2 domain.MemberList
	err = json.Unmarshal(data, &ml2)
	require.NoError(t, err)

	require.Equal(t, ml.Slice(), ml2.Slice())

	// The rebuilt list must also support by-ID lookup.
	got, ok := ml2.GetByID(aliceID)
	require.True(t, ok)
	require.Equal(t, domain.Member{InstanceID: aliceID, Nick: "alice", Mode: domain.ModeOp}, got)
}

func TestMemberList_zero_value_is_safe(t *testing.T) {
	var ml domain.MemberList

	require.Equal(t, 0, ml.Len())
	require.False(t, ml.HasID(aliceID))
	require.False(t, ml.HasNick("alice"))
	require.Empty(t, ml.Slice())

	_, ok := ml.GetByID(aliceID)
	require.False(t, ok)

	_, ok = ml.GetByNick("alice")
	require.False(t, ok)
}
