package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestMemberModes_With covers RFC 2811 §4.1: channel-operator and
// voice are independent, so applying one flag never disturbs the
// other, and a flag that is not a per-member mode changes nothing.
func TestMemberModes_With(t *testing.T) {
	op := domain.MemberModes{Operator: true}
	voiced := domain.MemberModes{Voice: true}
	both := domain.MemberModes{Operator: true, Voice: true}

	tests := []struct {
		name  string
		start domain.MemberModes
		flag  domain.Mode
		add   bool
		want  domain.MemberModes
	}{
		{name: "voice grant keeps op", start: op, flag: domain.ModeChannelVoice, add: true, want: both},
		{name: "voice revoke keeps op", start: both, flag: domain.ModeChannelVoice, add: false, want: op},
		{name: "op grant keeps voice", start: voiced, flag: domain.ModeOperator, add: true, want: both},
		{name: "op revoke keeps voice", start: both, flag: domain.ModeOperator, add: false, want: voiced},
		{name: "op grant on plain member", start: domain.MemberModes{}, flag: domain.ModeOperator, add: true, want: op},
		{name: "voice revoke on plain member", start: domain.MemberModes{}, flag: domain.ModeChannelVoice, add: false, want: domain.MemberModes{}},
		{name: "attribute flag changes nothing", start: both, flag: domain.ModeModerated, add: true, want: both},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tc.start.With(tc.flag, tc.add))
		})
	}
}

// TestMemberModes_derived_views covers the three read-side views of a
// privilege set: whether a given flag is held, the display rank the
// nick-list prefix comes from, and the mode-letter rendering.
func TestMemberModes_derived_views(t *testing.T) {
	tests := []struct {
		name      string
		modes     domain.MemberModes
		wantOp    bool
		wantVoice bool
		wantRank  domain.NickMode
		wantIRC   string
	}{
		{
			name:     "no privileges",
			modes:    domain.MemberModes{},
			wantRank: domain.ModeNone,
			wantIRC:  "",
		},
		{
			name:      "voice only",
			modes:     domain.MemberModes{Voice: true},
			wantVoice: true,
			wantRank:  domain.ModeVoice,
			wantIRC:   "v",
		},
		{
			name:     "op only",
			modes:    domain.MemberModes{Operator: true},
			wantOp:   true,
			wantRank: domain.ModeOp,
			wantIRC:  "o",
		},
		{
			name:      "op and voice ranks as op",
			modes:     domain.MemberModes{Operator: true, Voice: true},
			wantOp:    true,
			wantVoice: true,
			wantRank:  domain.ModeOp,
			wantIRC:   "ov",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantOp, tc.modes.Has(domain.ModeOperator))
			require.Equal(t, tc.wantVoice, tc.modes.Has(domain.ModeChannelVoice))
			require.False(t, tc.modes.Has(domain.ModeModerated))
			require.Equal(t, tc.wantRank, tc.modes.Rank())
			require.Equal(t, tc.wantIRC, tc.modes.IRCString())
		})
	}
}

func TestMemberModes_JSON_round_trip(t *testing.T) {
	tests := []struct {
		name  string
		modes domain.MemberModes
		want  string
	}{
		{name: "none", modes: domain.MemberModes{}, want: `""`},
		{name: "op", modes: domain.MemberModes{Operator: true}, want: `"o"`},
		{name: "voice", modes: domain.MemberModes{Voice: true}, want: `"v"`},
		{name: "both", modes: domain.MemberModes{Operator: true, Voice: true}, want: `"ov"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.modes)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(encoded))

			var decoded domain.MemberModes
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			require.Equal(t, tc.modes, decoded)
		})
	}
}

// TestMemberModes_UnmarshalJSON_keeps_the_letters_it_knows covers a
// record written by a build that understands a per-member mode this
// one does not, which is what a downgrade or a future half-op letter
// produces. Refusing the record would fail the whole channel load,
// and one member's privileges must not decide whether anyone can
// join, so the decode keeps the letters it understands and drops the
// rest. Refusing belongs to the MODE-command parse path, where a
// caller is validating input.
func TestMemberModes_UnmarshalJSON_keeps_the_letters_it_knows(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
		want    domain.MemberModes
	}{
		{name: "known letters only", encoded: `"ov"`, want: domain.MemberModes{Operator: true, Voice: true}},
		{name: "unknown letter among known ones", encoded: `"ovz"`, want: domain.MemberModes{Operator: true, Voice: true}},
		{name: "unknown letter alone", encoded: `"z"`, want: domain.MemberModes{}},
		{name: "an attribute mode is not a member mode", encoded: `"m"`, want: domain.MemberModes{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var modes domain.MemberModes

			require.NoError(t, json.Unmarshal([]byte(tc.encoded), &modes))
			require.Equal(t, tc.want, modes)
		})
	}
}

// TestMemberModes_UnmarshalJSON_rejects_a_non_string pins that a
// malformed record is still an error. Tolerating an unknown letter is
// not the same as tolerating a value of the wrong shape.
func TestMemberModes_UnmarshalJSON_rejects_a_non_string(t *testing.T) {
	var modes domain.MemberModes

	require.Error(t, json.Unmarshal([]byte(`2`), &modes))
}
