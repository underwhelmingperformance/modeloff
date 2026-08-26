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
