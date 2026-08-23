package command

import (
	"fmt"
	"reflect"
	"strings"
)

// UnsupportedNargsTagError reports the project-specific nargs tag,
// which is not part of the supported Kong-aligned grammar.
type UnsupportedNargsTagError struct {
	Field string
}

func (e *UnsupportedNargsTagError) Error() string {
	return fmt.Sprintf("field %s uses unsupported nargs tag", e.Field)
}

// UnsupportedCLIScopeError reports a cli tag other than cli:"-".
type UnsupportedCLIScopeError struct {
	Field string
	Value string
}

func (e *UnsupportedCLIScopeError) Error() string {
	return fmt.Sprintf("field %s has unsupported cli scope %q", e.Field, e.Value)
}

// InvalidMaxTagError reports a max tag that is not a positive integer.
type InvalidMaxTagError struct {
	Field string
	Value string
}

func (e *InvalidMaxTagError) Error() string {
	return fmt.Sprintf("field %s has invalid max value %q", e.Field, e.Value)
}

// MaxOnNonSliceError reports a max tag on a non-slice field.
type MaxOnNonSliceError struct {
	Field string
	Type  reflect.Type
}

func (e *MaxOnNonSliceError) Error() string {
	return fmt.Sprintf("field %s of type %s uses max but is not a slice", e.Field, e.Type)
}

// TooManyValuesError reports a slice field that exceeds its max tag.
type TooManyValuesError struct {
	Name    string
	Maximum int
	Actual  int
}

func (e *TooManyValuesError) Error() string {
	return fmt.Sprintf("field %s accepts at most %d values but received %d", e.Name, e.Maximum, e.Actual)
}

// XORConflictError reports more than one supplied member of an XOR group.
type XORConflictError struct {
	Group  string
	Fields []string
}

func (e *XORConflictError) Error() string {
	return fmt.Sprintf("fields %s cannot be used together in xor group %s", strings.Join(e.Fields, ", "), e.Group)
}

// MissingXORGroupError reports a required XOR group with no supplied member.
type MissingXORGroupError struct {
	Group  string
	Fields []string
}

func (e *MissingXORGroupError) Error() string {
	return fmt.Sprintf("xor group %s requires one of %s", e.Group, strings.Join(e.Fields, ", "))
}

// ToolSchemaPropertyCollisionError reports an XOR group whose
// synthetic property would replace a real tool property.
type ToolSchemaPropertyCollisionError struct {
	Property string
}

func (e *ToolSchemaPropertyCollisionError) Error() string {
	return fmt.Sprintf("xor group property %q conflicts with a tool field", e.Property)
}

// UnsupportedProviderSchemaKeywordError reports a structural JSON
// Schema keyword that cannot be represented by the provider subset.
type UnsupportedProviderSchemaKeywordError struct {
	Tool     string
	Keyword  string
	Location []string
}

// UnsupportedToolSchemaTypeError reports a Go field type whose JSON
// representation cannot be expressed by the generated strict tool schema.
type UnsupportedToolSchemaTypeError struct {
	Type reflect.Type
}

func (e *UnsupportedToolSchemaTypeError) Error() string {
	return fmt.Sprintf("tool schema does not support Go type %s", e.Type)
}

// UnsupportedBooleanToolSchemaError reports a boolean JSON Schema,
// which the generated object-shaped tool schema cannot represent.
type UnsupportedBooleanToolSchemaError struct {
	Value    bool
	Location []string
}

func (e *UnsupportedBooleanToolSchemaError) Error() string {
	location := "schema root"
	if len(e.Location) > 0 {
		location = strings.Join(e.Location, "/")
	}

	return fmt.Sprintf("tool schema does not support boolean schema %t at %s",
		e.Value, location)
}

// ToolSchemaEncodingError reports a custom schema that could not be
// converted into the generic representation used by tool generation.
type ToolSchemaEncodingError struct {
	Err error
}

func (e *ToolSchemaEncodingError) Error() string {
	return fmt.Sprintf("encode tool schema: %s", e.Err)
}

func (e *ToolSchemaEncodingError) Unwrap() error {
	return e.Err
}

func (e *UnsupportedProviderSchemaKeywordError) Error() string {
	return fmt.Sprintf("tool %s schema uses unsupported provider keyword %s at %s",
		e.Tool, e.Keyword, strings.Join(e.Location, "/"))
}

// UnsupportedToolXORError reports a tool field assigned to more than
// one XOR group. One flat field cannot be placed under two synthetic
// schema properties without changing the application representation.
type UnsupportedToolXORError struct {
	Field  string
	Groups []string
}

func (e *UnsupportedToolXORError) Error() string {
	return fmt.Sprintf("tool field %s belongs to multiple xor groups: %s", e.Field, strings.Join(e.Groups, ", "))
}

// ToolSchemaCompileError reports a generated tool schema that the
// runtime validator could not compile.
type ToolSchemaCompileError struct {
	Tool string
	Err  error
}

func (e *ToolSchemaCompileError) Error() string {
	return fmt.Sprintf("compile argument schema for tool %s: %s", e.Tool, e.Err)
}

func (e *ToolSchemaCompileError) Unwrap() error {
	return e.Err
}

// ToolArgumentViolation identifies one failed JSON Schema keyword at
// the path the model supplied.
type ToolArgumentViolation struct {
	InstanceLocation []string
	KeywordLocation  []string
}

