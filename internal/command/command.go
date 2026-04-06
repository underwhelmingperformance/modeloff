// Package command provides generic infrastructure for parsing and
// completing IRC-style slash commands. It is independent of any
// particular application — concrete command types and execution
// contexts are defined by the consumer.
package command

import (
	"fmt"
	"reflect"
	"strings"
)

// Command is the interface that parsed command structs must
// implement. C is the run context type provided by the consumer, R
// is the return type (e.g. tea.Cmd).
type Command[C any, R any] interface {
	Run(C) R
}

// Parser wraps a Set and returns typed Command values from Parse.
// C is the run context and R is the return type.
type Parser[C any, R any] struct {
	set Set
}

// BuildParser reflects over a grammar struct and produces a typed
// Parser. Each field tagged with `cmd:""` becomes a command node.
func BuildParser[C any, R any](grammar any) Parser[C, R] {
	return Parser[C, R]{set: Build(grammar)}
}

// Set returns the underlying command Set for use with completion
// and other infrastructure that does not need the type parameters.
func (p Parser[C, R]) Set() Set {
	return p.set
}

// Parse tokenises a raw slash-command string, resolves the matching
// node, populates fields, and asserts the result implements
// Command[C, R].
func (p Parser[C, R]) Parse(input string) (Command[C, R], error) {
	parsed, err := p.set.ParseValue(input)
	if err != nil {
		return nil, err
	}

	cmd, ok := parsed.(Command[C, R])
	if !ok {
		return nil, fmt.Errorf("parsed command %T does not implement the expected command interface", parsed)
	}

	return cmd, nil
}

// Build reflects over a grammar struct and produces a Set. Each
// field tagged with `cmd:""` becomes a command node. Name derives
// from the field name (kebab-cased) or from a `name:""` tag. Help
// comes from the `help:""` tag. The grammar must be a pointer to a
// struct.
func Build(grammar any) Set {
	nodes, err := build(grammar)
	if err != nil {
		panic(fmt.Sprintf("building command set: %v", err))
	}

	return Set{Commands: nodes}
}

// SubcommandError is returned when a group node is invoked without
// specifying a subcommand.
type SubcommandError struct {
	Node *Node
}

func (e *SubcommandError) Error() string {
	names := make([]string, 0, len(e.Node.Children))
	for _, child := range e.Node.Children {
		names = append(names, child.Name)
	}

	return fmt.Sprintf(
		"/%s requires a subcommand: %s",
		e.Node.Name, strings.Join(names, ", "),
	)
}

// ParseValue tokenises a raw slash-command string, resolves the
// matching node in the set, and populates a command struct from the
// arguments. The returned value is the concrete command struct
// (value type, not pointer).
//
// For group nodes (nodes with children), the first argument is
// matched against child names and parsing recurses into the matched
// child. This continues until a leaf node is reached.
func (s Set) ParseValue(input string) (any, error) {
	input = strings.TrimSpace(input)

	if input == "" || input[0] != '/' {
		return nil, fmt.Errorf("not a command: %q", input)
	}

	fields := strings.Fields(input)
	name := strings.TrimPrefix(fields[0], "/")
	args := fields[1:]

	node := s.Find(name)
	if node == nil {
		return nil, fmt.Errorf("unknown command: /%s", name)
	}

	// Walk into children for group nodes.
	path := "/" + name

	for len(node.Children) > 0 {
		if len(args) == 0 {
			return nil, &SubcommandError{Node: node}
		}

		child := node.Find(args[0])
		if child == nil {
			return nil, fmt.Errorf("unknown subcommand %q for %s", args[0], path)
		}

		path += " " + child.Name
		node = child
		args = args[1:]
	}

	if node.factory == nil {
		return nil, fmt.Errorf("command %s has no factory", path)
	}

	cmd := node.factory()

	if err := ParseInto(cmd, args); err != nil {
		return nil, err
	}

	// Dereference so callers get value types (e.g. JoinCommand, not
	// *JoinCommand), matching the convention used throughout the UI.
	return reflect.ValueOf(cmd).Elem().Interface(), nil
}
