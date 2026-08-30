package components

import (
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/laney/modeloff/internal/ui"
)

// TestKeyMapsListEveryBinding pins that each keymap's Bindings method
// returns every field the struct declares. A component advertises its
// keys from that list, so a field missing from the list is a key the
// component handles but neither the status bar nor the help view ever
// shows.
//
// Reflection belongs here and not in Bindings itself: the chat screen
// rebuilds its binding list on every frame, and walking a struct's
// fields per frame costs more than asserting once that the
// hand-written list holds every declared field.
func TestKeyMapsListEveryBinding(t *testing.T) {
	maps := map[string]struct {
		value    any
		bindings []ui.KeyBinding
	}{
		"rich textarea": {DefaultRichTextareaKeyMap, DefaultRichTextareaKeyMap.Bindings()},
		"input bar":     {DefaultInputBarKeyMap, DefaultInputBarKeyMap.Bindings()},
		"chat view":     {DefaultChatViewKeyMap, DefaultChatViewKeyMap.Bindings()},
		"colour palette": {
			DefaultColourPaletteKeyMap,
			DefaultColourPaletteKeyMap.Bindings(),
		},
	}

	for name, km := range maps {
		t.Run(name, func(t *testing.T) {
			declared := reflect.TypeOf(km.value)

			listed := map[string]bool{}
			for _, binding := range km.bindings {
				listed[binding.Help().Key] = true
			}

			var missing []string
			for i := range declared.NumField() {
				field := reflect.ValueOf(km.value).Field(i).Interface().(ui.KeyBinding)
				if !listed[field.Help().Key] {
					missing = append(missing, declared.Field(i).Name)
				}
			}

			require.Empty(t, missing, "Bindings omits fields the keymap declares")
			require.Len(t, km.bindings, declared.NumField(),
				"Bindings and the struct disagree about how many bindings there are")
		})
	}
}

// TestInputBarAdvertisesEveryEditorBinding pins that the bar names
// every editing key the operator can reach through it. The two
// exclusions are the ones the bar and the editor compute for
// themselves: a key the bar matches before the editor sees it, and a
// key the editor's own configuration makes inert.
func TestInputBarAdvertisesEveryEditorBinding(t *testing.T) {
	bar := NewInputBar("testuser")

	advertised := map[string]bool{}
	for _, binding := range bar.KeyBindings() {
		advertised[binding.Help().Key] = true
	}

	claimed := bar.claimedKeys()

	var missing []string
	for _, binding := range bar.input.Bindings() {
		if shadowed(binding, claimed) {
			continue
		}
		if !advertised[binding.Help().Key] {
			missing = append(missing, binding.Help().Key)
		}
	}

	require.Empty(t, missing, "the bar routes these editor keys and names none of them")
}

// TestInputBarAdvertisesThePaletteItRoutes pins the same property for
// the colour palette, which is a mode with its own keys: while it is
// open the bar offers those and nothing else.
func TestInputBarAdvertisesThePaletteItRoutes(t *testing.T) {
	var bar ui.Component = NewInputBar("testuser")
	bar, _ = bar.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})

	require.Equal(t, DefaultColourPaletteKeyMap.Bindings(),
		bar.(InputBar).KeyBindings())
}
