package command

import (
	"fmt"
	"strings"
)

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
