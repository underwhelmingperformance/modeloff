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

	message := domain.ChannelMessage{Channel: "#test", From: "alice", Body: "hello", At: at}
	notice := domain.ChannelSystemNotice{Channel: "#test", Text: "OpenRouter API key saved.", At: at}
	join := domain.ChannelJoin{Channel: "#test", Nick: "alice", At: at}

	tests := map[string]struct {
		kind  domain.ChannelKind
		event domain.ChannelEvent
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

func TestNickColourSeed_prefers_instance_id(t *testing.T) {
	tests := []struct {
		name string
		id   domain.InstanceID
		nick domain.Nick
		want string
	}{
		{name: "id present uses id", id: "abc123", nick: "alice", want: "abc123"},
		{name: "rename keeps id-derived seed", id: "abc123", nick: "bob", want: "abc123"},
		{name: "empty id falls back to nick", id: "", nick: "alice", want: "alice"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, nickColourSeed(tt.id, tt.nick))
		})
	}
}

func TestRenderWhoisEvent_uses_stored_snapshot(t *testing.T) {
	at := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	whois := domain.ChannelWhois{
		Channel:  "#dev",
		Nick:     "alice",
		ModelID:  "anthropic/claude-3-haiku",
		Persona:  "a cheerful pirate",
		Channels: []domain.ChannelName{"#dev", "#help"},
		At:       at,
	}

	rendered := stripLine(renderWhoisEvent(whois))
	require.Contains(t, rendered, "alice is anthropic/claude-3-haiku")
	require.Contains(t, rendered, "persona: a cheerful pirate")
	require.Contains(t, rendered, "channels: #dev, #help")
}

func TestRenderWhoisEvent_snapshot_takes_precedence_over_live_instance(t *testing.T) {
	at := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	// A mutable instance whose live state will diverge from the
	// snapshot. The renderer must ignore it when snapshot fields are
	// populated, exactly to keep historical /whois lines immutable.
	live := domain.NewModelInstance("inst-1", "bob", "anthropic/claude-3-haiku", "a grumpy parrot", nil)

	whois := domain.ChannelWhois{
		Channel:  "#dev",
		Nick:     "alice",
		ModelID:  "anthropic/claude-3-haiku",
		Persona:  "a cheerful pirate",
		Channels: []domain.ChannelName{"#dev"},
		Instance: live,
		At:       at,
	}

	rendered := stripLine(renderWhoisEvent(whois))
	require.Contains(t, rendered, "alice is anthropic/claude-3-haiku",
		"snapshot Nick must beat the live Instance.Nick()")
	require.Contains(t, rendered, "persona: a cheerful pirate",
		"snapshot Persona must beat the live Instance.Persona()")
	require.NotContains(t, rendered, "bob")
	require.NotContains(t, rendered, "grumpy parrot")

	// Renaming after emission must not retroactively rewrite the
	// rendered line — the snapshot is frozen.
	live.SetNick("carol")
	live.SetPersona("a relentlessly chatty bot")

	require.Equal(t, rendered, stripLine(renderWhoisEvent(whois)),
		"snapshot whois render must not change when the underlying Instance mutates")
}

func TestRenderWhoisEvent_legacy_instance_fallback(t *testing.T) {
	at := time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC)

	inst := domain.NewModelInstance("inst-1", "alice", "anthropic/claude-3-haiku", "a cheerful pirate", nil)

	whois := domain.ChannelWhois{
		Channel:  "#dev",
		Instance: inst,
		At:       at,
	}

	rendered := stripLine(renderWhoisEvent(whois))
	require.Contains(t, rendered, "alice is anthropic/claude-3-haiku")
	require.Contains(t, rendered, "persona: a cheerful pirate")
}

func TestRenderChannelEvent_system_notice_style_changes_by_kind(t *testing.T) {
	notice := domain.ChannelSystemNotice{
		Channel: "#test",
		Text:    "OpenRouter API key saved.",
		At:      time.Date(2026, 4, 19, 10, 0, 0, 0, time.UTC),
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

	channelRendered := render(domain.KindChannel)
	dmRendered := render(domain.KindDM)
	statusRendered := render(domain.KindStatus)

	require.NotEqual(t, channelRendered, statusRendered,
		"system notice rendering must differ between KindChannel and KindStatus")
	require.Equal(t, channelRendered, dmRendered,
		"DM must render system notices identically to a regular channel")
	require.Contains(t, stripLine(channelRendered), "✓")
	require.Contains(t, stripLine(statusRendered), "***")
}
