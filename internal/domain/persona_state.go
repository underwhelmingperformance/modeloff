package domain

import "time"

// PersonaRevisionID identifies one immutable revision of an instance's
// evolving personality state.
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

// PersonaRevision is one immutable point in an instance's persona
// history. Description is the persona in force while this revision is
// active. Revision zero has no parent, experiences or amendments, and its
// description is [PersonaLineage.Baseline]: it is the persona the instance
// was created with, and no reflection produced it.
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
type Experience struct {
	ID         ExperienceID
	InstanceID InstanceID
	Kind       ExperienceKind
	Summary    string
	SubjectID  *InstanceID
	Confidence Confidence
	OccurredAt time.Time
	CreatedAt  time.Time
	Sources    []ReflectionEventRef
}

// PersonaAmendment is one tendency a persona revision applies. Its text,
// scope, evidence and expiry are fixed when it is written; ConsolidatedAt
// is the one field a later reflection writes.
//
// ConsolidatedAt records when a reflection folded this tendency into a
// persona description. It is a fact about the amendment's history and not
// its current standing: which amendments apply is decided by the active
// revision's set, so a rollback to a revision from before the
// consolidation makes the amendment apply again with the mark still on
// it.
type PersonaAmendment struct {
	ID             PersonaAmendmentID
	InstanceID     InstanceID
	Scope          AmendmentScope
	Counterpart    *InstanceID
	Tendency       string
	Confidence     Confidence
	Evidence       []ExperienceID
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	SupersedesID   *PersonaAmendmentID
	ConsolidatedAt *time.Time
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
// makes the change at this revision readable. It is nil for revision zero.
type PersonaInspection struct {
	Nick         Nick
	Lineage      PersonaLineage
	Revision     PersonaRevision
	Parent       *PersonaRevision
	Experiences  []Experience
	Amendments   []PersonaAmendment
	Counterparts []PersonaCounterpart
	RecentRuns   []ReflectionRun
	Transitions  []PersonaTransition
}
