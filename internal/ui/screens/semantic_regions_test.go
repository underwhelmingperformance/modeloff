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

	body, status := splitBodyAndStatus(tm.CurrentView())
	columns := uitest.VisibleColumns(body)
	require.Equal(t, 3, len(columns))

	require.Equal(t, []string{"Channels", "#general", "▸#random"}, uitest.NonEmptyColumn(columns[0]))

	content := make([]string, 0)
	for _, line := range uitest.NonEmptyColumn(columns[1]) {
		content = append(content, uitest.CompactLine(line))
	}
	require.Equal(t, []string{
		"*** Created channel #random",
		"*** ChanServ sets mode +o testuser",
		"testuser >",
	}, content)

	require.Equal(t, []string{"Nicks", "@testuser"}, uitest.NonEmptyColumn(columns[2]))

	tokens := strings.Fields(status)
	require.Subset(t, tokens, []string{"^D/^U", "^O", "↵", "^W", "^C"},
		"status bar must surface core navigation, submit and quit bindings")
}
