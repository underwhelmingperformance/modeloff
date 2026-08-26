package modelmanager

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/laney/modeloff/internal/api"
	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
	"github.com/laney/modeloff/internal/store"
)

const (
	maxReflectionExperiences   = 3
	maxReflectionAmendments    = 4
	maxActivePersonaAmendments = 12
	maxReflectionKeyLength     = 64
)

// An accepted amendment expires unless a later run supersedes it, so a
// tendency has to keep being observed to keep applying. How long it lasts
// is the only use the code makes of the confidence the reflecting model
// gave it: a week, a month or a quarter. Nothing here measures how well
// the evidence supports the tendency, and the confidence label is the
// model's own reading of that.
const (
	lowConfidenceAmendmentLifetime    = 7 * 24 * time.Hour
	mediumConfidenceAmendmentLifetime = 30 * 24 * time.Hour
	highConfidenceAmendmentLifetime   = 90 * 24 * time.Hour
)

// reflectionEpisodeGap is the quiet interval that separates one episode
// from the next. Lines of a single exchange arrive minutes apart, so a
// shorter interval would read two turns of one conversation as two
// separate occasions.
const reflectionEpisodeGap = time.Hour

var reflectionKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// reflectionRolePrefixes are the Chat Completions role headers a proposal
// may not open with. It is the whole set the API defines, including the
// deprecated "function", so no real role reads as ordinary text.
var reflectionRolePrefixes = []string{
	"system:", "developer:", "assistant:", "user:", "tool:", "function:",
}

// ReflectionValidationReason identifies one deterministic proposal refusal.
type ReflectionValidationReason string

// Reflection proposal validation reasons.
const (
	ReflectionTooManyItems         ReflectionValidationReason = "too_many_items"
	ReflectionInvalidKey           ReflectionValidationReason = "invalid_key"
	ReflectionDuplicateKey         ReflectionValidationReason = "duplicate_key"
	ReflectionInvalidKind          ReflectionValidationReason = "invalid_kind"
	ReflectionInvalidConfidence    ReflectionValidationReason = "invalid_confidence"
	ReflectionInvalidText          ReflectionValidationReason = "invalid_text"
	ReflectionRolePrefixed         ReflectionValidationReason = "role_prefixed"
	ReflectionUnknownSource        ReflectionValidationReason = "unknown_source"
	ReflectionDuplicateSource      ReflectionValidationReason = "duplicate_source"
	ReflectionInvalidSubject       ReflectionValidationReason = "invalid_subject"
	ReflectionInvalidScope         ReflectionValidationReason = "invalid_scope"
	ReflectionUnknownEvidence      ReflectionValidationReason = "unknown_evidence"
	ReflectionInsufficientEvidence ReflectionValidationReason = "insufficient_evidence"
	ReflectionUnknownAmendment     ReflectionValidationReason = "unknown_amendment"
	ReflectionDuplicateRetraction  ReflectionValidationReason = "duplicate_retraction"
	ReflectionStateLimit           ReflectionValidationReason = "state_limit"
	ReflectionNamedContext         ReflectionValidationReason = "named_context"
	ReflectionNoDescription        ReflectionValidationReason = "no_description"
)

// ReflectionValidationError reports the first deterministic proposal refusal.
type ReflectionValidationError struct {
	Reason ReflectionValidationReason
	Field  string
}

func (e *ReflectionValidationError) Error() string {
	return fmt.Sprintf("reject persona reflection field %s: %s", e.Field, e.Reason)
}

type reflectionValidationInput struct {
	RunID      domain.ReflectionRunID
	ModelID    domain.ModelID
	StartedAt  time.Time
	FinishedAt time.Time
	Snapshot   store.PendingReflectionSnapshot
	Proposal   api.ReflectionProposal
	Aliases    *reflectionAliases
}

type validatedReflectionExperience struct {
	Draft   store.PersonaExperienceDraft
	Sources []store.ReflectionEvent
}

type reflectionAmendmentValidator struct {
	aliases       *reflectionAliases
	experiences   map[string]validatedReflectionExperience
	episodes      map[domain.ReflectionSequence]int
	acceptedAt    time.Time
	global        bool
	relationships map[domain.InstanceID]struct{}
	superseded    map[domain.PersonaAmendmentID]struct{}
}

type validatedAmendmentEvidence struct {
	keys    []string
	sources []store.ReflectionEvent
}

