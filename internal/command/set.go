package command

import (
	"encoding/json"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"

	"github.com/laney/modeloff/internal/domain"
)

// Suggestion is a single completion option. Every suggestion carries
// its own Usage text so the popover can display it when the
// suggestion is selected, without promoting any entry to a special
// header. Aliases, when present, are alternate names that should
// match the suggestion during filtering — they are not displayed
// as separate entries.
type Suggestion struct {
	Value   string
	Label   string
	Detail  string
	Usage   string
	Aliases []string
}

// SuggestionState describes whether a suggestion source completed
// normally or is in an explicit error state.
type SuggestionState uint8

const (
	// SuggestionStateReady is the default state: the source has
	// produced its suggestion list (which may legitimately be empty)
	// and the popover should render the result as-is.
	SuggestionStateReady SuggestionState = iota

	// SuggestionStateError signals that the source could not produce
	// suggestions for an underlying reason — typically an upstream
	// API failure such as `loadLiveModels` not yet recovering. The
	// popover suppresses itself in this state so the user does not
	// see an empty error-state shell.
	SuggestionStateError
)

// SuggestionResult is the completion-layer result from a source.
// An error state is distinct from a healthy empty suggestion list.
type SuggestionResult struct {
	Suggestions []Suggestion
	State       SuggestionState
}

// KindProvider is implemented by completion contexts that know the
// current channel kind. [CompletionSet] uses this to filter commands.
type KindProvider interface {
	ChannelKind() domain.ChannelKind
}

// SuggestionSource returns suggestions for the current argument. The
// first parameter is the caller-defined completion context the
// grammar is parameterised on; sources receive it typed.
type SuggestionSource[C KindProvider] func(ctx C, state InvocationState[C]) SuggestionResult

// Positional describes a positional command argument.
type Positional[C KindProvider] struct {
	Name     string
	Help     string
	Optional bool
	Variadic bool
	Nargs    *int
	Source   SuggestionSource[C]
}

// Flag describes a named flag argument (e.g. --persona).
type Flag[C KindProvider] struct {
	Name     string
	Help     string
	Boolean  bool
	Optional bool
	Variadic bool
	Source   SuggestionSource[C]
}

// ToolDescriber is implemented by commands that need rich,
// multi-line tool descriptions beyond what fits in a struct tag.
type ToolDescriber interface {
	ToolDescription() string
}

// parseRequiredKind maps a `kind` struct tag value to a channel kind
// restriction. A nil return means the command is available in all
// channel kinds.
func parseRequiredKind(tag string) *domain.ChannelKind {
	switch tag {
	case "channel":
		k := domain.KindChannel
		return &k
	default:
		return nil
	}
}

// parseRequiredCapabilities splits a `caps:"a,b,c"` struct tag value
// into the corresponding [Capability] slice. Whitespace around each
// entry is trimmed; empty entries are skipped. An empty or missing
// tag yields a nil slice (the command has no capability requirements
// and is universally visible).
func parseRequiredCapabilities(tag string) []Capability {
	if tag == "" {
		return nil
	}

	parts := strings.Split(tag, ",")
	caps := make([]Capability, 0, len(parts))

	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}

		caps = append(caps, Capability(trimmed))
	}

	if len(caps) == 0 {
		return nil
	}

	return caps
}

// Node is a command in the command tree. Leaf nodes (no children)
// are executable commands. Non-leaf nodes are command groups whose
// children are subcommands.
type Node[C KindProvider] struct {
	Parent               *Node[C]
	Name                 string
	Aliases              []string
	Help                 string
	RequiredKind         *domain.ChannelKind
	RequiredCapabilities []Capability
	Tool                 bool
	ToolDesc             string

	// ToolOnly hides the node from the slash-command parser and
	// completion path; only the model-tool path picks it up. The
	// zero value is slash-callable.
	ToolOnly    bool
	Positionals []Positional[C]
	Flags       []Flag[C]
	Children    []*Node[C]

	// factory creates a zero-valued pointer to the command struct for
	// parsing. Nil for group nodes that have no struct of their own.
	factory func() any
	fields  []fieldMeta
}

// Names yields the canonical name followed by any aliases.
func (n *Node[C]) Names() iter.Seq[string] {
	return func(yield func(string) bool) {
		if !yield(n.Name) {
			return
		}

		for _, alias := range n.Aliases {
			if !yield(alias) {
				return
			}
		}
	}
}

