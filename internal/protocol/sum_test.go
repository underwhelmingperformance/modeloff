package protocol_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestCommand_sum_membership(t *testing.T) {
	t.Parallel()

	channel := domain.ChannelName("#general")
	nick := domain.Nick("alice")

	cases := []struct {
		name string
		cmd  protocol.Command
	}{
		{"join", protocol.Join{Channel: channel}},
		{"part", protocol.Part{Channel: channel, Reason: "bye"}},
		{"privmsg", protocol.PrivMsg{Target: channel, Body: "hello"}},
		{"action", protocol.Action{Target: channel, Body: "waves"}},
		{"topic", protocol.Topic{Channel: channel, Body: "discuss"}},
		{"invite", protocol.Invite{Nick: nick, Channel: channel}},
		{"kick", protocol.Kick{Nick: nick, Channel: channel}},
		{"nick", protocol.Nick{New: nick}},
		{"whois", protocol.Whois{Nick: nick}},
		{"list", protocol.List{}},
		{"addmodel", protocol.AddModel{Model: "anthropic/claude", Persona: "p"}},
		{"quit", protocol.Quit{Reason: "gone"}},
		{"kill", protocol.Kill{Nick: nick, Reason: "spam"}},
		{"oper", protocol.Oper{Name: "name", Password: "pw"}},
		{"channelmode", protocol.ChannelMode{Channel: channel, Changes: []protocol.ChannelModeChange{
			{Flag: domain.ModeOperator, Add: true, Target: nick},
		}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			switch tc.cmd.(type) {
			case protocol.Join,
				protocol.Part,
				protocol.PrivMsg,
				protocol.Action,
				protocol.Topic,
				protocol.Invite,
				protocol.Kick,
				protocol.Nick,
				protocol.Whois,
				protocol.List,
				protocol.AddModel,
				protocol.Quit,
				protocol.Kill,
				protocol.Oper,
				protocol.ChannelMode:
				// member of the sum
			default:
				t.Fatalf("command %T is not a member of the protocol Command sum", tc.cmd)
			}
		})
	}
}

func TestEvent_sum_membership(t *testing.T) {
	t.Parallel()

	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	channel := domain.ChannelName("#general")
	nick := domain.Nick("alice")

	cases := []struct {
		name  string
		event protocol.Event
	}{
		{"message", domain.Message{Target: channel, From: nick, Body: "hi", At: at}},
		{"join", domain.Join{Target: channel, Nick: nick, At: at}},
		{"part", domain.Part{Target: channel, Nick: nick, At: at}},
		{"quit", domain.Quit{Nick: nick, At: at}},
		{"topic_change", domain.TopicChange{Target: channel, Topic: "t", By: nick, At: at}},
		{"mode_change", domain.ModeChange{Target: channel, Nick: nick, Flag: domain.ModeOperator, Add: true, By: nick, At: at}},
		{"model_invited", domain.ModelInvited{Target: channel, Nick: nick, By: nick, At: at}},
		{"model_kicked", domain.ModelKicked{Target: channel, Nick: nick, By: nick, At: at}},
		{"nick_change", domain.NickChange{OldNick: nick, NewNick: "ally", At: at}},
		{"help", domain.Help{Target: channel, At: at}},
		{"whois", domain.Whois{Target: channel, Nick: nick, At: at}},
		{"list_reply", domain.ListReply{Channel: channel, Members: 1, At: at}},
		{"list_end", domain.ListEnd{At: at}},
		{"system_notice", domain.SystemNotice{Target: channel, Text: "n", At: at}},
		{"model_dispatch_started", domain.ModelDispatchStarted{At: at}},
		{"model_dispatch_done", domain.ModelDispatchDone{At: at}},
		{"names_reply", domain.NamesReplyEvent{Channel: channel, At: at}},
		{"welcome", domain.Welcome{ServerName: "modeloff", Nick: nick, At: at}},
		{"reconnected", domain.Reconnected{At: at}},
		{"model_unavailable_error", domain.ModelUnavailableError{Channel: channel, Nick: nick, At: at}},
		{"unknown_nick_error", domain.UnknownNickError{Nick: nick}},
		{"no_such_channel_error", domain.NoSuchChannelError{Channel: channel}},
		{"nick_in_use_error", domain.NickInUseError{Nick: nick}},
		{"not_operator_error", domain.NotOperatorError{Command: "ADDMODEL"}},
		{"unknown_command_error", domain.UnknownCommandError{Name: "typo"}},
		{"unknown_config_key_error", domain.UnknownConfigKeyError{Key: "bogus"}},
		{"invalid_duration_error", domain.InvalidDurationError{Input: "5xq", Err: fmt.Errorf("bad")}},
		{"unsupported_model_error", domain.UnsupportedModelError{ModelID: "test/model"}},
		{"killed", protocol.Killed{By: nick, Reason: "spam", At: at}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			switch tc.event.(type) {
			case domain.Message,
				domain.Join,
				domain.Part,
				domain.Quit,
				domain.TopicChange,
				domain.ModeChange,
				domain.ModelInvited,
				domain.ModelKicked,
				domain.NickChange,
				domain.Help,
				domain.Whois,
				domain.ListReply,
				domain.ListEnd,
				domain.SystemNotice,
				domain.ModelDispatchStarted,
				domain.ModelDispatchDone,
				domain.NamesReplyEvent,
				domain.Welcome,
				domain.Reconnected,
				domain.ModelUnavailableError,
				domain.UnknownNickError,
				domain.NoSuchChannelError,
				domain.NickInUseError,
				domain.NotOperatorError,
				domain.UnknownCommandError,
				domain.UnknownConfigKeyError,
				domain.InvalidDurationError,
				domain.UnsupportedModelError,
				protocol.Killed:
				// member of the sum
			default:
				t.Fatalf("event %T is not a member of the protocol Event sum", tc.event)
			}
		})
	}
}

func TestNotOperatorError_implements_error(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  protocol.NotOperatorError
		want string
	}{
		{
			name: "with command",
			err:  protocol.NotOperatorError{Command: "AddModel"},
			want: "permission denied: AddModel requires operator privileges",
		},
		{
			name: "without command",
			err:  protocol.NotOperatorError{},
			want: "permission denied: not an operator",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.EqualError(t, tc.err, tc.want)
		})
	}
}

func TestModeOperator_value(t *testing.T) {
	t.Parallel()

	require.Equal(t, domain.Mode('o'), domain.ModeOperator)
}
