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

// Invocation is the structured form of a raw slash command.
type Invocation struct {
	Raw    string
	Name   string
	Args   []string
	Parsed Command
}

// Suggestion is a single completion option.
type Suggestion struct {
	Value  string
	Label  string
	Detail string
}

// SuggestionSource returns suggestions for the current argument.
type SuggestionSource func(CompletionContext, InvocationState) []Suggestion

// ArgSpec describes a command argument in display order.
type ArgSpec struct {
	Name     string
	Help     string
	Optional bool
	FreeForm bool
	Source   SuggestionSource
}

// Spec is a first-class command definition owned by a UI model.
type Spec struct {
	Name    string
	Help    string
	Usage   string
	Args    []ArgSpec
	Handler func(Invocation) tea.Cmd
}

// Scope is the command set currently in scope for a model.
type Scope struct {
	Commands []Spec
}

// InvocationState describes the current parse state for completion.
type InvocationState struct {
	Raw          string
	Name         string
	Args         []string
	Command      *Spec
	CurrentIndex int
	CurrentToken string
}

// Completion describes the UI state for the command popover.
type Completion struct {
	Visible      bool
	Usage        string
	Help         string
	Suggestions  []Suggestion
	ReplaceStart int
	ReplaceEnd   int
	AppendSpace  bool
	SuppressList bool
}

type token struct {
	Text  string
	Start int
	End   int
}

// Merge combines scopes from most-local to least-local precedence.
func Merge(scopes ...Scope) Scope {
	merged := Scope{}
	seen := map[string]struct{}{}

	for _, scope := range scopes {
		for _, spec := range scope.Commands {
			if _, ok := seen[spec.Name]; ok {
				continue
			}

			seen[spec.Name] = struct{}{}
			merged.Commands = append(merged.Commands, spec)
		}
	}

	return merged
}

// Execute resolves and executes a command handler from the given scope.
func Execute(scope Scope, raw string) (tea.Cmd, error) {
	invocation, spec, err := resolveInvocation(scope, raw)
	if err != nil {
		return nil, err
	}

	if spec.Handler == nil {
		return nil, fmt.Errorf("command /%s has no handler", spec.Name)
	}

	return spec.Handler(invocation), nil
}

// Complete resolves the completion state for the current buffer.
func Complete(scope Scope, raw string, cursor int, ctx CompletionContext) Completion {
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
		suggestions := filterSuggestions(commandSuggestions(scope), prefix)
		completion := Completion{
			Visible:      true,
			Suggestions:  suggestions,
			ReplaceStart: start,
			ReplaceEnd:   end,
			AppendSpace:  true,
		}

		if spec := exactSpec(scope, prefix); spec != nil {
			completion.Usage = spec.Usage
			completion.Help = spec.Help
			completion.SuppressList = true
			return completion
		}

		if len(suggestions) > 0 {
			if spec := exactSpec(scope, strings.TrimPrefix(suggestions[0].Value, "/")); spec != nil {
				completion.Usage = spec.Usage
				completion.Help = spec.Help
			}
		}

		if completion.Usage == "" {
			completion.Usage = "/<command>"
			completion.Help = "Type a slash command."
		}

		return completion
	}

	name := ""
	if len(tokens) > 0 {
		name = tokens[0].Text
	}

	spec := exactSpec(scope, name)
	if spec == nil {
		return Completion{
			Visible:      true,
			Usage:        "/<command>",
			Help:         "Unknown command.",
			ReplaceStart: start,
			ReplaceEnd:   end,
		}
	}

	args := make([]string, 0, len(tokens)-1)
	for _, tok := range tokens[1:] {
		args = append(args, tok.Text)
	}

	state := InvocationState{
		Raw:          raw,
		Name:         name,
		Args:         args,
		Command:      spec,
		CurrentIndex: index - 1,
		CurrentToken: prefix,
	}

	completion := Completion{
		Visible:      true,
		Usage:        spec.Usage,
		Help:         spec.Help,
		ReplaceStart: start,
		ReplaceEnd:   end,
		AppendSpace:  hasContinuation(spec.Args, state.CurrentIndex),
	}

	arg := resolveArg(spec.Args, state.CurrentIndex)
	if arg == nil {
		completion.SuppressList = true
		return completion
	}

	if arg.FreeForm {
		completion.SuppressList = true
		if arg.Help != "" {
			completion.Help = arg.Help
		}
		return completion
	}

	if arg.Help != "" {
		completion.Help = arg.Help
	}

	if arg.Source == nil {
		completion.SuppressList = true
		return completion
	}

	completion.Suggestions = filterSuggestions(arg.Source(ctx, state), prefix)
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

func resolveInvocation(scope Scope, raw string) (Invocation, Spec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '/' {
		return Invocation{}, Spec{}, fmt.Errorf("not a command: %q", raw)
	}

	fields := strings.Fields(raw[1:])
	if len(fields) == 0 {
		return Invocation{}, Spec{}, fmt.Errorf("unknown command: /")
	}

	name := fields[0]
	spec := exactSpec(scope, name)
	if spec == nil {
		return Invocation{}, Spec{}, fmt.Errorf("unknown command: /%s", name)
	}

	parsed, err := Parse(raw)
	if err != nil {
		return Invocation{}, Spec{}, err
	}

	return Invocation{
		Raw:    raw,
		Name:   name,
		Args:   fields[1:],
		Parsed: parsed,
	}, *spec, nil
}

func exactSpec(scope Scope, name string) *Spec {
	for _, spec := range scope.Commands {
		if spec.Name == name {
			specCopy := spec
			return &specCopy
		}
	}

	return nil
}

func commandSuggestions(scope Scope) []Suggestion {
	suggestions := make([]Suggestion, 0, len(scope.Commands))

	for _, spec := range scope.Commands {
		suggestions = append(suggestions, Suggestion{
			Value:  spec.Name,
			Label:  "/" + spec.Name,
			Detail: spec.Help,
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

func resolveArg(args []ArgSpec, index int) *ArgSpec {
	if index < 0 {
		return nil
	}

	if index < len(args) {
		return &args[index]
	}

	if len(args) == 0 {
		return nil
	}

	last := args[len(args)-1]
	if !last.FreeForm {
		return nil
	}

	return &last
}

func hasContinuation(args []ArgSpec, index int) bool {
	if index < 0 {
		return len(args) > 0
	}

	for i := index + 1; i < len(args); i++ {
		if args[i].FreeForm {
			return true
		}

		if args[i].Source != nil || !args[i].Optional {
			return true
		}
	}

	return false
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
