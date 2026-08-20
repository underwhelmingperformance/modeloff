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
		{name: "flood limit only", modes: domain.ChannelModes{FloodLimit: 30}, want: "+f 30"},
		{name: "mixed booleans and parametric", modes: domain.ChannelModes{
			TopicLock: true, NoExternal: true, UserLimit: 20, Key: "s3cret", FloodLimit: 30,
		}, want: "+ntlkf 20 s3cret 30"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.modes.IRCString())
		})
	}
}

func TestParseChannelModes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want domain.ChannelModes
	}{
		{name: "empty set", in: "+", want: domain.ChannelModes{}},
		{name: "single boolean", in: "+t", want: domain.ChannelModes{TopicLock: true}},
		{name: "multiple booleans", in: "+nt", want: domain.ChannelModes{NoExternal: true, TopicLock: true}},
		{name: "all booleans", in: "+aimnpqst", want: domain.ChannelModes{
			Anonymous: true, InviteOnly: true, Moderated: true, NoExternal: true,
			Private: true, Quiet: true, Secret: true, TopicLock: true,
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := domain.ParseChannelModes(tt.in)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestModeArgumentFor pins what each mode letter takes alongside it.
// The `MODE` validator, the slash-command argument parser and the
// broadcast renderer all decide from this one answer, so a wrong
// entry here is a wrong answer in three places at once.
func TestModeArgumentFor(t *testing.T) {
	tests := []struct {
		flag domain.Mode
		want domain.ModeArgument
	}{
		{flag: domain.ModeOperator, want: domain.ModeArgNick},
		{flag: domain.ModeChannelVoice, want: domain.ModeArgNick},
		{flag: domain.ModeAnonymous, want: domain.ModeArgNone},
		{flag: domain.ModeInviteOnly, want: domain.ModeArgNone},
		{flag: domain.ModeModerated, want: domain.ModeArgNone},
		{flag: domain.ModeNoExternal, want: domain.ModeArgNone},
		{flag: domain.ModePrivate, want: domain.ModeArgNone},
		{flag: domain.ModeQuiet, want: domain.ModeArgNone},
		{flag: domain.ModeSecret, want: domain.ModeArgNone},
		{flag: domain.ModeTopicLock, want: domain.ModeArgNone},
		{flag: domain.ModeUserLimit, want: domain.ModeArgCount},
		{flag: domain.ModeFloodLimit, want: domain.ModeArgCount},
		{flag: domain.ModeKey, want: domain.ModeArgText},
		{flag: domain.Mode('z'), want: domain.ModeArgUnknown},
	}

	for _, tt := range tests {
		t.Run(string(rune(tt.flag)), func(t *testing.T) {
			require.Equal(t, tt.want, domain.ModeArgumentFor(tt.flag))
		})
	}
}

// TestChannelModes_ApplyChannelMode checks that each flag writes to
// its own field and leaves the rest of the set alone, and that the
// remove form clears it.
func TestChannelModes_ApplyChannelMode(t *testing.T) {
	// Every field set, so a change that touched a field it should not
	// shows up as a difference from this.
	full := domain.ChannelModes{
		Anonymous: true, InviteOnly: true, Moderated: true, NoExternal: true,
		Private: true, Quiet: true, Secret: true, TopicLock: true,
		UserLimit: 20, Key: "s3cret", FloodLimit: 30,
	}

	tests := []struct {
		name  string
		start domain.ChannelModes
		flag  domain.Mode
		add   bool
		param string
		want  domain.ChannelModes
	}{
		{
			name: "boolean add", start: domain.ChannelModes{}, flag: domain.ModeTopicLock, add: true,
			want: domain.ChannelModes{TopicLock: true},
		},
		{
			name: "boolean remove", start: full, flag: domain.ModeTopicLock,
			want: func() domain.ChannelModes { m := full; m.TopicLock = false; return m }(),
		},
		{
			name: "count add", start: domain.ChannelModes{}, flag: domain.ModeUserLimit, add: true, param: "12",
			want: domain.ChannelModes{UserLimit: 12},
		},
		{
			name: "count remove", start: full, flag: domain.ModeUserLimit,
			want: func() domain.ChannelModes { m := full; m.UserLimit = 0; return m }(),
		},
		{
			name: "flood limit add", start: domain.ChannelModes{}, flag: domain.ModeFloodLimit, add: true, param: "7",
			want: domain.ChannelModes{FloodLimit: 7},
		},
		{
			name: "flood limit remove", start: full, flag: domain.ModeFloodLimit,
			want: func() domain.ChannelModes { m := full; m.FloodLimit = 0; return m }(),
		},
		{
			name: "text add", start: domain.ChannelModes{}, flag: domain.ModeKey, add: true, param: "pw",
			want: domain.ChannelModes{Key: "pw"},
		},
		{
			name: "text remove", start: full, flag: domain.ModeKey,
			want: func() domain.ChannelModes { m := full; m.Key = ""; return m }(),
		},
		{
			name: "member mode writes nothing", start: full, flag: domain.ModeOperator, add: true, param: "alice",
			want: full,
		},
		{
			name: "unknown flag writes nothing", start: full, flag: domain.Mode('z'), add: true,
			want: full,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.start
			got.ApplyChannelMode(tt.flag, tt.add, tt.param)

			require.Equal(t, tt.want, got)
		})
	}
}

func TestParseChannelModes_roundtrips_through_IRCString(t *testing.T) {
	in := domain.ChannelModes{NoExternal: true, TopicLock: true, Secret: true}

	got, err := domain.ParseChannelModes(in.IRCString())
	require.NoError(t, err)
	require.Equal(t, in, got)
}

func TestParseChannelModes_errors(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{name: "empty string", in: "", wantErr: domain.MalformedChannelModeError{Input: ""}},
		{name: "missing leading plus", in: "nt", wantErr: domain.MalformedChannelModeError{Input: "nt"}},
		{name: "leading minus", in: "-nt", wantErr: domain.MalformedChannelModeError{Input: "-nt"}},
		{name: "member mode operator", in: "+o", wantErr: domain.UnknownModeFlagError{Flag: domain.ModeOperator}},
		{name: "member mode voice", in: "+v", wantErr: domain.UnknownModeFlagError{Flag: domain.ModeChannelVoice}},
		{name: "parametric user limit", in: "+l", wantErr: domain.UnknownModeFlagError{Flag: domain.ModeUserLimit}},
		{name: "parametric key", in: "+k", wantErr: domain.UnknownModeFlagError{Flag: domain.ModeKey}},
		{name: "parametric flood limit", in: "+f", wantErr: domain.UnknownModeFlagError{Flag: domain.ModeFloodLimit}},
		{name: "unrecognised letter", in: "+z", wantErr: domain.UnknownModeFlagError{Flag: domain.Mode('z')}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := domain.ParseChannelModes(tt.in)
			require.Equal(t, tt.wantErr, err)
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

func TestInvitations_AddRemoveContains(t *testing.T) {
	var s domain.Invitations

	require.False(t, s.Contains("inst-alpha"))

	s.Add("inst-alpha")
	require.True(t, s.Contains("inst-alpha"))

	s.Add("inst-alpha")
	require.True(t, s.Contains("inst-alpha"))
	require.Equal(t, 1, len(s))

	s.Add("inst-beta")
	require.True(t, s.Contains("inst-beta"))
	require.Equal(t, 2, len(s))

	require.True(t, s.Remove("inst-alpha"))
	require.False(t, s.Contains("inst-alpha"))
	require.True(t, s.Contains("inst-beta"))

	require.False(t, s.Remove("inst-alpha"))
}

func TestInvitations_JSONRoundtripSorted(t *testing.T) {
	var s domain.Invitations
	s.Add("inst-charlie")
	s.Add("inst-alpha")
	s.Add("inst-bravo")

	data, err := json.Marshal(s)
	require.NoError(t, err)
	require.Equal(t, `["inst-alpha","inst-bravo","inst-charlie"]`, string(data))

	var out domain.Invitations
	require.NoError(t, json.Unmarshal(data, &out))
	require.True(t, out.Contains("inst-alpha"))
	require.True(t, out.Contains("inst-bravo"))
	require.True(t, out.Contains("inst-charlie"))
	require.Equal(t, 3, len(out))
}

func TestInvitations_EmptyMarshalsAsNull(t *testing.T) {
	var s domain.Invitations
	data, err := json.Marshal(s)
	require.NoError(t, err)
	require.Equal(t, "null", string(data))
}

func TestInvitations_NullUnmarshalsAsEmpty(t *testing.T) {
	var s domain.Invitations
	s.Add("inst-ghost")

	require.NoError(t, json.Unmarshal([]byte("null"), &s))
	require.Equal(t, 0, len(s))
	require.False(t, s.Contains("inst-ghost"))
}
