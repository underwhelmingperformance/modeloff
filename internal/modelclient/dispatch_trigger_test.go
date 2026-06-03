package modelclient

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestDispatchTrigger(t *testing.T) {
	self := domain.InstanceID("inst-self")
	other := domain.InstanceID("inst-other")
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		ev      domain.ProtocolEvent
		channel domain.ChannelName
		ok      bool
	}{
		{"message triggers", domain.Message{Target: "#dev", From: "alice", InstanceID: other, Body: "hi", At: at}, "#dev", true},
		{"join triggers", domain.Join{Target: "#dev", Nick: "alice", InstanceID: other, At: at}, "#dev", true},
		{"part by another triggers", domain.Part{Target: "#dev", Nick: "alice", InstanceID: other, At: at}, "#dev", true},
		{"part by self does not trigger", domain.Part{Target: "#dev", Nick: "me", InstanceID: self, At: at}, "", false},
		{"invite addressed to self triggers", domain.ModelInvited{Target: "#dev", Nick: "me", InstanceID: self, By: "alice", At: at}, "#dev", true},
		{"invite addressed to another does not trigger", domain.ModelInvited{Target: "#dev", Nick: "alice", InstanceID: other, By: "bob", At: at}, "", false},
		{"poke triggers", domain.PokeEvent{Channel: "#dev", At: at}, "#dev", true},
		{"quit does not trigger", domain.Quit{At: at}, "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			channel, _, ok := dispatchTrigger(self, tc.ev)

			require.Equal(t, tc.channel, channel)
			require.Equal(t, tc.ok, ok)
		})
	}
}
