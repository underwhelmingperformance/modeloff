package screens_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestChatScreen_semantic_regions_expose_sidebar_and_chat_content(t *testing.T) {
	t.Skip("Pending MessageList redesign: same focus/bus race as the rejoin" +
		" test — `HistoryLoadedMsg` snapshots can land out of order with" +
		" live-appended events, leaving the chat region missing some" +
		" of the channel's content. The fix is to remove" +
		" `HistoryLoadedMsg`/`loadHistory` and have MessageList read" +
		" scrollback through a getter.")

	h := newTestSession(t)
	user := h.user
	uitest.SeedChannel(t, user, "#general")
	uitest.SeedChannel(t, user, "#random")

	tm := newChatApp(t, h)
	// Wait for the channel-creation scrollback line so the join's
	// initial render has fully landed before the snapshot is taken.
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
		{"Channels", "&modeloff", "#general", "▸#random"},
		{
			"*** Created channel #random",
			"testuser >",
		},
		{"Nicks", "@testuser"},
	}, got, "the chat layout must expose three semantic columns: sidebar, chat content, nicks")

	tokens := strings.Fields(status)
	require.Subset(t, tokens, []string{"^D/^U", "^O", "↵", "^W", "^C"},
		"status bar must surface core navigation, submit and quit bindings")
}
