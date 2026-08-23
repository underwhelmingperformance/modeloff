package modelclient

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

func TestDispatchTrigger(t *testing.T) {
	self := domain.NewModelInstance("inst-self", "me", "test/model", "", nil)
	other := domain.InstanceID("inst-other")
	at := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		ev      domain.ProtocolEvent
		channel domain.ChannelName
		ok      bool
	}{
		{"message triggers", domain.Message{Source: domain.ClientSource(other, "alice"), Target: "#dev", Body: "hi", At: at}, "#dev", true},
		{"join triggers", domain.Join{Source: domain.ClientSource(other, "alice"), Target: "#dev", At: at}, "#dev", true},
		{"join by self does not trigger", domain.Join{Source: domain.ClientSource(self.ID(), "me"), Target: "#dev", At: at}, "", false},
		{"part by another triggers", domain.Part{Source: domain.ClientSource(other, "alice"), Target: "#dev", At: at}, "#dev", true},
		{"part by self does not trigger", domain.Part{Source: domain.ClientSource(self.ID(), "me"), Target: "#dev", At: at}, "", false},
		{"delivered invite triggers", domain.Invited{Source: domain.ClientSource(other, "alice"), Target: "#dev", Invitee: "me", At: at}, "#dev", true},
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
