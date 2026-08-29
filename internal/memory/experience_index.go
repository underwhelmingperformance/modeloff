package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	chromem "github.com/philippgille/chromem-go"

	"github.com/laney/modeloff/internal/domain"
)

// ExperienceIndex is a semantic index over one instance's own
// experiences, so a reflection can ask when else something like this
// happened.
//
// The recall tools a reflection holds navigate structurally, by window
// and by the events a belief already cites, so an instance can follow
// only links it already holds. That finds what it went looking for and
// never what it did not know to look for, which is the question a
// pattern is hiding behind. It is also what lets salience reach past the
// recent: without it the experiences an instance re-cites are the ones it
// can already navigate to, and the ranking acquires the recency bias the
// decay term already supplies.
//
// Experiences are indexed and raw events are not. An experience is
// summarised prose written for the purpose and arrives a handful per
// accepted run, so the collection is the same order of size as an
// instance's memories. Embedding the event stream would cost one call
// per substantive delivery, which is the whole of the chat volume.
type ExperienceIndex struct {
	db            *chromem.DB
	embeddingFunc chromem.EmbeddingFunc
	searchable    func() bool
}

// experienceCollection names one instance's experience collection. It is
// kept apart from the memory collection under the bare instance id,
// because a memory and an experience are different levels and the memory
// index reconciles its own collection against the memory table, which
// would take every experience with it as an orphan.
func experienceCollection(id domain.InstanceID) string {
	return experienceCollectionPrefix + string(id)
}

// experienceCollectionPrefix separates the experience namespace from the
// memory one, whose collections are named by the bare instance id.
const experienceCollectionPrefix = "experiences:"

// memoryCollectionName reports whether a chromem collection name belongs
// to the memory index, which is every name outside another namespace.
func memoryCollectionName(name string) bool {
	return !strings.HasPrefix(name, experienceCollectionPrefix)
}

// Searchable reports whether the embedding endpoint behind this index was
// reachable at its last probe. A caller offers the tool only when it is,
// and falls back to the structural recall.
func (x *ExperienceIndex) Searchable() bool {
	if x == nil || x.searchable == nil {
		return false
	}

	return x.searchable()
}

// Index records the given experiences for later recall. It is
// best-effort: the store is the source of truth, and an experience the
// index never received is reachable through every other path a
// reflection has.
func (x *ExperienceIndex) Index(
	ctx context.Context,
	id domain.InstanceID,
	experiences []domain.Experience,
) error {
	if len(experiences) == 0 {
		return nil
	}

	collection, err := x.db.GetOrCreateCollection(
		experienceCollection(id), nil, x.embeddingFunc,
	)
	if err != nil {
		return fmt.Errorf("open experience index: %w", err)
	}

	documents := make([]chromem.Document, 0, len(experiences))
	for _, experience := range experiences {
		documents = append(documents, chromem.Document{
			ID:      experienceDocumentID(experience.ID),
			Content: experience.Summary,
			Metadata: map[string]string{
				"kind": string(experience.Kind),
			},
		})
	}
	if err := collection.AddDocuments(ctx, documents, 1); err != nil {
		return fmt.Errorf("index experiences: %w", err)
	}

	return nil
}

// Reconcile makes the instance's collection hold exactly `retained`.
//
// The index is derived state and the store is the source of truth, and
// the two drift in both directions. Retention removes experiences from
// the store and leaves their documents here, where they take places in
// a result the store then drops, so a query whose nearest matches are
// all stale answers with nothing while live experiences rank below them.
// A reset of the vector database, which a changed embedding model
// performs, drops documents the store still holds, and only a newly
// accepted experience is ever indexed.
//
// The healthy path costs one count and one lookup per retained
// experience, and no embedding call. Drift in either direction rebuilds
// the collection from what the caller supplies.
func (x *ExperienceIndex) Reconcile(
	ctx context.Context,
	id domain.InstanceID,
	retained []domain.Experience,
) error {
	collection := x.db.GetCollection(experienceCollection(id), x.embeddingFunc)
	if collection != nil && collection.Count() == len(retained) && x.holdsAll(ctx, collection, retained) {
		return nil
	}

	if err := x.DeleteInstance(ctx, id); err != nil {
		return err
	}

	return x.Index(ctx, id, retained)
}

// holdsAll reports whether the collection has a document for every
// retained experience. Counting alone would miss a collection holding as
// many stale documents as the store has live ones.
func (x *ExperienceIndex) holdsAll(
	ctx context.Context,
	collection *chromem.Collection,
	retained []domain.Experience,
) bool {
	for _, experience := range retained {
		if _, err := collection.GetByID(ctx, experienceDocumentID(experience.ID)); err != nil {
			return false
		}
	}

	return true
}

// experienceDocumentID is the document id one experience is stored under.
func experienceDocumentID(id domain.ExperienceID) string {
	return strconv.FormatInt(int64(id), 10)
}

// Search returns the ids of the instance's experiences most like query,
// most alike first. The caller reads the experiences themselves from the
// store, which is what keeps the index derived: an id the store no longer
// answers to simply drops out of the result.
func (x *ExperienceIndex) Search(
	ctx context.Context,
	id domain.InstanceID,
	query string,
	limit int,
) ([]domain.ExperienceID, error) {
	if limit <= 0 {
		return nil, nil
	}

	collection := x.db.GetCollection(experienceCollection(id), x.embeddingFunc)
	if collection == nil || collection.Count() == 0 {
		return nil, nil
	}

	results, err := collection.Query(ctx, query, min(limit, collection.Count()), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("search experiences: %w", err)
	}

	ids := make([]domain.ExperienceID, 0, len(results))
	for _, result := range results {
		parsed, err := strconv.ParseInt(result.ID, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, domain.ExperienceID(parsed))
	}

	return ids, nil
}

// DeleteInstance removes one instance's experience collection. It is a
// no-op if the instance never had one.
func (x *ExperienceIndex) DeleteInstance(_ context.Context, id domain.InstanceID) error {
	if err := x.db.DeleteCollection(experienceCollection(id)); err != nil {
		return fmt.Errorf("delete experience index: %w", err)
	}

	return nil
}

// Experiences returns the experience index backed by this store's own
// vector database and embedding function, so both levels are searched
// through one endpoint and one directory.
func (s *IndexedStore) Experiences() *ExperienceIndex {
	return &ExperienceIndex{
		db: s.db, embeddingFunc: s.embeddingFunc, searchable: s.Searchable,
	}
}

// ExperienceSearcher is the optional capability a memory [Store]
// implements when it can also index an instance's experiences. A store
// without it leaves a reflection with its structural recall alone.
type ExperienceSearcher interface {
	Experiences() *ExperienceIndex
}
