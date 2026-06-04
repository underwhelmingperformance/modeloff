package command

import (
	"encoding/json"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"sort"
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
// which the simple kind-switch above can't describe. The reflected
// schema is hardened for OpenAI strict-mode function calling via
// [makeStrictObjectSchema] before being returned.
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

	makeStrictObjectSchema(out)

	return out
}

// makeStrictObjectSchema walks a JSON-Schema fragment in place and
// enforces the invariants OpenAI strict-mode function calling
// requires of every object node: `additionalProperties: false`,
// `required` lists every property in `properties`, and properties
// that were not originally required gain a nullable type union so
// the model can omit them by emitting `null`. The schema's own `$id`
// and `$schema` metadata keys are stripped — they're invopop noise
// that some strict providers reject.
//
// The walk recurses into `properties`, `items`, and the `oneOf` /
// `anyOf` / `allOf` combinators. Original `required` ordering is
// preserved; newly-added entries are appended in alphabetical order
// so the output is deterministic.
func makeStrictObjectSchema(schema map[string]any) {
	delete(schema, "$id")
	delete(schema, "$schema")

	enforceStrictObject(schema)
	recurseStrictChildren(schema)
}

// enforceStrictObject applies the strict-mode invariants to schema
// when it is an object node carrying properties: `required` lists
// every property, a property that was not originally required gains a
// nullable type so the model can omit it by emitting `null`, and
// additional properties are forbidden. Original `required` ordering is
// preserved; newly-required names are appended alphabetically so the
// output is deterministic. A non-object node, or one without
// properties, is left untouched.
func enforceStrictObject(schema map[string]any) {
	if !isObjectSchema(schema) {
		return
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return
	}

	required, originallyRequired := collectRequired(schema, len(props))

	missing := make([]string, 0, len(props))
	for name := range props {
		if !originallyRequired[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	for _, name := range missing {
		required = append(required, name)
		if sub, ok := props[name].(map[string]any); ok {
			makeNullable(sub)
		}
	}

	schema["required"] = required
	schema["additionalProperties"] = false
}

// collectRequired reads the schema's existing `required` keyword,
// which the JSON-Schema source may encode as `[]string` or `[]any`,
// returning the names in their original order and a membership set.
// `capacity` sizes the result slice for the appends the caller will
// make.
func collectRequired(schema map[string]any, capacity int) ([]string, map[string]bool) {
	originallyRequired := make(map[string]bool)
	required := make([]string, 0, capacity)

	switch r := schema["required"].(type) {
	case []string:
		for _, k := range r {
			originallyRequired[k] = true
			required = append(required, k)
		}
	case []any:
		for _, v := range r {
			if s, ok := v.(string); ok {
				originallyRequired[s] = true
				required = append(required, s)
			}
		}
	}

	return required, originallyRequired
}

// recurseStrictChildren applies the strict walk to every nested schema
// reachable from schema: its `properties`, its `items`, and the
// `oneOf` / `anyOf` / `allOf` combinators.
func recurseStrictChildren(schema map[string]any) {
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, p := range props {
			if sub, ok := p.(map[string]any); ok {
				makeStrictObjectSchema(sub)
			}
		}
	}

	if items, ok := schema["items"].(map[string]any); ok {
		makeStrictObjectSchema(items)
	}

	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		arr, ok := schema[key].([]any)
		if !ok {
			continue
		}

		for _, v := range arr {
			if sub, ok := v.(map[string]any); ok {
				makeStrictObjectSchema(sub)
			}
		}
	}
}

// isObjectSchema reports whether schema has an `object` type. The
// `type` keyword may be a single string or a union array, so both
// shapes are checked.
func isObjectSchema(schema map[string]any) bool {
	t, ok := schema["type"]
	if !ok {
		return false
	}

	switch v := t.(type) {
	case string:
		return v == "object"
	case []any:
		for _, x := range v {
			if x == "object" {
				return true
			}
		}
	}

	return false
}

// makeNullable widens a schema's `type` to include `"null"`,
// turning a scalar type into a `[<type>, "null"]` union. Idempotent.
func makeNullable(schema map[string]any) {
	t, ok := schema["type"]
	if !ok {
		schema["type"] = []any{"null"}
		return
	}

	switch v := t.(type) {
	case string:
		if v == "null" {
			return
		}
		schema["type"] = []any{v, "null"}
	case []any:
		for _, x := range v {
			if x == "null" {
				return
			}
		}
		schema["type"] = append(v, "null")
	}
}
