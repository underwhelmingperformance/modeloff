package screens

import (
	"time"

	"github.com/laney/modeloff/internal/domain"
)

// dmWindow is the chat-screen's presentation state for one direct
// conversation. The stable peer identity addresses the conversation;
// nick is the most recent display name the client has observed.
type dmWindow struct {
	peer    domain.InstanceID
	nick    domain.Nick
	created time.Time
}

func newDMWindow(peer domain.InstanceID, nick domain.Nick, created time.Time) *dmWindow {
	return &dmWindow{peer: peer, nick: nick, created: created}
}

func (w *dmWindow) Name() domain.ChannelName { return domain.ChannelName(w.peer) }
func (w *dmWindow) Created() time.Time       { return w.created }
func (*dmWindow) Kind() domain.ChannelKind   { return domain.KindDM }

func (w *dmWindow) observeNick(nick domain.Nick) bool {
	if nick == "" || nick == w.nick {
		return false
	}

	w.nick = nick
	return true
}

func (w *dmWindow) DisplayName() string {
	if w.nick != "" {
		return string(w.nick)
	}

	return string(w.peer)
}

func (w *dmWindow) Less(other domain.Window) bool {
	if other.Kind() != domain.KindDM {
		return false
	}

	return w.Name() < other.Name()
}
