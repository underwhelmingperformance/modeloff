package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	storemod "github.com/laney/modeloff/internal/store"
)

type contextSummaryAuthorityState struct {
	ReadAuthorityChanged   bool
	CommitAuthorityChanged bool
	Stored                 []storemod.ContextSummary
}

type invitationContextSummaryState struct {
	Summaries              []storemod.ContextSummary
	ReadError              error
	CommitAuthorityChanged bool
	Stored                 []storemod.ContextSummary
}

func TestSession_context_summaries_require_current_channel_authority(t *testing.T) {
	sess, backing := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#Dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))
	guard, err := client.sub.GuardWindow(ctx, protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)

	source := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource(protocol.UserClientID, "testuser"),
		Target: "#Dev", Body: "earlier context", At: fixedTime,
	}
	committed, err := sess.CommitContextSummary(ctx, guard, storemod.ContextSummaryUpdate{
		InstanceID: botty.ID(), Window: protocol.ChannelWindowTarget("#dev"),
		Summary: "The user supplied earlier context.", Sources: []protocol.IRCMessage{source},
		CreatedAt: fixedTime,
	})
	require.NoError(t, err)

	want := storemod.ContextSummary{
		ID: committed.ID, InstanceID: botty.ID(), Window: protocol.ChannelWindowTarget("#Dev"),
		Summary: "The user supplied earlier context.", Sources: []protocol.IRCMessage{source},
		CreatedAt: fixedTime,
	}
	got, err := sess.ContextSummaries(ctx, guard)
	require.NoError(t, err)
	require.Equal(t, []storemod.ContextSummary{want}, got)
	require.Equal(t, want, committed)

	resp, sendErr := sess.Handle(ctx, client, protocol.Part{Channel: "#dev"})
	require.NoError(t, sendErr)
	require.NoError(t, resp.Err)

	_, readErr := sess.ContextSummaries(ctx, guard)
	_, commitErr := sess.CommitContextSummary(ctx, guard, storemod.ContextSummaryUpdate{
		InstanceID: botty.ID(), Window: protocol.ChannelWindowTarget("#Dev"),
		Summary: "stale", Sources: []protocol.IRCMessage{source}, CreatedAt: fixedTime,
	})
	stored, err := backing.ContextSummaries(ctx, botty.ID(), protocol.ChannelWindowTarget("#Dev"))
	require.NoError(t, err)
	require.Equal(t, contextSummaryAuthorityState{
		ReadAuthorityChanged:   true,
		CommitAuthorityChanged: true,
		Stored:                 []storemod.ContextSummary{},
	}, contextSummaryAuthorityState{
		ReadAuthorityChanged:   errors.Is(readErr, protocol.ErrWindowAuthorityChanged),
		CommitAuthorityChanged: errors.Is(commitErr, protocol.ErrWindowAuthorityChanged),
		Stored:                 stored,
	})
}

func TestSession_context_summaries_require_current_direct_peer_authority(t *testing.T) {
	sess, backing := newTestSession(t)
	ctx := t.Context()

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	alice, aliceClient := seedPassiveInstance(t, sess, "alice", "test/model")
	target := protocol.DirectWindowTarget(alice.ID())
	guard, err := client.sub.GuardWindow(ctx, target)
	require.NoError(t, err)

	source := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.ClientSource(alice.ID(), alice.Nick()),
		Target: string(botty.ID()), Body: "private context", At: fixedTime,
	}
	committed, err := sess.CommitContextSummary(ctx, guard, storemod.ContextSummaryUpdate{
		InstanceID: botty.ID(), Window: target, Summary: "Alice supplied private context.",
		Sources: []protocol.IRCMessage{source}, CreatedAt: fixedTime,
	})
	require.NoError(t, err)
	require.Equal(t, storemod.ContextSummary{
		ID: committed.ID, InstanceID: botty.ID(), Window: target,
		Summary: "Alice supplied private context.", Sources: []protocol.IRCMessage{source},
		CreatedAt: fixedTime,
	}, committed)

	resp, sendErr := sess.Handle(ctx, aliceClient, protocol.Quit{Reason: "leaving"})
	require.NoError(t, sendErr)
	require.NoError(t, resp.Err)

	_, readErr := sess.ContextSummaries(ctx, guard)
	_, commitErr := sess.CommitContextSummary(ctx, guard, storemod.ContextSummaryUpdate{
		InstanceID: botty.ID(), Window: target, Summary: "stale",
		Sources: []protocol.IRCMessage{source}, CreatedAt: fixedTime,
	})
	stored, err := backing.ContextSummaries(ctx, botty.ID(), target)
	require.NoError(t, err)
	require.Equal(t, contextSummaryAuthorityState{
		ReadAuthorityChanged:   true,
		CommitAuthorityChanged: true,
		Stored:                 []storemod.ContextSummary{},
	}, contextSummaryAuthorityState{
		ReadAuthorityChanged:   errors.Is(readErr, protocol.ErrWindowAuthorityChanged),
		CommitAuthorityChanged: errors.Is(commitErr, protocol.ErrWindowAuthorityChanged),
		Stored:                 stored,
	})
}

