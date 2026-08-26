package chatcmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

func TestContext_ActiveName_distinguishes_self_DM_from_absence(t *testing.T) {
	type result struct {
		name domain.ChannelName
		ok   bool
	}

	tests := []struct {
		name   string
		active domain.Window
		want   result
	}{
		{name: "self DM", active: domain.WindowKey(""), want: result{ok: true}},
		{name: "no active window"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := (Context{Active: tc.active}).ActiveName()

			require.Equal(t, tc.want, result{name: got, ok: ok})
		})
	}
}

func TestContext_errorResult_preserves_the_issuing_window(t *testing.T) {
	tests := []struct {
		name       string
		active     domain.Window
		wantTarget domain.ChannelName
	}{
		{name: "channel window", active: domain.WindowKey("#dev"), wantTarget: "#dev"},
		{name: "DM window", active: domain.WindowKey("claud3"), wantTarget: "claud3"},
		{name: "self DM window", active: domain.WindowKey("")},
		{name: "no active window"},
	}

	wantErr := errors.New("boom")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := (Context{Active: tt.active}).errorResult("join", wantErr)
			result, ok := message.(CommandErrorResult)
			if ok {
				result.Error.At = time.Time{}
			}

			type assertionSnapshot struct {
				OK     bool
				Result CommandErrorResult
			}

			require.Equal(t, assertionSnapshot{
				OK: true,
				Result: CommandErrorResult{
					Error: domain.ErrorEvent{
						Operation: "join", Err: wantErr, Target: tt.wantTarget,
					},
				},
			}, assertionSnapshot{
				OK:     ok,
				Result: result,
			})
		})
	}
}

func TestInviteCommand_Run_preserves_reply_events_with_a_typed_refusal(t *testing.T) {
	at := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	notice := domain.SystemNotice{
		Target: "#dev", Text: "no such nick: nobody", At: at,
	}
	refusal := domain.UnknownNickError{Nick: "nobody", At: at}
	client := toolTestClient{send: func(
		_ context.Context,
		command protocol.Command,
	) (protocol.Response, error) {
		require.Equal(t, protocol.Invite{
			Nick: "nobody", Channel: "#dev",
			Window: protocol.ChannelWindowTarget("#dev"),
		}, command)

		return protocol.Response{
			Events: []protocol.Event{notice},
			Err:    refusal,
		}, nil
	}}

	message := (InviteCommand{Nick: "nobody"}).Run(t.Context(), Context{
		Client: client,
		Active: domain.WindowKey("#dev"),
	})()
	reply, ok := message.(ReplyEvents)
	var gotRefusal domain.UnknownNickError
	if ok && reply.Error != nil {
		reply.Error.At = time.Time{}
		require.ErrorAs(t, reply.Error.Err, &gotRefusal)
		gotRefusal.At = time.Time{}
		reply.Error.Err = gotRefusal
	}

	type assertionSnapshot struct {
		OK    bool
		Reply ReplyEvents
	}

	require.Equal(t, assertionSnapshot{
		OK: true,
		Reply: ReplyEvents{
			Events: []domain.ProtocolEvent{notice},
			Error: &domain.ErrorEvent{
				Operation: "invite",
				Err:       domain.UnknownNickError{Nick: "nobody"},
				Target:    "#dev",
			},
		},
	}, assertionSnapshot{
		OK:    ok,
		Reply: reply,
	})
}
