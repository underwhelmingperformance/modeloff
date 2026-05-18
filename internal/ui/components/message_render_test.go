package components

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/language"

	"github.com/laney/modeloff/internal/domain"
)

// testKind is a minimal [command.KindProvider] for tests in the
// internal components package. The black-box components_test package
// defines its own identically-named type.
type testKind domain.ChannelKind

func (k testKind) ChannelKind() domain.ChannelKind { return domain.ChannelKind(k) }

// noTimestamp disables the timestamp prefix so rendered-line assertions
// focus on the body shape, not on locale-dependent date formatting.
func noTimestamp() *string {
	empty := ""
	return &empty
}

func stripLine(s string) string {
	return strings.TrimRight(ansi.Strip(s), " ")
}

func TestRenderChannelEvent_by_kind(t *testing.T) {
	at := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	message := domain.Message{Target: "#test", From: "alice", Body: "hello", At: at}
	notice := domain.SystemNotice{Target: "#test", Text: "OpenRouter API key saved.", At: at}
	join := domain.Join{Target: "#test", Nick: "alice", At: at}
	invited := domain.ModelInvited{Target: "#test", Nick: "alice", By: "laney", At: at}

	tests := map[string]struct {
		kind  domain.ChannelKind
		event domain.PersistableEvent
		want  string
	}{
		"channel message":         {kind: domain.KindChannel, event: message, want: "<alice> hello"},
		"dm message":              {kind: domain.KindDM, event: message, want: "<alice> hello"},
		"status message":          {kind: domain.KindStatus, event: message, want: "<alice> hello"},
		"channel system notice":   {kind: domain.KindChannel, event: notice, want: "✓ OpenRouter API key saved."},
		"dm system notice":        {kind: domain.KindDM, event: notice, want: "✓ OpenRouter API key saved."},
		"status system notice":    {kind: domain.KindStatus, event: notice, want: "*** OpenRouter API key saved."},
		"channel join on channel": {kind: domain.KindChannel, event: join, want: "*** alice has joined #test"},
		"channel join on dm":      {kind: domain.KindDM, event: join, want: "*** alice has joined #test"},
		"channel join on status":  {kind: domain.KindStatus, event: join, want: "*** alice has joined #test"},
		"invite renders as invitation, not as join": {
			kind:  domain.KindChannel,
			event: invited,
			want:  "*** laney invited alice to #test",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := renderChannelEvent[testKind](
				tc.event,
				tc.kind,
				80,
				nil,
				"testuser",
				nil,
				noTimestamp(),
				language.BritishEnglish,
			)

			require.Equal(t, tc.want, stripLine(got))
		})
	}
}

func TestRenderWhoisEvent_uses_stored_snapshot(t *testing.T) {
	at := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	whois := domain.Whois{
		Target:   "#dev",
		Nick:     "alice",
		ModelID:  "anthropic/claude-3-haiku",
		Persona:  "a cheerful pirate",
		Channels: []domain.ChannelName{"#dev", "#help"},
		At:       at,
	}

	want := strings.Join([]string{
		"*** alice is anthropic/claude-3-haiku",
		"***   persona: a cheerful pirate",
		"***   channels: #dev, #help",
	}, "\n")
	require.Equal(t, want, stripWhois(renderWhoisEvent(whois)))
}

// stripWhois strips ANSI from a multi-line whois render and trims
// trailing whitespace from each line, since lipgloss may pad lines.
func stripWhois(s string) string {
	lines := strings.Split(ansi.Strip(s), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}

	return strings.Join(lines, "\n")
}

func TestRenderChannelEvent_system_notice_style_changes_by_kind(t *testing.T) {
	notice := domain.SystemNotice{
		Target: "#test",
		Text:   "OpenRouter API key saved.",
		At:     time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC),
	}

	render := func(kind domain.ChannelKind) string {
		return renderChannelEvent[testKind](
			notice,
			kind,
			80,
			nil,
			"testuser",
			nil,
			noTimestamp(),
			language.BritishEnglish,
		)
	}

	require.Equal(t, "✓ OpenRouter API key saved.", stripLine(render(domain.KindChannel)))
	require.Equal(t, "✓ OpenRouter API key saved.", stripLine(render(domain.KindDM)))
	require.Equal(t, "*** OpenRouter API key saved.", stripLine(render(domain.KindStatus)))
}
