package components_test

import (
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
		Channels: []domain.Channel{
			{Name: "#general", Kind: domain.KindChannel},
			{Name: "#random", Kind: domain.KindChannel},
		},
		Active: "#random",
		Unread: map[domain.ChannelName]int{"#general": 2},
	})

	chat := newChatViewWithEvents("#random", "testuser", "", []domain.StoredEvent{
		{Event: domain.Message{Target: "#random", From: "alice", Body: "hello", At: time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)}},
		{Event: domain.Message{Target: "#random", From: "botty", Body: "hi there", At: time.Date(2025, 1, 1, 10, 1, 0, 0, time.UTC)}},
	})

	nicklist := components.NewNickList(members(
		member("alice", domain.ModeOp),
		member("botty", domain.ModeVoice),
	))

	layout := components.NewMainLayout(sidebarModel.(components.ChannelSidebar), chat)
	layout.NickList = nicklist

	columns := visibleColumns(layout.View(120, 10))
	require.Equal(t, 3, len(columns))

	require.Equal(t, []string{"Channels", "#general (2)", "▸#random"}, nonEmptyColumn(columns[0]))

	content := make([]string, 0, len(nonEmptyColumn(columns[1])))
	for _, line := range nonEmptyColumn(columns[1]) {
		content = append(content, uitest.CompactLine(line))
	}

	require.Equal(t, []string{
		"[10:00:00] <alice> hello",
		"[10:01:00] <botty> hi there",
		"testuser >",
	}, content)

	require.Equal(t, []string{"Nicks", "@alice", "+botty"}, nonEmptyColumn(columns[2]))
}
