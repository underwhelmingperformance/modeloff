package api

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/protocol"
)

// renderReflectionRequest writes each message under the role it was
// sent with, JSON bodies indented so a changed field shows on a line of
// its own. An added message or a changed role lands in the comparison,
// and a role this reflection request never sends renders as UNKNOWN.
func renderReflectionRequest(t *testing.T, params openai.ChatCompletionNewParams) string {
	t.Helper()

	var text bytes.Buffer
	for _, message := range params.Messages {
		role, body := "UNKNOWN", ""
		switch {
		case message.OfSystem != nil:
			role, body = "SYSTEM", message.OfSystem.Content.OfString.Value
		case message.OfUser != nil:
			role, body = "USER", message.OfUser.Content.OfString.Value
		case message.OfAssistant != nil:
			role, body = "ASSISTANT", message.OfAssistant.Content.OfString.Value
		}

		text.WriteString(role)
		text.WriteString("\n")
		var indented bytes.Buffer
		if json.Indent(&indented, []byte(body), "", "  ") == nil {
			text.Write(indented.Bytes())
		} else {
			text.WriteString(body)
		}
		text.WriteString("\n\n")
	}

	return strings.TrimRight(text.String(), "\n") + "\n"
}

// TestReflectionRequestParams_renders_the_prompt_and_the_input compares
// every message of a reflection request against a hand-edited golden.
//
// The validator checks a proposal's structure. Whether a clause is a
// disposition or a behaviour is decided by the instruction alone, and
// the instruction names the input's fields, so both belong in one
// golden.
func TestReflectionRequestParams_renders_the_prompt_and_the_input(t *testing.T) {
	at := time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)
	expires := at.Add(30 * 24 * time.Hour)
	input := ReflectionInput{
		Description:  "careful and curious, and slow to be convinced",
		Baseline:     "careful and curious",
		Participants: []ReflectionParticipant{{Token: "p1", Nick: "alice"}},
		Experiences: []ReflectionInputExperience{{
			Kind:    domain.ExperienceObservation,
			Summary: "Alice supplied a reproduction before agreeing.",
			Subject: "p1", Confidence: domain.ConfidenceHigh,
			OccurredAt: at, Sources: []domain.ReflectionSequence{11},
		}},
		Amendments: []ReflectionInputAmendment{{
			Token: "a1", Scope: domain.AmendmentRelationship, Counterpart: "p1",
			Tendency:   "Asks alice for a reproduction before agreeing.",
			Confidence: domain.ConfidenceMedium,
			CreatedAt:  at, ExpiresAt: &expires,
		}},
		Events: []ReflectionInputEvent{{
			Sequence: 12, WindowKind: ReflectionWindowChannel, Window: "#dev",
			Participant: "p1",
			Message: protocol.IRCMessage{
				Kind:   protocol.KindPrivMsg,
				Source: domain.LegacyClientSource("alice"),
				Target: "#dev", Body: "the reproduction is in the issue",
			},
			Substantive: true,
		}},
	}

	params, err := reflectionRequestParams("test/reflection", "inst-botty", input, nil)
	require.NoError(t, err)

	require.Equal(t,
		loadAPIGolden(t, "reflection_request.golden.txt"),
		renderReflectionRequest(t, params),
	)
}

// loadAPIGolden reads a golden under testdata.
func loadAPIGolden(t *testing.T, name string) string {
	t.Helper()

	root, err := os.OpenRoot("testdata")
	require.NoError(t, err)
	defer func() { _ = root.Close() }()

	contents, err := root.ReadFile(name)
	require.NoError(t, err)

	return string(contents)
}

// reflectionWindowCase is one conversation a reflection event can
// belong to.
type reflectionWindowCase struct {
	Name   string
	Target protocol.WindowTarget
}

// reflectionWindowEffect is the label the request carries for a case.
type reflectionWindowEffect struct {
	Kind ReflectionWindowKind
}

// TestReflectionWindowKindFor covers both labels against the sealed
// target, which is the whole of what a reflection event can name.
func TestReflectionWindowKindFor(t *testing.T) {
	cases := []reflectionWindowCase{
		{Name: "a channel", Target: protocol.ChannelWindowTarget("#dev")},
		{Name: "a direct message", Target: protocol.DirectWindowTarget("inst-alice")},
	}
	want := []reflectionWindowEffect{
		{Kind: ReflectionWindowChannel},
		{Kind: ReflectionWindowDirect},
	}

	got := make([]reflectionWindowEffect, 0, len(cases))
	for _, testCase := range cases {
		t.Run(testCase.Name, func(*testing.T) {
			got = append(got, reflectionWindowEffect{
				Kind: ReflectionWindowKindFor(testCase.Target),
			})
		})
	}

	require.Equal(t, want, got)
}
