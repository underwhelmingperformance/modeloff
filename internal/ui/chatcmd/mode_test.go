package chatcmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestModeCommand_ToCommand(t *testing.T) {
	tests := []struct {
		name    string
		flags   string
		args    []string
		want    []protocol.ChannelModeChange
		wantErr any
	}{
		{
			name:  "single +o",
			flags: "+o",
			args:  []string{"alice"},
			want:  []protocol.ChannelModeChange{{Flag: domain.ModeOperator, Add: true, Target: "alice"}},
		},
		{
			name:  "single -o",
			flags: "-o",
			args:  []string{"alice"},
			want:  []protocol.ChannelModeChange{{Flag: domain.ModeOperator, Add: false, Target: "alice"}},
		},
		{
			name:  "boolean toggle +t",
			flags: "+t",
			args:  nil,
			want:  []protocol.ChannelModeChange{{Flag: domain.ModeTopicLock, Add: true}},
		},
		{
			name:  "boolean toggle -i",
			flags: "-i",
			args:  nil,
			want:  []protocol.ChannelModeChange{{Flag: domain.ModeInviteOnly, Add: false}},
		},
		{
			name:  "parametric +l add",
			flags: "+l",
			args:  []string{"10"},
			want:  []protocol.ChannelModeChange{{Flag: domain.ModeUserLimit, Add: true, Param: "10"}},
		},
		{
			name:  "parametric -l remove takes no param",
			flags: "-l",
			args:  nil,
			want:  []protocol.ChannelModeChange{{Flag: domain.ModeUserLimit, Add: false}},
		},
		{
			name:  "parametric +k with key",
			flags: "+k",
			args:  []string{"secret"},
			want:  []protocol.ChannelModeChange{{Flag: domain.ModeKey, Add: true, Param: "secret"}},
		},
		{
			name:  "compound +ov on two nicks",
			flags: "+ov",
			args:  []string{"alice", "bob"},
			want: []protocol.ChannelModeChange{
				{Flag: domain.ModeOperator, Add: true, Target: "alice"},
				{Flag: domain.ModeChannelVoice, Add: true, Target: "bob"},
			},
		},
		{
			name:  "compound sign flip +o-v",
			flags: "+o-v",
			args:  []string{"alice", "bob"},
			want: []protocol.ChannelModeChange{
				{Flag: domain.ModeOperator, Add: true, Target: "alice"},
				{Flag: domain.ModeChannelVoice, Add: false, Target: "bob"},
			},
		},
		{
			name:  "compound booleans only",
			flags: "+tn",
			args:  nil,
			want: []protocol.ChannelModeChange{
				{Flag: domain.ModeTopicLock, Add: true},
				{Flag: domain.ModeNoExternal, Add: true},
			},
		},
		{
			name:  "compound mixed parametric and member",
			flags: "+ovk",
			args:  []string{"alice", "bob", "s3cret"},
			want: []protocol.ChannelModeChange{
				{Flag: domain.ModeOperator, Add: true, Target: "alice"},
				{Flag: domain.ModeChannelVoice, Add: true, Target: "bob"},
				{Flag: domain.ModeKey, Add: true, Param: "s3cret"},
			},
		},
		{
			name:  "compound with revoke and add",
			flags: "+t-i+l",
			args:  []string{"5"},
			want: []protocol.ChannelModeChange{
				{Flag: domain.ModeTopicLock, Add: true},
				{Flag: domain.ModeInviteOnly, Add: false},
				{Flag: domain.ModeUserLimit, Add: true, Param: "5"},
			},
		},
		{
			name:    "empty flag string rejected",
			flags:   "",
			args:    nil,
			wantErr: "mode: empty flag string",
		},
		{
			name:    "unknown flag rejected",
			flags:   "+x",
			args:    nil,
			wantErr: "", // parseChannelModeString doesn't reject unknowns; they pass through and the dispatcher rejects them. covered by dispatcher test
			want:    []protocol.ChannelModeChange{{Flag: domain.Mode('x'), Add: true}},
		},
		{
			name:    "missing nick on +o",
			flags:   "+o",
			args:    nil,
			wantErr: &domain.MissingModeParamError{},
		},
		{
			name:    "missing key on +k add",
			flags:   "+k",
			args:    nil,
			wantErr: &domain.MissingModeParamError{},
		},
		{
			name:    "surplus arg rejected",
			flags:   "+o",
			args:    []string{"alice", "leftover"},
			wantErr: "surplus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := ModeCommand{Flags: tt.flags, Args: tt.args}
			got, err := cmd.ToCommand(Context{Active: "#chan"})

			switch want := tt.wantErr.(type) {
			case nil:
				require.NoError(t, err)
				cm, ok := got.(protocol.ChannelMode)
				require.True(t, ok, "expected protocol.ChannelMode, got %T", got)
				require.Equal(t, domain.ChannelName("#chan"), cm.Channel)
				require.Equal(t, tt.want, cm.Changes)
			case string:
				if want == "" {
					require.NoError(t, err)
					cm, ok := got.(protocol.ChannelMode)
					require.True(t, ok)
					require.Equal(t, tt.want, cm.Changes)

					return
				}
				require.Error(t, err)
				require.ErrorContains(t, err, want)
			default:
				require.Error(t, err)
				require.ErrorAs(t, err, tt.wantErr)
			}
		})
	}
}
