package command

import (
	"slices"
	"strings"
)

// Suggestion is a single completion option. Every suggestion carries
// its own Usage text so the popover can display it when the
// suggestion is selected, without promoting any entry to a special
// header.
type Suggestion struct {
	Value  string
	Label  string
	Detail string
	Usage  string
}

// SuggestionSource returns suggestions for the current argument.
// Sources are closures that capture whatever live data they need.
type SuggestionSource func(InvocationState) []Suggestion

// Positional describes a positional command argument.
type Positional struct {
	Name     string
	Help     string
	Optional bool
	Variadic bool
	Nargs    *int
	Source   SuggestionSource
}

// Flag describes a named flag argument (e.g. --persona).
type Flag struct {
	Name     string
	Help     string
	Optional bool
	Variadic bool
	Source   SuggestionSource
}

// Node is a command in the command tree. Leaf nodes (no children)
// are executable commands. Non-leaf nodes are command groups whose
// children are subcommands.
type Node struct {
	Parent      *Node
	Name        string
	Help        string
	Positionals []Positional
	Flags       []Flag
	Children    []*Node

	// factory creates a zero-valued pointer to the command struct for
	// parsing. Nil for group nodes that have no struct of their own.
	factory func() any
}

// Usage returns a human-readable usage string for this node,
// generated from its name, positionals, and flags. This mirrors
// Kong's Node.Summary().
func (n *Node) Usage() string {
	var b strings.Builder

	b.WriteString("/")
	b.WriteString(n.Path())

	for _, p := range n.Positionals {
		b.WriteString(" ")

		if p.Optional {
			b.WriteString("[")
			b.WriteString(p.Name)
			b.WriteString("]")
		} else {
			b.WriteString("<")
			b.WriteString(p.Name)
			b.WriteString(">")
		}
	}

	for _, f := range n.AllFlags() {
		b.WriteString(" [")
		b.WriteString(f.Name)

		if f.Variadic {
			b.WriteString(" <")
			// Strip the -- prefix for the placeholder.
			b.WriteString(strings.TrimPrefix(f.Name, "--"))
			b.WriteString(">")
		}

		b.WriteString("]")
	}

	if len(n.Children) > 0 && len(n.Positionals) == 0 {
		b.WriteString(" <command>")
	}

	return b.String()
}

// Path returns the node's command path relative to the set root.
func (n *Node) Path() string {
	if n == nil {
		return ""
	}

	if n.Parent == nil {
		return n.Name
	}

	parent := n.Parent.Path()
	if parent == "" {
		return n.Name
	}

	return parent + " " + n.Name
}

// Leaf returns true if this node has no children.
func (n *Node) Leaf() bool {
	return len(n.Children) == 0
}

// AllFlags returns flags visible at this node, starting with
// ancestors and ending with the node's own flags.
func (n *Node) AllFlags() []Flag {
	bindings := allFlagBindings(n)
	flags := make([]Flag, 0, len(bindings))

	for _, binding := range bindings {
		flags = append(flags, *binding.Flag)
	}

	return flags
}

// Find looks up a direct child node by name.
func (n *Node) Find(name string) *Node {
	for _, child := range n.Children {
		if child.Name == name {
			return child
		}
	}

	return nil
}

// Set is the set of commands available in a given context. It acts
// as the root of the command tree.
type Set struct {
	Commands []*Node
}

type flagBinding struct {
	Owner *Node
	Flag  *Flag
}

// Completer is implemented by command structs that provide their own
// suggestion sources. The returned map keys are positional or flag
// names.
type Completer interface {
	Sources() map[string]SuggestionSource
}

// Find looks up a top-level node by name.
func (s Set) Find(name string) *Node {
	for _, node := range s.Commands {
		if node.Name == name {
			return node
		}
	}

	return nil
}

func (s Set) linkParents() {
	for _, node := range s.Commands {
		linkNode(node, nil)
	}
}

func linkNode(node, parent *Node) {
	if node == nil {
		return
	}

	node.Parent = parent

	for _, child := range node.Children {
		linkNode(child, node)
	}
}

// InvocationState describes the current parse state for completion.
type InvocationState struct {
	Raw          string
	Name         string
	Args         []string
	Command      *Node
	CurrentIndex int
	CurrentToken string
}

