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

// Invocation records the populated values for each node on the
// matched command branch, from the top-level command to the selected
// leaf.
type Invocation struct {
	Path []NodeValue
}

// NodeValue is a parsed node value within an Invocation.
type NodeValue struct {
	Node  *Node
	Value any
}

// Selected returns the matched leaf node.
func (i Invocation) Selected() *Node {
	if len(i.Path) == 0 {
		return nil
	}

	return i.Path[len(i.Path)-1].Node
}

// Leaf returns the parsed value for the selected leaf node.
func (i Invocation) Leaf() any {
	if len(i.Path) == 0 {
		return nil
	}

	return i.Path[len(i.Path)-1].Value
}

// ValueFor returns the parsed value associated with the given node.
func (i Invocation) ValueFor(node *Node) (any, bool) {
	for _, entry := range i.Path {
		if entry.Node == node {
			return entry.Value, true
		}
	}

	return nil, false
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
	invocation, err := p.ParseInvocation(input)
	if err != nil {
		return nil, err
	}

	cmd, ok := invocation.Leaf().(Command[C, R])
	if !ok {
		return nil, fmt.Errorf("parsed command %T does not implement the expected command interface", invocation.Leaf())
	}

	return cmd, nil
}

// ParseInvocation returns the full parsed branch, including ancestor
// values and the selected leaf.
func (p Parser[C, R]) ParseInvocation(input string) (Invocation, error) {
	return p.set.ParseInvocation(input)
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
		e.Node.Path(), strings.Join(names, ", "),
	)
}

// ParseValue tokenises a raw slash-command string, resolves the
// matching branch in the set, and returns the selected leaf value.
func (s Set) ParseValue(input string) (any, error) {
	invocation, err := s.ParseInvocation(input)
	if err != nil {
		return nil, err
	}

	return invocation.Leaf(), nil
}

type nodeState struct {
	args            []string
	positionalIndex int
	variadic        bool
}

// ParseInvocation tokenises a raw slash-command string, resolves the
// matching branch in the set, and populates values for each matched
// node from the top-level command to the selected leaf.
func (s Set) ParseInvocation(input string) (Invocation, error) {
	s.linkParents()

	input = strings.TrimSpace(input)

	if input == "" || input[0] != '/' {
		return Invocation{}, fmt.Errorf("not a command: %q", input)
	}

	fields := strings.Fields(input)
	name := strings.TrimPrefix(fields[0], "/")
	args := fields[1:]

	node := s.Find(name)
	if node == nil {
		return Invocation{}, fmt.Errorf("unknown command: /%s", name)
	}

	path := []*Node{node}
	values := map[*Node]any{}
	states := map[*Node]*nodeState{
		node: {},
	}

	if node.factory != nil {
		values[node] = node.factory()
	}

	current := node

	for i := 0; i < len(args); i++ {
		tok := args[i]

		if binding, ok := findFlagBinding(current, tok); ok {
			state := states[binding.Owner]
			state.args = append(state.args, tok)

			if i+1 >= len(args) {
				continue
			}

			if binding.Flag.Variadic {
				state.args = append(state.args, args[i+1:]...)
				i = len(args)
				continue
			}

			state.args = append(state.args, args[i+1])
			i++
			continue
		}

		state := states[current]

		if strings.HasPrefix(tok, "--") {
			state.args = append(state.args, args[i:]...)
			break
		}

		if state.variadic {
			state.args = append(state.args, tok)
			continue
		}

		if pos := resolvePositional(current.Positionals, state.positionalIndex); pos != nil {
			state.args = append(state.args, tok)

			if pos.Variadic {
				state.variadic = true
				continue
			}

			state.positionalIndex++
			continue
		}

		if child := current.Find(tok); child != nil {
			current = child
			path = append(path, child)
			states[child] = &nodeState{}

			if child.factory != nil {
				values[child] = child.factory()
			}

			continue
		}

		if len(current.Children) > 0 {
			return Invocation{}, fmt.Errorf("unknown subcommand %q for /%s", tok, current.Path())
		}

		state.args = append(state.args, tok)
	}

	if len(current.Children) > 0 {
		return Invocation{}, &SubcommandError{Node: current}
	}

	return assembleInvocation(path, values, states)
}

// assembleInvocation parses the collected args for each node on the
// matched branch and builds the final Invocation.
func assembleInvocation(
	path []*Node,
	values map[*Node]any,
	states map[*Node]*nodeState,
) (Invocation, error) {
	invocation := Invocation{
		Path: make([]NodeValue, 0, len(path)),
	}

	for _, pathNode := range path {
		value := values[pathNode]

		if pathNode.factory == nil {
			if len(pathNode.Children) == 0 {
				return Invocation{}, fmt.Errorf("command /%s has no factory", pathNode.Path())
			}

			invocation.Path = append(invocation.Path, NodeValue{
				Node: pathNode,
			})
			continue
		}

		if err := ParseInto(value, states[pathNode].args); err != nil {
			return Invocation{}, err
		}

		invocation.Path = append(invocation.Path, NodeValue{
			Node:  pathNode,
			Value: reflect.ValueOf(value).Elem().Interface(),
		})
	}

	return invocation, nil
}