// DisplayName returns the slash-prefixed command name with any
// aliases in parentheses, e.g. "/join (/j, /jo)". For subcommand
// nodes this returns only the local name, not the full ancestor
// path.
func (n *Node[C]) DisplayName() string {
	var b strings.Builder

	b.WriteString("/")
	b.WriteString(n.Name)

	rest := false
	for _, alias := range n.Aliases {
		if !rest {
			b.WriteString(" (/")
			rest = true
		} else {
			b.WriteString(", /")
		}

		b.WriteString(alias)
	}

	if rest {
		b.WriteString(")")
	}

	return b.String()
}

// Usage returns the argument synopsis for this node, e.g.
// "<channel>", "[--persona <persona>]". It does not include the
// command name or aliases — use DisplayName for that.
func (n *Node[C]) Usage() string {
	var b strings.Builder

	for _, p := range n.Positionals {
		if b.Len() > 0 {
			b.WriteString(" ")
		}

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
		if b.Len() > 0 {
			b.WriteString(" ")
		}

		b.WriteString("[")
		b.WriteString(f.Name)

		if f.Variadic {
			b.WriteString(" <")
			b.WriteString(strings.TrimPrefix(f.Name, "--"))
			b.WriteString(">")
		}

		b.WriteString("]")
	}

	if len(n.Children) > 0 && len(n.Positionals) == 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}

		b.WriteString("<command>")
	}

	return b.String()
}

// FullUsage returns DisplayName and Usage joined together, e.g.
// "/join (/j, /jo) <channel>".
func (n *Node[C]) FullUsage() string {
	usage := n.Usage()
	if usage == "" {
		return n.DisplayName()
	}

	return n.DisplayName() + " " + usage
}

// Path returns the node's command path relative to the set root.
func (n *Node[C]) Path() string {
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
func (n *Node[C]) Leaf() bool {
	return len(n.Children) == 0
}

// ToolDescription returns the tool description using three tiers:
//  1. If value implements ToolDescriber, use its ToolDescription().
//  2. Else if the tool:"..." tag has a non-empty value, use that.
//  3. Else fall back to the help:"" tag text.
func (n *Node[C]) ToolDescription(value any) string {
	if d, ok := value.(ToolDescriber); ok {
		return d.ToolDescription()
	}

	if n.ToolDesc != "" {
		return n.ToolDesc
	}

	return n.Help
}

// NewZero returns a zero-valued pointer to the command struct
// for this node. This is useful for type assertions without
// needing parsed arguments.
func (n *Node[C]) NewZero() any {
	if n.factory == nil {
		return nil
	}

	return n.factory()
}

// ToolName returns the canonical model-tool name for this node.
func (n *Node[C]) ToolName() string {
	return toSnakeCase(n.Path())
}

// ToolParameters returns the JSON-schema-like parameter object for a
// tool-capable leaf node. Every property name appears in
// `required` regardless of whether the field is optional; optional
// fields carry a nullable type union (`["string", "null"]`,
// `["array", "null"]`, …) so providers that enforce strict
// function-call schemas (Azure OpenAI) accept the schema.
func (n *Node[C]) ToolParameters() map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(n.fields))

	for _, field := range n.fields {
		name := toSnakeCase(field.name)
		properties[name] = toolSchemaForField(field)
		required = append(required, name)
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}

	if len(required) > 0 {
		schema["required"] = required
	}

	return schema
}

// ToolValue decodes structured tool arguments into the leaf value.
func (n *Node[C]) ToolValue(rawArgs json.RawMessage) (any, error) {
	if !n.Leaf() {
		return nil, fmt.Errorf("node /%s is not a tool leaf", n.Path())
	}

	if n.factory == nil {
		return nil, fmt.Errorf("node /%s has no factory", n.Path())
	}

	if len(rawArgs) == 0 {
		rawArgs = []byte("{}")
	}

	var args map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("decode tool args for /%s: %w", n.Path(), err)
	}

	value := n.factory()
	target := reflect.ValueOf(value).Elem()

	for _, field := range n.fields {
		key := toSnakeCase(field.name)
		raw, ok := args[key]

		if !ok || string(raw) == "null" {
			if field.optional {
				continue
			}

			return nil, &MissingArgError{Name: key}
		}

		if err := json.Unmarshal(raw, target.Field(field.index).Addr().Interface()); err != nil {
			return nil, fmt.Errorf("decode tool field %q for /%s: %w", key, n.Path(), err)
		}
	}

	return target.Interface(), nil
}

// AllFlags returns flags visible at this node, starting with
// ancestors and ending with the node's own flags.
func (n *Node[C]) AllFlags() []Flag[C] {
	bindings := allFlagBindings(n)
	flags := make([]Flag[C], 0, len(bindings))

	for _, binding := range bindings {
		flags = append(flags, *binding.Flag)
	}

	return flags
}

