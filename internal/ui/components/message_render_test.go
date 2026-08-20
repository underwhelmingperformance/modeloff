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
	invited := domain.Invited{Target: "#test", Nick: "alice", By: "laney", At: at}

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

func TestChannelModeChangeText(t *testing.T) {
	at := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	tests := map[string]struct {
		event domain.ChannelModeChange
		want  string
	}{
		"boolean": {
			event: domain.ChannelModeChange{
				Target: "#dev", Flag: domain.ModeInviteOnly, Add: true, By: "laney", At: at,
			},
			want: "laney sets mode +i on #dev",
		},
		"member": {
			event: domain.ChannelModeChange{
				Target: "#dev", Nick: "botty", Flag: domain.ModeOperator, Add: true, By: "laney", At: at,
			},
			want: "laney sets mode +o botty on #dev",
		},
		"parametric": {
			event: domain.ChannelModeChange{
				Target: "#dev", Param: "20", Flag: domain.ModeUserLimit, Add: true, By: "laney", At: at,
			},
			want: "laney sets mode +l 20 on #dev",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := renderChannelEvent[testKind](
				tc.event,
				domain.KindChannel,
				80,
				nil,
				"testuser",
				nil,
				noTimestamp(),
				language.BritishEnglish,
			)

			require.Equal(t, "*** "+tc.want, stripLine(got))
		})
	}
}

func TestRenderWhoisEvent_human_user_has_no_dangling_line(t *testing.T) {
	at := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	whois := domain.Whois{
		Target: "#dev",
		Nick:   "laney",
		At:     at,
	}

	want := "*** laney is the human user"
	require.Equal(t, want, stripWhois(renderWhoisEvent(whois)))
}

func TestRenderMessage_anonymous_body(t *testing.T) {
	message := domain.Message{
		Target: "#dev", From: domain.AnonymousNick, InstanceID: "alice-instance",
		Body: "hi", At: time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC),
	}

	got := renderMessage(message, nil, "testuser", noTimestamp(), language.BritishEnglish)
	require.Equal(t, "<anonymous> hi", stripLine(got))
}

// TestNickStyleFor_anonymous_lines_share_one_colour and its sibling
// below assert on [nickStyleFor]'s returned [lipgloss.Style] rather
// than on a rendered string. lipgloss disables colour output
// entirely when its renderer detects no terminal, which is always
// the case under `go test`, so a rendered line carries no ANSI codes
// to compare and cannot show whether the colour varies by sender.
func TestNickStyleFor_anonymous_lines_share_one_colour(t *testing.T) {
	alice := domain.Message{From: domain.AnonymousNick, InstanceID: "alice-instance"}
	bob := domain.Message{From: domain.AnonymousNick, InstanceID: "bob-instance"}

	require.Equal(t, nickStyleFor(alice).GetForeground(), nickStyleFor(bob).GetForeground(),
		"two anonymous senders with different instance ids must share one nick colour")
}

func TestNickStyleFor_named_lines_vary_by_instance(t *testing.T) {
	alice := domain.Message{From: "alice", InstanceID: "alice-instance"}
	bob := domain.Message{From: "bob", InstanceID: "bob-instance"}

	require.NotEqual(t, nickStyleFor(alice).GetForeground(), nickStyleFor(bob).GetForeground())
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
