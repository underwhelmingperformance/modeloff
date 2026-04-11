package command

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type joinCmd struct {
	Channel channel `arg:"channel" help:"Channel to join"`
}

type channel string

func (c *channel) Decode(raw string) error {
	if !strings.HasPrefix(raw, "#") {
		raw = "#" + raw
	}

	*c = channel(raw)
	return nil
}

type kickCmd struct {
	Nick string `arg:"" help:"Nick to kick"`
}

type msgCmd struct {
	Nick string   `arg:"" help:"Nick to message"`
	Body []string `arg:"" nargs:"1" help:"Message text"`
}

type topicCmd struct {
	Topic []string `arg:"" optional:"" help:"Topic text"`
}

type emptyCmd struct{}

type flagCmd struct {
	Model   string `arg:"" optional:"" help:"Model to invite"`
	Persona string `optional:"" help:"Optional persona"`
}

type mixedCmd struct {
	Target string   `arg:"" help:"Target"`
	Force  string   `optional:"" help:"Force flag"`
	Rest   []string `arg:"" optional:"" help:"Remaining"`
}

type portCmd struct {
	Port int `arg:"" help:"Port number"`
}

type boolFlagCmd struct {
	Reset bool   `optional:"" help:"Reset value"`
	Name  string `arg:"" optional:"" help:"Name"`
}

func TestParseInto_single_positional(t *testing.T) {
	cmd := &kickCmd{}

	err := ParseInto(cmd, []string{"botty"})

	require.NoError(t, err)
	require.Equal(t, kickCmd{Nick: "botty"}, *cmd)
}

func TestParseInto_positional_with_custom_decoder(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  channel
	}{
		{"without prefix", "general", channel("#general")},
		{"already prefixed", "#general", channel("#general")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &joinCmd{}

			require.NoError(t, ParseInto(cmd, []string{tt.input}))
			require.Equal(t, joinCmd{Channel: tt.want}, *cmd)
		})
	}
}

func TestParseInto_variadic_positional(t *testing.T) {
	cmd := &msgCmd{}

	err := ParseInto(cmd, []string{"alice", "hello", "world"})

	require.NoError(t, err)
	require.Equal(t, msgCmd{Nick: "alice", Body: []string{"hello", "world"}}, *cmd)
}

func TestParseInto_variadic_with_nargs_satisfied(t *testing.T) {
	cmd := &msgCmd{}

	err := ParseInto(cmd, []string{"alice", "hi"})

	require.NoError(t, err)
	require.Equal(t, msgCmd{Nick: "alice", Body: []string{"hi"}}, *cmd)
}

func TestParseInto_variadic_with_nargs_violated(t *testing.T) {
	cmd := &msgCmd{}

	err := ParseInto(cmd, []string{"alice"})

	var me *MissingArgError
	require.ErrorAs(t, err, &me)
	require.Equal(t, "body", me.Name)
}

func TestParseInto_optional_variadic_absent(t *testing.T) {
	cmd := &topicCmd{}

	err := ParseInto(cmd, nil)

	require.NoError(t, err)
	require.Equal(t, topicCmd{}, *cmd)
}

func TestParseInto_optional_variadic_present(t *testing.T) {
	cmd := &topicCmd{}

	err := ParseInto(cmd, []string{"General", "Discussion"})

	require.NoError(t, err)
	require.Equal(t, topicCmd{Topic: []string{"General", "Discussion"}}, *cmd)
}

func TestParseInto_no_fields(t *testing.T) {
	cmd := &emptyCmd{}

	err := ParseInto(cmd, nil)

	require.NoError(t, err)
}

func TestParseInto_no_fields_with_extra_args(t *testing.T) {
	cmd := &emptyCmd{}

	err := ParseInto(cmd, []string{"extra"})

	var ee *ExtraArgsError
	require.ErrorAs(t, err, &ee)
	require.Equal(t, []string{"extra"}, ee.Args)
}

func TestParseInto_missing_required(t *testing.T) {
	cmd := &kickCmd{}

	err := ParseInto(cmd, nil)

	var me *MissingArgError
	require.ErrorAs(t, err, &me)
	require.Equal(t, "nick", me.Name)
}

func TestParseInto_flag_parsing(t *testing.T) {
	cmd := &flagCmd{}

	err := ParseInto(cmd, []string{"some-model", "--persona", "Helpful assistant"})

	require.NoError(t, err)
	require.Equal(t, flagCmd{Model: "some-model", Persona: "Helpful assistant"}, *cmd)
}

