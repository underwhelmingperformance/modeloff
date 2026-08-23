// Package command provides generic infrastructure for parsing and
// completing IRC-style slash commands. Concrete command types and
// execution contexts are defined by the consumer; the cancellation
// context is threaded through explicitly as the first parameter to
// [Command.Run].
package command

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Command is the interface that parsed command structs must
// implement. C is the run context type provided by the consumer, R
// is the return type (e.g. tea.Cmd). The cancellation context is
// threaded as an explicit first parameter so consumers do not have
// to stuff it into C.
type Command[C any, R any] interface {
	Run(context.Context, C) R
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
	optionsEnded    bool
}

type invocationToken struct {
	text  string
	start int
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

// RedactedRaw returns input unchanged, unless it resolves to a
// command node the grammar marks `secret:""` — /config api-key is
// the one case today. A secret-bearing input is collapsed to its
// command path with the argument text dropped, so a caller logging
// the command a user ran never logs the credential that followed it,
// while still naming which command ran.
func (p Parser[K, C, R]) RedactedRaw(input string) string {
	node, secret := p.set.resolveSecret(input)
	if !secret {
		return input
	}

	return "/" + node.Path() + " <redacted>"
}

// IsSecret reports whether input resolves to a command node the
// grammar marks `secret:""`, the same resolution RedactedRaw uses.
// It gives a caller that only needs the yes/no answer — an input
// bar deciding whether to keep a typed line in history, say — that
// answer without redacting text of its own.
func (p Parser[K, C, R]) IsSecret(input string) bool {
	_, secret := p.set.resolveSecret(input)

	return secret
}

// SecretChecker is the narrow, type-parameter-free view of a
// [Parser] a caller outside the `command` package's generic surface
// can hold: any Parser instantiation satisfies it. Components that
// only need to ask "does this line carry a credential" — the input
// bar's history exclusion, for instance — take this instead of a
// concrete Parser, so they need not name the grammar's completion,
// run-context and result type parameters.
type SecretChecker interface {
	IsSecret(input string) bool
}

// ParseInvocation returns the full parsed branch, including ancestor
// values and the selected leaf. A bare invocation of a group node
// (one with `cmd:""` children) whose own value implements
// Command[C, R] runs the group itself, so a grammar can give a
// childful command a bare-form action alongside its subcommands:
// the irssi `/set` convention of a bare command reporting its
// current state. A group node whose value does not implement
// Command[C, R] still surfaces a [SubcommandError] for a bare
// invocation.
func (p Parser[K, C, R]) ParseInvocation(input string) (Invocation[K], error) {
	invocation, err := p.set.ParseInvocation(input)
	if err == nil {
		return invocation, nil
	}

	var subErr *SubcommandError
	if errors.As(err, &subErr) {
		if _, ok := invocation.Leaf().(Command[C, R]); ok {
			return invocation, nil
		}
	}

	return Invocation[K]{}, err
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

	tokens := scanInvocationTokens(input)
	if len(tokens) == 0 || tokens[0].text == "" || tokens[0].text[0] != '/' {
		return Invocation[K]{}, &NotACommandError{Input: input}
	}

	name := strings.TrimPrefix(tokens[0].text, "/")
	args := tokens[1:]

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

	current, err := parseInvocationArgs(input, args, node, &path, values, states)
	if err != nil {
		return Invocation[K]{}, err
	}

	invocation, err := buildInvocation(path, values, states)
	if err != nil {
		return Invocation[K]{}, err
	}

	if len(current.Children) > 0 {
		// current still has unconsumed children: no subcommand token
		// was given. The invocation is still returned alongside the
		// error, with current's own value fully populated from
		// whatever args it did consume, so a typed caller such as
		// [Parser.ParseInvocation] can run the group itself when its
		// value implements the caller's Command interface (irssi's
		// bare `/set` reporting current state, for example).
		return invocation, subcommandErrorFor(current)
	}

	return invocation, nil
}

func parseInvocationArgs[K KindProvider](
	raw string,
	args []invocationToken,
	root *Node[K],
	path *[]*Node[K],
	values map[*Node[K]]any,
	states map[*Node[K]]*nodeState,
) (*Node[K], error) {
	current := root

	for i := 0; i < len(args); i++ {
		next, nextIndex, done, err := consumeInvocationToken(raw, args, i, current, path, values, states)
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
	raw string,
	args []invocationToken,
	index int,
	current *Node[K],
	path *[]*Node[K],
	values map[*Node[K]]any,
	states map[*Node[K]]*nodeState,
) (*Node[K], int, bool, error) {
	tok := args[index]
	state := states[current]
	optionKind := classifyOptionToken(tok.text)
	flagName, _, hasAttachedValue := splitLongFlagToken(tok.text)
	if !hasAttachedValue {
		flagName = tok.text
	}

	if pos := resolvePositional(current.Positionals, state.positionalIndex); pos != nil {
		if pos.Passthrough == PassthroughModePartial && optionKind == optionTokenEnd {
			state.args = append(state.args, raw[tok.start:])
			state.optionsEnded = true

			return current, len(args), true, nil
		}

		_, knownFlag := findFlagBinding(current, flagName)
		startPassthrough := pos.Passthrough != PassthroughModeNone && !knownFlag
		if pos.Passthrough == PassthroughModePartial && optionKind.isFlag() {
			startPassthrough = false
		}

		if startPassthrough {
			state.args = append(state.args, raw[tok.start:])
			return current, len(args), true, nil
		}
	}

	if binding, ok := findFlagBinding(current, flagName); ok {
		nextIndex, done := consumeInvocationFlag(args, index, hasAttachedValue, binding, states)
		return current, nextIndex, done, nil
	}

	if optionKind == optionTokenEnd {
		state.args = append(state.args, invocationTokenTexts(args[index:])...)
		return current, index, true, nil
	}

	if optionKind.isFlag() {
		state.args = append(state.args, invocationTokenTexts(args[index:])...)
		return current, index, true, nil
	}

	if state.variadic {
		state.args = append(state.args, tok.text)
		return current, index, false, nil
	}

	if pos := resolvePositional(current.Positionals, state.positionalIndex); pos != nil {
		state.args = append(state.args, tok.text)
		if pos.Variadic {
			state.variadic = true
			return current, index, false, nil
		}

		state.positionalIndex++
		return current, index, false, nil
	}

	if child := current.Find(tok.text); child != nil {
		appendInvocationNode(path, child, values, states)
		return child, index, false, nil
	}

	if len(current.Children) > 0 {
		return nil, index, false, &UnknownSubcommandError{Name: tok.text, Path: current.Path()}
	}

	state.args = append(state.args, tok.text)
	return current, index, false, nil
}

type optionTokenKind uint8

const (
	optionTokenValue optionTokenKind = iota
	optionTokenLoneDash
	optionTokenEnd
	optionTokenLong
	optionTokenShort
)

func classifyOptionToken(value string) optionTokenKind {
	switch {
	case value == "-":
		return optionTokenLoneDash
	case value == "--":
		return optionTokenEnd
	case strings.HasPrefix(value, "--"):
		return optionTokenLong
	case len(value) > 1 && value[0] == '-' && value[1] >= '0' && value[1] <= '9':
		return optionTokenValue
	case strings.HasPrefix(value, "-"):
		return optionTokenShort
	default:
		return optionTokenValue
	}
}

func (k optionTokenKind) isFlag() bool {
	return k == optionTokenLong || k == optionTokenShort
}

func splitLongFlagToken(value string) (string, string, bool) {
	if !strings.HasPrefix(value, "--") {
		return "", "", false
	}

	name, attached, ok := strings.Cut(value, "=")
	if !ok || name == "--" {
		return "", "", false
	}

	return name, attached, true
}

func consumeInvocationFlag[K KindProvider](
	args []invocationToken,
	index int,
	hasAttachedValue bool,
	binding flagBinding[K],
	states map[*Node[K]]*nodeState,
) (int, bool) {
	state := states[binding.Owner]
	state.args = append(state.args, args[index].text)

	if binding.Flag.Boolean || hasAttachedValue {
		return index, false
	}

	if index+1 >= len(args) {
		return index, false
	}

	if binding.Flag.Variadic {
		state.args = append(state.args, invocationTokenTexts(args[index+1:])...)
		return len(args), true
	}

	state.args = append(state.args, args[index+1].text)
	return index + 1, false
}

func invocationTokenTexts(tokens []invocationToken) []string {
	values := make([]string, len(tokens))
	for i, token := range tokens {
		values[i] = token.text
	}

	return values
}

func scanInvocationTokens(input string) []invocationToken {
	var tokens []invocationToken

	for i := 0; i < len(input); {
		for i < len(input) {
			r, size := utf8.DecodeRuneInString(input[i:])
			if !unicode.IsSpace(r) {
				break
			}
			i += size
		}

		if i >= len(input) {
			break
		}

		start := i
		for i < len(input) {
			r, size := utf8.DecodeRuneInString(input[i:])
			if unicode.IsSpace(r) {
				break
			}
			i += size
		}

		tokens = append(tokens, invocationToken{
			text:  input[start:i],
			start: start,
		})
	}

	return tokens
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

		state := states[pathNode]
		if err := parseInto(value, state.args, state.optionsEnded); err != nil {
			return Invocation[K]{}, err
		}

		invocation.Path = append(invocation.Path, NodeValue[K]{
			Node:  pathNode,
			Value: reflect.ValueOf(value).Elem().Interface(),
		})
	}

	return invocation, nil
}
