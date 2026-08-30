package screens

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/store/storetest"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
)

// personaReviewFixture is a chat-screen focused on `#dev`, over a fake
// API the test drives through two Go channels: the fake writes each
// request it receives to `requests`, and returns whatever the test
// writes to `answers`.
type personaReviewFixture struct {
	screen   ChatScreen
	api      *uitest.FakeAPI
	requests chan api.PersonaRequest
	answers  chan string
}

func newPersonaReviewFixture(t *testing.T) *personaReviewFixture {
	t.Helper()

	f := &personaReviewFixture{
		requests: make(chan api.PersonaRequest, 8),
		answers:  make(chan string, 8),
	}

	f.api = &uitest.FakeAPI{}
	// The review checks the model before it asks for a description, so
	// the catalogue has to hold the models these tests add.
	f.api.ListModelsFn = func(context.Context) ([]api.ModelInfo, error) {
		return []api.ModelInfo{
			{ID: "vendor/model", SupportedParameters: []string{"tools"}},
			{ID: "vendor/other", SupportedParameters: []string{"tools"}},
		}, nil
	}
	f.api.GeneratePersonaFn = func(
		ctx context.Context, _ domain.ModelID, req api.PersonaRequest,
	) (string, error) {
		f.requests <- req

		select {
		case answer := <-f.answers:
			return answer, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	stored := storetest.NewMemoryStore(t)
	sess, mgr, user := uitest.NewTestSession(
		t, stored, f.api, nil, nil, "test-key", "small/model", t.Context)

	screen, err := NewChatScreen(t.Context, sess, mgr, user, nil, nil, domain.KindChannel)
	require.NoError(t, err)

	window := newWindow(domain.NewChannelWindow("#dev", time.Time{}))
	screen.channels.Insert(window)
	screen, _ = screen.focus("#dev")

	f.screen = screen

	return f
}

// propose delivers the request `/add-model` produces, in the window it
// was typed in. The chat-screen reads that window from the enclosing
// [chatcmd.CommandResult] and drops a proposal naming any other, so a
// proposal delivered without one starts no review.
func (f *personaReviewFixture) propose(
	screen ChatScreen, model domain.ModelID,
) (ChatScreen, tea.Cmd) {
	return screen.route(chatcmd.CommandResult{
		IssuingWindow:         activeWindowIdentity(screen),
		IssuingWindowRevision: activeWindowRevision(screen),
		Message: chatcmd.PersonaProposalRequested{
			Channel: screen.activeName(), Model: model,
		},
	})
}

// request waits for the generation request the screen dispatched. The
// command runs on its own goroutine, so the test waits for the fake to
// write the request it received to `requests`.
func (f *personaReviewFixture) request(t *testing.T, cmd tea.Cmd) api.PersonaRequest {
	t.Helper()

	go collectMsgs(cmd)

	select {
	case req := <-f.requests:
		return req
	case <-time.After(testWait):
		t.Fatal("no persona request reached the api client")
	}

	return api.PersonaRequest{}
}

// testWait bounds a wait on the fake's channels. Nothing sleeps for it
// on a passing run: it is what makes a lost message fail the test,
// which a wait without a bound would leave hanging.
const testWait = 5 * time.Second

// TestChatScreen_add_model_without_a_persona_opens_the_review covers
// what the chat-screen does with the request `/add-model` returns when
// no persona was typed: it records a review and asks for a description,
// which arrives as the first candidate.
//
// The end-to-end path, from the typed command through the selector to
// the JOIN, is `TestChatScreen_add_model_command`.
func TestChatScreen_add_model_without_a_persona_opens_the_review(t *testing.T) {
	f := newPersonaReviewFixture(t)

	screen, cmd := f.propose(f.screen, "vendor/model")

	require.NotNil(t, screen.personaReview)
	require.Equal(t, api.PersonaRequest{}, f.request(t, cmd))

	screen, _ = screen.route(personaCandidateMsg{
		seq: screen.personaReview.seq, text: "a terse reviewer",
	})

	require.Equal(t, []string{"a terse reviewer"}, screen.personaReview.candidates)
}

// TestChatScreen_persona_reroll_sends_the_adjustment pins what the
// operator's steering becomes: the description they passed over, with
// what they typed as the reason it was turned down.
func TestChatScreen_persona_reroll_sends_the_adjustment(t *testing.T) {
	f := newPersonaReviewFixture(t)

	screen, cmd := f.propose(f.screen, "vendor/model")
	f.request(t, cmd)

	screen, _ = screen.route(personaCandidateMsg{
		seq: screen.personaReview.seq, text: "a terse reviewer",
	})

	before := screen.personaReview.seq
	screen, cmd = screen.route(components.PersonaRerollMsg{
		Review: screen.personaReview.id, Adjustment: "more sceptical",
	})

	require.Equal(t, api.PersonaRequest{
		Rejected: []api.RejectedPersona{{
			Description: "a terse reviewer",
			Reason:      "more sceptical",
		}},
	}, f.request(t, cmd))
	require.Greater(t, screen.personaReview.seq, before)
}

// TestChatScreen_persona_candidate_from_an_older_request_is_dropped
// covers a description arriving after the operator asked for another.
// The request it belongs to is one they have moved past, so it is not
// the description they are looking at.
func TestChatScreen_persona_candidate_from_an_older_request_is_dropped(t *testing.T) {
	f := newPersonaReviewFixture(t)

	screen, cmd := f.propose(f.screen, "vendor/model")
	f.request(t, cmd)

	stale := screen.personaReview.seq
	screen, _ = screen.route(personaCandidateMsg{seq: stale, text: "a terse reviewer"})

	screen, cmd = screen.route(components.PersonaRerollMsg{Review: screen.personaReview.id})
	f.request(t, cmd)

	screen, _ = screen.route(personaCandidateMsg{
		seq: stale, text: "a description nobody waited for",
	})

	require.Equal(t, []string{"a terse reviewer"}, screen.personaReview.candidates)
}

// TestChatScreen_persona_failure_keeps_the_review pins that a failed
// request leaves the descriptions written before it in place, so the
// operator can accept one or ask for another.
func TestChatScreen_persona_failure_keeps_the_review(t *testing.T) {
	f := newPersonaReviewFixture(t)

	screen, cmd := f.propose(f.screen, "vendor/model")
	f.request(t, cmd)

	screen, _ = screen.route(personaCandidateMsg{
		seq: screen.personaReview.seq, text: "a terse reviewer",
	})
	screen, cmd = screen.route(components.PersonaRerollMsg{Review: screen.personaReview.id})
	f.request(t, cmd)

	upstream := errors.New("upstream unreachable")
	screen, _ = screen.route(personaProposalFailedMsg{
		seq: screen.personaReview.seq, err: upstream,
	})

	require.Equal(t, struct {
		Candidates []string
		Generating bool
		Err        error
	}{
		Candidates: []string{"a terse reviewer"},
		Err:        upstream,
	}, struct {
		Candidates []string
		Generating bool
		Err        error
	}{
		Candidates: screen.personaReview.candidates,
		Generating: screen.personaReview.generating,
		Err:        screen.personaReview.err,
	})
}

// TestChatScreen_persona_cancel_ends_the_request covers Esc: the review
// is forgotten, the context the request runs under is cancelled, and
// the selector is closed. The fake blocks until its context ends, so
// the request returning is what proves the cancellation.
func TestChatScreen_persona_cancel_ends_the_request(t *testing.T) {
	f := newPersonaReviewFixture(t)

	screen, cmd := f.propose(f.screen, "vendor/model")

	done := make(chan []tea.Msg, 1)
	go func() { done <- collectMsgs(cmd) }()
	<-f.requests

	screen, cmd = screen.route(components.PersonaCancelMsg{Review: screen.personaReview.id})

	select {
	case msgs := <-done:
		require.ErrorIs(t, proposalFailure(t, msgs), context.Canceled)
	case <-time.After(testWait):
		t.Fatal("the generation request outlived the review")
	}

	require.Nil(t, screen.personaReview)
	require.Equal(t, []tea.Msg{components.PersonaSelectorMsg{Revision: 2}}, collectMsgs(cmd))
}

// proposalFailure is the error the generation request ended with.
func proposalFailure(t *testing.T, msgs []tea.Msg) error {
	t.Helper()

	for _, msg := range msgs {
		if failed, ok := msg.(personaProposalFailedMsg); ok {
			return failed.err
		}
	}

	t.Fatalf("the request produced no failure among %d messages", len(msgs))

	return nil
}

// TestChatScreen_leaving_the_window_ends_the_review pins the rule that
// makes one review enough: it belongs to the window it was started in.
func TestChatScreen_leaving_the_window_ends_the_review(t *testing.T) {
	f := newPersonaReviewFixture(t)

	screen := f.screen
	screen.channels.Insert(newWindow(domain.NewChannelWindow("#other", time.Time{})))

	screen, cmd := f.propose(screen, "vendor/model")
	f.request(t, cmd)

	screen, _ = screen.focus("#other")

	require.Nil(t, screen.personaReview)
}

// TestChatScreen_a_second_add_model_is_refused covers `/add-model`
// typed again while a review is open. The open one is what the selector
// is showing, so the second starts nothing: the review keeps the model
// and the request number it had, and no second request reaches the
// upstream.
func TestChatScreen_a_second_add_model_is_refused(t *testing.T) {
	f := newPersonaReviewFixture(t)

	screen, cmd := f.propose(f.screen, "vendor/model")
	f.request(t, cmd)

	before := screen.personaReview.seq

	screen, cmd = f.propose(screen, "vendor/other")

	collectMsgs(cmd)

	require.Equal(t, struct {
		Model    domain.ModelID
		Seq      uint64
		Requests int
	}{Model: "vendor/model", Seq: before}, struct {
		Model    domain.ModelID
		Seq      uint64
		Requests int
	}{
		Model:    screen.personaReview.model,
		Seq:      screen.personaReview.seq,
		Requests: len(f.requests),
	})
}
