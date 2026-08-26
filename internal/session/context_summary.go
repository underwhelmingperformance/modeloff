package session

import (
	"context"

	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// ContextSummaries returns the summaries for the actor and window
// represented by guard.
func (s *Session) ContextSummaries(
	ctx context.Context,
	guard protocol.WindowGuard,
) ([]store.ContextSummary, error) {
	issued, err := issuedGuard(guard, "read context summaries")
	if err != nil {
		return nil, err
	}

	summaries, err := issued.contextSummaries(ctx)
	if err != nil {
		return nil, err
	}
	if summaries == nil {
		summaries = []store.ContextSummary{}
	}

	return summaries, nil
}

// CommitContextSummary replaces summary segments while guard still
// grants the actor access to the same window.
func (s *Session) CommitContextSummary(
	ctx context.Context,
	guard protocol.WindowGuard,
	update store.ContextSummaryUpdate,
) (store.ContextSummary, error) {
	issued, err := issuedGuard(guard, "commit context summary")
	if err != nil {
		return store.ContextSummary{}, err
	}

	return issued.commitContextSummary(ctx, update)
}

func (guard windowGuard) contextSummaries(ctx context.Context) ([]store.ContextSummary, error) {
	s := guard.client.sess

	var summaries []store.ContextSummary
	err := guard.RunWithAuthority(ctx, func() error {
		var err error
		summaries, err = s.store.ContextSummaries(
			ctx,
			guard.client.instance.ID(),
			canonicalWindowGuardTarget(guard),
		)

		return err
	})
	if err != nil {
		return nil, err
	}

	return summaries, nil
}

func (guard windowGuard) commitContextSummary(
	ctx context.Context,
	update store.ContextSummaryUpdate,
) (store.ContextSummary, error) {
	s := guard.client.sess

	canonical := canonicalWindowGuardTarget(guard)
	if update.InstanceID != guard.client.instance.ID() ||
		!protocol.EqualWindowTarget(update.Window, canonical) {
		return store.ContextSummary{}, protocol.ErrWindowAuthorityChanged
	}
	update.Window = canonical

	var committed store.ContextSummary
	err := guard.RunWithAuthority(ctx, func() error {
		var err error
		committed, err = s.store.CommitContextSummary(ctx, update)

		return err
	})
	if err != nil {
		return store.ContextSummary{}, err
	}

	return committed, nil
}

// An invitation turn runs before the JOIN, so the actor has no
// earlier context in the channel and nothing it writes there would
// belong to a membership it does not yet hold.
func (invitationGuard) contextSummaries(context.Context) ([]store.ContextSummary, error) {
	return []store.ContextSummary{}, nil
}

func (invitationGuard) commitContextSummary(
	context.Context,
	store.ContextSummaryUpdate,
) (store.ContextSummary, error) {
	return store.ContextSummary{}, protocol.ErrWindowAuthorityChanged
}
