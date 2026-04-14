package ui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/key"
)

// KeyBinding wraps the upstream key.Binding with additional UI state
// such as whether the bound action is currently active (e.g. bold
// formatting is on).
type KeyBinding struct {
	key.Binding

	// Active indicates the bound action is currently engaged. The
	// status bar renders active bindings in bold.
	Active bool
}

// Bind wraps a key.Binding in a KeyBinding.
func Bind(b key.Binding) KeyBinding {
	return KeyBinding{Binding: b}
}

// Matches reports whether a key message matches any of the given
// KeyBindings, delegating to the upstream key.Matches.
func Matches[K fmt.Stringer](k K, bindings ...KeyBinding) bool {
	inner := make([]key.Binding, len(bindings))
	for i, b := range bindings {
		inner[i] = b.Binding
	}

	return key.Matches(k, inner...)
}

// Keybinding is implemented by models that want to contribute
// keybindings to the active help area.
type Keybinding interface {
	KeyBindings() []KeyBinding
}

// CollectKeyBindings walks the provided child models in order and
// returns the keybindings contributed by those that implement
// Keybinding.
func CollectKeyBindings(models ...Model) []KeyBinding {
	var bindings []KeyBinding

	for _, model := range models {
		contributor, ok := model.(Keybinding)
		if !ok {
			continue
		}

		bindings = append(bindings, contributor.KeyBindings()...)
	}

	return bindings
}

// ActiveKeyBindings filters out disabled bindings and removes
// duplicate help entries while preserving order.
func ActiveKeyBindings(bindings []KeyBinding) []KeyBinding {
	seen := map[string]struct{}{}
	active := make([]KeyBinding, 0, len(bindings))

	for _, binding := range bindings {
		if !binding.Enabled() {
			continue
		}

		help := binding.Help()
		if help.Key == "" && help.Desc == "" {
			continue
		}

		label := help.Key + "\x00" + help.Desc
		if _, ok := seen[label]; ok {
			continue
		}

		seen[label] = struct{}{}
		active = append(active, binding)
	}

	return active
}

// WithBindingEnabled returns a copy of the binding with its enabled
// state set to the provided value.
func WithBindingEnabled(binding KeyBinding, enabled bool) KeyBinding {
	bindingCopy := binding
	bindingCopy.SetEnabled(enabled)

	return bindingCopy
}

// WithBindingActive returns a copy of the binding with its Active
// state set to the provided value.
func WithBindingActive(binding KeyBinding, active bool) KeyBinding {
	binding.Active = active

	return binding
}