func validateReflectionProposal(
	input reflectionValidationInput,
) (store.PersonaReflectionAcceptance, error) {
	if len(input.Proposal.Experiences) > maxReflectionExperiences {
		return rejectReflection(ReflectionTooManyItems, "experiences")
	}
	if len(input.Proposal.Amendments) > maxReflectionAmendments {
		return rejectReflection(ReflectionTooManyItems, "amendments")
	}

	events := make(map[domain.ReflectionSequence]store.ReflectionEvent, len(input.Snapshot.Events))
	for _, event := range input.Snapshot.Events {
		events[event.Sequence] = event
	}
	experienceByKey := make(
		map[string]validatedReflectionExperience,
		len(input.Proposal.Experiences),
	)
	usedSources := make(map[domain.ReflectionSequence]struct{})
	experienceDrafts := make(
		[]store.PersonaExperienceDraft, 0, len(input.Proposal.Experiences),
	)
	for index, proposal := range input.Proposal.Experiences {
		field := fmt.Sprintf("experiences[%d]", index)
		validated, err := validateReflectionExperience(
			proposal, field, input.Aliases, events, usedSources,
		)
		if err != nil {
			return store.PersonaReflectionAcceptance{}, err
		}
		if _, exists := experienceByKey[proposal.Key]; exists {
			return rejectReflection(ReflectionDuplicateKey, field+".key")
		}
		experienceByKey[proposal.Key] = validated
		experienceDrafts = append(experienceDrafts, validated.Draft)
	}

	episodes := episodesBySequence(input.Snapshot.Events)
	amendmentDrafts, err := validateReflectionAmendments(
		input.Proposal.Amendments, input.Aliases, experienceByKey,
		episodes, input.FinishedAt,
	)
	if err != nil {
		return store.PersonaReflectionAcceptance{}, err
	}
	retractions, err := validateReflectionRetractions(
		input.Proposal.Retract, input.Aliases,
	)
	if err != nil {
		return store.PersonaReflectionAcceptance{}, err
	}
	persona, err := validateReflectionDescription(
		input.Proposal.Persona, input.Snapshot, input.Aliases, experienceByKey,
	)
	if err != nil {
		return store.PersonaReflectionAcceptance{}, err
	}
	if err := requireDistinctRemovals(
		retractions, persona.Consolidate, amendmentDrafts,
	); err != nil {
		return store.PersonaReflectionAcceptance{}, err
	}

	removed := len(retractions) + len(persona.Consolidate)
	for _, draft := range amendmentDrafts {
		if draft.SupersedesID != nil {
			removed++
		}
	}
	if len(input.Snapshot.Persona.Amendments)-removed+len(amendmentDrafts) >
		maxActivePersonaAmendments {
		return rejectReflection(ReflectionStateLimit, "amendments")
	}

	return store.PersonaReflectionAcceptance{
		RunID: input.RunID, InstanceID: input.Snapshot.Persona.Lineage.InstanceID,
		BaseRevisionID:  input.Snapshot.Persona.Revision.ID,
		PriorCheckpoint: input.Snapshot.Range.Checkpoint,
		HighWaterMark:   input.Snapshot.Range.Through,
		ModelID:         input.ModelID, StartedAt: input.StartedAt, FinishedAt: input.FinishedAt,
		Description:             persona.Description,
		DescriptionEvidenceKeys: persona.EvidenceKeys,
		Experiences:             experienceDrafts, Amendments: amendmentDrafts,
		Retract: retractions, Consolidate: persona.Consolidate,
	}, nil
}

