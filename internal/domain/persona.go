package domain

import "time"

// PersonaRevisionID identifies one revision of an instance's evolving
// personality state.
type PersonaRevisionID int64

// ExperienceID identifies one reflection-derived experience.
type ExperienceID int64

// PersonaAmendmentID identifies one reflection-derived amendment.
type PersonaAmendmentID int64

// ReflectionSequence identifies one event in an instance's private
// reflection stream.
type ReflectionSequence int64

// ReflectionRunID identifies one reflection attempt across retries.
type ReflectionRunID string

// Confidence records the reflection model's bounded confidence in an
// experience or amendment.
type Confidence string

const (
	// ConfidenceLow records weak or ambiguous evidence.
	ConfidenceLow Confidence = "low"
	// ConfidenceMedium records evidence that supports more than one reading.
	ConfidenceMedium Confidence = "medium"
	// ConfidenceHigh records direct or repeatedly corroborated evidence.
	ConfidenceHigh Confidence = "high"
)

// ExperienceKind distinguishes observation from interpretation and
// participant assertion.
type ExperienceKind string

const (
	// ExperienceObservation records an event the instance directly observed.
	ExperienceObservation ExperienceKind = "observation"
	// ExperienceInterpretation records the instance's reading of events.
	ExperienceInterpretation ExperienceKind = "interpretation"
	// ExperienceAssertion records a claim made by a participant.
	ExperienceAssertion ExperienceKind = "assertion"
	// ExperienceRelationship records an experience scoped to one counterpart.
	ExperienceRelationship ExperienceKind = "relationship"
)

// AmendmentScope determines whether a tendency applies globally or to one
// counterpart.
type AmendmentScope string

const (
	// AmendmentGlobal applies a tendency across conversations.
	AmendmentGlobal AmendmentScope = "global"
	// AmendmentRelationship applies a tendency to one counterpart.
	AmendmentRelationship AmendmentScope = "relationship"
)

// ReflectionOutcome records the durable result of one reflection attempt.
type ReflectionOutcome string

const (
	// ReflectionNoChange records a successful reflection with no new state.
	ReflectionNoChange ReflectionOutcome = "no_change"
	// ReflectionAccepted records a committed reflection proposal.
	ReflectionAccepted ReflectionOutcome = "accepted"
	// ReflectionRejected records a proposal refused by validation.
	ReflectionRejected ReflectionOutcome = "rejected"
	// ReflectionStale records a proposal whose base state changed before commit.
	ReflectionStale ReflectionOutcome = "stale"
	// ReflectionFailed records a reflection that did not produce a proposal.
	ReflectionFailed ReflectionOutcome = "failed"
	// ReflectionShadow records a validated proposal without changing persona
	// state.
	ReflectionShadow ReflectionOutcome = "shadow"
	// ReflectionDiscarded records a validated proposal the server did not
	// apply because reflection stopped being active before it committed.
	ReflectionDiscarded ReflectionOutcome = "discarded"
)

// PersonaTransitionKind describes why the active revision pointer changed.
type PersonaTransitionKind string

const (
	// PersonaTransitionReflection activates a revision created by reflection.
	PersonaTransitionReflection PersonaTransitionKind = "reflection"
	// PersonaTransitionReset selects revision zero.
	PersonaTransitionReset PersonaTransitionKind = "reset"
	// PersonaTransitionRollback selects an earlier revision.
	PersonaTransitionRollback PersonaTransitionKind = "rollback"
	// PersonaTransitionOperator activates a revision an operator wrote by
	// giving the instance a replacement description.
	PersonaTransitionOperator PersonaTransitionKind = "operator"
)

// PersonaLineage identifies the immutable baseline and the active revision for
// one model instance. Baseline is revision zero's description, which is what a
// reset returns to; the persona in force is the active revision's.
type PersonaLineage struct {
	InstanceID        InstanceID
	Baseline          string
	CurrentRevisionID PersonaRevisionID
	Checkpoint        ReflectionSequence
	CreatedAt         time.Time
	ReflectedAt       *time.Time
}

