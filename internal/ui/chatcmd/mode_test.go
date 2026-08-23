package chatcmd

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestModeChanges_UnmarshalText(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    ModeChanges
		wantErr any
	}{
		{
			name: "single operator grant",
			raw:  "+o alice",
			want: ModeChanges{{Change: Add, Mode: ModeOperator{Target: "alice"}}},
		},
		{
			name: "single operator revoke",
			raw:  "-o alice",
			want: ModeChanges{{Change: Remove, Mode: ModeOperator{Target: "alice"}}},
		},
		{
			name: "boolean changes",
			raw:  "+tn-i",
			want: ModeChanges{
				{Change: Add, Mode: ModeTopicLock{}},
				{Change: Add, Mode: ModeNoExternal{}},
				{Change: Remove, Mode: ModeInviteOnly{}},
			},
		},
		{
			name: "parametric changes",
			raw:  "+lkf 10 secret 30",
			want: ModeChanges{
				{Change: Add, Mode: ModeUserLimit{Limit: 10}},
				{Change: Add, Mode: ModeKey{Key: "secret"}},
				{Change: Add, Mode: ModeFloodLimit{Limit: 30}},
			},
		},
		{
			name: "mixed directions and parameters",
			raw:  "+ov-i+l alice bob 5",
			want: ModeChanges{
				{Change: Add, Mode: ModeOperator{Target: "alice"}},
				{Change: Add, Mode: ModeVoice{Target: "bob"}},
				{Change: Remove, Mode: ModeInviteOnly{}},
				{Change: Add, Mode: ModeUserLimit{Limit: 5}},
			},
		},
		{
			name:    "empty",
			raw:     "",
			wantErr: &EmptyModeChangesError{},
		},
		{
			name:    "sign only",
			raw:     "+",
			wantErr: &EmptyModeChangesError{},
		},
		{
			name:    "unknown flag",
			raw:     "+x",
			wantErr: domain.UnknownModeFlagError{},
		},
		{
			name:    "missing member",
			raw:     "+o",
			wantErr: domain.MissingModeParamError{},
		},
		{
			name:    "missing value",
			raw:     "+k",
			wantErr: domain.MissingModeParamError{},
		},
		{
			name:    "invalid count",
			raw:     "+l zero",
			wantErr: &InvalidPositiveIntError{},
		},
		{
			name:    "surplus argument",
			raw:     "+o alice leftover",
			wantErr: &SurplusModeArgumentsError{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got ModeChanges
			err := got.UnmarshalText([]byte(test.raw))

			if test.wantErr != nil {
				require.Error(t, err)
				requireErrorAsType(t, err, test.wantErr)
				return
			}

			require.NoError(t, err)
			require.Equal(t, test.want, got)
		})
	}
}

func TestModeCommand_CLI_preserves_typed_mode_error(t *testing.T) {
	_, err := testParser.Parse("/mode +x")

	require.Equal(t, domain.UnknownModeFlagError{Flag: 'x'}, err)
}

func TestModeCommand_ToCommand(t *testing.T) {
	changes := ModeChanges{
		{Change: Remove, Mode: ModeInviteOnly{}},
		{Change: Add, Mode: ModeUserLimit{Limit: 10}},
	}

	got, err := (ModeCommand{Changes: changes}).ToCommand(Context{Active: domain.WindowKey("#chan")})

	require.NoError(t, err)
	require.Equal(t, protocol.ChannelMode{
		Channel: "#chan",
		Changes: []protocol.ChannelModeChange{
			{Flag: domain.ModeInviteOnly, Add: false},
			{Flag: domain.ModeUserLimit, Add: true, Param: "10"},
		},
	}, got)
}

func TestModeChanges_Validate(t *testing.T) {
	tests := []struct {
		name    string
		changes ModeChanges
		wantErr any
	}{
		{
			name:    "empty batch",
			changes: ModeChanges{},
			wantErr: &EmptyModeChangesError{},
		},
		{
			name:    "unknown operation",
			changes: ModeChanges{{Change: "toggle", Mode: ModeQuiet{}}},
			wantErr: &UnknownModeOperationError{},
		},
		{
			name:    "missing mode",
			changes: ModeChanges{{Change: Add}},
			wantErr: &MissingModeError{},
		},
		{
			name:    "member mode requires target",
			changes: ModeChanges{{Change: Add, Mode: ModeOperator{}}},
			wantErr: domain.MissingModeParamError{},
		},
		{
			name:    "remove rejects value",
			changes: ModeChanges{{Change: Remove, Mode: ModeKey{Key: "unused"}}},
			wantErr: &UnexpectedModeParameterError{},
		},
		{
			name: "batch bound",
			changes: func() ModeChanges {
				changes := make(ModeChanges, maxModeChanges+1)
				for index := range changes {
					changes[index] = ModeChange{Change: Add, Mode: ModeQuiet{}}
				}
				return changes
			}(),
			wantErr: &command.TooManyValuesError{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.changes.Validate()

			require.Error(t, err)
			requireErrorAsType(t, err, test.wantErr)
		})
	}
}

func TestModeChange_UnmarshalJSON_rejects_unknown_mode(t *testing.T) {
	var change ModeChange
	err := json.Unmarshal([]byte(`{"change":"add","mode":"ban"}`), &change)

	var unknown *UnknownModeError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, &UnknownModeError{Mode: "ban"}, unknown)
}

func requireErrorAsType(t *testing.T, err error, errorType any) {
	t.Helper()

	target := reflect.New(reflect.TypeOf(errorType)).Interface()
	require.ErrorAs(t, err, target)
}