func validateReflectionExperience(
	proposal api.ReflectionExperienceProposal,
	field string,
	aliases *reflectionAliases,
	events map[domain.ReflectionSequence]store.ReflectionEvent,
	usedSources map[domain.ReflectionSequence]struct{},
) (validatedReflectionExperience, error) {
	if len(proposal.Key) == 0 || len(proposal.Key) > maxReflectionKeyLength ||
		!reflectionKeyPattern.MatchString(proposal.Key) {
		return validatedReflectionExperience{}, validationError(
			ReflectionInvalidKey, field+".key",
		)
	}
	if !validExperienceKind(proposal.Kind) {
		return validatedReflectionExperience{}, validationError(
			ReflectionInvalidKind, field+".kind",
		)
	}
	if !validReflectionConfidence(proposal.Confidence) {
		return validatedReflectionExperience{}, validationError(
			ReflectionInvalidConfidence, field+".confidence",
		)
	}
	if err := validateReflectionText(proposal.Summary, field+".summary"); err != nil {
		return validatedReflectionExperience{}, err
	}
	if len(proposal.Sources) == 0 {
		return validatedReflectionExperience{}, validationError(
			ReflectionInsufficientEvidence, field+".sources",
		)
	}

	var subject *domain.InstanceID
	if proposal.Subject != "" {
		id, known := aliases.instanceID(proposal.Subject)
		if !known {
			return validatedReflectionExperience{}, validationError(
				ReflectionInvalidSubject, field+".subject",
			)
		}
		subject = &id
	}
	if (proposal.Kind == domain.ExperienceRelationship ||
		proposal.Kind == domain.ExperienceAssertion) && subject == nil {
		return validatedReflectionExperience{}, validationError(
			ReflectionInvalidSubject, field+".subject",
		)
	}
	if proposal.Kind != domain.ExperienceRelationship &&
		proposal.Kind != domain.ExperienceAssertion && subject != nil {
		return validatedReflectionExperience{}, validationError(
			ReflectionInvalidSubject, field+".subject",
		)
	}

	sources := make([]store.ReflectionEvent, 0, len(proposal.Sources))
	refs := make([]domain.ReflectionEventRef, 0, len(proposal.Sources))
	var occurredAt time.Time
	for index, sequence := range proposal.Sources {
		sourceField := fmt.Sprintf("%s.sources[%d]", field, index)
		event, exists := events[sequence]
		if !exists {
			return validatedReflectionExperience{}, validationError(
				ReflectionUnknownSource, sourceField,
			)
		}
		if _, exists := usedSources[sequence]; exists {
			return validatedReflectionExperience{}, validationError(
				ReflectionDuplicateSource, sourceField,
			)
		}
		if subject != nil && !reflectionEventInvolves(event, *subject) {
			return validatedReflectionExperience{}, validationError(
				ReflectionInvalidSubject, field+".subject",
			)
		}
		sourceID, identified := event.Message.Source.InstanceID()
		if proposal.Kind == domain.ExperienceAssertion &&
			(!identified || sourceID != *subject) {
			return validatedReflectionExperience{}, validationError(
				ReflectionInvalidSubject, field+".subject",
			)
		}
		usedSources[sequence] = struct{}{}
		sources = append(sources, event)
		refs = append(refs, domain.ReflectionEventRef{Sequence: sequence})
		if occurredAt.Before(event.Message.At) {
			occurredAt = event.Message.At
		}
	}

	return validatedReflectionExperience{
		Draft: store.PersonaExperienceDraft{
			Key: proposal.Key, Kind: proposal.Kind, Summary: proposal.Summary,
			SubjectID: subject, Confidence: proposal.Confidence,
			OccurredAt: occurredAt, Sources: refs,
		},
		Sources: sources,
	}, nil
}

func validateReflectionAmendments(
	proposals []api.ReflectionAmendmentProposal,
	aliases *reflectionAliases,
	experiences map[string]validatedReflectionExperience,
	episodes map[domain.ReflectionSequence]int,
	acceptedAt time.Time,
) ([]store.PersonaAmendmentDraft, error) {
	drafts := make([]store.PersonaAmendmentDraft, 0, len(proposals))
	validator := reflectionAmendmentValidator{
		aliases:       aliases,
		experiences:   experiences,
		episodes:      episodes,
		acceptedAt:    acceptedAt,
		relationships: make(map[domain.InstanceID]struct{}),
		superseded:    make(map[domain.PersonaAmendmentID]struct{}),
	}
	for index, proposal := range proposals {
		field := fmt.Sprintf("amendments[%d]", index)
		draft, err := validator.validate(proposal, field)
		if err != nil {
			return nil, err
		}
		drafts = append(drafts, draft)
	}

	return drafts, nil
}