func TestSession_invitation_context_has_no_prior_summaries(t *testing.T) {
	sess, backing := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, client := seedPassiveInstance(t, sess, "botty", "test/model")
	resp, sendErr := userClient(t, sess).Send(ctx, protocol.Invite{
		Nick: botty.Nick(), Channel: "#dev",
	})
	require.NoError(t, sendErr)
	require.NoError(t, resp.Err)
	guard, err := client.sub.GuardInvitation(ctx, "#dev")
	require.NoError(t, err)

	summaries, readErr := sess.ContextSummaries(ctx, guard)
	_, commitErr := sess.CommitContextSummary(ctx, guard, storemod.ContextSummaryUpdate{
		InstanceID: botty.ID(), Window: protocol.ChannelWindowTarget("#dev"),
		Summary: "not admitted", Sources: []protocol.IRCMessage{{
			Kind: protocol.KindInvite, Target: "#dev", Subject: string(botty.Nick()), At: fixedTime,
		}}, CreatedAt: fixedTime,
	})
	stored, err := backing.ContextSummaries(ctx, botty.ID(), protocol.ChannelWindowTarget("#dev"))
	require.NoError(t, err)
	require.Equal(t, invitationContextSummaryState{
		Summaries: []storemod.ContextSummary{}, Stored: []storemod.ContextSummary{},
		CommitAuthorityChanged: true,
	}, invitationContextSummaryState{
		Summaries: summaries, ReadError: readErr,
		CommitAuthorityChanged: errors.Is(commitErr, protocol.ErrWindowAuthorityChanged),
		Stored:                 stored,
	})
}

// TestSession_context_summaries_refuse_a_guard_the_session_did_not_issue
// pins the same boundary the model-turn journal sits behind: a
// summary belongs to one actor and one window, and only a guard the
// session issued names them.
func TestSession_context_summaries_refuse_a_guard_the_session_did_not_issue(t *testing.T) {
	sess, backing := newTestSession(t)
	ctx := t.Context()
	require.NoError(t, userJoin(ctx, t, sess, "#dev"))

	botty, _ := seedPassiveInstance(t, sess, "botty", "test/model")
	require.NoError(t, joinAs(ctx, sess, botty, "#dev", ""))

	summaries, readErr := sess.ContextSummaries(ctx, foreignWindowGuard{})
	committed, commitErr := sess.CommitContextSummary(ctx, foreignWindowGuard{},
		storemod.ContextSummaryUpdate{
			InstanceID: botty.ID(),
			Window:     protocol.ChannelWindowTarget("#dev"),
			Summary:    "unauthorised",
			CreatedAt:  fixedTime,
		})
	stored, storedErr := backing.ContextSummaries(
		ctx, botty.ID(), protocol.ChannelWindowTarget("#dev"),
	)

	type assertionSnapshot struct {
		Summaries   []storemod.ContextSummary
		ReadError   string
		Committed   storemod.ContextSummary
		CommitError string
		Stored      []storemod.ContextSummary
		StoredError error
	}

	require.Equal(t, assertionSnapshot{
		ReadError: "read context summaries: window guard session.foreignWindowGuard " +
			"was not issued by this session",
		CommitError: "commit context summary: window guard session.foreignWindowGuard " +
			"was not issued by this session",
		Stored: []storemod.ContextSummary{},
	}, assertionSnapshot{
		Summaries:   summaries,
		ReadError:   readErr.Error(),
		Committed:   committed,
		CommitError: commitErr.Error(),
		Stored:      stored,
		StoredError: storedErr,
	})
}