// Completion describes the UI state for the command popover.
type Completion struct {
	Visible      bool
	Suggestions  []Suggestion
	ReplaceStart int
	ReplaceEnd   int
	AppendSpace  bool
}

type token struct {
	Text  string
	Start int
	End   int
}

// Merge combines command sets from most-local to least-local precedence.
func Merge(sets ...Set) Set {
	merged := Set{}
	seen := map[string]struct{}{}

	for _, set := range sets {
		for _, node := range set.Commands {
			if _, ok := seen[node.Name]; ok {
				continue
			}

			seen[node.Name] = struct{}{}
			merged.Commands = append(merged.Commands, node)
		}
	}

	return merged
}

// Complete resolves the completion state for the current buffer.
func Complete(set Set, raw string, cursor int) Completion {
	set.linkParents()

	raw = clampRaw(raw)
	if !strings.HasPrefix(raw, "/") {
		return Completion{}
	}

	runes := []rune(raw)
	cursor = clampCursor(cursor, len(runes))

	tokens := scanTokens(runes)
	index, start, end := currentToken(tokens, runes, cursor)
	prefix := ""
	if start <= end && start < len(runes) {
		prefix = string(runes[start:end])
	}

	if index == 0 {
		return Completion{
			Visible:      true,
			Suggestions:  filterSuggestions(commandSuggestions(set), prefix),
			ReplaceStart: start,
			ReplaceEnd:   end,
			AppendSpace:  true,
		}
	}

	name := ""
	if len(tokens) > 0 {
		name = tokens[0].Text
	}

	node := set.Find(name)
	if node == nil {
		return Completion{
			Visible:      true,
			ReplaceStart: start,
			ReplaceEnd:   end,
		}
	}

	args := make([]string, 0, len(tokens)-1)
	for _, tok := range tokens[1:] {
		args = append(args, tok.Text)
	}

	preceding := argTokensFrom(tokens, 1, index)
	cctx := classifyForCompletion(node, preceding)

	if cctx.invalid {
		return Completion{
			Visible:      true,
			ReplaceStart: start,
			ReplaceEnd:   end,
		}
	}

	state := InvocationState{
		Raw:          raw,
		Name:         name,
		Args:         args,
		Command:      cctx.node,
		CurrentIndex: cctx.positionalIndex,
		CurrentToken: prefix,
	}

	completion := Completion{
		Visible:      true,
		ReplaceStart: start,
		ReplaceEnd:   end,
		AppendSpace:  true,
	}

	// Flag value completion: previous token was a flag name.
	if cctx.expectingFlagValue != nil {
		flag := cctx.expectingFlagValue

		if flag.Source != nil {
			completion.Suggestions = filterSuggestions(flag.Source(state), prefix)
		}

		return completion
	}

	// Flag name completion: current token starts with "--".
	if strings.HasPrefix(prefix, "--") {
		completion.Suggestions = filterSuggestions(flagSuggestions(cctx.node, cctx.usedFlags), prefix)
		return completion
	}

	if len(cctx.node.Children) > 0 && resolvePositional(cctx.node.Positionals, cctx.positionalIndex) == nil {
		completion.Suggestions = filterSuggestions(groupSuggestions(cctx.node, cctx.usedFlags), prefix)
		return completion
	}

	// Positional completion.
	pos := resolvePositional(cctx.node.Positionals, cctx.positionalIndex)
	if pos != nil && pos.Source != nil {
		completion.Suggestions = filterSuggestions(pos.Source(state), prefix)
		completion.AppendSpace = hasContinuation(cctx.node, cctx.positionalIndex)
		return completion
	}

	if pos != nil {
		completion.AppendSpace = hasContinuation(cctx.node, cctx.positionalIndex)
		return completion
	}

	// Past all positionals: offer flag names.
	flags := flagSuggestions(cctx.node, cctx.usedFlags)
	if len(flags) > 0 {
		completion.Suggestions = filterSuggestions(flags, prefix)
		return completion
	}

	completion.AppendSpace = false
	return completion
}

func clampRaw(raw string) string {
	return string([]rune(raw))
}

func clampCursor(cursor, length int) int {
	if cursor < 0 {
		return 0
	}

	if cursor > length {
		return length
	}

	return cursor
}

