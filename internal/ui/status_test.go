package ui_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
)

type statusStub struct {
	items []ui.StatusItem
}

func (s statusStub) StatusItems() []ui.StatusItem {
	return s.items
}

func TestCollectStatusItems_preserves_provider_order(t *testing.T) {
	items := ui.CollectStatusItems(
		statusStub{items: []ui.StatusItem{{ID: "left"}}},
		"ignored",
		statusStub{items: []ui.StatusItem{{ID: "right-1"}, {ID: "right-2"}}},
	)

	require.Equal(t, []ui.StatusItem{
		{ID: "left"},
		{ID: "right-1"},
		{ID: "right-2"},
	}, items)
}
