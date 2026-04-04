package command

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// argSpecMeta is ArgSpec without the Source field, which is not
// comparable.
type argSpecMeta struct {
	Name     string
	Help     string
	Optional bool
	FreeForm bool
	Nargs    *int
}

func toMeta(specs []ArgSpec) []argSpecMeta {
	out := make([]argSpecMeta, len(specs))

	for i, s := range specs {
		out[i] = argSpecMeta{
			Name:     s.Name,
			Help:     s.Help,
			Optional: s.Optional,
			FreeForm: s.FreeForm,
			Nargs:    s.Nargs,
		}
	}

	return out
}

func intPtr(n int) *int { return &n }

func TestParseArgTags(t *testing.T) {
	tests := []struct {
		name string
		cmd  Command
		want []argTag
	}{
		{
			name: "single arg with explicit name",
			cmd:  JoinCommand{},
			want: []argTag{
				{Name: "channel", Help: "Channel to join or create", FieldIndex: 0},
			},
		},
		{
			name: "no fields",
			cmd:  LeaveCommand{},
			want: nil,
		},
		{
			name: "multiple optional args with kebab-case fallback",
			cmd:  InviteCommand{},
			want: []argTag{
				{Name: "model", Help: "Model to invite", Optional: true, FieldIndex: 0},
				{Name: "persona", Help: "Optional persona", Optional: true, FieldIndex: 1},
			},
		},
		{
			name: "variadic field with nargs",
			cmd:  MsgCommand{},
			want: []argTag{
				{Name: "nick", Help: "Nick to message", FieldIndex: 0},
				{Name: "body", Help: "Message text", FreeForm: true, Nargs: intPtr(1), FieldIndex: 1},
			},
		},
		{
			name: "arg tag overrides field name",
			cmd:  NickCommand{},
			want: []argTag{
				{Name: "new-nick", Help: "New nickname", FieldIndex: 0},
			},
		},
		{
			name: "optional variadic without nargs",
			cmd:  TopicCommand{},
			want: []argTag{
				{Name: "topic", Help: "Topic text", Optional: true, FreeForm: true, FieldIndex: 0},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseArgTags(tt.cmd)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestBuildArgSpecs(t *testing.T) {
	tests := []struct {
		name    string
		tags    []argTag
		sources map[string]SuggestionSource
		want    []argSpecMeta
	}{
		{
			name: "resolves sources by arg name",
			tags: []argTag{
				{Name: "channel", Help: "Channel to join", FieldIndex: 0},
			},
			sources: map[string]SuggestionSource{
				"channel": ChannelsSource(),
			},
			want: []argSpecMeta{
				{Name: "channel", Help: "Channel to join"},
			},
		},
		{
			name: "unknown source is silently ignored",
			tags: []argTag{
				{Name: "nick", Help: "Nick", FieldIndex: 0},
			},
			sources: map[string]SuggestionSource{
				"nonexistent": ChannelsSource(),
			},
			want: []argSpecMeta{
				{Name: "nick", Help: "Nick"},
			},
		},
		{
			name: "preserves nargs on variadic",
			tags: []argTag{
				{Name: "body", Help: "Text", FreeForm: true, Nargs: intPtr(1), FieldIndex: 0},
			},
			sources: nil,
			want: []argSpecMeta{
				{Name: "body", Help: "Text", FreeForm: true, Nargs: intPtr(1)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs := buildArgSpecs(tt.tags, tt.sources)

			got := toMeta(specs)
			require.Equal(t, tt.want, got)

			if tt.sources != nil {
				for i, tag := range tt.tags {
					if _, ok := tt.sources[tag.Name]; ok {
						require.NotNil(t, specs[i].Source)
					}
				}
			}
		})
	}
}

func TestToKebabCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Channel", "channel"},
		{"FooBar", "foo-bar"},
		{"Nick", "nick"},
		{"ModelID", "model-id"},
		{"HTMLParser", "html-parser"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			require.Equal(t, tt.want, toKebabCase(tt.input))
		})
	}
}

type testHandleCmd struct {
	Channel string `arg:"channel" help:"Test channel"`
}

func (testHandleCmd) commandMarker() {}

func TestHandle_builds_spec_from_tags(t *testing.T) {
	spec := Handle("test", "Test command.", "/test <channel>",
		map[string]SuggestionSource{"channel": ChannelsSource()},
		func(_ testHandleCmd) tea.Cmd { return nil },
	)

	require.Equal(t, "test", spec.Name)
	require.Equal(t, "Test command.", spec.Help)
	require.Equal(t, "/test <channel>", spec.Usage)
	require.Equal(t, []argSpecMeta{
		{Name: "channel", Help: "Test channel"},
	}, toMeta(spec.Args))
	require.NotNil(t, spec.Args[0].Source)
	require.NotNil(t, spec.Handler)
}

type testNoArgsCmd struct{}

func (testNoArgsCmd) commandMarker() {}

func TestHandle_no_args(t *testing.T) {
	spec := Handle("empty", "No args.", "/empty",
		nil,
		func(_ testNoArgsCmd) tea.Cmd { return nil },
	)

	require.Equal(t, "empty", spec.Name)
	require.Empty(t, spec.Args)
}

func TestHandle_handler_receives_typed_command(t *testing.T) {
	var received testHandleCmd

	spec := Handle("test", "Test.", "/test <channel>",
		nil,
		func(cmd testHandleCmd) tea.Cmd {
			received = cmd
			return nil
		},
	)

	spec.Handler(Invocation{
		Parsed: testHandleCmd{Channel: "#general"},
	})

	require.Equal(t, testHandleCmd{Channel: "#general"}, received)
}

func TestHandle_panics_on_wrong_type(t *testing.T) {
	spec := Handle("test", "Test.", "/test",
		nil,
		func(_ testHandleCmd) tea.Cmd { return nil },
	)

	require.Panics(t, func() {
		spec.Handler(Invocation{
			Parsed: LeaveCommand{},
		})
	})
}
