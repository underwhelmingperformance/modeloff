package uitest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderTerminal_replays_full_and_diff_frames(t *testing.T) {
	output := []byte("\x1b[?1049h\x1b[H\x1b[2Jalpha\nbeta\ngamma\r\x1bMchanged")

	view, err := renderTerminal(output, 20, 3)
	require.NoError(t, err)
	require.Equal(t, "alpha\nchanged\ngamma", view)
}

func TestRenderTerminal_applies_line_insertion(t *testing.T) {
	output := []byte("\x1b[?1049h\x1b[H\x1b[2Jalpha\nbeta\ngamma\x1b[2;1H\x1b[Linserted")

	view, err := renderTerminal(output, 20, 4)
	require.NoError(t, err)
	require.Equal(t, "alpha\ninserted\nbeta\ngamma", view)
}

func TestRenderTerminal_uses_the_alternate_screen(t *testing.T) {
	output := []byte("shell\x1b[?1049h\x1b[H\x1b[2Japp")

	view, err := renderTerminal(output, 20, 2)
	require.NoError(t, err)
	require.Equal(t, "app", view)
}
