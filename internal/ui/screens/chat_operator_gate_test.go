package screens

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/command"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// unprivilegedClient holds no capabilities, which is what a client that
// never authenticated as the operator looks like.
type unprivilegedClient struct {
	protocol.Client
}

func (unprivilegedClient) Caps() command.CapabilityHolder {
	return command.NoCapabilities()
}

// operatorGateEffect is what running an operator-gated command produced.
type operatorGateEffect struct {
	Refused bool
	Command string
}

// TestChatScreen_refuses_an_operator_command_without_the_capability pins
// that `caps:"operator"` authorises and does not only hide.
//
// The tag already keeps a command out of completion, out of help and out
// of every model's tool registry. None of that stops a line the user
// types anyway, and `/persona <nick> --reset` reaches the manager
// directly, so without a check here the tag guaranteed only the half a
// model is on the wrong side of.
func TestChatScreen_refuses_an_operator_command_without_the_capability(t *testing.T) {
	screen := newScreenFixture(t)
	screen.client = unprivilegedClient{Client: screen.client}

	cmd := screen.handleCommand(components.CommandSubmitMsg{
		Raw: "/persona botty --reset",
	})
	require.NotNil(t, cmd)

	got := operatorGateEffect{}
	for _, msg := range collectMsgs(cmd) {
		result, ok := msg.(chatcmd.CommandResult)
		if !ok {
			continue
		}
		event, ok := result.Message.(domain.ErrorEvent)
		if !ok {
			continue
		}
		var refusal domain.NotOperatorError
		if errors.As(event.Err, &refusal) {
			got = operatorGateEffect{Refused: true, Command: refusal.Command}
		}
	}

	require.Equal(t, operatorGateEffect{Refused: true, Command: "persona"}, got)
}