func TestParseInto_flag_without_value(t *testing.T) {
	cmd := &flagCmd{}

	err := ParseInto(cmd, []string{"some-model", "--persona"})

	var me *MissingFlagValueError
	require.ErrorAs(t, err, &me)
	require.Equal(t, "--persona", me.Flag)
}

func TestParseInto_flag_only(t *testing.T) {
	cmd := &flagCmd{}

	err := ParseInto(cmd, []string{"--persona", "Be nice"})

	require.NoError(t, err)
	require.Equal(t, flagCmd{Persona: "Be nice"}, *cmd)
}

func TestParseInto_unknown_flag(t *testing.T) {
	cmd := &flagCmd{}

	err := ParseInto(cmd, []string{"--unknown", "value"})

	var uf *UnknownFlagError
	require.ErrorAs(t, err, &uf)
	require.Equal(t, "--unknown", uf.Flag)
}

func TestParseInto_mixed_positional_flag_variadic(t *testing.T) {
	cmd := &mixedCmd{}

	err := ParseInto(cmd, []string{"target1", "--force", "yes", "a", "b"})

	require.NoError(t, err)
	require.Equal(t, mixedCmd{Target: "target1", Force: "yes", Rest: []string{"a", "b"}}, *cmd)
}

func TestParseInto_int_field(t *testing.T) {
	cmd := &portCmd{}

	err := ParseInto(cmd, []string{"8080"})

	require.NoError(t, err)
	require.Equal(t, portCmd{Port: 8080}, *cmd)
}

func TestParseInto_int_decode_error(t *testing.T) {
	cmd := &portCmd{}

	err := ParseInto(cmd, []string{"notanumber"})

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "notanumber", de.Value)
}

func TestParseInto_flag_before_positional(t *testing.T) {
	cmd := &flagCmd{}

	err := ParseInto(cmd, []string{"--persona", "Be nice", "some-model"})

	require.NoError(t, err)
	require.Equal(t, flagCmd{Model: "some-model", Persona: "Be nice"}, *cmd)
}

func TestParseInto_variadic_flag_consumes_remaining(t *testing.T) {
	type varFlagCmd struct {
		Model string   `arg:"" optional:"" help:"Model"`
		Tags  []string `optional:"" help:"Tags"`
	}

	cmd := &varFlagCmd{}

	err := ParseInto(cmd, []string{"model-a", "--tags", "x", "y", "z"})

	require.NoError(t, err)
	require.Equal(t, varFlagCmd{Model: "model-a", Tags: []string{"x", "y", "z"}}, *cmd)
}

func TestParseInto_empty_string_is_rejected_for_required_field(t *testing.T) {
	cmd := &kickCmd{}

	err := ParseInto(cmd, []string{""})

	var me *MissingArgError
	require.ErrorAs(t, err, &me)
	require.Equal(t, "nick", me.Name)
}

func TestParseInto_extra_args_after_positionals(t *testing.T) {
	cmd := &kickCmd{}

	err := ParseInto(cmd, []string{"botty", "extra"})

	var ee *ExtraArgsError
	require.ErrorAs(t, err, &ee)
	require.Equal(t, []string{"extra"}, ee.Args)
}

func TestParseInto_variadic_slice_flag(t *testing.T) {
	type tagCmd struct {
		Name string   `arg:"" help:"Name"`
		Tags []string `optional:"" help:"Tags"`
	}

	cmd := &tagCmd{}

	err := ParseInto(cmd, []string{"widget", "--tags", "red", "blue", "green"})

	require.NoError(t, err)
	require.Equal(t, tagCmd{Name: "widget", Tags: []string{"red", "blue", "green"}}, *cmd)
}

func TestParseInto_bool_flag_without_value(t *testing.T) {
	cmd := &boolFlagCmd{}

	err := ParseInto(cmd, []string{"--reset"})

	require.NoError(t, err)
	require.Equal(t, boolFlagCmd{Reset: true}, *cmd)
}

func TestParseInto_bool_flag_before_positional(t *testing.T) {
	cmd := &boolFlagCmd{}

	err := ParseInto(cmd, []string{"--reset", "widget"})

	require.NoError(t, err)
	require.Equal(t, boolFlagCmd{Reset: true, Name: "widget"}, *cmd)
}
