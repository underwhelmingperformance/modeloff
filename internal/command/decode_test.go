package command

import (
	"fmt"
	"math/bits"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type plainString string

type prefixed string

func (p *prefixed) Decode(raw string) error {
	if !strings.HasPrefix(raw, "#") {
		raw = "#" + raw
	}

	*p = prefixed(raw)
	return nil
}

type failingDecoder string

func (f *failingDecoder) Decode(raw string) error {
	return fmt.Errorf("always fails: %s", raw)
}

type myInt int

type myFloat float64

type uppercaser string

func (u *uppercaser) UnmarshalText(text []byte) error {
	*u = uppercaser(strings.ToUpper(string(text)))
	return nil
}

type strictText string

func (s *strictText) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		return fmt.Errorf("empty text not allowed")
	}

	*s = strictText(text)
	return nil
}

func TestRegistry_ForType_resolves_FieldDecoder_first(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[prefixed]())

	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[prefixed]()).Elem()
	require.NoError(t, dec.Decode("general", target))
	require.Equal(t, prefixed("#general"), target.Interface())
}

func TestRegistry_ForType_exact_type_before_kind(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	custom := DecoderFunc(func(raw string, target reflect.Value) error {
		target.SetString("custom:" + raw)
		return nil
	})
	r.RegisterType(reflect.TypeFor[plainString](), custom)

	dec := r.ForType(reflect.TypeFor[plainString]())

	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[plainString]()).Elem()
	require.NoError(t, dec.Decode("hello", target))
	require.Equal(t, plainString("custom:hello"), target.Interface())
}

func TestRegistry_ForType_kind_fallback_for_aliases(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	tests := []struct {
		name  string
		typ   reflect.Type
		input string
		want  any
	}{
		{
			name:  "string alias",
			typ:   reflect.TypeFor[plainString](),
			input: "hello",
			want:  plainString("hello"),
		},
		{
			name:  "plain string",
			typ:   reflect.TypeFor[string](),
			input: "world",
			want:  "world",
		},
		{
			name:  "int alias",
			typ:   reflect.TypeFor[myInt](),
			input: "42",
			want:  myInt(42),
		},
		{
			name:  "float64 alias",
			typ:   reflect.TypeFor[myFloat](),
			input: "3.14",
			want:  myFloat(3.14),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := r.ForType(tt.typ)
			require.NotNil(t, dec)

			target := reflect.New(tt.typ).Elem()
			require.NoError(t, dec.Decode(tt.input, target))
			require.Equal(t, tt.want, target.Interface())
		})
	}
}

func TestRegistry_FieldDecoder_error_propagates(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[failingDecoder]())
	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[failingDecoder]()).Elem()
	err := dec.Decode("anything", target)

	require.EqualError(t, err, "always fails: anything")
}

func TestRegistry_primitive_kinds(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	tests := []struct {
		name  string
		typ   reflect.Type
		input string
		want  any
	}{
		{"bool true", reflect.TypeFor[bool](), "true", true},
		{"bool false", reflect.TypeFor[bool](), "false", false},
		{"int", reflect.TypeFor[int](), "99", int(99)},
		{"int8", reflect.TypeFor[int8](), "-1", int8(-1)},
		{"int16", reflect.TypeFor[int16](), "1000", int16(1000)},
		{"int32", reflect.TypeFor[int32](), "70000", int32(70000)},
		{"int64", reflect.TypeFor[int64](), "9999999", int64(9999999)},
		{"uint", reflect.TypeFor[uint](), "42", uint(42)},
		{"uint8", reflect.TypeFor[uint8](), "255", uint8(255)},
		{"uint16", reflect.TypeFor[uint16](), "65535", uint16(65535)},
		{"uint32", reflect.TypeFor[uint32](), "100000", uint32(100000)},
		{"uint64", reflect.TypeFor[uint64](), "18446744073709551615", uint64(18446744073709551615)},
		{"float32", reflect.TypeFor[float32](), "1.5", float32(1.5)},
		{"float64", reflect.TypeFor[float64](), "2.718", float64(2.718)},
		{"string", reflect.TypeFor[string](), "hello", "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := r.ForType(tt.typ)
			require.NotNil(t, dec)

			target := reflect.New(tt.typ).Elem()
			require.NoError(t, dec.Decode(tt.input, target))
			require.Equal(t, tt.want, target.Interface())
		})
	}
}

