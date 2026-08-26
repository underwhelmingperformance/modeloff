package modelclient

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

const (
	maxPersonaContextExperiences  = 6
	maxPersonaContextAmendments   = 6
	maxPersonaExperienceTextBytes = 1600
	maxPersonaAmendmentTextBytes  = 1600
	personaContextReplyPrefix     = "what you know about the people here: "
)

type personaContextExperience struct {
	Kind       domain.ExperienceKind `json:"kind"`
	Summary    string                `json:"summary"`
	Subject    domain.Nick           `json:"subject,omitempty"`
	Confidence domain.Confidence     `json:"confidence"`
}

type personaContextTendency struct {
	Scope       domain.AmendmentScope `json:"scope"`
	Counterpart domain.Nick           `json:"counterpart,omitempty"`
	Tendency    string                `json:"tendency"`
	Confidence  domain.Confidence     `json:"confidence"`
}

// personaContextSelection is the rendered body of the people block: the
// experiences and tendencies that bear on whoever the instance is talking to
// this turn. Truncated says the selection left some of them out, and appears
// only when it did, because the block sits past the cacheable prefix and every
// field is paid for on each turn.
type personaContextSelection struct {
	Experiences []personaContextExperience `json:"experiences"`
	Tendencies  []personaContextTendency   `json:"tendencies"`
	Truncated   bool                       `json:"truncated,omitempty"`
}

type relevantPersonaExperience struct {
	experience domain.Experience
	subject    domain.Nick
	priority   int
}

type relevantPersonaAmendment struct {
	amendment   domain.PersonaAmendment
	counterpart domain.Nick
	priority    int
}

// personaContextReply renders what the instance knows about the people it is
// talking to this turn, so it can judge how to treat them and what to expect
// from them. It reports whether the selection held anything to render.
//
// The persona itself is the active revision's description, which
// [buildSystemPrompt] states in the instance state. This block carries neither
// that description nor the revision it came from: the instance is its persona,
// and the turn has no use for a revision number.
func personaContextReply(
	target protocol.WindowTarget,
	snapshot store.PersonaSnapshot,
	relevant []protocol.IRCMessage,
	projection providerTargetProjection,
	now time.Time,
) (protocol.IRCMessage, bool, error) {
	selection := selectPersonaContext(snapshot, target, relevant, projection, now)
	if len(selection.Experiences) == 0 && len(selection.Tendencies) == 0 {
		return protocol.IRCMessage{}, false, nil
	}

	body, err := json.Marshal(selection)
	if err != nil {
		return protocol.IRCMessage{}, false, err
	}

	return protocol.IRCMessage{
		Kind:   protocol.KindServerReply,
		Source: domain.ServerSource("modeloff"),
		Target: string(protocol.WindowKey(target)),
		Body:   personaContextReplyPrefix + string(body),
	}, true, nil
}

func selectPersonaContext(
	snapshot store.PersonaSnapshot,
	target protocol.WindowTarget,
	relevant []protocol.IRCMessage,
	projection providerTargetProjection,
	now time.Time,
) personaContextSelection {
	participants := personaContextParticipants(
		snapshot.Lineage.InstanceID, target, relevant, projection,
	)
	experiences := relevantPersonaExperiences(snapshot.Experiences, participants)
	amendments := relevantPersonaAmendments(snapshot.Amendments, participants, now)
	selection := personaContextSelection{
		Experiences: make([]personaContextExperience, 0, min(
			len(experiences), maxPersonaContextExperiences,
		)),
		Tendencies: make([]personaContextTendency, 0, min(
			len(amendments), maxPersonaContextAmendments,
		)),
	}

	experienceBytes := 0
	for _, candidate := range experiences {
		if len(selection.Experiences) == maxPersonaContextExperiences ||
			experienceBytes+len(candidate.experience.Summary) > maxPersonaExperienceTextBytes {
			selection.Truncated = true
			continue
		}
		selection.Experiences = append(selection.Experiences, personaContextExperience{
			Kind: candidate.experience.Kind, Summary: candidate.experience.Summary,
			Subject: candidate.subject, Confidence: candidate.experience.Confidence,
		})
		experienceBytes += len(candidate.experience.Summary)
	}

	amendmentBytes := 0
	for _, candidate := range amendments {
		if len(selection.Tendencies) == maxPersonaContextAmendments ||
			amendmentBytes+len(candidate.amendment.Tendency) > maxPersonaAmendmentTextBytes {
			selection.Truncated = true
			continue
		}
		selection.Tendencies = append(selection.Tendencies, personaContextTendency{
			Scope: candidate.amendment.Scope, Counterpart: candidate.counterpart,
			Tendency:   candidate.amendment.Tendency,
			Confidence: candidate.amendment.Confidence,
		})
		amendmentBytes += len(candidate.amendment.Tendency)
	}

	return selection
}

