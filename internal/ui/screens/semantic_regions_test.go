package screens_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestChatScreen_semantic_regions_expose_sidebar_and_chat_content(t *testing.T) {
	sess := newTestSession(t)
	uitest.SeedChannel(t, sess, "#general")
	uitest.SeedChannel(t, sess, "#random")

	tm := newChatApp(t, sess)
	// Wait for the startup focus-restore to settle so the rendered
	// content column carries the banner from the active channel.
	tm.WaitFor("Created channel #random")

	body, status := uitest.SplitBodyAndStatus(tm.CurrentView())
	columns := uitest.VisibleColumns(body)

	got := make([][]string, len(columns))
	for i, col := range columns {
		col = uitest.NonEmptyColumn(col)
		got[i] = make([]string, len(col))
		for j, line := range col {
			got[i][j] = uitest.CompactLine(line)
		}
	}

	require.Equal(t, [][]string{
		{"Channels", "#general", "▸#random"},
		{
			"*** Created channel #random",
			"*** ChanServ sets mode +o testuser",
			"testuser >",
		},
		{"Nicks", "@testuser"},
	}, got, "the chat layout must expose three semantic columns: sidebar, chat content, nicks")

	tokens := strings.Fields(status)
	require.Subset(t, tokens, []string{"^D/^U", "^O", "↵", "^W", "^C"},
		"status bar must surface core navigation, submit and quit bindings")
}
