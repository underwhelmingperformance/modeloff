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

	window := protocol.ChannelWindowTarget("#dev")
	filed := func(event domain.PersistableEvent) []storedReply {
		return []storedReply{{window: window, event: domain.StoredEvent{Event: event}}}
	}

	tests := []struct {
		name  string
		reply domain.ProtocolEvent
		want  []storedReply
	}{
		{
			name:  "a WHOIS snapshot is filed",
			reply: domain.Whois{Nick: "alice", At: at},
			want:  filed(domain.Whois{Nick: "alice", At: at}),
		},
		{
			name:  "a LIST row is filed",
			reply: domain.ListReply{Channel: "#dev", At: at},
			want:  filed(domain.ListReply{Channel: "#dev", At: at}),
		},
		{
			name:  "a topic snapshot is filed",
			reply: domain.TopicInfo{Target: "#dev", Topic: "release work", TopicSetBy: "alice", At: at},
			want: filed(domain.TopicInfo{
				Target: "#dev", Topic: "release work", TopicSetBy: "alice", At: at,
			}),
		},
		{
			name:  "a system notice is filed",
			reply: domain.SystemNotice{Target: "#dev", Text: "no such nick: ghost", At: at},
			want:  filed(domain.SystemNotice{Target: "#dev", Text: "no such nick: ghost", At: at}),
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

			_, err := mc.Send(t.Context(), protocol.List{Window: window})
			require.NoError(t, err)

			require.Equal(t, tc.want, mc.hist.snapshotRepliesFor("#dev"))
		})
	}
}

func TestSend_files_reply_events_returned_with_a_command_refusal(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	notice := domain.SystemNotice{
		Target: "#dev", Text: "no such nick: ghost", At: sess.Now(),
	}
	refusal := domain.UnknownNickError{Nick: "ghost", At: sess.Now()}
	sess.handleFn = func(protocol.Command) protocol.Response {
		return protocol.Response{
			Events: []protocol.Event{notice},
			Err:    refusal,
		}
	}
	mc := newTestModelClient(sess)

	response, err := mc.Send(t.Context(), protocol.Invite{
		Nick: "ghost", Channel: "#dev",
		Window: protocol.ChannelWindowTarget("#other"),
	})

	require.NoError(t, err)
	require.Equal(t, protocol.Response{
		Events: []protocol.Event{notice},
		Err:    refusal,
	}, response)
	require.Equal(t, []storedReply{{
		window: protocol.ChannelWindowTarget("#other"),
		event:  domain.StoredEvent{Event: notice},
	}}, mc.hist.snapshotRepliesFor("#other"))
	require.Empty(t, mc.hist.snapshotRepliesFor("#dev"))
}

func TestFileBatch_files_a_join_topic_reply(t *testing.T) {
	t.Parallel()

	sess := newFakeSession()
	mc := newTestModelClient(sess)
	topic := domain.TopicInfo{
		Target:     "#dev",
		Topic:      "release work",
		TopicSetBy: "alice",
		TopicSetAt: sess.Now(),
		At:         sess.Now(),
	}

	require.Empty(t, mc.fileBatch(t.Context(), []protocol.Delivery{{Event: topic}}))
	require.Equal(t, []storedReply{{
		window: protocol.ChannelWindowTarget("#dev"),
		event:  domain.StoredEvent{Event: topic},
	}}, mc.hist.snapshotRepliesFor("#dev"))
}
