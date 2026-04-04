package command

import (
	"fmt"
	"reflect"
	"strings"
)

// MissingArgError is returned when a required positional argument is
// not provided.
type MissingArgError struct {
	Name string
}

func (e *MissingArgError) Error() string {
	return fmt.Sprintf("missing required argument <%s>", e.Name)
}

// ExtraArgsError is returned when more positional arguments are
// provided than the command accepts.
type ExtraArgsError struct {
	Args []string
}

func (e *ExtraArgsError) Error() string {
	return fmt.Sprintf("unexpected arguments: %s", strings.Join(e.Args, " "))
}

// MissingFlagValueError is returned when a flag is present but its
// value is missing.
type MissingFlagValueError struct {
	Flag string
}

func (e *MissingFlagValueError) Error() string {
	return fmt.Sprintf("flag %s requires a value", e.Flag)
}

// UnknownFlagError is returned when an unrecognised flag is
// encountered.
type UnknownFlagError struct {
	Flag string
}

func (e *UnknownFlagError) Error() string {
	return fmt.Sprintf("unknown flag %s", e.Flag)
}

// fieldMeta holds the resolved metadata for a single struct field
// during parsing.
type fieldMeta struct {
	name     string
	help     string
	index    int
	isFlag   bool
	flagName string
	optional bool
	variadic bool
	nargs    *int
	decoder  Decoder
}

var defaultRegistry = NewRegistry().RegisterDefaults()

// ParseInto populates a command struct from raw arguments using
// struct tags and the decoder registry. Fields with `arg` are
// positional; fields without `arg` are flags.
func ParseInto(cmd any, args []string) error {
	fields, err := resolveFieldMetas(cmd)
	if err != nil {
		return err
	}

	if len(fields) == 0 && len(args) > 0 {
		return &ExtraArgsError{Args: args}
	}

	if len(fields) == 0 {
		return nil
	}

	flags, positionals := classifyArgs(args, fields)

	v := reflect.ValueOf(cmd).Elem()

	if err := applyFlags(v, fields, flags); err != nil {
		return err
	}

	if err := applyPositionals(v, fields, positionals); err != nil {
		return err
	}

	return validateRequired(v, fields)
}

// classifyArgs separates raw args into flag values and positional
// values. Scalar flags consume the next token; variadic flags
// (slice-typed) consume all remaining tokens.
func classifyArgs(args []string, fields []fieldMeta) (map[string][]string, []string) {
	flagMeta := map[string]fieldMeta{}

	for _, f := range fields {
		if f.isFlag {
			flagMeta[f.flagName] = f
		}
	}

	flags := map[string][]string{}
	var positionals []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		if fm, ok := flagMeta[a]; ok {
			if i+1 >= len(args) {
				flags[a] = nil
				continue
			}

			if fm.variadic {
				flags[a] = args[i+1:]
				i = len(args)
			} else {
				flags[a] = []string{args[i+1]}
				i++
			}

			continue
		}

		if strings.HasPrefix(a, "--") {
			flags[a] = nil
			continue
		}

		positionals = append(positionals, a)
	}

	return flags, positionals
}

// applyFlags decodes flag values into the corresponding struct
// fields.
func applyFlags(v reflect.Value, fields []fieldMeta, flags map[string][]string) error {
	known := map[string]bool{}

	for _, f := range fields {
		if !f.isFlag {
			continue
		}

		known[f.flagName] = true
		values, ok := flags[f.flagName]

		if !ok {
			continue
		}

		if len(values) == 0 {
			return &MissingFlagValueError{Flag: f.flagName}
		}

		for _, raw := range values {
			if err := f.decoder.Decode(raw, v.Field(f.index)); err != nil {
				return err
			}
		}
	}

	for flag := range flags {
		if !known[flag] {
			return &UnknownFlagError{Flag: flag}
		}
	}

	return nil
}

// applyPositionals decodes positional arguments into the
// corresponding struct fields in order.
func applyPositionals(v reflect.Value, fields []fieldMeta, positionals []string) error {
	pos := 0

	for _, f := range fields {
		if f.isFlag {
			continue
		}

		if f.variadic {
			remaining := positionals[pos:]

			if len(remaining) == 0 {
				break
			}

			for _, raw := range remaining {
				if err := f.decoder.Decode(raw, v.Field(f.index)); err != nil {
					return err
				}
			}

			pos = len(positionals)
			break
		}

		if pos >= len(positionals) {
			break
		}

		if err := f.decoder.Decode(positionals[pos], v.Field(f.index)); err != nil {
			return err
		}

		pos++
	}

	if pos < len(positionals) {
		return &ExtraArgsError{Args: positionals[pos:]}
	}

	return nil
}

// validateRequired checks that all required fields have been
// populated.
func validateRequired(v reflect.Value, fields []fieldMeta) error {
	for _, f := range fields {
		if f.optional || f.isFlag {
			continue
		}

		fv := v.Field(f.index)

		if fv.IsZero() {
			return &MissingArgError{Name: f.name}
		}

		if f.nargs != nil && fv.Kind() == reflect.Slice && fv.Len() < *f.nargs {
			return &MissingArgError{Name: f.name}
		}
	}

	return nil
}
