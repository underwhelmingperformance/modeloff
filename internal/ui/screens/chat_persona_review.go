package screens

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/chatcmd"
	"github.com/laney/modeloff/internal/ui/components"
)

// personaReviewTick is how often the elapsed time on a running request
// is redrawn. The selector shows that time in whole seconds, so one
// tick per second is what makes the count move without redrawing a row
// that would read the same.
const personaReviewTick = time.Second

// personaReview is the `/add-model` the operator is reviewing. It ends
// on an `ADDMODEL` the session accepts, on Esc, or on the operator
// leaving the window; a refused `ADDMODEL` leaves it open with the
// reason on screen. There is at most one, and it belongs to the window
// it was started in, so no review state survives a window change.
type personaReview struct {
	// window is the exact window the review belongs to, and not its
	// name: a channel parted and rejoined is a new window under the
	// same name, and a review started in the one before it belongs to
	// neither the new window nor the operator's attention.
	window domain.Window
	model  domain.ModelID

	// candidates holds each description the review was given, in arrival
	// order. The selector owns which one the operator is looking at, and
	// its accept and re-roll messages report that index.
	//
	// rejected is what they passed over: each description with the
	// reason, which is the adjustment they typed or a plain request for
	// a different one. `ProposePersona` adds its own refusals to the
	// request it builds, so the model can be told about more than these.
	candidates []string
	rejected   []api.RejectedPersona

	// id identifies the review. Every decision names the id of the
	// review it was taken in, and the handlers discard a decision whose
	// id is not the open review's, so a decision from a review the
	// operator has since left changes nothing. The selector reads the
	// same id to tell one review from the next.
	id uint64

	// seq identifies the newest request this review has made. The
	// numbers come from the chat-screen and never repeat, so a result
	// from an earlier request, or from a review the operator has since
	// abandoned, is stamped with a number no handler will match and is
	// discarded.
	seq uint64

	generating bool
	startedAt  time.Time
	accepting  bool
	err        error

	cancel context.CancelFunc
}

// personaCandidateMsg delivers a description written for a review.
type personaCandidateMsg struct {
	seq  uint64
	text string
}

// personaProposalFailedMsg reports a request that produced no
// description, and why.
type personaProposalFailedMsg struct {
	seq uint64
	err error
}

// personaReviewTickMsg redraws the elapsed time on a running request.
type personaReviewTickMsg struct{ seq uint64 }

// personaAcceptedMsg reports whether the `ADDMODEL` for an accepted
// description failed. The command's synchronous reply events travel
// separately, in a [chatcmd.CommandResult], so this holds the error
// alone.
type personaAcceptedMsg struct {
	seq uint64
	err error
}

// routePersonaReview handles the invite-time persona review. The
// session sees none of it. What reaches the wire is an `ADDMODEL`
// naming the description the operator accepted, and a review abandoned
// before they accepted one sends nothing at all.
// `issuing` is the window the message was produced in, which for a
// proposal is the window `/add-model` was typed in. It is nil for a
// message the chat-screen raised itself.
func (s ChatScreen) routePersonaReview(issuing domain.Window, msg tea.Msg) (ChatScreen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case chatcmd.PersonaProposalRequested:
		next, cmd := s.startPersonaReview(issuing, msg)
		return next, cmd, true

	case personaCandidateMsg:
		next, cmd := s.handlePersonaCandidate(msg)
		return next, cmd, true

	case personaProposalFailedMsg:
		next, cmd := s.handlePersonaProposalFailed(msg)
		return next, cmd, true

	case personaReviewTickMsg:
		next, cmd := s.handlePersonaReviewTick(msg)
		return next, cmd, true

	case components.PersonaAcceptMsg:
		next, cmd := s.handlePersonaAccept(msg)
		return next, cmd, true

	case components.PersonaRerollMsg:
		next, cmd := s.handlePersonaReroll(msg)
		return next, cmd, true

	case components.PersonaCancelMsg:
		next, cmd := s.cancelPersonaReview(msg)
		return next, cmd, true

	case personaAcceptedMsg:
		next, cmd := s.handlePersonaAccepted(msg)
		return next, cmd, true
	}

	return s, nil, false
}

// startPersonaReview opens a review for the model the operator named.
//
// Two guards refuse the request. A second review is refused while one
// is open, with a notice naming the model whose review is open,
// because there is one selector to show them in. A proposal whose
// issuing window is no longer the active one is dropped: the operator
// left that window after typing the command, and the selector would
// open in the window they moved to.
func (s ChatScreen) startPersonaReview(
	issuing domain.Window,
	msg chatcmd.PersonaProposalRequested,
) (ChatScreen, tea.Cmd) {
	if s.personaReview != nil {
		return s, s.logReplyEvent(activeWindowIdentity(s), domain.SystemNotice{
			Target: msg.Channel,
			Text: fmt.Sprintf("A persona review for %s is still open. Finish it or press Esc first.",
				s.personaReview.model),
			At: time.Now(),
		})
	}

	if issuing == nil || issuing != activeWindowIdentity(s) {
		return s, nil
	}

	s.personaReviewSeq++
	s.personaReview = &personaReview{
		id: s.personaReviewSeq, window: issuing, model: msg.Model,
	}

	return s.requestPersona()
}

