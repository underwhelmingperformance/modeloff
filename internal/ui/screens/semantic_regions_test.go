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
	// Wait for the ChanServ-mode line too — that's the last
	// protocol-bus event in the user-join sequence, so its presence
	// guarantees the join → mode-change pair has fully rendered
	// before the snapshot is taken.
	tm.WaitFor("Created channel #random", "ChanServ sets mode +o testuser")

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
		{"Channels", "&modeloff", "#general", "▸#random"},
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