func (v *reflectionAmendmentValidator) validate(
	proposal api.ReflectionAmendmentProposal,
	field string,
) (store.PersonaAmendmentDraft, error) {
	lifetime, known := reflectionAmendmentLifetime(proposal.Confidence)
	if !known {
		return store.PersonaAmendmentDraft{}, validationError(
			ReflectionInvalidConfidence, field+".confidence",
		)
	}
	if err := validateReflectionText(proposal.Tendency, field+".tendency"); err != nil {
		return store.PersonaAmendmentDraft{}, err
	}
	if len(proposal.EvidenceKeys) == 0 {
		return store.PersonaAmendmentDraft{}, validationError(
			ReflectionInsufficientEvidence, field+".evidence_keys",
		)
	}

	counterpart, err := v.validateScope(proposal, field)
	if err != nil {
		return store.PersonaAmendmentDraft{}, err
	}
	supersedes, err := v.validateSupersession(proposal.Supersedes, field)
	if err != nil {
		return store.PersonaAmendmentDraft{}, err
	}
	evidence, err := v.validateEvidence(proposal.EvidenceKeys, counterpart, field)
	if err != nil {
		return store.PersonaAmendmentDraft{}, err
	}
	if proposal.Scope == domain.AmendmentGlobal &&
		!spansEpisodes(evidence.sources, v.episodes) {
		return store.PersonaAmendmentDraft{}, validationError(
			ReflectionInsufficientEvidence, field+".evidence_keys",
		)
	}

	expiresAt := v.acceptedAt.Add(lifetime)

	return store.PersonaAmendmentDraft{
		Scope: proposal.Scope, Counterpart: counterpart,
		Tendency: proposal.Tendency, Confidence: proposal.Confidence,
		EvidenceKeys: evidence.keys,
		ExpiresAt:    &expiresAt, SupersedesID: supersedes,
	}, nil
}

// validateSupersession resolves the amendment a proposal replaces. An
// empty token replaces nothing, which is how the schema says the field
// was left out.
func (v *reflectionAmendmentValidator) validateSupersession(
	token string,
	field string,
) (*domain.PersonaAmendmentID, error) {
	if token == "" {
		return nil, nil
	}
	id, known := v.aliases.amendmentID(token)
	if !known {
		return nil, validationError(ReflectionUnknownAmendment, field+".supersedes")
	}
	if _, duplicate := v.superseded[id]; duplicate {
		return nil, validationError(
			ReflectionDuplicateRetraction, field+".supersedes",
		)
	}
	v.superseded[id] = struct{}{}

	return &id, nil
}

func (v *reflectionAmendmentValidator) validateScope(
	proposal api.ReflectionAmendmentProposal,
	field string,
) (*domain.InstanceID, error) {
	switch proposal.Scope {
	case domain.AmendmentGlobal:
		if proposal.Counterpart != "" || v.global {
			return nil, validationError(
				ReflectionInvalidScope, field+".counterpart",
			)
		}
		v.global = true

		return nil, nil
	case domain.AmendmentRelationship:
		if proposal.Counterpart == "" {
			return nil, validationError(
				ReflectionInvalidScope, field+".counterpart",
			)
		}
		id, known := v.aliases.instanceID(proposal.Counterpart)
		if !known {
			return nil, validationError(
				ReflectionInvalidSubject, field+".counterpart",
			)
		}
		if _, exists := v.relationships[id]; exists {
			return nil, validationError(
				ReflectionInvalidScope, field+".counterpart",
			)
		}
		v.relationships[id] = struct{}{}

		return &id, nil
	default:
		return nil, validationError(ReflectionInvalidScope, field+".scope")
	}
}

func (v *reflectionAmendmentValidator) validateEvidence(
	keys []string,
	counterpart *domain.InstanceID,
	field string,
) (validatedAmendmentEvidence, error) {
	evidence := validatedAmendmentEvidence{keys: slices.Clone(keys)}
	seen := make(map[string]struct{}, len(keys))
	for index, key := range keys {
		keyField := fmt.Sprintf("%s.evidence_keys[%d]", field, index)
		if _, duplicate := seen[key]; duplicate {
			return validatedAmendmentEvidence{}, validationError(
				ReflectionDuplicateKey, keyField,
			)
		}
		seen[key] = struct{}{}
		experience, exists := v.experiences[key]
		if !exists {
			return validatedAmendmentEvidence{}, validationError(
				ReflectionUnknownEvidence, keyField,
			)
		}
		if counterpart != nil && !experienceMatchesCounterpart(experience, *counterpart) {
			return validatedAmendmentEvidence{}, validationError(
				ReflectionInvalidSubject, field+".counterpart",
			)
		}
		evidence.sources = append(evidence.sources, experience.Sources...)
	}

	return evidence, nil
}

