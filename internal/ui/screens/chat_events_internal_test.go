package screens

import (
	"context"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/protocol"
)

// protocolListenerEffect is what the listener command produced, and
// whether it produced anything at all.
type protocolListenerEffect struct {
	Returned bool
	Msg      tea.Msg
}

// TestChatScreen_listenForProtocolEvents_ends_with_the_application pins that
// the wait ends when the application context is cancelled. Nothing else ends
// it: the subscription that writes the deliveries belongs to the session,
// which the same cancellation stops, so a command still waiting on a
// delivery holds its goroutine for as long as the process runs.
func TestChatScreen_listenForProtocolEvents_ends_with_the_application(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		screen := ChatScreen{
			baseContext: func() context.Context { return ctx },
			client:      queuedEventClient{events: make(chan protocol.Delivery)},
		}
		listen := screen.listenForProtocolEvents()

		var got protocolListenerEffect
		go func() { got = protocolListenerEffect{Returned: true, Msg: listen()} }()
		synctest.Wait()
		waiting := got

		cancel()
		synctest.Wait()

		require.Equal(t, []protocolListenerEffect{
			{}, {Returned: true},
		}, []protocolListenerEffect{waiting, got})
	})
}