// Find looks up a direct child node by name, falling back to
// aliases if no exact name match is found.
func (n *Node[C]) Find(name string) *Node[C] {
	for _, child := range n.Children {
		if child.Name == name {
			return child
		}
	}

	for _, child := range n.Children {
		if slices.Contains(child.Aliases, name) {
			return child
		}
	}

	return nil
}

// Set is the set of commands available in a given context. It acts
// as the root of the command tree.
type Set[C KindProvider] struct {
	Commands []*Node[C]
}

// Completable computes completions for a raw input string at the
// given cursor position. Implementations bind a Set together with
// a caller-defined context and channel kind.
type Completable interface {
	Complete(raw string, cursor int) Completion
}

type flagBinding[C KindProvider] struct {
	Owner *Node[C]
	Flag  *Flag[C]
}

// Completer is implemented by command structs that provide their own
// suggestion sources. The returned map keys are positional or flag
// names.
type Completer[C KindProvider] interface {
	Sources() map[string]SuggestionSource[C]
}

// Find looks up a top-level node by name, falling back to aliases
// if no exact name match is found. Tool-only nodes (registered with
// `tool:""` but no `cmd:""`) are skipped — they are not callable as
// slash commands.
func (s Set[C]) Find(name string) *Node[C] {
	for _, node := range s.Commands {
		if node.ToolOnly {
			continue
		}

		if node.Name == name {
			return node
		}
	}

	for _, node := range s.Commands {
		if node.ToolOnly {
			continue
		}

		if slices.Contains(node.Aliases, name) {
			return node
		}
	}

	return nil
}

// ToolNodes returns every tool-capable leaf node in the set.
func (s Set[C]) ToolNodes() []*Node[C] {
	var nodes []*Node[C]

	var walk func(node *Node[C])
	walk = func(node *Node[C]) {
		if node == nil {
			return
		}

		if node.Tool && node.Leaf() {
			nodes = append(nodes, node)
		}

		for _, child := range node.Children {
			walk(child)
		}
	}

	for _, node := range s.Commands {
		walk(node)
	}

	return nodes
}

func (s Set[C]) linkParents() {
	for _, node := range s.Commands {
		linkNode(node, nil)
	}
}

func linkNode[C KindProvider](node, parent *Node[C]) {
	if node == nil {
		return
	}

	node.Parent = parent

	for _, child := range node.Children {
		linkNode(child, node)
	}
}

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

// Merge combines command sets from most-local to least-local precedence.
// A command is skipped if its name or any of its aliases collide with a
// name or alias already claimed by a higher-priority set.
func Merge[C KindProvider](sets ...Set[C]) Set[C] {
	merged := Set[C]{}
	seen := map[string]struct{}{}

	for _, set := range sets {
		for _, node := range set.Commands {
			skip := false
			for name := range node.Names() {
				if _, ok := seen[name]; ok {
					skip = true
					break
				}
			}

			if skip {
				continue
			}

			for name := range node.Names() {
				seen[name] = struct{}{}
			}

			merged.Commands = append(merged.Commands, node)
		}
	}

	return merged
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

func toolSchemaForField(field fieldMeta) map[string]any {
	typ := field.typ
	schema := toolSchemaForType(typ)

	if field.optional {
		schema["type"] = []any{schema["type"], "null"}
	}

	if field.help != "" {
		schema["description"] = field.help
	}

	return schema
}

func toolSchemaForType(typ reflect.Type) map[string]any {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	switch typ.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}

	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}

	case reflect.Slice:
		return map[string]any{
			"type":  "array",
			"items": toolSchemaForType(typ.Elem()),
		}

	case reflect.Struct:
		return structSchemaViaReflector(typ)

	default:
		return map[string]any{"type": "string"}
	}
}

// structSchemaViaReflector reflects a Go struct into a JSON-Schema
// fragment via invopop/jsonschema. Used for tool-parameter fields
// whose type is a nested struct (e.g. `[]protocol.ReplySpan`),
// which the simple kind-switch above can't describe.
func structSchemaViaReflector(typ reflect.Type) map[string]any {
	reflector := jsonschema.Reflector{DoNotReference: true}
	schema := reflector.ReflectFromType(typ)

	data, err := json.Marshal(schema)
	if err != nil {
		return map[string]any{"type": "object"}
	}

	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]any{"type": "object"}
	}

	return out
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

		if !holds(caps, node.RequiredCapabilities) {
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

		if !holds(caps, node.RequiredCapabilities) {
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
