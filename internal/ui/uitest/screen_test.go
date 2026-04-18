package uitest

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVirtualScreen_replays_diff_frames_into_full_view(t *testing.T) {
	tests := []struct {
		name string
		feed []string
		want string
	}{
		{
			name: "single frame writes lines as-is",
			feed: []string{
				"\rline one\r\nline two",
			},
			want: "line one\nline two",
		},
		{
			name: "second frame with skipped lines preserves prior content",
			feed: []string{
				// First frame: \r then three lines separated by \r\n,
				// matching the bubbletea standard renderer's per-line
				// emission. The trailing \r repositions the cursor to
				// column zero before the next frame starts.
				"\rline one\x1b[K\r\nline two\x1b[K\r\nline three\x1b[K\r",
				// Second frame: cursor-up two to land on row zero,
				// then \n to skip the unchanged first line, then
				// rewrite the middle line with EraseLineRight, then
				// \r\n + skip + trailing \r to end the frame.
				"\x1b[2A\nupdated two\x1b[K\r\n\r",
			},
			want: "line one\nupdated two\nline three",
		},
		{
			name: "erase line right truncates from cursor",
			feed: []string{
				"hello world",
				"\rhello\x1b[K",
			},
			want: "hello",
		},
		{
			name: "erase entire line clears the row",
			feed: []string{
				"keep me",
				"\r\x1b[2K",
			},
			want: "",
		},
		{
			name: "cursor home repositions for full repaint",
			feed: []string{
				"\rfirst\r\nsecond",
				"\x1b[Hnew\x1b[K\r\nALSO new\x1b[K",
			},
			want: "new\nALSO new",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newVirtualScreen()
			for _, chunk := range tt.feed {
				s.feed([]byte(chunk))
			}

			require.Equal(t, tt.want, s.view())
		})
	}
}

func TestVirtualScreen_strips_sgr_escapes_from_state_machine(t *testing.T) {
	s := newVirtualScreen()
	s.feed([]byte("\x1b[31mred\x1b[0m text"))

	require.Equal(t, "red text", s.view())
}
