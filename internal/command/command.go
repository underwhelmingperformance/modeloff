// Package command provides generic infrastructure for parsing and
// completing IRC-style slash commands. It is independent of any
// particular application — concrete command types and execution
// contexts are defined by the consumer.
package command

import (
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
// K is the completion-context type (for grammar parameterisation), C
// is the run context and R is the return type.
type Parser[K KindProvider, C any, R any] struct {
	set Set[K]
}

// Invocation records the populated values for each node on the
// matched command branch, from the top-level command to the selected
// leaf.
type Invocation[K KindProvider] struct {
	Path []NodeValue[K]
}

// NodeValue is a parsed node value within an Invocation.
type NodeValue[K KindProvider] struct {
	Node  *Node[K]
	Value any
}

// subcommandErrorFor builds a [SubcommandError] that captures the
// group's path and the names and aliases of its direct children, so
// the error surface itself does not need a grammar type parameter.
func subcommandErrorFor[K KindProvider](node *Node[K]) *SubcommandError {
	var names []string
	for _, child := range node.Children {
		for name := range child.Names() {
			names = append(names, name)
		}
	}

	return &SubcommandError{
		Path:     node.Path(),
		Children: names,
	}
}

type nodeState struct {
	args            []string
	positionalIndex int
	variadic        bool
}

// Selected returns the matched leaf node.
func (i Invocation[K]) Selected() *Node[K] {
	if len(i.Path) == 0 {
		return nil
	}

	return i.Path[len(i.Path)-1].Node
}

// Leaf returns the parsed value for the selected leaf node.
func (i Invocation[K]) Leaf() any {
	if len(i.Path) == 0 {
		return nil
	}

	return i.Path[len(i.Path)-1].Value
}

// ValueFor returns the parsed value associated with the given node.
func (i Invocation[K]) ValueFor(node *Node[K]) (any, bool) {
	for _, entry := range i.Path {
		if entry.Node == node {
			return entry.Value, true
		}
	}

	return nil, false
}

// ValueAtPath returns the parsed value for the node at the given
// command path, such as "config" or "config set".
func (i Invocation[K]) ValueAtPath(path string) (any, bool) {
	for _, entry := range i.Path {
		if entry.Node != nil && entry.Node.Path() == path {
			return entry.Value, true
		}
	}

	return nil, false
}

// BuildParser reflects over a grammar struct and produces a typed
// Parser. Each field tagged with `cmd:""` becomes a command node.
func BuildParser[K KindProvider, C any, R any](grammar any) (Parser[K, C, R], error) {
	set, err := Build[K](grammar)
	if err != nil {
		return Parser[K, C, R]{}, err
	}

	return Parser[K, C, R]{set: set}, nil
}

// Set returns the underlying command Set for use with completion
// and other infrastructure that does not need the type parameters.
func (p Parser[K, C, R]) Set() Set[K] {
	return p.set
}

// Parse tokenises a raw slash-command string, resolves the matching
// node, populates fields, and asserts the result implements
// Command[C, R].
func (p Parser[K, C, R]) Parse(input string) (Command[C, R], error) {
	invocation, err := p.ParseInvocation(input)
	if err != nil {
		return nil, err
	}

	cmd, ok := invocation.Leaf().(Command[C, R])
	if !ok {
		return nil, &InterfaceError{Value: invocation.Leaf()}
	}

	return cmd, nil
}

// ParseInvocation returns the full parsed branch, including ancestor
// values and the selected leaf.
func (p Parser[K, C, R]) ParseInvocation(input string) (Invocation[K], error) {
	return p.set.ParseInvocation(input)
}

// Build reflects over a grammar struct and produces a Set. Each
// field tagged with `cmd:""` becomes a command node. Name derives
// from the field name (kebab-cased) or from a `name:""` tag. Help
// comes from the `help:""` tag. The grammar must be a pointer to a
// struct.
func Build[K KindProvider](grammar any) (Set[K], error) {
	nodes, err := build[K](grammar)
	if err != nil {
		return Set[K]{}, err
	}

	return Set[K]{Commands: nodes}, nil
}

// ParseValue tokenises a raw slash-command string, resolves the
// matching branch in the set, and returns the selected leaf value.
func (s Set[K]) ParseValue(input string) (any, error) {
	invocation, err := s.ParseInvocation(input)
	if err != nil {
		return nil, err
	}

	return invocation.Leaf(), nil
}

// ParseInvocation tokenises a raw slash-command string, resolves the
// matching branch in the set, and populates values for each matched
// node from the top-level command to the selected leaf.
func (s Set[K]) ParseInvocation(input string) (Invocation[K], error) {
	s.linkParents()

	input = strings.TrimSpace(input)

	if input == "" || input[0] != '/' {
		return Invocation[K]{}, &NotACommandError{Input: input}
	}

	fields := strings.Fields(input)
	name := strings.TrimPrefix(fields[0], "/")
	args := fields[1:]

	node := s.Find(name)
	if node == nil {
		return Invocation[K]{}, &UnknownCommandError{Name: name}
	}

	path := []*Node[K]{node}
	values := map[*Node[K]]any{}
	states := map[*Node[K]]*nodeState{
		node: {},
	}

	if node.factory != nil {
		values[node] = node.factory()
	}

	current, err := parseInvocationArgs(args, node, &path, values, states)
	if err != nil {
		return Invocation[K]{}, err
	}

	if len(current.Children) > 0 {
		return Invocation[K]{}, subcommandErrorFor(current)
	}

	return buildInvocation(path, values, states)
}

func parseInvocationArgs[K KindProvider](
	args []string,
	root *Node[K],
	path *[]*Node[K],
	values map[*Node[K]]any,
	states map[*Node[K]]*nodeState,
) (*Node[K], error) {
	current := root

	for i := 0; i < len(args); i++ {
		next, nextIndex, done, err := consumeInvocationToken(args, i, current, path, values, states)
		if err != nil {
			return nil, err
		}

		current = next
		i = nextIndex

		if done {
			break
		}
	}

	return current, nil
}

func consumeInvocationToken[K KindProvider](
	args []string,
	index int,
	current *Node[K],
	path *[]*Node[K],
	values map[*Node[K]]any,
	states map[*Node[K]]*nodeState,
) (*Node[K], int, bool, error) {
	tok := args[index]

	if binding, ok := findFlagBinding(current, tok); ok {
		nextIndex, done := consumeInvocationFlag(args, index, binding, states)
		return current, nextIndex, done, nil
	}

	state := states[current]

	if strings.HasPrefix(tok, "--") {
		state.args = append(state.args, args[index:]...)
		return current, index, true, nil
	}

	if state.variadic {
		state.args = append(state.args, tok)
		return current, index, false, nil
	}

	if pos := resolvePositional(current.Positionals, state.positionalIndex); pos != nil {
		state.args = append(state.args, tok)
		if pos.Variadic {
			state.variadic = true
			return current, index, false, nil
		}

		state.positionalIndex++
		return current, index, false, nil
	}

	if child := current.Find(tok); child != nil {
		appendInvocationNode(path, child, values, states)
		return child, index, false, nil
	}

	if len(current.Children) > 0 {
		return nil, index, false, &UnknownSubcommandError{Name: tok, Path: current.Path()}
	}

	state.args = append(state.args, tok)
	return current, index, false, nil
}

func consumeInvocationFlag[K KindProvider](
	args []string,
	index int,
	binding flagBinding[K],
	states map[*Node[K]]*nodeState,
) (int, bool) {
	state := states[binding.Owner]
	state.args = append(state.args, args[index])

	if binding.Flag.Boolean {
		return index, false
	}

	if index+1 >= len(args) {
		return index, false
	}

	if binding.Flag.Variadic {
		state.args = append(state.args, args[index+1:]...)
		return len(args), true
	}

	state.args = append(state.args, args[index+1])
	return index + 1, false
}

func appendInvocationNode[K KindProvider](path *[]*Node[K], node *Node[K], values map[*Node[K]]any, states map[*Node[K]]*nodeState) {
	*path = append(*path, node)
	states[node] = &nodeState{}

	if node.factory != nil {
		values[node] = node.factory()
	}
}

func buildInvocation[K KindProvider](path []*Node[K], values map[*Node[K]]any, states map[*Node[K]]*nodeState) (Invocation[K], error) {
	invocation := Invocation[K]{
		Path: make([]NodeValue[K], 0, len(path)),
	}

	for _, pathNode := range path {
		value := values[pathNode]

		if pathNode.factory == nil {
			if len(pathNode.Children) == 0 {
				return Invocation[K]{}, &NoFactoryError{Path: pathNode.Path()}
			}

			invocation.Path = append(invocation.Path, NodeValue[K]{
				Node: pathNode,
			})
			continue
		}

		if err := ParseInto(value, states[pathNode].args); err != nil {
			return Invocation[K]{}, err
		}

		invocation.Path = append(invocation.Path, NodeValue[K]{
			Node:  pathNode,
			Value: reflect.ValueOf(value).Elem().Interface(),
		})
	}

	return invocation, nil
}