func TestRegistry_decode_errors_are_typed(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	tests := []struct {
		name         string
		typ          reflect.Type
		input        string
		wantExpected string
	}{
		{"bad bool", reflect.TypeFor[bool](), "notbool", "bool"},
		{"bad int", reflect.TypeFor[int](), "abc", fmt.Sprintf("int%d", bits.UintSize)},
		{"bad uint", reflect.TypeFor[uint](), "-1", fmt.Sprintf("uint%d", bits.UintSize)},
		{"bad float", reflect.TypeFor[float64](), "xyz", "float64"},
		{"bad int8", reflect.TypeFor[int8](), "999", "int8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := r.ForType(tt.typ)
			target := reflect.New(tt.typ).Elem()
			err := dec.Decode(tt.input, target)

			var de *DecodeError
			require.ErrorAs(t, err, &de)
			require.Equal(t, tt.input, de.Value)
			require.Equal(t, tt.wantExpected, de.Expected)
			require.NotNil(t, de.Unwrap())
		})
	}
}

func TestRegistry_no_decoder_for_unknown_type(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[struct{}]())

	require.Nil(t, dec)
}

func TestRegistry_resolution_precedence(t *testing.T) {
	tests := []struct {
		name  string
		setup func(r *Registry)
		typ   reflect.Type
		input string
		want  any
	}{
		{
			name: "FieldDecoder beats registered type",
			setup: func(r *Registry) {
				r.RegisterType(reflect.TypeFor[prefixed](), DecoderFunc(func(_ string, target reflect.Value) error {
					target.SetString("should-not-reach")
					return nil
				}))
			},
			typ:   reflect.TypeFor[prefixed](),
			input: "general",
			want:  prefixed("#general"),
		},
		{
			name: "registered type beats kind",
			setup: func(r *Registry) {
				r.RegisterType(reflect.TypeFor[string](), DecoderFunc(func(raw string, target reflect.Value) error {
					target.SetString("custom:" + raw)
					return nil
				}))
			},
			typ:   reflect.TypeFor[string](),
			input: "test",
			want:  "custom:test",
		},
		{
			name: "registered type beats TextUnmarshaler",
			setup: func(r *Registry) {
				r.RegisterType(reflect.TypeFor[uppercaser](), DecoderFunc(func(raw string, target reflect.Value) error {
					target.SetString("custom:" + raw)
					return nil
				}))
			},
			typ:   reflect.TypeFor[uppercaser](),
			input: "hello",
			want:  uppercaser("custom:hello"),
		},
		{
			name:  "FieldDecoder beats TextUnmarshaler",
			setup: func(_ *Registry) {},
			typ:   reflect.TypeFor[prefixed](),
			input: "test",
			want:  prefixed("#test"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry().RegisterDefaults()
			tt.setup(r)

			dec := r.ForType(tt.typ)

			target := reflect.New(tt.typ).Elem()
			require.NoError(t, dec.Decode(tt.input, target))
			require.Equal(t, tt.want, target.Interface())
		})
	}
}

func TestRegistry_slice(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	tests := []struct {
		name   string
		typ    reflect.Type
		inputs []string
		want   any
	}{
		{
			name:   "strings",
			typ:    reflect.TypeFor[[]string](),
			inputs: []string{"hello", "world"},
			want:   []string{"hello", "world"},
		},
		{
			name:   "ints",
			typ:    reflect.TypeFor[[]int](),
			inputs: []string{"1", "2", "3"},
			want:   []int{1, 2, 3},
		},
		{
			name:   "string aliases",
			typ:    reflect.TypeFor[[]plainString](),
			inputs: []string{"a", "b"},
			want:   []plainString{"a", "b"},
		},
		{
			name:   "custom decoders",
			typ:    reflect.TypeFor[[]prefixed](),
			inputs: []string{"general", "#random"},
			want:   []prefixed{"#general", "#random"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := r.ForType(tt.typ)
			require.NotNil(t, dec)

			target := reflect.New(tt.typ).Elem()
			for _, input := range tt.inputs {
				require.NoError(t, dec.Decode(input, target))
			}

			require.Equal(t, tt.want, target.Interface())
		})
	}
}

func TestRegistry_slice_decode_error_is_typed(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[[]int]())
	target := reflect.New(reflect.TypeFor[[]int]()).Elem()
	err := dec.Decode("notanint", target)

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "notanint", de.Value)
}

func TestRegistry_TextUnmarshaler(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[uppercaser]())
	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[uppercaser]()).Elem()
	require.NoError(t, dec.Decode("hello", target))
	require.Equal(t, uppercaser("HELLO"), target.Interface())
}

func TestRegistry_TextUnmarshaler_error_wraps_as_DecodeError(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[strictText]())
	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[strictText]()).Elem()
	err := dec.Decode("", target)

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "", de.Value)
	require.Equal(t, "text", de.Expected)
}

func TestRegistry_duration_via_registered_type(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[time.Duration]())
	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[time.Duration]()).Elem()
	require.NoError(t, dec.Decode("5m", target))
	require.Equal(t, 5*time.Minute, target.Interface())
}

