package command

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// secretAPIKeyCommand stands in for chatcmd's real APIKeyConfig: a
// single optional positional carrying a credential.
type secretAPIKeyCommand struct {
	Value string `arg:"" optional:"" help:"API key"`
}

// secretBaseURLCommand is the non-secret sibling, so a test can pin
// that the marker is per-node and does not leak across a group's
// other children.
type secretBaseURLCommand struct {
	URL string `arg:"" optional:"" help:"Base URL"`
}

type secretConfigCommand struct {
	APIKey  secretAPIKeyCommand  `cmd:"" name:"api-key" help:"Set API key." secret:""`
	BaseURL secretBaseURLCommand `cmd:"" name:"base-url" help:"Set base URL."`
}

type secretGrammar struct {
	Config secretConfigCommand `cmd:"" help:"Config."`
	Quit   testQuitCommand     `cmd:"" help:"Quit."`
}

func TestNode_Secret_from_grammar_tag(t *testing.T) {
	set, err := Build[testCtx](&secretGrammar{})
	require.NoError(t, err)

	config := set.Find("config")
	require.NotNil(t, config)
	require.False(t, config.Secret, "the group node itself carries no secret tag")

	apiKey := config.Find("api-key")
	require.NotNil(t, apiKey)
	require.True(t, apiKey.Secret)

	baseURL := config.Find("base-url")
	require.NotNil(t, baseURL)
	require.False(t, baseURL.Secret, "the marker must not leak to an unmarked sibling")

	require.Same(t, config, set.Find("CONFIG"), "Set.Find must fold ascii case")
	require.Same(t, apiKey, config.Find("API-KEY"), "Node.Find must fold ascii case")
}

func TestParser_RedactedRaw(t *testing.T) {
	parser, err := BuildParser[testCtx, testContext, testResult](&secretGrammar{})
	require.NoError(t, err)

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "secret command is collapsed",
			input: "/config api-key sk-or-v1-super-secret",
			want:  "/config api-key <redacted>",
		},
		{
			name:  "secret command with a trailing extra token is still collapsed",
			input: "/config api-key sk-or-v1-super-secret extra-token",
			want:  "/config api-key <redacted>",
		},
		{
			name:  "uppercase command name is still collapsed",
			input: "/CONFIG api-key sk-or-v1-super-secret",
			want:  "/config api-key <redacted>",
		},
		{
			name:  "uppercase subcommand name is still collapsed",
			input: "/config API-KEY sk-or-v1-super-secret",
			want:  "/config api-key <redacted>",
		},
		{
			name:  "mixed-case command and subcommand names are still collapsed",
			input: "/Config Api-Key sk-or-v1-super-secret",
			want:  "/config api-key <redacted>",
		},
		{
			name:  "non-secret sibling is unchanged",
			input: "/config base-url https://example.com",
			want:  "/config base-url https://example.com",
		},
		{
			name:  "bare parent command is unchanged",
			input: "/config",
			want:  "/config",
		},
		{
			name:  "unrelated leaf command is unchanged",
			input: "/quit",
			want:  "/quit",
		},
		{
			name:  "unknown command is unchanged",
			input: "/bogus",
			want:  "/bogus",
		},
		{
			name:  "not a command is unchanged",
			input: "hello there",
			want:  "hello there",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, parser.RedactedRaw(tt.input))
		})
	}
}