func commandSuggestions(set Set) []Suggestion {
	suggestions := make([]Suggestion, 0, len(set.Commands))

	for _, node := range set.Commands {
		suggestions = append(suggestions, Suggestion{
			Value:  node.Name,
			Label:  "/" + node.Name,
			Detail: node.Help,
			Usage:  node.Usage(),
		})
	}

	return suggestions
}

// completionClassification holds the result of classifying the tokens
// preceding the cursor into flags and positionals.
type completionClassification struct {
	node               *Node
	positionalIndex    int
	expectingFlagValue *Flag
	usedFlags          map[string]bool
	invalid            bool
}

// argTokensFrom returns the token texts between startIndex and
// currentIndex. This generalises argTokens for subcommand parsing
// where arguments begin after the subcommand token rather than
// after index 0.
func argTokensFrom(tokens []token, startIndex, currentIndex int) []string {
	end := min(currentIndex, len(tokens))

	if end <= startIndex {
		return nil
	}

	out := make([]string, 0, end-startIndex)
	for _, tok := range tokens[startIndex:end] {
		out = append(out, tok.Text)
	}

	return out
}

// classifyForCompletion walks the preceding argument tokens and
// determines the current positional index, whether we're expecting a
// flag value, and which flags have already been used.
func classifyForCompletion(node *Node, preceding []string) completionClassification {
	cc := completionClassification{
		node:      node,
		usedFlags: map[string]bool{},
	}

	for i := 0; i < len(preceding); i++ {
		tok := preceding[i]

		if binding, ok := findFlagBinding(cc.node, tok); ok {
			cc.usedFlags[tok] = true

			if binding.Flag.Variadic {
				// Variadic flag consumes remaining tokens.
				if i+1 >= len(preceding) {
					cc.expectingFlagValue = binding.Flag
				}
				return cc
			}

			// Scalar flag: next token is its value.
			if i+1 < len(preceding) {
				i++
			} else {
				// Current token is the flag value.
				cc.expectingFlagValue = binding.Flag
				return cc
			}

			continue
		}

		pos := resolvePositional(cc.node.Positionals, cc.positionalIndex)
		if pos != nil {
			if !pos.Variadic {
				cc.positionalIndex++
				continue
			}

			return cc
		}

		child := cc.node.Find(tok)
		if child != nil {
			cc.node = child
			cc.positionalIndex = 0
			continue
		}

		if len(cc.node.Children) > 0 {
			cc.invalid = true
			return cc
		}

		// Not a recognised flag: it's a positional.
		cc.positionalIndex++
	}

	return cc
}

func flagSuggestions(node *Node, used map[string]bool) []Suggestion {
	var suggestions []Suggestion

	for _, binding := range allFlagBindings(node) {
		if used[binding.Flag.Name] {
			continue
		}

		suggestions = append(suggestions, Suggestion{
			Value:  binding.Flag.Name,
			Label:  binding.Flag.Name,
			Detail: binding.Flag.Help,
		})
	}

	return suggestions
}

func groupSuggestions(node *Node, used map[string]bool) []Suggestion {
	suggestions := childSuggestions(node)
	suggestions = append(suggestions, flagSuggestions(node, used)...)
	return suggestions
}

func childSuggestions(node *Node) []Suggestion {
	suggestions := make([]Suggestion, 0, len(node.Children))

	for _, child := range node.Children {
		suggestions = append(suggestions, Suggestion{
			Value:  child.Name,
			Label:  child.Name,
			Detail: child.Help,
		})
	}

	return suggestions
}

func scanTokens(runes []rune) []token {
	var tokens []token

	for i := 1; i < len(runes); {
		for i < len(runes) && runes[i] == ' ' {
			i++
		}

		if i >= len(runes) {
			break
		}

		start := i
		for i < len(runes) && runes[i] != ' ' {
			i++
		}

		tokens = append(tokens, token{
			Text:  string(runes[start:i]),
			Start: start,
			End:   i,
		})
	}

	return tokens
}

func currentToken(tokens []token, runes []rune, cursor int) (int, int, int) {
	if len(tokens) == 0 {
		return 0, 1, cursor
	}

	if cursor <= 1 {
		return 0, 1, tokens[0].End
	}

	if cursor > 0 && runes[cursor-1] == ' ' {
		count := 0
		for _, tok := range tokens {
			if tok.End <= cursor {
				count++
			}
		}

		return count, cursor, cursor
	}

	for i, tok := range tokens {
		if cursor < tok.Start {
			return i, tok.Start, tok.Start
		}

		if cursor <= tok.End {
			return i, tok.Start, tok.End
		}
	}

	return len(tokens), cursor, cursor
}

