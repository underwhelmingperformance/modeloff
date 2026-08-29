package memory

import (
	"testing"
	"time"

	chromem "github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// experienceRecallEffect is what a similarity recall returned, and
// whether the index would answer at all.
type experienceRecallEffect struct {
	Searchable bool
	IDs        []domain.ExperienceID
}

// newTestExperienceIndex builds an index over an in-memory chromem
// database, backed by the same store type production uses.
func newTestExperienceIndex(t *testing.T, embedder chromem.EmbeddingFunc) *ExperienceIndex {
	t.Helper()

	backing := NewStoreAdapter(nil)
	store := NewIndexedStoreFromDB(t.Context(), backing, chromem.NewDB(), embedder)

	return store.Experiences()
}

// TestExperienceIndex_finds_an_old_experience_by_resemblance pins the
// property the index exists for: an episode is reachable because it is
// like what is happening now, and not because it happened recently.
//
// Without it a reflection navigates only by window and by the events a
// belief already cites, so it can confirm what it went looking for and
// never find what it did not know to look for. That is also what would
// leave salience re-ranking only the recent, since an instance can only
// return to what it can reach.
func TestExperienceIndex_finds_an_old_experience_by_resemblance(t *testing.T) {
	ctx := t.Context()
	index := newTestExperienceIndex(t, fakeEmbedder(3, map[string]int{
		"reproduction": 0,
		"deployment":   1,
	}))
	const actor = domain.InstanceID("inst-botty")
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	require.NoError(t, index.Index(ctx, actor, []domain.Experience{
		{
			ID: 1, InstanceID: actor, Kind: domain.ExperienceObservation,
			Summary: "Alice asked for a reproduction before agreeing.", OccurredAt: at,
		},
		{
			ID: 2, InstanceID: actor, Kind: domain.ExperienceObservation,
			Summary:    "The deployment went out without a rollback plan.",
			OccurredAt: at.Add(90 * 24 * time.Hour),
		},
	}))

	ids, err := index.Search(ctx, actor, "somebody wanting a reproduction", 5)
	require.NoError(t, err)

	require.Equal(t, experienceRecallEffect{
		Searchable: true, IDs: []domain.ExperienceID{1},
	}, experienceRecallEffect{
		Searchable: index.Searchable(), IDs: ids[:1],
	})
}

// TestExperienceIndex_answers_nothing_for_an_instance_it_never_saw pins
// that a search costs nothing and fails nothing before anything is
// indexed, which is every instance's state until its first accepted run.
func TestExperienceIndex_answers_nothing_for_an_instance_it_never_saw(t *testing.T) {
	index := newTestExperienceIndex(t, trivialEmbedder())

	ids, err := index.Search(t.Context(), "inst-nobody", "anything at all", 5)

	require.NoError(t, err)
	require.Equal(t, experienceRecallEffect{Searchable: true}, experienceRecallEffect{
		Searchable: index.Searchable(), IDs: ids,
	})
}

// TestExperienceIndex_is_unsearchable_without_a_working_endpoint pins
// what a caller reads before offering the tool. The index is derived
// state over an embedding endpoint, and a reflection whose endpoint is
// unreachable keeps its structural recall rather than being offered a
// tool that answers nothing.
func TestExperienceIndex_is_unsearchable_without_a_working_endpoint(t *testing.T) {
	index := newTestExperienceIndex(t, failingEmbedder())

	require.False(t, index.Searchable())
}

// experienceLifetimeEffect is what a reopened index and a deleted
// instance leave behind.
type experienceLifetimeEffect struct {
	AfterReopen []domain.ExperienceID
	AfterDelete []domain.ExperienceID
}

// TestExperienceIndex_survives_a_reopen_and_goes_with_its_instance pins
// the two ends of an experience collection's life.
//
// Opening the store reconciles the memory index against the memories
// table, and an experience collection holds no memories. Read as an
// instance id it would answer with none, and every experience an
// instance had accepted would be dropped on the next start, with nothing
// to rebuild it from: only a newly accepted experience is ever indexed.
//
// Deleting the instance is the other end. Nothing else reads that
// collection, so without this it would outlive every row the instance
// had.
func TestExperienceIndex_survives_a_reopen_and_goes_with_its_instance(t *testing.T) {
	ctx := t.Context()
	const actor = domain.InstanceID("inst-botty")
	embedder := fakeEmbedder(3, map[string]int{"reproduction": 0, "deployment": 1})

	backing := NewStoreAdapter(nil)
	db := chromem.NewDB()
	store := NewIndexedStoreFromDB(ctx, backing, db, embedder)
	require.NoError(t, store.Experiences().Index(ctx, actor, []domain.Experience{{
		ID: 1, InstanceID: actor, Kind: domain.ExperienceObservation,
		Summary: "Alice asked for a reproduction before agreeing.",
	}}))

	// The same database opened again, which is what a restart does.
	reopened := NewIndexedStoreFromDB(ctx, backing, db, embedder)
	afterReopen, err := reopened.Experiences().Search(ctx, actor, "a reproduction", 5)
	require.NoError(t, err)

	require.NoError(t, reopened.DeleteInstance(ctx, actor))
	afterDelete, err := reopened.Experiences().Search(ctx, actor, "a reproduction", 5)
	require.NoError(t, err)

	require.Equal(t, experienceLifetimeEffect{
		AfterReopen: []domain.ExperienceID{1},
	}, experienceLifetimeEffect{
		AfterReopen: afterReopen, AfterDelete: afterDelete,
	})
}

// reconcileCase is one way the index and the store can drift apart.
type reconcileCase struct {
	Name string
	// Indexed is what the collection holds before reconciliation, and
	// Retained what the store still answers for. Query is chosen so the
	// drifted document would win the one place asked for.
	Indexed  []domain.ExperienceID
	Retained []domain.ExperienceID
	Query    string
}

// reconcileEffect is what a search for the second experience returns
// once the two are back in step.
type reconcileEffect struct {
	Found []domain.ExperienceID
}

// TestExperienceIndex_Reconcile_brings_the_collection_back_into_step
// covers both directions the index drifts.
//
// Retention removes an experience from the store and leaves its
// document, which then takes a place in a result the store drops, so a
// query whose nearest matches are all stale answers with nothing.
// Resetting the vector database, which changing the embedding model
// does, drops documents the store still holds, and only a newly accepted
// experience is ever indexed.
func TestExperienceIndex_Reconcile_brings_the_collection_back_into_step(t *testing.T) {
	const actor = domain.InstanceID("inst-botty")
	summaries := map[domain.ExperienceID]string{
		1: "Alice asked for a reproduction before agreeing.",
		2: "The deployment went out without a rollback plan.",
	}
	experiences := func(ids []domain.ExperienceID) []domain.Experience {
		built := make([]domain.Experience, 0, len(ids))
		for _, id := range ids {
			built = append(built, domain.Experience{
				ID: id, InstanceID: actor,
				Kind: domain.ExperienceObservation, Summary: summaries[id],
			})
		}

		return built
	}

	cases := []reconcileCase{
		{
			Name:     "the store dropped one the index kept",
			Indexed:  []domain.ExperienceID{1, 2},
			Retained: []domain.ExperienceID{2},
			Query:    "a reproduction",
		},
		{
			Name:     "the index lost one the store kept",
			Indexed:  nil,
			Retained: []domain.ExperienceID{2},
			Query:    "a deployment",
		},
		{
			// The counts match, so only the presence check separates
			// these two sets.
			Name:     "the counts agree and the ids do not",
			Indexed:  []domain.ExperienceID{1},
			Retained: []domain.ExperienceID{2},
			Query:    "a reproduction",
		},
		{
			Name:     "the two already agree",
			Indexed:  []domain.ExperienceID{2},
			Retained: []domain.ExperienceID{2},
			Query:    "a deployment",
		},
	}
	want := []reconcileEffect{
		{Found: []domain.ExperienceID{2}},
		{Found: []domain.ExperienceID{2}},
		{Found: []domain.ExperienceID{2}},
		{Found: []domain.ExperienceID{2}},
	}

	got := make([]reconcileEffect, 0, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := t.Context()
			index := newTestExperienceIndex(t, fakeEmbedder(3, map[string]int{
				"reproduction": 0,
				"deployment":   1,
			}))
			require.NoError(t, index.Index(ctx, actor, experiences(testCase.Indexed)))

			require.NoError(t, index.Reconcile(ctx, actor, experiences(testCase.Retained)))

			found, err := index.Search(ctx, actor, testCase.Query, 1)
			require.NoError(t, err)
			got = append(got, reconcileEffect{Found: found})
		})
	}

	require.Equal(t, want, got)
}
