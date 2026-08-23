// Package ui contains the components that make up the modeloff TUI.
// Root is the only Bubble Tea model and delegates to an internal component.
package ui

import (
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// Component is a stateful part of the UI's update and drawing tree.
type Component interface {
	// Init returns any command the component needs when it is first attached.
	Init() tea.Cmd

	// Update handles a message and returns the replacement component and any
	// command it produced.
	Update(msg tea.Msg) (Component, tea.Cmd)

	// Draw renders the component into area on a shared screen buffer.
	Draw(screen uv.Screen, area uv.Rectangle)
}
