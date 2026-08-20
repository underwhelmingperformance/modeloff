package modelclient

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// TestSend_files_the_issuers_own_replies covers which of a command's
// synchronous reply events land in the model's private replies ring.
//
// The ring has to hold what the dispatcher writes to the instance
// reply log, and nothing else. Anything the dispatcher persists but
// this switch drops is a reply the model only meets again after a
// reattach reloads the log, so a refused INVITE would vanish from the
// turn that issued it and reappear on the next connection.
// [domain.ListEnd] is the other way round: it terminates the LIST
// stream on the wire, carries no transcript line, and the dispatcher
// does not persist it.
func TestSend_files_the_issuers_own_replies(t *testing.T) {
	t.Parallel()

	at := newFakeSession().Now()

	tests := []struct {
		name  string
		reply domain.ProtocolEvent
		want  []domain.StoredEvent
	}{
		{
			name:  "a WHOIS snapshot is filed",
			reply: domain.Whois{Nick: "alice", Target: "#dev", At: at},
			want:  []domain.StoredEvent{{Event: domain.Whois{Nick: "alice", Target: "#dev", At: at}}},
		},
		{
			name:  "a LIST row is filed",
			reply: domain.ListReply{Channel: "#dev", At: at},
			want:  []domain.StoredEvent{{Event: domain.ListReply{Channel: "#dev", At: at}}},
		},
		{
			name:  "a system notice is filed",
			reply: domain.SystemNotice{Target: "#dev", Text: "no such nick: ghost", At: at},
			want:  []domain.StoredEvent{{Event: domain.SystemNotice{Target: "#dev", Text: "no such nick: ghost", At: at}}},
		},
		{
			name:  "the LIST terminator is not filed",
			reply: domain.ListEnd{At: at},
			want:  nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sess := newFakeSession()
			sess.handleFn = func(protocol.Command) protocol.Response {
				return protocol.Response{Events: []domain.ProtocolEvent{tc.reply}}
			}

			mc := newTestModelClient(sess)

			_, err := mc.Send(t.Context(), protocol.List{})
			require.NoError(t, err)

			require.Equal(t, tc.want, mc.hist.snapshotReplies())
		})
	}
}
