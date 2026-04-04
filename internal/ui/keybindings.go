package ui

import "github.com/charmbracelet/bubbles/key"

// Keybinding is implemented by models that want to contribute
// keybindings to the active help area.
type Keybinding interface {
	KeyBindings() []key.Binding
}

// CollectKeyBindings walks the provided child models in order and
// returns the keybindings contributed by those that implement
// Keybinding.
func CollectKeyBindings(models ...Model) []key.Binding {
	var bindings []key.Binding

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
func ActiveKeyBindings(bindings []key.Binding) []key.Binding {
	seen := map[string]struct{}{}
	active := make([]key.Binding, 0, len(bindings))

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
func WithBindingEnabled(binding key.Binding, enabled bool) key.Binding {
	bindingCopy := binding
	bindingCopy.SetEnabled(enabled)

	return bindingCopy
}
