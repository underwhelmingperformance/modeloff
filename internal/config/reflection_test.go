package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type reflectionModeParseCase struct {
	Name     string
	Input    string
	Mode     ReflectionMode
	Resolved ReflectionMode
	Rendered string
	Invalid  bool
}

func TestParseReflectionMode_accepts_only_the_configured_modes(t *testing.T) {
	cases := []reflectionModeParseCase{
		{
			Name: "absent", Input: "",
			Mode: ReflectionUnset, Resolved: DefaultReflectionMode, Rendered: "active",
		},
		{
			Name: "disabled", Input: "disabled",
			Mode: ReflectionDisabled, Resolved: ReflectionDisabled, Rendered: "disabled",
		},
		{
			Name: "shadow", Input: " SHADOW ",
			Mode: ReflectionShadow, Resolved: ReflectionShadow, Rendered: "shadow",
		},
		{
			Name: "active", Input: "active",
			Mode: ReflectionActive, Resolved: ReflectionActive, Rendered: "active",
		},
		{
			Name: "unrecognised", Input: "automatic",
			Mode: ReflectionUnset, Resolved: DefaultReflectionMode, Rendered: "active",
			Invalid: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.Name, func(t *testing.T) {
			mode, err := ParseReflectionMode(testCase.Input)
			var invalid *InvalidReflectionModeError

			require.Equal(t, testCase, reflectionModeParseCase{
				Name: testCase.Name, Input: testCase.Input,
				Mode: mode, Resolved: mode.Resolved(), Rendered: mode.String(),
				Invalid: errors.As(err, &invalid),
			})
		})
	}
}

// TestDefaultReflectionMode_runs_reflection pins the default an installation
// that has never written the setting runs under. An instance that never
// reflects keeps the description it was created with.
func TestDefaultReflectionMode_runs_reflection(t *testing.T) {
	require.Equal(t, ReflectionActive, Config{}.ReflectionMode.Resolved())
}
