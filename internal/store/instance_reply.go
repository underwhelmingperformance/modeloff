package store

import (
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// InstanceReplyRecord is one persisted private reply and the client
// window in which its command was issued. ID remains store-internal
// ordering state and is not exposed through the actor subscription.
type InstanceReplyRecord struct {
	ID     int64
	Window protocol.WindowTarget
	Event  domain.IssuerReply
}
