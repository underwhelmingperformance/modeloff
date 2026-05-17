package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestChannelModes_IRCString(t *testing.T) {
	tests := []struct {
		name  string
		modes domain.ChannelModes
		want  string
	}{
		{name: "zero", modes: domain.ChannelModes{}, want: "+"},
		{name: "single boolean", modes: domain.ChannelModes{TopicLock: true}, want: "+t"},
		{name: "all booleans in canonical order", modes: domain.ChannelModes{
			Anonymous: true, InviteOnly: true, Moderated: true, NoExternal: true,
			Private: true, Quiet: true, Secret: true, TopicLock: true,
		}, want: "+aimnpqst"},
		{name: "user-limit only", modes: domain.ChannelModes{UserLimit: 10}, want: "+l 10"},
		{name: "key only", modes: domain.ChannelModes{Key: "secret"}, want: "+k secret"},
		{name: "limit then key", modes: domain.ChannelModes{UserLimit: 5, Key: "pw"}, want: "+lk 5 pw"},
		{name: "mixed booleans and parametric", modes: domain.ChannelModes{
			TopicLock: true, NoExternal: true, UserLimit: 20, Key: "s3cret",
		}, want: "+ntlk 20 s3cret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.modes.IRCString())
		})
	}
}

func TestChannelModes_JSONRoundtrip(t *testing.T) {
	in := domain.ChannelModes{
		Moderated:  true,
		NoExternal: true,
		UserLimit:  20,
		Key:        "s3cret",
	}

	data, err := json.Marshal(in)
	require.NoError(t, err)

	var out domain.ChannelModes
	require.NoError(t, json.Unmarshal(data, &out))
	require.Equal(t, in, out)
}

func TestChannelModes_JSONZeroValueOmitsFields(t *testing.T) {
	data, err := json.Marshal(domain.ChannelModes{})
	require.NoError(t, err)

	require.Equal(t, "{}", string(data))
}

func TestChannelModes_LegacyRowHydratesAsZero(t *testing.T) {
	// A row that pre-dates the modes field has no `modes` key at
	// all; standard JSON semantics give us the zero value.
	var modes domain.ChannelModes
	require.NoError(t, json.Unmarshal([]byte(`{}`), &modes))
	require.Equal(t, domain.ChannelModes{}, modes)
}

func TestInvitedNicks_AddRemoveContains(t *testing.T) {
	var s domain.InvitedNicks

	require.False(t, s.Contains("alpha"))

	s.Add("alpha")
	require.True(t, s.Contains("alpha"))

	s.Add("alpha")
	require.True(t, s.Contains("alpha"))
	require.Equal(t, 1, len(s))

	s.Add("beta")
	require.True(t, s.Contains("beta"))
	require.Equal(t, 2, len(s))

	require.True(t, s.Remove("alpha"))
	require.False(t, s.Contains("alpha"))
	require.True(t, s.Contains("beta"))

	require.False(t, s.Remove("alpha"))
}

func TestInvitedNicks_JSONRoundtripSorted(t *testing.T) {
	var s domain.InvitedNicks
	s.Add("charlie")
	s.Add("alpha")
	s.Add("bravo")

	data, err := json.Marshal(s)
	require.NoError(t, err)
	require.Equal(t, `["alpha","bravo","charlie"]`, string(data))

	var out domain.InvitedNicks
	require.NoError(t, json.Unmarshal(data, &out))
	require.True(t, out.Contains("alpha"))
	require.True(t, out.Contains("bravo"))
	require.True(t, out.Contains("charlie"))
	require.Equal(t, 3, len(out))
}

func TestInvitedNicks_EmptyMarshalsAsNull(t *testing.T) {
	var s domain.InvitedNicks
	data, err := json.Marshal(s)
	require.NoError(t, err)
	require.Equal(t, "null", string(data))
}

func TestInvitedNicks_NullUnmarshalsAsEmpty(t *testing.T) {
	var s domain.InvitedNicks
	s.Add("ghost")

	require.NoError(t, json.Unmarshal([]byte("null"), &s))
	require.Equal(t, 0, len(s))
	require.False(t, s.Contains("ghost"))
}