func personaContextParticipants(
	selfID domain.InstanceID,
	target protocol.WindowTarget,
	relevant []protocol.IRCMessage,
	projection providerTargetProjection,
) map[domain.InstanceID]domain.Nick {
	participants := make(map[domain.InstanceID]domain.Nick)
	for _, message := range relevant {
		id, identified := message.Source.InstanceID()
		if !identified || id == selfID {
			continue
		}
		// A NICK carries the nick being left behind in its source and the
		// one taken in its target, so reading the source would name a
		// counterpart by a nick they no longer answer to.
		if message.Kind == protocol.KindNick {
			if message.Target != "" {
				participants[id] = domain.Nick(message.Target)
			}

			continue
		}
		if message.Source.Nick() == "" {
			continue
		}
		participants[id] = message.Source.Nick()
	}
	peer, direct := protocol.DirectWindowPeer(target)
	if direct && projection.direct && projection.peerID == peer && projection.peerNick != "" {
		participants[peer] = projection.peerNick
	}

	return participants
}

func relevantPersonaExperiences(
	experiences []domain.Experience,
	participants map[domain.InstanceID]domain.Nick,
) []relevantPersonaExperience {
	relevant := make([]relevantPersonaExperience, 0, len(experiences))
	for _, experience := range experiences {
		candidate := relevantPersonaExperience{experience: experience, priority: 1}
		if experience.SubjectID != nil {
			nick, present := participants[*experience.SubjectID]
			if !present {
				continue
			}
			candidate.subject = nick
			candidate.priority = 0
		}
		relevant = append(relevant, candidate)
	}
	slices.SortStableFunc(relevant, func(a, b relevantPersonaExperience) int {
		if a.priority != b.priority {
			return a.priority - b.priority
		}
		if byTime := b.experience.OccurredAt.Compare(a.experience.OccurredAt); byTime != 0 {
			return byTime
		}

		return int(b.experience.ID - a.experience.ID)
	})

	return relevant
}

func relevantPersonaAmendments(
	amendments []domain.PersonaAmendment,
	participants map[domain.InstanceID]domain.Nick,
	now time.Time,
) []relevantPersonaAmendment {
	relevant := make([]relevantPersonaAmendment, 0, len(amendments))
	for _, amendment := range amendments {
		if amendment.ExpiresAt != nil && !amendment.ExpiresAt.After(now) {
			continue
		}
		candidate := relevantPersonaAmendment{amendment: amendment, priority: 1}
		switch amendment.Scope {
		case domain.AmendmentGlobal:
		case domain.AmendmentRelationship:
			if amendment.Counterpart == nil {
				continue
			}
			nick, present := participants[*amendment.Counterpart]
			if !present {
				continue
			}
			candidate.counterpart = nick
			candidate.priority = 0
		default:
			continue
		}
		relevant = append(relevant, candidate)
	}
	slices.SortStableFunc(relevant, func(a, b relevantPersonaAmendment) int {
		if a.priority != b.priority {
			return a.priority - b.priority
		}
		if byTime := b.amendment.CreatedAt.Compare(a.amendment.CreatedAt); byTime != 0 {
			return byTime
		}

		return int(b.amendment.ID - a.amendment.ID)
	})

	return relevant
}
