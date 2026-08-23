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
	name        string
	help        string
	toolHelp    string
	index       int
	typ         reflect.Type
	isFlag      bool
	boolFlag    bool
	flagName    string
	optional    bool
	variadic    bool
	cliHidden   bool
	toolHidden  bool
	xorGroups   []string
	maxItems    *int
	passthrough PassthroughMode
	decoder     Decoder
}

// PassthroughMode controls when a positional stops flag parsing.
// The values match Kong's passthrough modes.
type PassthroughMode uint8

const (
	// PassthroughModeNone leaves ordinary flag parsing active.
	PassthroughModeNone PassthroughMode = iota

	// PassthroughModeAll passes an unknown flag through before the
	// first positional value. Recognised flags remain active until
	// that value; everything after it passes through.
	PassthroughModeAll

	// PassthroughModePartial validates flags until the passthrough
	// positional receives its first value, then passes through the
	// remaining positional input.
	PassthroughModePartial
)

var defaultRegistry = NewRegistry().RegisterDefaults()

// ParseInto populates a command struct from raw arguments using
// struct tags and the decoder registry. Fields with `arg` are
// positional; fields without `arg` are flags.
func ParseInto(cmd any, args []string) error {
	return parseInto(cmd, args, false)
}

func parseInto(cmd any, args []string, optionsEnded bool) error {
	fields, err := resolveCLIFieldMetas(cmd)
	if err != nil {
		return err
	}

	if len(fields) == 0 && len(args) > 0 {
		return &ExtraArgsError{Args: args}
	}

	if len(fields) == 0 {
		return nil
	}

	flags, positionals := classifyArgs(args, fields, optionsEnded)

	v := reflect.ValueOf(cmd).Elem()

	present := map[int]bool{}

	if err := applyFlags(v, fields, flags, present); err != nil {
		return err
	}

	if err := applyPositionals(v, fields, positionals, present); err != nil {
		return err
	}

	if err := validateFields(fields, present); err != nil {
		return err
	}
	if err := validatePresentValues(v, fields, present); err != nil {
		return err
	}
	if err := validateDecodedValue(reflect.ValueOf(cmd)); err != nil {
		return &ValueValidationError{Name: "command", Err: err}
	}

	return nil
}

// classifyArgs separates raw args into flag values and positional
// values. Scalar flags consume the next token; variadic flags
// (slice-typed) consume all remaining tokens.
func classifyArgs(args []string, fields []fieldMeta, optionsEnded bool) (map[string][]string, []string) {
	classification := argumentClassification{
		fields:         fields,
		flagMeta:       flagMetas(fields),
		flags:          map[string][]string{},
		passingThrough: optionsEnded,
	}

	for i := 0; i < len(args); i++ {
		next, done := classification.consume(args, i)
		if done {
			break
		}
		i = next
	}

	return classification.flags, classification.positionals
}

type argumentClassification struct {
	fields          []fieldMeta
	flagMeta        map[string]fieldMeta
	flags           map[string][]string
	positionals     []string
	positionalIndex int
	passingThrough  bool
}

func flagMetas(fields []fieldMeta) map[string]fieldMeta {
	metas := map[string]fieldMeta{}
	for _, field := range fields {
		if field.isFlag {
			metas[field.flagName] = field
		}
	}

	return metas
}

func (c *argumentClassification) consume(args []string, index int) (int, bool) {
	if c.passingThrough {
		c.positionals = append(c.positionals, args[index:]...)
		return index, true
	}

	value := args[index]
	flagName, attached, hasAttachedValue := splitLongFlagToken(value)
	if !hasAttachedValue {
		flagName = value
	}
	if meta, ok := c.flagMeta[flagName]; ok {
		return c.consumeFlag(args, index, flagName, attached, hasAttachedValue, meta)
	}

	positional := positionalField(c.fields, c.positionalIndex)
	optionKind := classifyOptionToken(value)
	if optionKind == optionTokenEnd {
		start := index + 1
		if positional != nil && positional.passthrough != PassthroughModeNone {
			start = index
		}

		c.positionals = append(c.positionals, args[start:]...)
		return index, true
	}
	if positional != nil && positional.passthrough == PassthroughModeAll {
		c.positionals = append(c.positionals, args[index:]...)
		return index, true
	}
	if optionKind.isFlag() {
		c.flags[value] = nil
		return index, false
	}

	c.positionals = append(c.positionals, value)
	if positional == nil {
		c.positionalIndex++
		return index, false
	}
	if positional.passthrough != PassthroughModeNone {
		c.passingThrough = true
	}
	if !positional.variadic {
		c.positionalIndex++
	}

	return index, false
}