func experienceMatchesCounterpart(
	experience validatedReflectionExperience,
	counterpart domain.InstanceID,
) bool {
	if experience.Draft.SubjectID != nil &&
		*experience.Draft.SubjectID != counterpart {
		return false
	}
	for _, source := range experience.Sources {
		if !reflectionEventInvolves(source, counterpart) {
			return false
		}
	}

	return true
}

func validateReflectionRetractions(
	proposals []string,
	aliases *reflectionAliases,
) ([]domain.PersonaAmendmentID, error) {
	retractions := make([]domain.PersonaAmendmentID, 0, len(proposals))
	seen := make(map[domain.PersonaAmendmentID]struct{}, len(proposals))
	for index, token := range proposals {
		field := fmt.Sprintf("retract_amendments[%d]", index)
		id, known := aliases.amendmentID(token)
		if !known {
			return nil, validationError(ReflectionUnknownAmendment, field)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, validationError(ReflectionDuplicateRetraction, field)
		}
		seen[id] = struct{}{}
		retractions = append(retractions, id)
	}

	return retractions, nil
}

// requireDistinctRemovals refuses a proposal that takes one amendment out
// of the active set twice. Retraction, supersession and consolidation are
// three ways of saying a tendency stops applying, and each says something
// different about why, so naming an amendment under two of them leaves no
// answer to which happened.
func requireDistinctRemovals(
	retractions []domain.PersonaAmendmentID,
	consolidate []domain.PersonaAmendmentID,
	amendments []store.PersonaAmendmentDraft,
) error {
	removed := make(map[domain.PersonaAmendmentID]struct{}, len(retractions))
	for _, id := range retractions {
		removed[id] = struct{}{}
	}
	for index, id := range consolidate {
		if _, duplicate := removed[id]; duplicate {
			return validationError(
				ReflectionDuplicateRetraction,
				fmt.Sprintf("persona.consolidates[%d]", index),
			)
		}
		removed[id] = struct{}{}
	}
	for index, draft := range amendments {
		if draft.SupersedesID == nil {
			continue
		}
		if _, duplicate := removed[*draft.SupersedesID]; duplicate {
			return validationError(
				ReflectionDuplicateRetraction,
				fmt.Sprintf("amendments[%d].supersedes", index),
			)
		}
		removed[*draft.SupersedesID] = struct{}{}
	}

	return nil
}

// validatedReflectionPersona is an accepted persona proposal: the replacement
// description, the experiences it was built from, and the tendencies it
// absorbs. Description is nil when the run proposed no persona change.
type validatedReflectionPersona struct {
	Description  *string
	EvidenceKeys []string
	Consolidate  []domain.PersonaAmendmentID
}

// validateReflectionDescription checks a proposed replacement persona
// description. An empty proposal changes nothing and returns a nil
// Description, which leaves the next revision on its parent's text.
//
// The evidence keys are kept, not just checked. A description that absorbs no
// tendency has no amendment to carry its citations, so the revision records
// them and an operator can read back what an accepted change rested on.
//
// The checks here are structural, and deliberately stop short of the
// distinction the description exists for. Whether a clause describes a
// disposition or a behaviour is a judgement, carried by the reflection
// prompt and by the operator's per-revision diff.
func validateReflectionDescription(
	proposal api.ReflectionPersonaProposal,
	snapshot store.PendingReflectionSnapshot,
	aliases *reflectionAliases,
	experiences map[string]validatedReflectionExperience,
) (validatedReflectionPersona, error) {
	if strings.TrimSpace(proposal.Description) == "" {
		if len(proposal.Consolidates) > 0 {
			return validatedReflectionPersona{}, validationError(
				ReflectionNoDescription, "persona.consolidates",
			)
		}

		return validatedReflectionPersona{}, nil
	}
	if err := validateReflectionText(
		proposal.Description, "persona.description",
	); err != nil {
		return validatedReflectionPersona{}, err
	}
	if descriptionNamesContext(
		proposal.Description, contextNames(snapshot, aliases),
	) {
		return validatedReflectionPersona{}, validationError(
			ReflectionNamedContext, "persona.description",
		)
	}
	if len(proposal.EvidenceKeys) == 0 {
		return validatedReflectionPersona{}, validationError(
			ReflectionInsufficientEvidence, "persona.evidence_keys",
		)
	}

	var sources []store.ReflectionEvent
	seen := make(map[string]struct{}, len(proposal.EvidenceKeys))
	for index, key := range proposal.EvidenceKeys {
		field := fmt.Sprintf("persona.evidence_keys[%d]", index)
		if _, duplicate := seen[key]; duplicate {
			return validatedReflectionPersona{}, validationError(
				ReflectionDuplicateKey, field,
			)
		}
		seen[key] = struct{}{}
		experience, exists := experiences[key]
		if !exists {
			return validatedReflectionPersona{}, validationError(
				ReflectionUnknownEvidence, field,
			)
		}
		sources = append(sources, experience.Sources...)
	}
	if !spansEpisodes(sources, episodesBySequence(snapshot.Events)) {
		return validatedReflectionPersona{}, validationError(
			ReflectionInsufficientEvidence, "persona.evidence_keys",
		)
	}

	consolidate, err := validateReflectionConsolidations(
		proposal.Consolidates, aliases,
	)
	if err != nil {
		return validatedReflectionPersona{}, err
	}
	description := proposal.Description

	return validatedReflectionPersona{
		Description:  &description,
		EvidenceKeys: slices.Clone(proposal.EvidenceKeys),
		Consolidate:  consolidate,
	}, nil
}

