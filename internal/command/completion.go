package command

import (
	"slices"
	"strings"

	"github.com/laney/modeloff/internal/domain"
)

// InvocationState describes the current parse state for completion.
type InvocationState[C KindProvider] struct {
	Raw          string
	Name         string
	Args         []string
	Command      *Node[C]
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

	// TypedPrefix is the literal text the user has typed in the
	// replacement region. The popover's Tab-accept consults it to
	// decide whether the user is committing an alias they typed
	// deliberately (in which case the replacement preserves the
	// typed text) or expanding a partial form to its canonical
	// `Value`.
	TypedPrefix string
}

type token struct {
	Text  string
	Start int
	End   int
}

// complete resolves the completion state for the current buffer.
// ctx is forwarded to SuggestionSources.
func complete[C KindProvider](set Set[C], ctx C, raw string, cursor int, kind domain.ChannelKind, caps CapabilityHolder) Completion {
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
			Suggestions:  filterSuggestions(commandSuggestions(set, kind, caps), prefix),
			ReplaceStart: start,
			ReplaceEnd:   end,
			AppendSpace:  true,
			TypedPrefix:  prefix,
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
			TypedPrefix:  prefix,
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
			TypedPrefix:  prefix,
		}
	}

	state := InvocationState[C]{
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
		TypedPrefix:  prefix,
	}

	// Flag value completion: previous token was a flag name.
	if cctx.expectingFlagValue != nil {
		flag := cctx.expectingFlagValue

		if flag.Source != nil {
			result := flag.Source(ctx, state)
			if result.State == SuggestionStateError {
				return Completion{}
			}

			completion.Suggestions = filterSuggestions(result.Suggestions, prefix)
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
		result := pos.Source(ctx, state)
		if result.State == SuggestionStateError {
			return Completion{}
		}

		completion.Suggestions = filterSuggestions(result.Suggestions, prefix)
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

func commandSuggestions[C KindProvider](set Set[C], kind domain.ChannelKind, caps CapabilityHolder) []Suggestion {
	suggestions := make([]Suggestion, 0, len(set.Commands))

	for _, node := range set.Commands {
		if node.ToolOnly {
			continue
		}

		if node.RequiredKind != nil && *node.RequiredKind != kind {
			continue
		}

		if !Holds(caps, node.RequiredCapabilities) {
			continue
		}

		suggestions = append(suggestions, Suggestion{
			Value:   node.Name,
			Label:   node.DisplayName(),
			Detail:  node.Help,
			Usage:   node.FullUsage(),
			Aliases: slices.Clone(node.Aliases),
		})
	}

	return suggestions
}

// VisibleCommands returns the subset of set.Commands that the holder
// permits, after applying the same [Capability] filter as the
// completion path. The result is a freshly allocated slice in the
// original Commands order; the underlying [*Node] pointers are
// shared. Callers (`/help` rendering, tool-registry enumeration)
// use this when they need the filtered node list outside the
// completion flow.
//
// A nil holder is treated as holding no capabilities — commands
// with non-empty [Node.RequiredCapabilities] are filtered out.
func VisibleCommands[C KindProvider](set Set[C], caps CapabilityHolder) []*Node[C] {
	visible := make([]*Node[C], 0, len(set.Commands))

	for _, node := range set.Commands {
		if node.ToolOnly {
			continue
		}

		if !Holds(caps, node.RequiredCapabilities) {
			continue
		}

		visible = append(visible, node)
	}

	return visible
}

// completionClassification holds the result of classifying the tokens
// preceding the cursor into flags and positionals.
type completionClassification[C KindProvider] struct {
	node               *Node[C]
	positionalIndex    int
	expectingFlagValue *Flag[C]
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
func classifyForCompletion[C KindProvider](node *Node[C], preceding []string) completionClassification[C] {
	cc := completionClassification[C]{
		node:      node,
		usedFlags: map[string]bool{},
	}

	for i := 0; i < len(preceding); i++ {
		tok := preceding[i]

		if binding, ok := findFlagBinding(cc.node, tok); ok {
			cc.usedFlags[tok] = true

			if binding.Flag.Boolean {
				continue
			}

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

func flagSuggestions[C KindProvider](node *Node[C], used map[string]bool) []Suggestion {
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

func groupSuggestions[C KindProvider](node *Node[C], used map[string]bool) []Suggestion {
	suggestions := childSuggestions(node)
	suggestions = append(suggestions, flagSuggestions(node, used)...)
	return suggestions
}

func childSuggestions[C KindProvider](node *Node[C]) []Suggestion {
	suggestions := make([]Suggestion, 0, len(node.Children))

	for _, child := range node.Children {
		suggestions = append(suggestions, Suggestion{
			Value:   child.Name,
			Label:   childDisplayLabel(child),
			Detail:  child.Help,
			Usage:   childFullUsage(child),
			Aliases: slices.Clone(child.Aliases),
		})
	}

	return suggestions
}

// childDisplayLabel returns the local name of a child node with any
// aliases appended in parentheses, e.g. "set (s)". Children do not
// carry a leading slash because they are subcommands, not top-level
// commands.
func childDisplayLabel[C KindProvider](child *Node[C]) string {
	if len(child.Aliases) == 0 {
		return child.Name
	}

	return child.Name + " (" + strings.Join(child.Aliases, ", ") + ")"
}

// childFullUsage mirrors Node.FullUsage for subcommand nodes using the
// slashless child label, e.g. "set (s) <key> <value>". If the child
// takes no arguments the result degrades to just the label.
func childFullUsage[C KindProvider](child *Node[C]) string {
	label := childDisplayLabel(child)

	args := child.Usage()
	if args == "" {
		return label
	}

	return label + " " + args
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

func resolvePositional[C KindProvider](positionals []Positional[C], index int) *Positional[C] {
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

func hasContinuation[C KindProvider](node *Node[C], index int) bool {
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

		if strings.HasPrefix(value, lower) || strings.HasPrefix(label, lower) || aliasHasPrefix(suggestion.Aliases, lower) {
			seen[key] = struct{}{}
			exact = append(exact, suggestion)
			continue
		}

		if strings.Contains(value, lower) || strings.Contains(label, lower) || aliasContains(suggestion.Aliases, lower) {
			seen[key] = struct{}{}
			contains = append(contains, suggestion)
		}
	}

	return append(exact, contains...)
}

func aliasHasPrefix(aliases []string, lower string) bool {
	for _, alias := range aliases {
		if strings.HasPrefix(strings.ToLower(alias), lower) {
			return true
		}
	}

	return false
}

func aliasContains(aliases []string, lower string) bool {
	for _, alias := range aliases {
		if strings.Contains(strings.ToLower(alias), lower) {
			return true
		}
	}

	return false
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
// The context parameter is ignored since the values are static.
func LiteralSource[C KindProvider](values ...Suggestion) SuggestionSource[C] {
	literals := append([]Suggestion(nil), values...)

	return func(_ C, _ InvocationState[C]) SuggestionResult {
		return SuggestionResult{Suggestions: slices.Clone(literals)}
	}
}

// ComposeSources concatenates multiple sources in declaration order.
// The aggregate state is SuggestionStateError only when every
// contributing source reports SuggestionStateError; a healthy source
// masks error-state peers so their partial suggestions still reach the
// caller.
func ComposeSources[C KindProvider](sources ...SuggestionSource[C]) SuggestionSource[C] {
	return func(ctx C, state InvocationState[C]) SuggestionResult {
		var suggestions []Suggestion
		hadSource := false
		allError := true

		for _, source := range sources {
			if source == nil {
				continue
			}

			hadSource = true
			result := source(ctx, state)
			suggestions = append(suggestions, result.Suggestions...)

			if result.State != SuggestionStateError {
				allError = false
			}
		}

		if hadSource && allError {
			return SuggestionResult{State: SuggestionStateError}
		}

		return SuggestionResult{Suggestions: suggestions}
	}
}

// CompletionSet binds a command Set with a typed completion context.
// C must implement KindProvider so that command filtering works.
//
// Caps optionally restricts the visible command set: commands whose
// [Node.RequiredCapabilities] are not all held are filtered out of
// both the popover suggestion list and the parse-time name
// resolution. A nil holder is treated as holding nothing — set it
// explicitly via [NoCapabilities] for unfiltered display, or supply
// a real holder bridged to runtime state (the user-client's modes,
// the calling model-client's modes, etc.).
type CompletionSet[C KindProvider] struct {
	Set[C]

	Ctx  C
	Caps CapabilityHolder
}

// Complete resolves the completion state for the current buffer.
func (cs CompletionSet[C]) Complete(raw string, cursor int) Completion {
	return complete(cs.Set, cs.Ctx, raw, cursor, cs.Ctx.ChannelKind(), cs.Caps)
}

func allFlagBindings[C KindProvider](node *Node[C]) []flagBinding[C] {
	if node == nil {
		return nil
	}

	var bindings []flagBinding[C]

	if node.Parent != nil {
		bindings = append(bindings, allFlagBindings(node.Parent)...)
	}

	for i := range node.Flags {
		bindings = append(bindings, flagBinding[C]{
			Owner: node,
			Flag:  &node.Flags[i],
		})
	}

	return dedupeFlagBindings(bindings)
}

func dedupeFlagBindings[C KindProvider](bindings []flagBinding[C]) []flagBinding[C] {
	if len(bindings) == 0 {
		return nil
	}

	latest := map[string]int{}
	for i, binding := range bindings {
		latest[binding.Flag.Name] = i
	}

	deduped := make([]flagBinding[C], 0, len(latest))
	for i, binding := range bindings {
		if latest[binding.Flag.Name] != i {
			continue
		}

		deduped = append(deduped, binding)
	}

	return deduped
}

func findFlagBinding[C KindProvider](node *Node[C], name string) (flagBinding[C], bool) {
	for _, binding := range allFlagBindings(node) {
		if binding.Flag.Name == name {
			return binding, true
		}
	}

	return flagBinding[C]{}, false
}
