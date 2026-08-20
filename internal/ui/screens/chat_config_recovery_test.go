package screens

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
)

// TestChatScreen_Init_surfaces_config_recovery_notice pins finding 5:
// main.go's loadConfig moves an unreadable config.json aside before
// the session or the chat screen exists, so it cannot report the
// recovery over the protocol bus the way Welcome or Reconnected do.
// WithConfigRecoveryNotice carries the backup path into the screen,
// and Init renders it into `&modeloff` once, so the user learns their
// config was reset there too, not only from main.go's stderr message
// and log line.
func TestChatScreen_Init_surfaces_config_recovery_notice(t *testing.T) {
	const backupPath = "/home/user/.config/modeloff/config.json.corrupt-20260101T000000Z"

	screen := newScreenFixture(t).WithConfigRecoveryNotice(backupPath)

	// The notice is appended synchronously, before Init builds its
	// returned Cmd batch; the batch itself is not run here since it
	// includes the protocol- and config-change listeners, which block
	// reading their channels until a live session delivers something.
	screen.Init()

	require.Equal(t, []string{
		"your config file could not be read; it was backed up to " + backupPath + " and defaults were used",
	}, scrollbackSystemNotices(screen.scrollbackOf(domain.StatusChannelName)))
}

// TestChatScreen_Init_without_config_recovery_shows_no_notice pins
// the no-op path: a normal config.json load must not add a notice
// nobody asked for.
func TestChatScreen_Init_without_config_recovery_shows_no_notice(t *testing.T) {
	screen := newScreenFixture(t)

	screen.Init()

	require.Empty(t, scrollbackSystemNotices(screen.scrollbackOf(domain.StatusChannelName)))
}
