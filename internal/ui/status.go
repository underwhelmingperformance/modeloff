package ui

// StatusSide determines where a status item is rendered in the bar.
type StatusSide int

const (
	// StatusSideLeft renders the item flush-left in the status bar.
	StatusSideLeft StatusSide = iota

	// StatusSideRight renders the item flush-right in the status bar.
	StatusSideRight
)

// StatusItem is a renderable status-bar contribution.
type StatusItem struct {
	ID       string
	Side     StatusSide
	Priority int
	Full     string
	Compact  string
}

// StatusProvider is implemented by models or helpers that expose
// status-bar items.
type StatusProvider interface {
	StatusItems() []StatusItem
}

// CollectStatusItems returns the status items contributed by the
// provided providers in order.
func CollectStatusItems(providers ...any) []StatusItem {
	var items []StatusItem

	for _, provider := range providers {
		contributor, ok := provider.(StatusProvider)
		if !ok {
			continue
		}

		items = append(items, contributor.StatusItems()...)
	}

	return items
}