func TestRegistry_no_decoder_for_composite_element(t *testing.T) {
	r := NewRegistry() // no defaults registered

	tests := []struct {
		name string
		typ  reflect.Type
	}{
		{"slice", reflect.TypeFor[[]string]()},
		{"map", reflect.TypeFor[map[string]string]()},
		{"pointer", reflect.TypeFor[*string]()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Nil(t, r.ForType(tt.typ))
		})
	}
}

func TestRegistry_map(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	tests := []struct {
		name   string
		typ    reflect.Type
		inputs []string
		want   any
	}{
		{
			name:   "string to string",
			typ:    reflect.TypeFor[map[string]string](),
			inputs: []string{"key=value", "foo=bar"},
			want:   map[string]string{"key": "value", "foo": "bar"},
		},
		{
			name:   "string to int",
			typ:    reflect.TypeFor[map[string]int](),
			inputs: []string{"port=8080"},
			want:   map[string]int{"port": 8080},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := r.ForType(tt.typ)
			require.NotNil(t, dec)

			target := reflect.New(tt.typ).Elem()
			for _, input := range tt.inputs {
				require.NoError(t, dec.Decode(input, target))
			}

			require.Equal(t, tt.want, target.Interface())
		})
	}
}

func TestRegistry_map_missing_equals(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[map[string]string]())
	target := reflect.New(reflect.TypeFor[map[string]string]()).Elem()
	err := dec.Decode("noequals", target)

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "noequals", de.Value)
	require.Equal(t, "key=value", de.Expected)
}

func TestRegistry_map_bad_value_type(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[map[string]int]())
	target := reflect.New(reflect.TypeFor[map[string]int]()).Elem()
	err := dec.Decode("port=abc", target)

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "abc", de.Value)
}

func TestRegistry_time(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[time.Time]())
	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[time.Time]()).Elem()
	require.NoError(t, dec.Decode("2024-01-15T10:30:00Z", target))

	expected, _ := time.Parse(time.RFC3339, "2024-01-15T10:30:00Z")
	require.Equal(t, expected, target.Interface())
}

func TestRegistry_time_bad_format(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[time.Time]())
	target := reflect.New(reflect.TypeFor[time.Time]()).Elem()
	err := dec.Decode("not-a-time", target)

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "not-a-time", de.Value)
	require.Equal(t, "time (RFC3339)", de.Expected)
}

func TestRegistry_url(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[*url.URL]())
	require.NotNil(t, dec)

	target := reflect.New(reflect.TypeFor[*url.URL]()).Elem()
	require.NoError(t, dec.Decode("https://example.com/path?q=1", target))

	want, _ := url.Parse("https://example.com/path?q=1")
	require.Equal(t, want, target.Interface())
}

func TestRegistry_url_bad(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[*url.URL]())
	target := reflect.New(reflect.TypeFor[*url.URL]()).Elem()
	err := dec.Decode("://bad", target)

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "://bad", de.Value)
	require.Equal(t, "url", de.Expected)
}

func TestRegistry_pointer(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	wantStr := "hello"
	wantInt := 42

	tests := []struct {
		name  string
		typ   reflect.Type
		input string
		want  any
	}{
		{"string", reflect.TypeFor[*string](), "hello", &wantStr},
		{"int", reflect.TypeFor[*int](), "42", &wantInt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := r.ForType(tt.typ)
			require.NotNil(t, dec)

			target := reflect.New(tt.typ).Elem()
			require.NoError(t, dec.Decode(tt.input, target))
			require.Equal(t, tt.want, target.Interface())
		})
	}
}

func TestRegistry_duration_bad(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	dec := r.ForType(reflect.TypeFor[time.Duration]())
	target := reflect.New(reflect.TypeFor[time.Duration]()).Elem()
	err := dec.Decode("notaduration", target)

	var de *DecodeError
	require.ErrorAs(t, err, &de)
	require.Equal(t, "notaduration", de.Value)
	require.Equal(t, "duration", de.Expected)
}

func TestRegistry_RegisterType_overwrites(t *testing.T) {
	r := NewRegistry().RegisterDefaults()

	original := r.ForType(reflect.TypeFor[string]())
	require.NotNil(t, original)

	custom := DecoderFunc(func(raw string, target reflect.Value) error {
		target.SetString("custom:" + raw)
		return nil
	})

	r.RegisterType(reflect.TypeFor[string](), custom)

	target := reflect.New(reflect.TypeFor[string]()).Elem()
	err := r.ForType(reflect.TypeFor[string]()).Decode("hello", target)

	require.NoError(t, err)
	require.Equal(t, "custom:hello", target.String())
}
