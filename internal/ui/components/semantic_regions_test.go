package components_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/domain"
	"github.com/laney/modeloff/internal/ui/components"
	"github.com/laney/modeloff/internal/ui/uitest"
)

func TestMainLayout_semantic_regions_expose_rendered_sidebar_chat_and_nicklist(t *testing.T) {
	sidebar := components.NewChannelSidebar()
	sidebarModel, _ := sidebar.Update(components.SetChannelsMsg{
		Channels: []domain.Window{
			domain.NewChannelWindow("#general", time.Time{}),
			domain.NewChannelWindow("#random", time.Time{}),
		},
		Active: "#random",
		Unread: map[domain.ChannelName]int{"#general": 2},
	})

	chat := newChatViewWithEvents("#random", "testuser", "", []domain.Event{
		domain.Message{Source: domain.LegacyClientSource("alice"), Target: "#random", Body: "hello", At: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)},
		domain.Message{Source: domain.LegacyClientSource("botty"), Target: "#random", Body: "hi there", At: time.Date(2025, 1, 1, 10, 1, 0, 0, time.UTC)},
	})

	nicklist := components.NewNickList(members(
		member("alice", op),
		member("botty", voiced),
	))

	layout := components.NewMainLayout(sidebarModel.(components.ChannelSidebar), chat)
	layout.NickList = nicklist

	columns := visibleColumns(renderToBuffer(layout, 120, 10))

	got := make([][]string, len(columns))
	for i, col := range columns {
		col = nonEmptyColumn(col)
		got[i] = make([]string, len(col))
		for j, line := range col {
			got[i][j] = uitest.CompactLine(line)
		}
	}

	// The chat column's second line is the window header's border
	// rule: a run of box-drawing dashes spanning the column's full
	// width, which varies with the terminal width MainLayout leaves
	// the chat pane after the sidebar and nick list.
	require.Equal(t, [][]string{
		{"Channels", "#general (2)", "▸#random"},
		{
			"#random",
			strings.Repeat("─", 93),
			"[10:00:00] <alice> hello",
			"[10:01:00] <botty> hi there",
			"testuser >",
		},
		{"Nicks", "@alice", "+botty"},
	}, got, "MainLayout must expose three semantic columns: sidebar, chat content, nick list")
}
