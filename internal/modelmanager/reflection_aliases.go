package modelmanager

import (
	"fmt"
	"slices"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// reflectionAliases replaces the identifiers one reflection request would
// otherwise carry with tokens allocated for that request alone.
//
// A proposal has to name a counterpart to scope a relationship and an
// amendment to supersede or retract it. Sending the real values would hand
// the reflecting model an instance id, which addresses a client on the
// wire, and persona row ids, which stay the same from one run to the next.
// Tokens are allocated in the order the request first mentions each
// identifier, and resolved back when the proposal returns.
//
// The reflecting instance itself has no token: participant returns the
// empty string for it, so no token a proposal can name resolves to the
// instance proposing it.
type reflectionAliases struct {
	self domain.InstanceID

	participants  []api.ReflectionParticipant
	participantAt map[domain.InstanceID]int
	instanceFor   map[string]domain.InstanceID

	// nicks holds every nick each participant was seen under, in first-
	// seen order. The request carries the latest one, which is what the
	// transcript reads as, and the name check needs all of them: a
	// participant who renamed mid-range is still nameable by the nick
	// they spoke under earlier.
	nicks map[domain.InstanceID][]domain.Nick

	amendmentTokens map[domain.PersonaAmendmentID]string
	amendmentIDs    map[string]domain.PersonaAmendmentID
}

func newReflectionAliases(self domain.InstanceID) *reflectionAliases {
	return &reflectionAliases{
		self:            self,
		participantAt:   make(map[domain.InstanceID]int),
		instanceFor:     make(map[string]domain.InstanceID),
		nicks:           make(map[domain.InstanceID][]domain.Nick),
		amendmentTokens: make(map[domain.PersonaAmendmentID]string),
		amendmentIDs:    make(map[string]domain.PersonaAmendmentID),
	}
}

// participant returns the token for id, allocating one on first mention.
// A non-empty nick is recorded against the token, so a participant the
// request saw speak is listed under the name the model reads in the
// transcript.
func (a *reflectionAliases) participant(
	id domain.InstanceID,
	nick domain.Nick,
) string {
	if id == a.self {
		return ""
	}

	index, allocated := a.participantAt[id]
	if !allocated {
		index = len(a.participants)
		token := fmt.Sprintf("p%d", index+1)
		a.participantAt[id] = index
		a.instanceFor[token] = id
		a.participants = append(a.participants, api.ReflectionParticipant{Token: token})
	}
	if nick != "" {
		a.participants[index].Nick = nick
		if !slices.Contains(a.nicks[id], nick) {
			a.nicks[id] = append(a.nicks[id], nick)
		}
	}

	return a.participants[index].Token
}

// everyNick returns every nick any participant was seen under.
func (a *reflectionAliases) everyNick() []domain.Nick {
	nicks := []domain.Nick{}
	for _, participant := range a.participants {
		id, known := a.instanceFor[participant.Token]
		if !known {
			continue
		}
		nicks = append(nicks, a.nicks[id]...)
	}

	return nicks
}

func (a *reflectionAliases) instanceID(token string) (domain.InstanceID, bool) {
	id, known := a.instanceFor[token]

	return id, known
}

// amendment returns the token for an active amendment, allocating one on
// first mention. Only amendments this request lists get a token, so a
// token resolving is also the proof that the amendment is active.
func (a *reflectionAliases) amendment(id domain.PersonaAmendmentID) string {
	if token, allocated := a.amendmentTokens[id]; allocated {
		return token
	}

	token := fmt.Sprintf("a%d", len(a.amendmentTokens)+1)
	a.amendmentTokens[id] = token
	a.amendmentIDs[token] = id

	return token
}

func (a *reflectionAliases) amendmentID(token string) (domain.PersonaAmendmentID, bool) {
	id, known := a.amendmentIDs[token]

	return id, known
}

// window names the conversation an event belongs to: a channel by its
// name, which every participant already reads in the transcript, and a
// direct message by the counterpart's token.
func (a *reflectionAliases) window(target protocol.WindowTarget) string {
	if peer, direct := protocol.DirectWindowPeer(target); direct {
		return a.participant(peer, "")
	}

	return string(protocol.WindowKey(target))
}
