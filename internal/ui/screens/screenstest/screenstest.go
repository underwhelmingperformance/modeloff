// Package screenstest provides test helpers for the screens package.
package screenstest

import (
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/ui/screens"
)

// SendProtocolEvent injects a protocol event into the chat screen's
// Update loop with the recipient-channel envelope that the session's
// fan-out would have produced in production. Use this from tests
// instead of sending raw `domain.*` values, which no production
// path produces.
func SendProtocolEvent(tm *teatest.TestModel, evt protocol.Event, targets []domain.ChannelName) {
	tm.Send(screens.NewProtocolEventForTest(evt, targets))
}