// requestPersona asks for one description and starts the tick that
// keeps the elapsed time the selector shows moving. The context is the
// review's own, so cancelling the review ends the request with it.
func (s ChatScreen) requestPersona() (ChatScreen, tea.Cmd) {
	review := s.personaReview

	if review.cancel != nil {
		review.cancel()
	}

	ctx, cancel := context.WithCancel(s.baseContext())
	review.cancel = cancel
	s.personaReviewSeq++
	review.seq = s.personaReviewSeq
	review.generating = true
	review.startedAt = time.Now()
	review.err = nil

	seq := review.seq
	rejected := append([]api.RejectedPersona(nil), review.rejected...)

	model := review.model

	propose := func() tea.Msg {
		// The model is checked before a description is asked for. A
		// model the catalogue does not have, or one without tool
		// calling, fails the `ADDMODEL` at the end of the review, and
		// the operator would have paid for a description and read it
		// before being told.
		if err := s.mgr.EnsureToolCapableModel(ctx, model); err != nil {
			return personaProposalFailedMsg{seq: seq, err: err}
		}

		text, err := s.mgr.ProposePersona(ctx, s.sess, rejected)
		if err != nil {
			return personaProposalFailedMsg{seq: seq, err: err}
		}

		return personaCandidateMsg{seq: seq, text: text}
	}

	return s, tea.Batch(propose, s.personaReviewTickCmd(seq), s.showPersonaSelector())
}

// personaReviewTickCmd schedules the next redraw of the elapsed time.
func (s ChatScreen) personaReviewTickCmd(seq uint64) tea.Cmd {
	return tea.Tick(personaReviewTick, func(time.Time) tea.Msg {
		return personaReviewTickMsg{seq: seq}
	})
}

// showPersonaSelector sends the selector the review as it now stands,
// and closes it when there is none. Each state is stamped with a
// revision one higher than the last, so the window can drop a state
// older than one it has already applied: this command is batched
// beside the one that makes the change, and a batch's commands run
// concurrently.
func (s *ChatScreen) showPersonaSelector() tea.Cmd {
	s.personaReviewRevision++
	revision := s.personaReviewRevision

	review := s.personaReview
	if review == nil {
		return msgCmd(components.PersonaSelectorMsg{Revision: revision})
	}

	var elapsed time.Duration
	if review.generating {
		elapsed = time.Since(review.startedAt)
	}

	return msgCmd(components.PersonaSelectorMsg{
		Revision: revision,
		State: &components.PersonaSelectorState{
			Review:     review.id,
			Model:      review.model,
			SmallModel: s.mgr.SmallModel(),
			Candidates: review.candidates,
			Generating: review.generating,
			Elapsed:    elapsed,
			Err:        review.err,
			Accepting:  review.accepting,
		},
	})
}

func (s ChatScreen) handlePersonaCandidate(msg personaCandidateMsg) (ChatScreen, tea.Cmd) {
	review := s.personaReview
	if review == nil || review.seq != msg.seq {
		return s, nil
	}

	review.generating = false
	review.candidates = append(review.candidates, msg.text)

	return s, s.showPersonaSelector()
}

func (s ChatScreen) handlePersonaProposalFailed(msg personaProposalFailedMsg) (ChatScreen, tea.Cmd) {
	review := s.personaReview
	if review == nil || review.seq != msg.seq {
		return s, nil
	}

	review.generating = false
	review.err = msg.err

	return s, s.showPersonaSelector()
}

func (s ChatScreen) handlePersonaReviewTick(msg personaReviewTickMsg) (ChatScreen, tea.Cmd) {
	review := s.personaReview
	if review == nil || review.seq != msg.seq || !review.generating {
		return s, nil
	}

	return s, tea.Batch(s.showPersonaSelector(), s.personaReviewTickCmd(msg.seq))
}