// validateReflectionConsolidations resolves the amendments a description
// absorbs. They are named the way a retraction names one, and the same
// two rules apply: the token has to belong to an active amendment this
// request listed, and no amendment may be named twice.
func validateReflectionConsolidations(
	tokens []string,
	aliases *reflectionAliases,
) ([]domain.PersonaAmendmentID, error) {
	consolidate := make([]domain.PersonaAmendmentID, 0, len(tokens))
	seen := make(map[domain.PersonaAmendmentID]struct{}, len(tokens))
	for index, token := range tokens {
		field := fmt.Sprintf("persona.consolidates[%d]", index)
		id, known := aliases.amendmentID(token)
		if !known {
			return nil, validationError(ReflectionUnknownAmendment, field)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, validationError(ReflectionDuplicateRetraction, field)
		}
		seen[id] = struct{}{}
		consolidate = append(consolidate, id)
	}

	return consolidate, nil
}

// reflectionContextNames is every name this run could have taken a
// description from. The names come from the run itself, so the check is a
// comparison and not a guess at what a name looks like.
//
// The two kinds are matched differently, so they are kept apart. Words
// holds each participant's token and every nick they were seen under,
// including one they have since renamed away from. Channels holds the name
// of every channel the events came from.
type reflectionContextNames struct {
	Words    []string
	Channels []string
}

func contextNames(
	snapshot store.PendingReflectionSnapshot,
	aliases *reflectionAliases,
) reflectionContextNames {
	names := reflectionContextNames{}
	for _, participant := range aliases.participants {
		names.Words = append(names.Words, participant.Token)
	}
	for _, nick := range aliases.everyNick() {
		names.Words = append(names.Words, string(nick))
	}
	for _, event := range snapshot.Events {
		if channel, named := protocol.ChannelWindowName(event.Source.Window); named {
			names.Channels = append(names.Channels, string(channel))
		}
	}

	return names
}

// descriptionNamesContext reports whether the description names anything
// the run gave it. The server's casemapping decides what counts as the
// same name.
//
// A nick is matched as a whole word, which keeps a nick like "al" from
// being found inside "already". A channel is matched wherever it appears,
// because a channel name may hold the punctuation a word split treats as a
// boundary: "#dev-ops.2" is one name, and a description ending "in #dev."
// still names #dev.
func descriptionNamesContext(description string, names reflectionContextNames) bool {
	folded := domain.CaseFold(description)
	for _, channel := range names.Channels {
		if strings.Contains(folded, domain.CaseFold(channel)) {
			return true
		}
	}

	forbidden := make(map[string]struct{}, len(names.Words))
	for _, word := range names.Words {
		forbidden[domain.CaseFold(word)] = struct{}{}
	}
	for _, word := range strings.FieldsFunc(description, isNameSeparator) {
		if _, named := forbidden[domain.CaseFold(word)]; named {
			return true
		}
	}

	return false
}

// isNameSeparator reports whether a rune ends a word for the purpose of
// the nick check. A nick may hold letters, digits and the specials RFC
// 2812 §2.3.1 admits, so those characters stay inside the word.
func isNameSeparator(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) &&
		!strings.ContainsRune("[]\\`_^{|}-", r)
}

