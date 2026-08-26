package modelclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/observability"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

func (mc *ModelClient) readContextSummaries(
	ctx context.Context,
	guard protocol.WindowGuard,
) ([]store.ContextSummary, error) {
	if mc.contexts == nil {
		return nil, nil
	}

	summaries, err := mc.contexts.ContextSummaries(ctx, guard)
	if errors.Is(err, protocol.ErrWindowAuthorityChanged) {
		return nil, errDispatchWindowClosed
	}
	if err != nil {
		return nil, observability.ErrWithKind(
			fmt.Errorf("read context summaries: %w", err),
			observability.ErrorKindStore,
		)
	}

	return summaries, nil
}

func contextSummaryMessages(
	window protocol.WindowContext,
	summaries []store.ContextSummary,
) []protocol.IRCMessage {
	messages := make([]protocol.IRCMessage, 0, len(summaries))
	for _, summary := range summaries {
		messages = append(messages, protocol.IRCMessage{
			Kind:   protocol.KindServerReply,
			Source: domain.ServerSource("modeloff"),
			Target: string(protocol.WindowKey(window.Target())),
			Body:   "summary of earlier context: " + summary.Summary,
			At:     summary.CreatedAt,
		})
	}

	return messages
}

// summarizedPrefix reports how much of the transcript and of the current
// burst the committed summaries already cover.
//
// Compaction reaches into the burst once the transcript alone is not
// enough, and a retry reloads those summaries and plans again from the
// same transcript and burst. Matching the transcript alone would leave
// the events a summary covers in the request beside the summary that
// replaced them, and the next compaction would summarise and commit them
// a second time.
func summarizedPrefix(
	recent, events []protocol.IRCMessage,
	summaries []store.ContextSummary,
) contextPrefix {
	covered := summarizedRecentPrefix(slices.Concat(recent, events), summaries)

	return splitContextPrefix(covered, len(recent))
}

func summarizedRecentPrefix(
	recent []protocol.IRCMessage,
	summaries []store.ContextSummary,
) int {
	if len(recent) == 0 || len(summaries) == 0 {
		return 0
	}

	var sources []protocol.IRCMessage
	for _, summary := range summaries {
		sources = append(sources, summary.Sources...)
	}
	limit := min(len(sources), len(recent))
	for overlap := limit; overlap > 0; overlap-- {
		if slices.EqualFunc(
			sources[len(sources)-overlap:], recent[:overlap],
			equalContextSource,
		) {
			return overlap
		}
	}

	return 0
}

func equalContextSource(left, right protocol.IRCMessage) bool {
	if !left.At.Equal(right.At) {
		return false
	}

	left.At = time.Time{}
	right.At = time.Time{}

	return left == right
}
