package modelmanager

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

// TestReflectionAPIInput_carries_no_internal_identifier pins that a
// reflection request names other clients and active amendments only by
// per-run token. An instance id reaching the provider would name a client
// the reflecting model could then address on the wire, and a persona row
// id is stable across runs.
func TestReflectionAPIInput_carries_no_internal_identifier(t *testing.T) {
	at := time.Date(2026, 8, 27, 14, 0, 0, 0, time.UTC)
	alice := domain.InstanceID("inst-alice")
	snapshot := store.PendingReflectionSnapshot{
		Persona: store.PersonaSnapshot{
			Lineage: domain.PersonaLineage{
				InstanceID: "inst-botty", Baseline: "careful and curious",
				CurrentRevisionID: 7,
			},
			Revision: domain.PersonaRevision{ID: 7, InstanceID: "inst-botty"},
			Experiences: []domain.Experience{{
				ID: 21, InstanceID: "inst-botty",
				Kind: domain.ExperienceRelationship, Summary: "Alice reproduced it.",
				SubjectID: &alice, Confidence: domain.ConfidenceHigh,
				OccurredAt: at, CreatedAt: at,
				Sources: []domain.ReflectionEventRef{{Sequence: 4}},
			}},
			Amendments: []domain.PersonaAmendment{{
				ID: 33, InstanceID: "inst-botty",
				Scope: domain.AmendmentRelationship, Counterpart: &alice,
				Tendency:   "Usually trusts Alice's corrections.",
				Confidence: domain.ConfidenceMedium,
				Evidence:   []domain.ExperienceID{21}, CreatedAt: at,
			}},
		},
		Events: []store.ReflectionEvent{{
			Sequence: 5, InstanceID: "inst-botty",
			Source: protocol.DirectHistoryRef(52, "inst-alice"),
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.ClientSource(alice, "alice"),
				Target: "botty", Body: "the reproduction is in the issue", At: at,
			},
			Substantive: true, CreatedAt: at,
		}},
	}

	input, aliases := reflectionAPIInput(snapshot)
	encoded, err := json.Marshal(input)
	require.NoError(t, err)

	resolved, known := aliases.instanceID("p1")
	require.Equal(t, aliasResolution{InstanceID: alice, Known: true},
		aliasResolution{InstanceID: resolved, Known: known})
	amendment, amendmentKnown := aliases.amendmentID("a1")
	require.Equal(t, aliasAmendmentResolution{ID: 33, Known: true},
		aliasAmendmentResolution{ID: amendment, Known: amendmentKnown})
	for _, leaked := range []string{"inst-alice", "inst-botty", "\"id\":33", "\"id\":21"} {
		require.NotContains(t, string(encoded), leaked)
	}
}

type aliasResolution struct {
	InstanceID domain.InstanceID
	Known      bool
}

type aliasAmendmentResolution struct {
	ID    domain.PersonaAmendmentID
	Known bool
}

func TestReflectionAPIInput_names_a_direct_window_by_token(t *testing.T) {
	at := time.Date(2026, 8, 27, 14, 30, 0, 0, time.UTC)
	snapshot := store.PendingReflectionSnapshot{
		Persona: store.PersonaSnapshot{
			Lineage:  domain.PersonaLineage{InstanceID: "inst-botty"},
			Revision: domain.PersonaRevision{ID: 1, InstanceID: "inst-botty"},
		},
		Events: []store.ReflectionEvent{{
			Sequence: 1, InstanceID: "inst-botty",
			Source: protocol.DirectHistoryRef(52, "inst-alice"),
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.ClientSource("inst-alice", "alice"),
				Target: "botty", Body: "hello", At: at,
			},
			Substantive: true, CreatedAt: at,
		}},
	}

	input, _ := reflectionAPIInput(snapshot)

	require.Equal(t, "p1", input.Events[0].Window)
	require.True(t, strings.HasPrefix(input.Events[0].Participant, "p"))
}
