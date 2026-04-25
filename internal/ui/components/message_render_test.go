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