// handlePersonaReroll asks for another description, recording the one
// on screen as passed over. What the operator typed becomes the reason
// that description was turned down, and when they typed nothing the
// reason is a bare request for another. The nick loop sends a refused
// suggestion back with its reason the same way.
func (s ChatScreen) handlePersonaReroll(msg components.PersonaRerollMsg) (ChatScreen, tea.Cmd) {
	review := s.personaReview
	if review == nil || review.id != msg.Review || review.generating || review.accepting {
		return s, nil
	}

	if msg.Index >= 0 && msg.Index < len(review.candidates) {
		reason := "Write a different one."
		if msg.Adjustment != "" {
			reason = msg.Adjustment
		}

		review.rejected = append(review.rejected, api.RejectedPersona{
			Description: review.candidates[msg.Index],
			Reason:      reason,
		})
	}

	return s.requestPersona()
}

// handlePersonaAccept sends the highlighted description as the persona
// of an ordinary `ADDMODEL`.
func (s ChatScreen) handlePersonaAccept(msg components.PersonaAcceptMsg) (ChatScreen, tea.Cmd) {
	review := s.personaReview
	if review == nil || review.id != msg.Review || review.generating || review.accepting {
		return s, nil
	}

	if msg.Index < 0 || msg.Index >= len(review.candidates) {
		return s, nil
	}

	// `ADDMODEL` names the window the command is issued from, which is
	// the review's own for as long as it is open. Refusing here is what
	// stops a review that outlived its window adding the model to
	// whatever window replaced it.
	if activeWindowIdentity(s) != review.window {
		return s, nil
	}

	review.accepting = true
	review.err = nil

	return s, tea.Batch(s.sendAcceptedPersona(review.candidates[msg.Index]), s.showPersonaSelector())
}

// sendAcceptedPersona runs the command the operator would have typed
// had they written the description themselves.
//
// The command's error status goes to the review as a
// [personaAcceptedMsg]. Its reply, when it has one, is wrapped in a
// [chatcmd.CommandResult] naming the review's window and rendered
// whether or not the review is still open: in that window, or wherever
// the chat-screen routes a reply for one that has since closed. The
// replies are a refusal and the notices `ADDMODEL` preparation
// reports. A successful `ADDMODEL` produces neither, so nothing is
// wrapped; its JOIN arrives over the protocol bus, as it does for an
// `/add-model` that named its persona outright.
func (s ChatScreen) sendAcceptedPersona(persona string) tea.Cmd {
	review := s.personaReview
	seq := review.seq
	window := activeWindowIdentity(s)
	revision := activeWindowRevision(s)

	rc := s.runContext()
	run := chatcmd.AddModelCommand{
		Model:   string(review.model),
		Persona: []string{persona},
	}.Run(s.baseContext(), rc)

	if run == nil {
		return nil
	}

	return func() tea.Msg {
		reply := run()

		outcome := personaAcceptedMsg{seq: seq}

		switch reply := reply.(type) {
		case chatcmd.CommandErrorResult:
			outcome.err = reply.Error.Err

		case chatcmd.ReplyEvents:
			if reply.Error != nil {
				outcome.err = reply.Error.Err
			}
		}

		if reply == nil {
			return outcome
		}

		return tea.BatchMsg{
			msgCmd(chatcmd.CommandResult{
				IssuingWindow:         window,
				IssuingWindowRevision: revision,
				Message:               reply,
			}),
			msgCmd(outcome),
		}
	}
}

// handlePersonaAccepted closes the review when `ADDMODEL` took the
// description, and returns the operator to it when it did not. Any
// failure the command comes back with, whatever produced it, leaves
// each description they have seen selectable, with the reason on the
// status line. Enter sends the same one again, and preparation runs
// afresh: a nick another client had taken is chosen again from the
// nicks free at that point.
func (s ChatScreen) handlePersonaAccepted(msg personaAcceptedMsg) (ChatScreen, tea.Cmd) {
	review := s.personaReview
	if review == nil || review.seq != msg.seq {
		return s, nil
	}

	if msg.err != nil {
		review.accepting = false
		review.err = msg.err

		return s, s.showPersonaSelector()
	}

	return s.closePersonaReview()
}

// cancelPersonaReview ends the review the operator abandoned. Nothing
// has been sent unless they had already accepted a description: an
// `ADDMODEL` in flight runs under the application context and is not
// cancelled here, so it goes on to succeed or fail on its own. A
// refusal still reaches the window; a success announces itself with the
// JOIN that follows.
func (s ChatScreen) cancelPersonaReview(msg components.PersonaCancelMsg) (ChatScreen, tea.Cmd) {
	if s.personaReview == nil || s.personaReview.id != msg.Review {
		return s, nil
	}

	return s.closePersonaReview()
}

// closePersonaReview cancels any generation request still running,
// forgets the review and takes the selector off the window.
func (s ChatScreen) closePersonaReview() (ChatScreen, tea.Cmd) {
	if s.personaReview == nil {
		return s, nil
	}

	if s.personaReview.cancel != nil {
		s.personaReview.cancel()
	}

	s.personaReview = nil

	return s, s.showPersonaSelector()
}