// PersonaCounts summarises how much accepted reflection state an
// instance's active persona revision rests on. It is what `/whois`
// reports, so an operator can see whether reflection has moved an
// instance without reading the whole revision through `/persona`.
//
// An instance with no persona lineage has the zero value, and so does
// the user's connection record.
type PersonaCounts struct {
	Revision    PersonaRevisionID `json:"revision,omitzero"`
	Experiences int               `json:"experiences,omitzero"`
	Tendencies  int               `json:"tendencies,omitzero"`
}

// PersonaRevision is one point in an instance's persona history. No
// reflection rewrites one; what can change is which experiences a
// revision still names, because the storage backstop unlinks an
// experience it removes from every revision that held it. Description is
// the persona in force while this revision is active. Revision zero has
// no parent, experiences or amendments, and its description is
// [PersonaLineage.Baseline]: it is the persona the instance was created
// with, and no reflection produced it.
//
// DescriptionEvidence names the experiences the description was built from,
// which is what an operator reads to judge whether an accepted change was
// argued for. A revision that keeps its parent's description keeps its
// parent's citations with it, so the text in force always names what it
// rests on. Revision zero cites nothing: its description is the persona the
// instance was created with.
type PersonaRevision struct {
	ID                  PersonaRevisionID
	InstanceID          InstanceID
	ParentID            *PersonaRevisionID
	Description         string
	DescriptionEvidence []ExperienceID
	ExperienceIDs       []ExperienceID
	AmendmentIDs        []PersonaAmendmentID
	CreatedAt           time.Time
}

// ReflectionEventRef identifies one event in the reflecting instance's
// private candidate stream.
type ReflectionEventRef struct {
	Sequence ReflectionSequence
}

// Experience is one occasion a reflection accepted, and what the instance
// made of it. [ExperienceKind] says which: something it observed, a
// reading it drew, or a claim somebody made.
//
// No reflection removes an experience. What changes is how live it is:
// LastCitedAt records when a revision last drew on it, either as evidence
// for a description or a tendency, or because the instance went back and
// read it during a reflection that reached a commit. SalienceAt is the
// ordering key that follows from it, and [SalienceAt] is what computes it.
//
// The storage backstop does remove one, from the bottom of that ranking
// and never one an active description or tendency cites. An instance
// that has accumulated more than the headroom therefore has old
// revisions naming fewer experiences than they were committed with, and
// a rollback to one of those restores the description and the
// experiences that are left.
type Experience struct {
	ID          ExperienceID
	InstanceID  InstanceID
	Kind        ExperienceKind
	Summary     string
	SubjectID   *InstanceID
	Confidence  Confidence
	OccurredAt  time.Time
	CreatedAt   time.Time
	LastCitedAt time.Time
	SalienceAt  time.Time
	Sources     []ReflectionEventRef
}

// ExperienceSalienceHalfLife is how long an experience nothing cites takes
// to become half as salient as it was.
//
// A month is roughly how long a working relationship keeps referring to the
// same events, and it is the middle of the three amendment lifetimes, so a
// tendency and the experiences behind it fade on comparable terms.
const ExperienceSalienceHalfLife = 30 * 24 * time.Hour

// SalienceAt is the ordering key for an experience: the moment it was last
// cited, brought forward by what its confidence is worth.
//
// Salience decays by halves, so ranking by confidence times the decay is
// the same as ranking by the time each experience would have been cited to
// stand where it does. Confidence therefore buys a fixed head start rather
// than a factor to recompute, and a stored timestamp is enough to order by
// at any later moment.
func SalienceAt(confidence Confidence, lastCitedAt time.Time) time.Time {
	return lastCitedAt.Add(confidenceHalfLives(confidence) * ExperienceSalienceHalfLife)
}