func (c *argumentClassification) consumeFlag(
	args []string,
	index int,
	name string,
	attached string,
	hasAttachedValue bool,
	meta fieldMeta,
) (int, bool) {
	if hasAttachedValue {
		c.flags[name] = []string{attached}
		return index, false
	}

	if meta.boolFlag {
		c.flags[name] = []string{"true"}
		return index, false
	}
	if index+1 >= len(args) {
		c.flags[name] = nil
		return index, false
	}
	if meta.variadic {
		c.flags[name] = args[index+1:]
		return index, true
	}

	c.flags[name] = []string{args[index+1]}
	return index + 1, false
}

func positionalField(fields []fieldMeta, index int) *fieldMeta {
	for i := range fields {
		if fields[i].isFlag {
			continue
		}
		if index == 0 {
			return &fields[i]
		}
		index--
	}

	return nil
}

// applyFlags decodes flag values into the corresponding struct
// fields.
func applyFlags(v reflect.Value, fields []fieldMeta, flags map[string][]string, present map[int]bool) error {
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
			if f.boolFlag {
				values = []string{"true"}
			} else {
				return &MissingFlagValueError{Flag: f.flagName}
			}
		}
		if err := validateMaximum(f, len(values)); err != nil {
			return err
		}

		for _, raw := range values {
			if err := f.decoder.Decode(raw, v.Field(f.index)); err != nil {
				return err
			}
		}
		present[f.index] = true
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
func applyPositionals(v reflect.Value, fields []fieldMeta, positionals []string, present map[int]bool) error {
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
			if err := validateMaximum(f, len(remaining)); err != nil {
				return err
			}

			for _, raw := range remaining {
				if err := f.decoder.Decode(raw, v.Field(f.index)); err != nil {
					return err
				}
			}
			present[f.index] = true

			pos = len(positionals)
			break
		}

		if pos >= len(positionals) {
			break
		}

		if err := f.decoder.Decode(positionals[pos], v.Field(f.index)); err != nil {
			return err
		}
		present[f.index] = true

		pos++
	}

	if pos < len(positionals) {
		return &ExtraArgsError{Args: positionals[pos:]}
	}

	return nil
}

func validateMaximum(field fieldMeta, count int) error {
	if field.maxItems == nil || count <= *field.maxItems {
		return nil
	}

	return &TooManyValuesError{Name: field.name, Maximum: *field.maxItems, Actual: count}
}

// validateFields checks required fields and XOR groups using input
// presence. A supplied zero value therefore remains distinct from an
// omitted field.
func validateFields(fields []fieldMeta, present map[int]bool) error {
	groups := map[string][]fieldMeta{}

	for _, f := range fields {
		for _, group := range f.xorGroups {
			groups[group] = append(groups[group], f)
		}
	}

	for group, members := range groups {
		var supplied []string
		required := false

		for _, member := range members {
			if present[member.index] {
				supplied = append(supplied, member.name)
			}
			if !member.optional {
				required = true
			}
		}

		if len(supplied) > 1 {
			return &XORConflictError{Group: group, Fields: supplied}
		}
		if required && len(supplied) == 0 {
			return &MissingXORGroupError{Group: group, Fields: fieldNames(members)}
		}
	}

	for _, f := range fields {
		if f.optional || len(f.xorGroups) > 0 {
			continue
		}

		if !present[f.index] {
			return &MissingArgError{Name: f.name}
		}
	}

	return nil
}

func fieldNames(fields []fieldMeta) []string {
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.name)
	}

	return names
}

func validatePresentValues(value reflect.Value, fields []fieldMeta, present map[int]bool) error {
	for _, field := range fields {
		if !present[field.index] {
			continue
		}
		if err := validateDecodedValue(value.Field(field.index)); err != nil {
			return &ValueValidationError{Name: field.name, Err: err}
		}
	}

	return nil
}