// ToolArgumentsValidationError reports model arguments that do not
// match the exact schema published for the tool.
type ToolArgumentsValidationError struct {
	Tool       string
	Violations []ToolArgumentViolation
	Err        error
}

func (e *ToolArgumentsValidationError) Error() string {
	return fmt.Sprintf("arguments for tool %s do not match its schema: %s", e.Tool, e.Err)
}

func (e *ToolArgumentsValidationError) Unwrap() error {
	return e.Err
}

// ToolArgumentsDecodeError reports JSON that passed schema validation
// but could not be represented by the corresponding Go field.
type ToolArgumentsDecodeError struct {
	Tool  string
	Field string
	Err   error
}

func (e *ToolArgumentsDecodeError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("decode arguments for tool %s: %s", e.Tool, e.Err)
	}

	return fmt.Sprintf("decode field %s for tool %s: %s", e.Field, e.Tool, e.Err)
}

func (e *ToolArgumentsDecodeError) Unwrap() error {
	return e.Err
}

// NotToolLeafError reports ToolValue called for a command group.
type NotToolLeafError struct {
	Path string
}

func (e *NotToolLeafError) Error() string {
	return fmt.Sprintf("node /%s is not a tool leaf", e.Path)
}

// ValueValidationError identifies the decoded field or command whose
// Validatable implementation rejected its value.
type ValueValidationError struct {
	Name string
	Err  error
}

func (e *ValueValidationError) Error() string {
	return fmt.Sprintf("validate %s: %s", e.Name, e.Err)
}

func (e *ValueValidationError) Unwrap() error {
	return e.Err
}

// NotACommandError is returned when the input does not begin with a
// slash prefix.
type NotACommandError struct {
	Input string
}

func (e *NotACommandError) Error() string {
	return fmt.Sprintf("not a command: %q", e.Input)
}

// UnknownCommandError is returned when no top-level command matches
// the given name.
type UnknownCommandError struct {
	Name string
}

func (e *UnknownCommandError) Error() string {
	return fmt.Sprintf("unknown command: /%s", e.Name)
}

// UnknownSubcommandError is returned when a token does not match any
// child command of the current node. The node's `Path()` is captured
// verbatim at construction time; see the call site in `command.go`.
type UnknownSubcommandError struct {
	Name string
	Path string
}

func (e *UnknownSubcommandError) Error() string {
	return fmt.Sprintf("unknown subcommand %q for /%s", e.Name, e.Path)
}

// AliasCollisionError is returned at build time when an alias
// conflicts with an existing command name or alias.
type AliasCollisionError struct {
	Alias         string
	Command       string
	ConflictsWith string
}

func (e *AliasCollisionError) Error() string {
	return fmt.Sprintf("alias %q on command %q conflicts with %s", e.Alias, e.Command, e.ConflictsWith)
}

// DuplicateCommandError is returned at build time when two commands
// share the same name.
type DuplicateCommandError struct {
	Name          string
	ConflictsWith string
}

// PassthroughOnFlagError reports a passthrough tag on a flag. Kong
// defines passthrough for positional arguments and commands; this
// parser supports the positional form.
type PassthroughOnFlagError struct {
	Field string
}

func (e *PassthroughOnFlagError) Error() string {
	return fmt.Sprintf("passthrough field %s is not a positional argument", e.Field)
}

// UnsupportedPassthroughModeError reports a passthrough mode other
// than Kong's all and partial modes.
type UnsupportedPassthroughModeError struct {
	Field string
	Mode  string
}

func (e *UnsupportedPassthroughModeError) Error() string {
	return fmt.Sprintf("passthrough field %s has unsupported mode %q", e.Field, e.Mode)
}

// PassthroughNotFinalError reports a positional after a passthrough
// positional. A passthrough consumes the rest of the command line,
// so no later positional could be populated.
type PassthroughNotFinalError struct {
	Field string
}

func (e *PassthroughNotFinalError) Error() string {
	return fmt.Sprintf("passthrough field %s is not the final positional argument", e.Field)
}

func (e *DuplicateCommandError) Error() string {
	return fmt.Sprintf("duplicate command name %q (conflicts with %s)", e.Name, e.ConflictsWith)
}

// NoFactoryError is returned when a leaf node has no factory function
// to produce its command struct.
type NoFactoryError struct {
	Path string
}

func (e *NoFactoryError) Error() string {
	return fmt.Sprintf("command /%s has no factory", e.Path)
}

// FieldError wraps an error with the struct field name that caused
// it during grammar building.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("field %s: %s", e.Field, e.Err)
}

func (e *FieldError) Unwrap() error {
	return e.Err
}

// InterfaceError is returned when a parsed command struct does
// not implement the expected Command interface.
type InterfaceError struct {
	Value any
}

func (e *InterfaceError) Error() string {
	return fmt.Sprintf("parsed command %T does not implement the expected command interface", e.Value)
}

// SubcommandError is returned when a group node is invoked without
// specifying a subcommand. The group's path and available children
// are captured as strings so the error surface doesn't need a type
// parameter.
type SubcommandError struct {
	Path     string
	Children []string
}

func (e *SubcommandError) Error() string {
	return fmt.Sprintf(
		"/%s requires a subcommand: %s",
		e.Path, strings.Join(e.Children, ", "),
	)
}
