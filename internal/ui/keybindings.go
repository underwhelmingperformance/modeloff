package ui

import (
	"fmt"
	"unicode"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// KeyBinding wraps the upstream key.Binding with additional UI state
// such as whether the bound action is currently active (e.g. bold
// formatting is on).
type KeyBinding struct {
	key.Binding

	// Active indicates the bound action is currently engaged. The
	// status bar renders active bindings in bold.
	Active bool

	// HelpGroup places the binding in the corresponding section of
	// the keyboard-help view.
	HelpGroup KeyHelpGroup

	// HintPriority controls whether and how prominently the status
	// bar presents the binding. KeyHintNone keeps the binding in the
	// keyboard-help view without using status-bar space.
	HintPriority KeyHintPriority
}

// KeyHelpGroup identifies a section in the keyboard-help view.
type KeyHelpGroup string

// Keyboard-help sections, in the order the help view renders them.
const (
	KeyHelpGeneral     KeyHelpGroup = "General"
	KeyHelpNavigation  KeyHelpGroup = "Navigation"
	KeyHelpMessaging   KeyHelpGroup = "Messaging"
	KeyHelpEditing     KeyHelpGroup = "Editing"
	KeyHelpFormatting  KeyHelpGroup = "Formatting"
	KeyHelpCompletion  KeyHelpGroup = "Completion"
	KeyHelpPersona     KeyHelpGroup = "Persona"
	KeyHelpPanels      KeyHelpGroup = "Panels"
	KeyHelpApplication KeyHelpGroup = "Application"
)

// KeyHintPriority controls a binding's status-bar precedence.
type KeyHintPriority uint8

// Status-bar hint priorities. A larger value takes precedence when
// the bar cannot show every eligible binding.
const (
	KeyHintNone KeyHintPriority = iota
	KeyHintLow
	KeyHintNormal
	KeyHintHigh
	KeyHintEssential
)

// Bind wraps a key.Binding in a KeyBinding.
func Bind(b key.Binding) KeyBinding {
	return KeyBinding{
		Binding:      b,
		HelpGroup:    KeyHelpGeneral,
		HintPriority: KeyHintNormal,
	}
}

// WithHelpMetadata returns a copy assigned to a keyboard-help group
// and a status-bar hint priority.
func (b KeyBinding) WithHelpMetadata(group KeyHelpGroup, priority KeyHintPriority) KeyBinding {
	b.HelpGroup = group
	b.HintPriority = priority

	return b
}

// Matches reports whether a key message matches any of the given
// KeyBindings, delegating to the upstream key.Matches.
//
// A modified letter matches one spelling however the terminal encodes
// the shift. Holding shift on M-b reaches the application as
// "alt+shift+b" under the Kitty keyboard protocol and as "alt+B" under
// xterm's modifyOtherKeys, and both mean the chord the binding spells
// "alt+b". Shift alone is not folded away: with no alt or ctrl, an
// upper-case code is the character the operator typed. A handler that
// goes on to test the shift modifier wants [NormaliseModifiedLetter],
// which sets the modifier that Matches drops.
func Matches[K fmt.Stringer](k K, bindings ...KeyBinding) bool {
	inner := make([]key.Binding, len(bindings))
	for i, b := range bindings {
		inner[i] = b.Binding
	}

	if key.Matches(k, inner...) {
		return true
	}

	folded, ok := foldModifiedLetter(k)

	return ok && key.Matches(folded, inner...)
}

// foldModifiedLetter rewrites a letter modified by alt or ctrl to its
// lower-case, unshifted spelling, and reports whether that changed
// anything. Only letters are folded, because only a letter has a case
// to fold: shift on a digit or a punctuation key produces a different
// character, not another case of the same character.
func foldModifiedLetter(k any) (tea.KeyPressMsg, bool) {
	msg, ok := k.(tea.KeyPressMsg)
	if !ok {
		return tea.KeyPressMsg{}, false
	}

	if msg.Mod&(tea.ModAlt|tea.ModCtrl) == 0 || !unicode.IsLetter(msg.Code) {
		return tea.KeyPressMsg{}, false
	}

	folded := msg
	folded.Code = unicode.ToLower(msg.Code)
	folded.Mod &^= tea.ModShift

	return folded, folded != msg
}

// NormaliseModifiedLetter rewrites a letter modified by alt or ctrl into
// one spelling: a lower-case code, with the shift modifier set when
// the terminal reported the shift as a capital letter and not as a
// modifier. A handler that tests the shift modifier therefore gets the
// same result whichever spelling its terminal sends, and [Matches]
// still matches the binding, which spells the chord without the
// shift.
func NormaliseModifiedLetter(msg tea.KeyPressMsg) tea.KeyPressMsg {
	if msg.Mod&(tea.ModAlt|tea.ModCtrl) == 0 || !unicode.IsUpper(msg.Code) {
		return msg
	}

	msg.Code = unicode.ToLower(msg.Code)
	msg.Mod |= tea.ModShift

	return msg
}

// Keybinding is implemented by components that contribute keybindings to
// input handling, the status bar and keyboard help.
type Keybinding interface {
	KeyBindings() []KeyBinding
}

// CollectKeyBindings walks the provided child components in order and
// returns the keybindings contributed by those that implement
// Keybinding.
func CollectKeyBindings(components ...Component) []KeyBinding {
	var bindings []KeyBinding

	for _, component := range components {
		contributor, ok := component.(Keybinding)
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