func resolvePositional(positionals []Positional, index int) *Positional {
	if index < 0 {
		return nil
	}

	if index < len(positionals) {
		return &positionals[index]
	}

	if len(positionals) == 0 {
		return nil
	}

	last := positionals[len(positionals)-1]
	if !last.Variadic {
		return nil
	}

	return &last
}

func hasContinuation(node *Node, index int) bool {
	if index < 0 {
		return len(node.Positionals) > 0 || len(node.Children) > 0 || len(node.AllFlags()) > 0
	}

	for i := index + 1; i < len(node.Positionals); i++ {
		if node.Positionals[i].Variadic {
			return true
		}

		if node.Positionals[i].Source != nil || !node.Positionals[i].Optional {
			return true
		}
	}

	return len(node.Children) > 0 || len(node.AllFlags()) > 0
}

func filterSuggestions(all []Suggestion, prefix string) []Suggestion {
	if prefix == "" {
		return dedupeSuggestions(all)
	}

	lower := strings.ToLower(prefix)
	exact := []Suggestion{}
	contains := []Suggestion{}
	seen := map[string]struct{}{}

	for _, suggestion := range all {
		key := suggestion.Value
		if _, ok := seen[key]; ok {
			continue
		}

		label := strings.ToLower(strings.TrimPrefix(suggestion.Label, "/"))
		value := strings.ToLower(strings.TrimPrefix(suggestion.Value, "/"))
		if strings.HasPrefix(value, lower) || strings.HasPrefix(label, lower) {
			seen[key] = struct{}{}
			exact = append(exact, suggestion)
			continue
		}

		if strings.Contains(value, lower) || strings.Contains(label, lower) {
			seen[key] = struct{}{}
			contains = append(contains, suggestion)
		}
	}

	return append(exact, contains...)
}

func dedupeSuggestions(all []Suggestion) []Suggestion {
	seen := map[string]struct{}{}
	filtered := make([]Suggestion, 0, len(all))

	for _, suggestion := range all {
		if _, ok := seen[suggestion.Value]; ok {
			continue
		}

		seen[suggestion.Value] = struct{}{}
		filtered = append(filtered, suggestion)
	}

	return filtered
}

// LiteralSource suggests a fixed set of values in the declared order.
func LiteralSource(values ...Suggestion) SuggestionSource {
	literals := append([]Suggestion(nil), values...)

	return func(_ InvocationState) []Suggestion {
		return slices.Clone(literals)
	}
}

// ComposeSources concatenates multiple sources in declaration order.
func ComposeSources(sources ...SuggestionSource) SuggestionSource {
	return func(state InvocationState) []Suggestion {
		var suggestions []Suggestion
		for _, source := range sources {
			if source == nil {
				continue
			}

			suggestions = append(suggestions, source(state)...)
		}

		return suggestions
	}
}

func allFlagBindings(node *Node) []flagBinding {
	if node == nil {
		return nil
	}

	var bindings []flagBinding

	if node.Parent != nil {
		bindings = append(bindings, allFlagBindings(node.Parent)...)
	}

	for i := range node.Flags {
		bindings = append(bindings, flagBinding{
			Owner: node,
			Flag:  &node.Flags[i],
		})
	}

	return dedupeFlagBindings(bindings)
}

func dedupeFlagBindings(bindings []flagBinding) []flagBinding {
	if len(bindings) == 0 {
		return nil
	}

	latest := map[string]int{}
	for i, binding := range bindings {
		latest[binding.Flag.Name] = i
	}

	deduped := make([]flagBinding, 0, len(latest))
	for i, binding := range bindings {
		if latest[binding.Flag.Name] != i {
			continue
		}

		deduped = append(deduped, binding)
	}

	return deduped
}

func findFlagBinding(node *Node, name string) (flagBinding, bool) {
	for _, binding := range allFlagBindings(node) {
		if binding.Flag.Name == name {
			return binding, true
		}
	}

	return flagBinding{}, false
}