// validateReflectionText checks one proposed summary or tendency. The text
// must pass [domain.ValidatePersona], and it must not open with a chat role
// header, which a reader of the rendered CURRENT_INSTANCE_STATE record
// would take for the start of a new turn.
//
// Do not extend this into a list of imperative openers or suspicious
// phrases. Such a list cannot tell an instruction from a description of
// behaviour: it refuses ordinary proposals like "Never volunteers an
// opinion until asked", and admits anything phrased around the list. What
// a proposal can reach is bounded by the evidence, scope and identity
// rules above, and by the prompt presenting the CURRENT_INSTANCE_STATE
// record as untrusted data.
func validateReflectionText(text, field string) error {
	if strings.TrimSpace(text) == "" || domain.ValidatePersona(text) != domain.PersonaAccepted {
		return validationError(ReflectionInvalidText, field)
	}
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, prefix := range reflectionRolePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return validationError(ReflectionRolePrefixed, field)
		}
	}

	return nil
}

func validExperienceKind(kind domain.ExperienceKind) bool {
	return kind == domain.ExperienceObservation ||
		kind == domain.ExperienceInterpretation ||
		kind == domain.ExperienceAssertion ||
		kind == domain.ExperienceRelationship
}

func validReflectionConfidence(confidence domain.Confidence) bool {
	return confidence == domain.ConfidenceLow ||
		confidence == domain.ConfidenceMedium ||
		confidence == domain.ConfidenceHigh
}

// reflectionAmendmentLifetime gives each confidence level the time an
// amendment drawn at that level stays active. It reports false for any
// other value, which is how the amendment path checks confidence: the
// check and the lifetime come from one list, so a level cannot be
// accepted without one.
func reflectionAmendmentLifetime(
	confidence domain.Confidence,
) (time.Duration, bool) {
	switch confidence {
	case domain.ConfidenceLow:
		return lowConfidenceAmendmentLifetime, true
	case domain.ConfidenceMedium:
		return mediumConfidenceAmendmentLifetime, true
	case domain.ConfidenceHigh:
		return highConfidenceAmendmentLifetime, true
	default:
		return 0, false
	}
}

func reflectionEventInvolves(
	event store.ReflectionEvent,
	instanceID domain.InstanceID,
) bool {
	sourceID, identified := event.Message.Source.InstanceID()
	if identified && sourceID == instanceID {
		return true
	}
	peer, direct := protocol.DirectWindowPeer(event.Source.Window)

	return direct && peer == instanceID
}

// spansEpisodes reports whether the cited events fall in more than one
// episode of the run's own stream.
//
// A global tendency needs more than one episode behind it, which is the
// structural half of "this is how the instance is" against "this is what
// the instance did once". Whether two episodes are really two occasions is
// a judgement, and the reflection prompt is where it is asked.
func spansEpisodes(
	cited []store.ReflectionEvent,
	episodes map[domain.ReflectionSequence]int,
) bool {
	if len(cited) < 2 {
		return false
	}

	first, found := 0, false
	for _, event := range cited {
		episode, known := episodes[event.Sequence]
		if !known {
			return false
		}
		if !found {
			first, found = episode, true

			continue
		}
		if episode != first {
			return true
		}
	}

	return false
}

// episodesBySequence numbers the episodes of one instance's stream, in
// order of the time each event was sent.
//
// The boundary comes from the stream and not from the citations. A
// proposal chooses what to cite, so comparing only the cited timestamps
// would read two lines an hour apart in one continuous conversation as two
// occasions; the events between them are what say otherwise.
func episodesBySequence(
	stream []store.ReflectionEvent,
) map[domain.ReflectionSequence]int {
	ordered := slices.Clone(stream)
	slices.SortFunc(ordered, func(first, second store.ReflectionEvent) int {
		return first.Message.At.Compare(second.Message.At)
	})

	episodes := make(map[domain.ReflectionSequence]int, len(ordered))
	episode := 0
	var previous time.Time
	for index, event := range ordered {
		if index > 0 && event.Message.At.Sub(previous) >= reflectionEpisodeGap {
			episode++
		}
		episodes[event.Sequence] = episode
		previous = event.Message.At
	}

	return episodes
}

func rejectReflection(
	reason ReflectionValidationReason,
	field string,
) (store.PersonaReflectionAcceptance, error) {
	return store.PersonaReflectionAcceptance{}, validationError(reason, field)
}

func validationError(
	reason ReflectionValidationReason,
	field string,
) *ReflectionValidationError {
	return &ReflectionValidationError{Reason: reason, Field: field}
}
