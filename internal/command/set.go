package command

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/laney/modeloff/internal/domain"
)

// ModelOption describes a live model suggestion for completion.
type ModelOption struct {
	ID          domain.ModelID
	Name        string
	Description string
}

// CompletionContext provides runtime data for command completion.
type CompletionContext struct {
	Channels      []domain.Channel
	Instances     []domain.ModelInstance
	ActiveChannel domain.ChannelName
	ActiveMembers []domain.Nick
	UserNick      domain.Nick
	LiveModels    []ModelOption
}

// Invocation is the result of parsing a raw slash command. It
// carries the selected node and the populated command struct. Parse
// validates eagerly, so if you have an Invocation it is guaranteed
// to be well-formed and runnable.
type Invocation struct {
	Raw    string
	Name   string
	Args   []string
	node   *Node
	parsed any // concrete command struct, read by the handler closure
}

// Run executes the handler on the selected command node.
func (inv *Invocation) Run() tea.Cmd {
	if inv.node.Handler == nil {
		return nil
	}

	return inv.node.Handler(inv)
}

// Parsed extracts the typed command struct from an Invocation.
func Parsed[T any](inv *Invocation) T {
	return inv.parsed.(T)
}

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
type SuggestionSource func(CompletionContext, InvocationState) []Suggestion

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
	Name        string
	Help        string
	Positionals []Positional
	Flags       []Flag
	Children    []*Node
	Handler     func(*Invocation) tea.Cmd

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
	b.WriteString(n.Name)

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

	for _, f := range n.Flags {
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

// Leaf returns true if this node has no children.
func (n *Node) Leaf() bool {
	return len(n.Children) == 0
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

// Bind attaches a typed handler to the named command node. The
// handler receives the parsed command struct directly.
func Bind[T any](set Set, name string, handler func(T) tea.Cmd) {
	node := set.Find(name)
	if node == nil {
		panic(fmt.Sprintf("command %q not found in set", name))
	}

	node.Handler = func(inv *Invocation) tea.Cmd {
		return handler(inv.parsed.(T))
	}
}

// SetSource attaches a suggestion source to the named positional
// argument on this node.
func (n *Node) SetSource(positionalName string, source SuggestionSource) {
	for i := range n.Positionals {
		if n.Positionals[i].Name == positionalName {
			n.Positionals[i].Source = source
			return
		}
	}
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

// Execute parses the raw input and runs the matched command's
// handler. It is a convenience for Parse followed by Run.
func Execute(set Set, raw string) (tea.Cmd, error) {
	inv, err := set.Parse(raw)
	if err != nil {
		return nil, err
	}

	if inv.node.Handler == nil {
		return nil, fmt.Errorf("command /%s has no handler", inv.Name)
	}

	return inv.Run(), nil
}

// Complete resolves the completion state for the current buffer.
func Complete(set Set, raw string, cursor int, ctx CompletionContext) Completion {
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

	// Classify preceding tokens (everything before the current token)
	// into flags and positionals so we know the true positional index
	// and whether we're completing a flag value.
	preceding := argTokens(tokens, index)
	cctx := classifyForCompletion(node, preceding)

	state := InvocationState{
		Raw:          raw,
		Name:         name,
		Args:         args,
		Command:      node,
		CurrentIndex: cctx.positionalIndex,
		CurrentToken: prefix,
	}

	completion := Completion{
		Visible:      true,
		ReplaceStart: start,
		ReplaceEnd:   end,
		AppendSpace:  true,
	}

	// Subcommand completion.
	if len(node.Children) > 0 {
		completion.Suggestions = filterSuggestions(childSuggestions(node), prefix)
		return completion
	}

	// Flag value completion: previous token was a flag name.
	if cctx.expectingFlagValue != nil {
		flag := cctx.expectingFlagValue

		if flag.Source != nil {
			completion.Suggestions = filterSuggestions(flag.Source(ctx, state), prefix)
		}

		return completion
	}

	// Flag name completion: current token starts with "--".
	if strings.HasPrefix(prefix, "--") {
		completion.Suggestions = filterSuggestions(flagSuggestions(node, cctx.usedFlags), prefix)
		return completion
	}

	// Positional completion.
	pos := resolvePositional(node.Positionals, cctx.positionalIndex)
	if pos != nil && !pos.Variadic && pos.Source != nil {
		completion.Suggestions = filterSuggestions(pos.Source(ctx, state), prefix)
		completion.AppendSpace = hasContinuation(node, cctx.positionalIndex)
		return completion
	}

	if pos != nil {
		completion.AppendSpace = hasContinuation(node, cctx.positionalIndex)
		return completion
	}

	// Past all positionals: offer flag names.
	flags := flagSuggestions(node, cctx.usedFlags)
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
	positionalIndex    int
	expectingFlagValue *Flag
	usedFlags          map[string]bool
}

// argTokens returns the token texts between the command name (index 0)
// and the current token at index.
func argTokens(tokens []token, currentIndex int) []string {
	// tokens[0] is the command name; arguments start at tokens[1].
	end := currentIndex
	if end > len(tokens) {
		end = len(tokens)
	}

	if end <= 1 {
		return nil
	}

	out := make([]string, 0, end-1)
	for _, tok := range tokens[1:end] {
		out = append(out, tok.Text)
	}

	return out
}

// classifyForCompletion walks the preceding argument tokens and
// determines the current positional index, whether we're expecting a
// flag value, and which flags have already been used.
func classifyForCompletion(node *Node, preceding []string) completionClassification {
	flagSet := map[string]*Flag{}
	for i := range node.Flags {
		flagSet[node.Flags[i].Name] = &node.Flags[i]
	}

	cc := completionClassification{
		usedFlags: map[string]bool{},
	}

	for i := 0; i < len(preceding); i++ {
		tok := preceding[i]

		if f, ok := flagSet[tok]; ok {
			cc.usedFlags[tok] = true

			if f.Variadic {
				// Variadic flag consumes remaining tokens.
				cc.expectingFlagValue = nil
				return cc
			}

			// Scalar flag: next token is its value.
			if i+1 < len(preceding) {
				i++
			} else {
				// Current token is the flag value.
				cc.expectingFlagValue = f
				return cc
			}

			continue
		}

		// Not a recognised flag: it's a positional.
		cc.positionalIndex++
	}

	return cc
}

func flagSuggestions(node *Node, used map[string]bool) []Suggestion {
	var suggestions []Suggestion

	for _, f := range node.Flags {
		if used[f.Name] {
			continue
		}

		suggestions = append(suggestions, Suggestion{
			Value:  f.Name,
			Label:  f.Name,
			Detail: f.Help,
		})
	}

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
		return len(node.Positionals) > 0 || len(node.Flags) > 0
	}

	for i := index + 1; i < len(node.Positionals); i++ {
		if node.Positionals[i].Variadic {
			return true
		}

		if node.Positionals[i].Source != nil || !node.Positionals[i].Optional {
			return true
		}
	}

	return len(node.Flags) > 0
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

	return func(_ CompletionContext, _ InvocationState) []Suggestion {
		return slices.Clone(literals)
	}
}

// ChannelsSource suggests known channels.
func ChannelsSource() SuggestionSource {
	return func(ctx CompletionContext, _ InvocationState) []Suggestion {
		suggestions := make([]Suggestion, 0, len(ctx.Channels))
		for _, ch := range ctx.Channels {
			suggestions = append(suggestions, Suggestion{
				Value:  string(ch.Name),
				Label:  string(ch.Name),
				Detail: channelDetail(ch),
			})
		}

		return suggestions
	}
}

// ActiveMembersSource suggests members of the active channel.
func ActiveMembersSource() SuggestionSource {
	return func(ctx CompletionContext, _ InvocationState) []Suggestion {
		suggestions := make([]Suggestion, 0, len(ctx.ActiveMembers))
		for _, nick := range ctx.ActiveMembers {
			if nick == ctx.UserNick {
				continue
			}

			suggestions = append(suggestions, Suggestion{
				Value: string(nick),
				Label: string(nick),
			})
		}

		return suggestions
	}
}

// InstancesSource suggests known instance nicks.
func InstancesSource() SuggestionSource {
	return func(ctx CompletionContext, _ InvocationState) []Suggestion {
		suggestions := make([]Suggestion, 0, len(ctx.Instances))
		for _, inst := range ctx.Instances {
			suggestions = append(suggestions, Suggestion{
				Value:  string(inst.Nick),
				Label:  string(inst.Nick),
				Detail: string(inst.ModelID),
			})
		}

		return suggestions
	}
}

// ReusableInstancesSource suggests instance nicks not already in the active channel.
func ReusableInstancesSource() SuggestionSource {
	return func(ctx CompletionContext, _ InvocationState) []Suggestion {
		suggestions := make([]Suggestion, 0, len(ctx.Instances))
		for _, inst := range ctx.Instances {
			if inst.Channels.Has(ctx.ActiveChannel) {
				continue
			}

			suggestions = append(suggestions, Suggestion{
				Value:  string(inst.Nick),
				Label:  string(inst.Nick),
				Detail: string(inst.ModelID),
			})
		}

		return suggestions
	}
}

// LiveModelsSource suggests live model identifiers.
func LiveModelsSource() SuggestionSource {
	return func(ctx CompletionContext, _ InvocationState) []Suggestion {
		suggestions := make([]Suggestion, 0, len(ctx.LiveModels))
		for _, model := range ctx.LiveModels {
			detail := model.Name
			if detail == "" {
				detail = model.Description
			}

			suggestions = append(suggestions, Suggestion{
				Value:  string(model.ID),
				Label:  string(model.ID),
				Detail: detail,
			})
		}

		return suggestions
	}
}

// ComposeSources concatenates multiple sources in declaration order.
func ComposeSources(sources ...SuggestionSource) SuggestionSource {
	return func(ctx CompletionContext, state InvocationState) []Suggestion {
		var suggestions []Suggestion
		for _, source := range sources {
			if source == nil {
				continue
			}

			suggestions = append(suggestions, source(ctx, state)...)
		}

		return suggestions
	}
}

func channelDetail(ch domain.Channel) string {
	if ch.Topic != "" {
		return ch.Topic
	}

	if ch.Kind == domain.KindDM {
		return "direct message"
	}

	return "channel"
}
