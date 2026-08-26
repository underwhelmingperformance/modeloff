package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

type contextSummaryRowCounts struct {
	SummaryRows int
	SourceRows  int
}

type contextSummariesByWindow struct {
	Dev   []ContextSummary
	Other []ContextSummary
}

func TestSQLiteStore_context_summary_replacement_reuses_exact_sources(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := t.Context()
	window := protocol.ChannelWindowTarget("#dev")
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	firstSources := []protocol.IRCMessage{
		{
			Kind: protocol.KindJoin, Source: domain.LegacyClientSource("alice"),
			Target: "#dev", At: at,
		},
		{
			Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("alice"),
			Target: "#dev", Body: "we chose sqlite", At: at.Add(time.Second),
		},
	}
	first, err := s.CommitContextSummary(ctx, ContextSummaryUpdate{
		InstanceID: "inst-botty", Window: window,
		Summary: "alice joined and the channel chose sqlite",
		Sources: firstSources, CreatedAt: at.Add(2 * time.Second),
	})
	require.NoError(t, err)

	newSource := protocol.IRCMessage{
		Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("bob"),
		Target: "#dev", Body: "the migration is version 12", At: at.Add(3 * time.Second),
	}
	replacement, err := s.CommitContextSummary(ctx, ContextSummaryUpdate{
		InstanceID: "inst-botty", Window: window,
		Summary: "the channel chose sqlite and is preparing migration 12",
		Sources: []protocol.IRCMessage{newSource}, Supersedes: []ContextSummaryID{first.ID},
		CreatedAt: at.Add(4 * time.Second),
	})
	require.NoError(t, err)

	got, err := s.ContextSummaries(ctx, "inst-botty", window)
	require.NoError(t, err)
	require.Equal(t, []ContextSummary{replacement}, got)
	require.Equal(t, ContextSummary{
		ID: replacement.ID, InstanceID: "inst-botty", Window: window,
		Summary:   "the channel chose sqlite and is preparing migration 12",
		Sources:   append(firstSources, newSource),
		CreatedAt: at.Add(4 * time.Second),
	}, replacement)

	var state contextSummaryRowCounts
	require.NoError(t, s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM context_summaries),
			(SELECT count(*) FROM context_summary_sources)
	`).Scan(&state.SummaryRows, &state.SourceRows))
	require.Equal(t, contextSummaryRowCounts{SummaryRows: 1, SourceRows: 3}, state)
}

func TestSQLiteStore_delete_context_summaries_for_window_is_scoped(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := t.Context()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	dev := protocol.ChannelWindowTarget("#dev")
	other := protocol.ChannelWindowTarget("#other")

	for _, window := range []protocol.WindowTarget{dev, other} {
		_, err := s.CommitContextSummary(ctx, ContextSummaryUpdate{
			InstanceID: "inst-botty", Window: window, Summary: "summary",
			Sources:   []protocol.IRCMessage{{Kind: protocol.KindPrivMsg, Body: string(protocol.WindowKey(window))}},
			CreatedAt: at,
		})
		require.NoError(t, err)
	}

	require.NoError(t, s.DeleteContextSummariesForWindow(ctx, "inst-botty", dev))

	devSummaries, err := s.ContextSummaries(ctx, "inst-botty", dev)
	require.NoError(t, err)
	otherSummaries, err := s.ContextSummaries(ctx, "inst-botty", other)
	require.NoError(t, err)
	require.Equal(t, contextSummariesByWindow{
		Dev:   []ContextSummary{},
		Other: otherSummaries,
	}, contextSummariesByWindow{
		Dev: devSummaries, Other: otherSummaries,
	})
}

// TestSQLiteStore_CommitContextSummary_bounds_retained_sources pins the
// per-window bound on projected source rows. Compaction supersedes every
// active segment, so the surviving summary's source range spans every
// message ever compacted for that window and ContextSummaries decodes
// the whole range on each turn.
func TestSQLiteStore_CommitContextSummary_bounds_retained_sources(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := t.Context()
	window := protocol.ChannelWindowTarget("#dev")
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	source := func(index int) protocol.IRCMessage {
		return protocol.IRCMessage{
			Kind: protocol.KindPrivMsg, Source: domain.LegacyClientSource("alice"),
			Target: "#dev", Body: fmt.Sprintf("line %d", index),
			At: at.Add(time.Duration(index) * time.Second),
		}
	}
	sourceRange := func(from, to int) []protocol.IRCMessage {
		messages := make([]protocol.IRCMessage, 0, to-from)
		for index := from; index < to; index++ {
			messages = append(messages, source(index))
		}

		return messages
	}

	const overflow = 40

	first, err := s.CommitContextSummary(ctx, ContextSummaryUpdate{
		InstanceID: "inst-botty", Window: window, Summary: "the first stretch",
		Sources: sourceRange(0, contextSummarySourceRetention), CreatedAt: at,
	})
	require.NoError(t, err)

	replacement, err := s.CommitContextSummary(ctx, ContextSummaryUpdate{
		InstanceID: "inst-botty", Window: window, Summary: "both stretches",
		Sources: sourceRange(
			contextSummarySourceRetention, contextSummarySourceRetention+overflow,
		),
		Supersedes: []ContextSummaryID{first.ID}, CreatedAt: at.Add(time.Hour),
	})
	require.NoError(t, err)

	var counts contextSummaryRowCounts
	require.NoError(t, s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM context_summaries),
			(SELECT count(*) FROM context_summary_sources)
	`).Scan(&counts.SummaryRows, &counts.SourceRows))
	require.Equal(t, contextSummaryRowCounts{
		SummaryRows: 1, SourceRows: contextSummarySourceRetention,
	}, counts)

	require.Equal(t, ContextSummary{
		ID: replacement.ID, InstanceID: "inst-botty", Window: window,
		Summary:   "both stretches",
		Sources:   sourceRange(overflow, contextSummarySourceRetention+overflow),
		CreatedAt: at.Add(time.Hour),
	}, replacement)
}
