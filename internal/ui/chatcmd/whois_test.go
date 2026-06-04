package chatcmd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestWhoisCommand_ToCommand_carries_issuing_window proves the
// `/whois` wire command stamps the active window so the dispatcher
// can route the reply back to where it was issued.
func TestWhoisCommand_ToCommand_carries_issuing_window(t *testing.T) {
	cmd := WhoisCommand{Nick: "claud3"}

	got, err := cmd.ToCommand(Context{Active: "#dev"})
	require.NoError(t, err)
	require.Equal(t, protocol.Whois{
		Nick:    domain.Nick("claud3"),
		Channel: "#dev",
	}, got)
}
