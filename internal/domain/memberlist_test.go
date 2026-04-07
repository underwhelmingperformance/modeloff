package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestMemberList_JSON_round_trip(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add("alice")
	ml.SetMode("alice", domain.ModeOp)
	ml.Add("bob")
	ml.SetMode("bob", domain.ModeVoice)

	data, err := json.Marshal(ml)
	require.NoError(t, err)
	t.Logf("JSON: %s", data)

	var ml2 domain.MemberList
	err = json.Unmarshal(data, &ml2)
	require.NoError(t, err)

	require.Equal(t, ml.Slice(), ml2.Slice())
}

func TestMemberList_sort_order(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add("zara")
	ml.SetMode("zara", domain.ModeVoice)
	ml.Add("alice")
	ml.SetMode("alice", domain.ModeOp)
	ml.Add("bob")

	expected := []domain.Member{
		{Nick: "alice", Mode: domain.ModeOp},
		{Nick: "zara", Mode: domain.ModeVoice},
		{Nick: "bob", Mode: domain.ModeNone},
	}

	require.Equal(t, expected, ml.Slice())
}

func TestMemberList_SetMode(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add("alice")
	ml.Add("bob")

	ml.SetMode("bob", domain.ModeOp)

	expected := []domain.Member{
		{Nick: "bob", Mode: domain.ModeOp},
		{Nick: "alice", Mode: domain.ModeNone},
	}

	require.Equal(t, expected, ml.Slice())
}

func TestMemberList_Remove(t *testing.T) {
	ml := domain.NewMemberList()
	ml.Add("alice")
	ml.Add("bob")

	ml.Remove(domain.Member{Nick: "alice", Mode: domain.ModeNone})

	expected := []domain.Member{
		{Nick: "bob", Mode: domain.ModeNone},
	}

	require.Equal(t, expected, ml.Slice())
}

func TestMemberList_zero_value_is_safe(t *testing.T) {
	var ml domain.MemberList

	require.Equal(t, 0, ml.Len())
	require.False(t, ml.Has("alice"))
	require.Empty(t, ml.Slice())
}