// confidenceHalfLives is how many half-lives of head start each confidence
// is worth: a medium experience counts as twice as salient as a low one,
// and a high one as four times.
func confidenceHalfLives(confidence Confidence) time.Duration {
	switch confidence {
	case ConfidenceHigh:
		return 2
	case ConfidenceMedium:
		return 1
	}

	return 0
}

// AmendmentDepartureKind identifies how a reflection removed a tendency
// from an active set. A reflection applies no other removal.
type AmendmentDepartureKind string

const (
	// AmendmentRetracted means a reflection withdrew the tendency without
	// putting anything in its place.
	AmendmentRetracted AmendmentDepartureKind = "retracted"
	// AmendmentConsolidated means a reflection folded the tendency into an
	// accepted persona description.
	AmendmentConsolidated AmendmentDepartureKind = "consolidated"
	// AmendmentSuperseded means a newer tendency replaced it, and that
	// tendency's SupersedesID names this one.
	AmendmentSuperseded AmendmentDepartureKind = "superseded"
)

// AmendmentDeparture records how and when a tendency left an active set.
type AmendmentDeparture struct {
	Kind AmendmentDepartureKind
	At   time.Time
}

// PersonaAmendment is one tendency a persona revision applies. Its text,
// scope, evidence and expiry are fixed when it is written; Departure is
// the one field a later reflection writes.
//
// Departure is the last removal recorded for this amendment, and a fact
// about its history and not its current standing: which amendments
// apply is decided by the active revision's set. A rollback can make the
// amendment active again without clearing Departure, and a later removal
// replaces it.
type PersonaAmendment struct {
	ID           PersonaAmendmentID
	InstanceID   InstanceID
	Scope        AmendmentScope
	Counterpart  *InstanceID
	Tendency     string
	Confidence   Confidence
	Evidence     []ExperienceID
	CreatedAt    time.Time
	ExpiresAt    *time.Time
	SupersedesID *PersonaAmendmentID
	Departure    *AmendmentDeparture
}

// ReflectionRun is the bounded diagnostic record for one reflection attempt.
type ReflectionRun struct {
	ID                  ReflectionRunID
	InstanceID          InstanceID
	BaseRevisionID      PersonaRevisionID
	PriorCheckpoint     ReflectionSequence
	HighWaterMark       ReflectionSequence
	ResultRevisionID    PersonaRevisionID
	ModelID             ModelID
	Outcome             ReflectionOutcome
	RejectionReason     string
	ProposedExperiences int
	AcceptedExperiences int
	ProposedAmendments  int
	AcceptedAmendments  int
	StartedAt           time.Time
	FinishedAt          time.Time
}

// PersonaTransition records one accepted change to an instance's active
// revision pointer.
type PersonaTransition struct {
	ID             int64
	InstanceID     InstanceID
	FromRevisionID PersonaRevisionID
	ToRevisionID   PersonaRevisionID
	Kind           PersonaTransitionKind
	At             time.Time
}

// PersonaCounterpart gives an operator the current display name for an actor
// that persona lineage names: the subject of an experience, or the counterpart
// of a relationship-scoped amendment.
type PersonaCounterpart struct {
	InstanceID InstanceID
	Nick       Nick
}

// PersonaInspection is the bounded operator view of one instance's
// active persona revision and recent reflection diagnostics.
//
// Parent is the revision the active one was derived from, which is what
// makes the change at this revision readable. It is nil for revision
// zero. Departed contains the amendments present in Parent and absent
// from Revision, each carrying its departure when one was recorded.
type PersonaInspection struct {
	Nick         Nick
	Lineage      PersonaLineage
	Revision     PersonaRevision
	Parent       *PersonaRevision
	Experiences  []Experience
	Amendments   []PersonaAmendment
	Departed     []PersonaAmendment
	Counterparts []PersonaCounterpart
	RecentRuns   []ReflectionRun
	Transitions  []PersonaTransition
}
