package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"math/big"
	"reflect"
	"sort"
	"strings"

	invjsonschema "github.com/invopop/jsonschema"
	validator "github.com/santhosh-tekuri/jsonschema/v6"

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
	Name        string
	Help        string
	Optional    bool
	Variadic    bool
	Passthrough PassthroughMode
	Source      SuggestionSource[C]
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

	// Secret marks a node whose arguments carry a credential, from
	// the grammar's `secret:""` tag. A raw command line that resolves
	// to a secret node must not be logged verbatim; see
	// Set.resolveSecret and Parser.RedactedRaw.
	Secret bool

	// ToolOnly hides the node from the slash-command parser and
	// completion path; only the model-tool path picks it up. The
	// zero value is slash-callable.
	ToolOnly    bool
	Positionals []Positional[C]
	Flags       []Flag[C]
	Children    []*Node[C]

	// factory creates a zero-valued pointer to the command struct for
	// parsing. Nil for group nodes that have no struct of their own.
	factory          func() any
	fields           []fieldMeta
	toolFields       []fieldMeta
	toolSchema       map[string]any
	toolValidator    *validator.Schema
	runtimeValidator *validator.Schema
	toolXORGroups    []toolXORGroup
}

type toolXORGroup struct {
	name    string
	members []fieldMeta
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
	if n.toolSchema != nil {
		return cloneToolSchema(n.toolSchema)
	}

	schema, _, err := buildToolParameters(n.toolFields)
	if err != nil {
		return map[string]any{}
	}

	provider, err := providerToolSchema(n.ToolName(), schema)
	if err != nil {
		return map[string]any{}
	}

	return provider
}

func buildToolParameters(fields []fieldMeta) (map[string]any, []toolXORGroup, error) {
	properties := map[string]any{}
	required := make([]string, 0, len(fields))
	groupIndexes := map[string]int{}
	fieldProperties := map[string]bool{}

	for _, field := range fields {
		name := toSnakeCase(field.name)
		fieldProperties[name] = true

		if len(field.xorGroups) > 1 {
			return nil, nil, &UnsupportedToolXORError{Field: field.name, Groups: field.xorGroups}
		}
		if len(field.xorGroups) == 1 {
			group := field.xorGroups[0]
			if _, ok := groupIndexes[group]; !ok {
				groupIndexes[group] = len(groupIndexes)
			}
			continue
		}

		fieldSchema, err := toolSchemaForField(field)
		if err != nil {
			return nil, nil, err
		}
		properties[name] = fieldSchema
		required = append(required, name)
	}

	groups := make([]toolXORGroup, len(groupIndexes))
	for name, index := range groupIndexes {
		if fieldProperties[name] {
			return nil, nil, &ToolSchemaPropertyCollisionError{Property: name}
		}
		groups[index].name = name
	}
	for _, field := range fields {
		if len(field.xorGroups) == 1 {
			index := groupIndexes[field.xorGroups[0]]
			groups[index].members = append(groups[index].members, field)
		}
	}

	for _, group := range groups {
		branches := make([]any, 0, len(group.members)+1)
		groupRequired := false

		for _, member := range group.members {
			branchField := member
			branchField.optional = false
			name := toSnakeCase(member.name)
			fieldSchema, err := toolSchemaForField(branchField)
			if err != nil {
				return nil, nil, err
			}
			branches = append(branches, map[string]any{
				"type": "object",
				"properties": map[string]any{
					name: fieldSchema,
				},
				"required":             []string{name},
				"additionalProperties": false,
			})
			if !member.optional {
				groupRequired = true
			}
		}

		if !groupRequired {
			branches = append(branches, map[string]any{"type": "null"})
		}

		properties[group.name] = map[string]any{"anyOf": branches}
		required = append(required, group.name)
	}

	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}

	if len(required) > 0 {
		schema["required"] = required
	}

	return schema, groups, nil
}

// ToolValue decodes structured tool arguments into the leaf value.
func (n *Node[C]) ToolValue(rawArgs json.RawMessage) (any, error) {
	if !n.Leaf() {
		return nil, &NotToolLeafError{Path: n.Path()}
	}

	if n.factory == nil {
		return nil, &NoFactoryError{Path: n.Path()}
	}

	if len(rawArgs) == 0 {
		rawArgs = []byte("{}")
	}
	if n.toolValidator == nil || n.runtimeValidator == nil {
		return nil, &ToolSchemaCompileError{Tool: n.ToolName(), Err: fmt.Errorf("tool schema is not compiled")}
	}

	instance, err := validator.UnmarshalJSON(bytes.NewReader(rawArgs))
	if err != nil {
		return nil, &ToolArgumentsDecodeError{Tool: n.ToolName(), Err: err}
	}
	if err := n.toolValidator.Validate(instance); err != nil {
		return nil, toolValidationError(n.ToolName(), err)
	}
	if err := n.runtimeValidator.Validate(instance); err != nil {
		return nil, toolValidationError(n.ToolName(), err)
	}

	var args map[string]json.RawMessage
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, &ToolArgumentsDecodeError{Tool: n.ToolName(), Err: err}
	}

	for _, group := range n.toolXORGroups {
		raw := args[group.name]
		delete(args, group.name)
		if string(raw) == "null" {
			continue
		}

		var wrapped map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wrapped); err != nil {
			return nil, &ToolArgumentsDecodeError{Tool: n.ToolName(), Field: group.name, Err: err}
		}
		maps.Copy(args, wrapped)
	}

	value := n.factory()
	target := reflect.ValueOf(value).Elem()
	present := map[int]bool{}

	for _, field := range n.toolFields {
		key := toSnakeCase(field.name)
		raw, ok := args[key]

		if !ok || string(raw) == "null" {
			if field.optional || len(field.xorGroups) > 0 {
				continue
			}

			return nil, &MissingArgError{Name: key}
		}

		if err := unmarshalToolField(raw, target.Field(field.index).Addr().Interface()); err != nil {
			return nil, &ToolArgumentsDecodeError{Tool: n.ToolName(), Field: key, Err: err}
		}
		present[field.index] = true
	}
	if err := validateFields(n.toolFields, present); err != nil {
		return nil, err
	}
	if err := validatePresentValues(target, n.toolFields, present); err != nil {
		return nil, err
	}
	if err := validateDecodedValue(reflect.ValueOf(value)); err != nil {
		return nil, &ValueValidationError{Name: n.ToolName(), Err: err}
	}

	return target.Interface(), nil
}

func unmarshalToolField(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}

	value = canonicaliseJSONIntegers(value, reflect.TypeOf(target))
	canonical, err := json.Marshal(value)
	if err != nil {
		return err
	}

	return json.Unmarshal(canonical, target)
}

func canonicaliseJSONIntegers(value any, typ reflect.Type) any {
	if typ == nil || implementsJSONUnmarshaler(typ) {
		return value
	}

	for typ.Kind() == reflect.Pointer {
		if value == nil {
			return value
		}

		typ = typ.Elem()
		if implementsJSONUnmarshaler(typ) {
			return value
		}
	}

	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if number, ok := canonicalJSONInteger(value); ok {
			return number
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok || typ.Key().Kind() != reflect.String {
			return value
		}

		for key, child := range object {
			object[key] = canonicaliseJSONIntegers(child, typ.Elem())
		}
	case reflect.Slice, reflect.Array:
		array, ok := value.([]any)
		if !ok {
			return value
		}

		for index, child := range array {
			array[index] = canonicaliseJSONIntegers(child, typ.Elem())
		}
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return value
		}

		fieldTypes := jsonFieldTypes(typ)
		for key, child := range object {
			fieldType, ok := fieldTypes[key]
			if !ok {
				continue
			}

			object[key] = canonicaliseJSONIntegers(child, fieldType)
		}
	}

	return value
}

func implementsJSONUnmarshaler(typ reflect.Type) bool {
	unmarshaler := reflect.TypeFor[json.Unmarshaler]()
	if typ.Implements(unmarshaler) {
		return true
	}

	return typ.Kind() != reflect.Pointer && reflect.PointerTo(typ).Implements(unmarshaler)
}

func jsonFieldTypes(typ reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	collectJSONFieldTypes(fields, typ)

	return fields
}

func collectJSONFieldTypes(fields map[string]reflect.Type, typ reflect.Type) {
	for field := range typ.Fields() {
		if !field.IsExported() {
			continue
		}

		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" && field.Anonymous {
			embedded := field.Type
			for embedded.Kind() == reflect.Pointer {
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				collectJSONFieldTypes(fields, embedded)
				continue
			}
		}
		if name == "" {
			name = field.Name
		}

		fields[name] = field.Type
	}
}

func canonicalJSONInteger(value any) (json.Number, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return "", false
	}

	rational, ok := new(big.Rat).SetString(number.String())
	if !ok || !rational.IsInt() {
		return "", false
	}

	return json.Number(rational.Num().String()), true
}

func compileToolSchema(name string, schema map[string]any) (*validator.Schema, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, &ToolSchemaCompileError{Tool: name, Err: err}
	}

	document, err := validator.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, &ToolSchemaCompileError{Tool: name, Err: err}
	}

	compiler := validator.NewCompiler()
	compiler.DefaultDraft(validator.Draft2020)
	resource := "urn:modeloff:tool:" + toSnakeCase(name)
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, &ToolSchemaCompileError{Tool: name, Err: err}
	}

	compiled, err := compiler.Compile(resource)
	if err != nil {
		return nil, &ToolSchemaCompileError{Tool: name, Err: err}
	}

	return compiled, nil
}

// providerToolSchema removes type-specific constraints rejected by
// strict endpoints such as Azure OpenAI. The runtime validator keeps
// the complete schema, and tool descriptions state the omitted bounds.
func providerToolSchema(tool string, runtime map[string]any) (map[string]any, error) {
	provider := cloneToolSchema(runtime)
	translateProviderSchema(provider)
	if keyword, location, ok := unsupportedProviderKeyword(provider, nil); ok {
		return nil, &UnsupportedProviderSchemaKeywordError{
			Tool:     tool,
			Keyword:  keyword,
			Location: location,
		}
	}
	removeUnsupportedStrictKeywords(provider)

	return provider, nil
}

func unsupportedProviderKeyword(schema map[string]any, location []string) (string, []string, bool) {
	keys := make([]string, 0, len(schema))
	for key := range schema {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if key == "additionalProperties" && schema[key] != false {
			return key, appendSchemaLocation(location, key), true
		}
		if isUnsupportedProviderStructuralKeyword(key) {
			return key, append(append([]string(nil), location...), key), true
		}

	}

	var foundKeyword string
	var foundLocation []string
	visitSchemaChildren(schema, location, func(child map[string]any, childLocation []string) bool {
		keyword, keywordLocation, ok := unsupportedProviderKeyword(child, childLocation)
		if !ok {
			return true
		}

		foundKeyword = keyword
		foundLocation = keywordLocation

		return false
	})
	if foundKeyword != "" {
		return foundKeyword, foundLocation, true
	}

	return "", nil, false
}

func visitSchemaChildren(
	schema map[string]any,
	location []string,
	visit func(map[string]any, []string) bool,
) {
	visitSchemaValues(schema, location, func(_ string, value any, childLocation []string) bool {
		child, ok := value.(map[string]any)
		if !ok {
			return true
		}

		return visit(child, childLocation)
	})
}

func visitSchemaValues(
	schema map[string]any,
	location []string,
	visit func(string, any, []string) bool,
) {
	for _, keyword := range []string{"properties", "$defs", "definitions", "dependentSchemas"} {
		children, ok := schema[keyword].(map[string]any)
		if !ok {
			continue
		}

		names := make([]string, 0, len(children))
		for name := range children {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			child := children[name]
			switch child.(type) {
			case bool, map[string]any:
			default:
				continue
			}
			childLocation := appendSchemaLocation(location, keyword, name)
			if !visit(keyword, child, childLocation) {
				return
			}
		}
	}

	for _, keyword := range []string{"items", "additionalProperties", "contains", "propertyNames", "not", "if", "then", "else"} {
		child := schema[keyword]
		switch child.(type) {
		case bool, map[string]any:
		default:
			continue
		}
		if !visit(keyword, child, appendSchemaLocation(location, keyword)) {
			return
		}
	}

	for _, keyword := range []string{"anyOf", "allOf", "oneOf", "prefixItems"} {
		children, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for index, item := range children {
			switch item.(type) {
			case bool, map[string]any:
			default:
				continue
			}
			childLocation := appendSchemaLocation(location, keyword, fmt.Sprintf("%d", index))
			if !visit(keyword, item, childLocation) {
				return
			}
		}
	}
}

func appendSchemaLocation(location []string, parts ...string) []string {
	next := append([]string(nil), location...)
	return append(next, parts...)
}

func isUnsupportedProviderStructuralKeyword(keyword string) bool {
	switch keyword {
	case "allOf", "dependentRequired", "dependentSchemas", "else", "if", "not", "prefixItems", "then":
		return true
	default:
		return false
	}
}

func translateProviderSchema(schema map[string]any) {
	if constant, ok := schema["const"]; ok {
		schema["enum"] = []any{constant}
		delete(schema, "const")
	}

	if oneOf, ok := schema["oneOf"].([]any); ok {
		anyOf, _ := schema["anyOf"].([]any)
		schema["anyOf"] = append(anyOf, oneOf...)
		delete(schema, "oneOf")
	}

	visitSchemaChildren(schema, nil, func(child map[string]any, _ []string) bool {
		translateProviderSchema(child)
		return true
	})
}

func cloneToolSchema(schema map[string]any) map[string]any {
	cloned := make(map[string]any, len(schema))
	for key, value := range schema {
		cloned[key] = cloneToolSchemaValue(value)
	}

	return cloned
}

func cloneToolSchemaValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneToolSchema(value)
	case []any:
		cloned := make([]any, len(value))
		for index, item := range value {
			cloned[index] = cloneToolSchemaValue(item)
		}

		return cloned
	case []string:
		return append([]string(nil), value...)
	default:
		return value
	}
}

func removeUnsupportedStrictKeywords(schema map[string]any) {
	for keyword := range schema {
		if isUnsupportedStrictSchemaKeyword(keyword) {
			delete(schema, keyword)
		}
	}

	for _, keyword := range []string{"properties", "$defs", "definitions"} {
		children, ok := schema[keyword].(map[string]any)
		if !ok {
			continue
		}
		for _, child := range children {
			if childSchema, ok := child.(map[string]any); ok {
				removeUnsupportedStrictKeywords(childSchema)
			}
		}
	}

	for _, keyword := range []string{"items", "additionalProperties", "not", "if", "then", "else"} {
		if child, ok := schema[keyword].(map[string]any); ok {
			removeUnsupportedStrictKeywords(child)
		}
	}

	for _, keyword := range []string{"anyOf", "allOf", "oneOf", "prefixItems"} {
		children, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for _, child := range children {
			if childSchema, ok := child.(map[string]any); ok {
				removeUnsupportedStrictKeywords(childSchema)
			}
		}
	}
}

func isUnsupportedStrictSchemaKeyword(keyword string) bool {
	switch keyword {
	case "contains", "format", "maxContains", "maxItems", "maxLength", "maxProperties", "maximum",
		"minContains", "minItems", "minLength", "minProperties", "minimum", "multipleOf", "pattern",
		"patternProperties", "propertyNames", "unevaluatedItems", "unevaluatedProperties", "uniqueItems":
		return true
	default:
		return false
	}
}

func toolValidationError(tool string, err error) error {
	validation, ok := err.(*validator.ValidationError)
	if !ok {
		return &ToolArgumentsValidationError{Tool: tool, Err: err}
	}

	return &ToolArgumentsValidationError{
		Tool:       tool,
		Violations: collectToolViolations(validation),
		Err:        err,
	}
}

func collectToolViolations(err *validator.ValidationError) []ToolArgumentViolation {
	violations := []ToolArgumentViolation{{
		InstanceLocation: append([]string(nil), err.InstanceLocation...),
		KeywordLocation:  append([]string(nil), err.ErrorKind.KeywordPath()...),
	}}

	for _, cause := range err.Causes {
		violations = append(violations, collectToolViolations(cause)...)
	}

	return violations
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
// aliases if no exact match is found under the server's ascii
// casemapping (domain.CaseFold): `/CONFIG` and `/config` name the
// same node.
func (n *Node[C]) Find(name string) *Node[C] {
	folded := domain.CaseFold(name)

	for _, child := range n.Children {
		if domain.CaseFold(child.Name) == folded {
			return child
		}
	}

	for _, child := range n.Children {
		for _, alias := range child.Aliases {
			if domain.CaseFold(alias) == folded {
				return child
			}
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

// resolveSecret resolves as many of input's leading tokens as name a
// command node, without decoding its arguments, and reports whether
// any node on that path is marked Secret. Stopping at name
// resolution keeps the answer available for a line whose arguments
// fail to decode, such as an accidental extra token after a pasted
// credential, since it never reaches ParseInvocation's full
// argument-consuming walk. The returned node is the deepest one
// resolved, for a caller that wants to log the command path without
// the argument that follows it. Name resolution goes through Find,
// so a command or subcommand name in any casing still resolves.
func (s Set[C]) resolveSecret(input string) (*Node[C], bool) {
	fields := strings.Fields(strings.TrimSpace(input))
	if len(fields) == 0 {
		return nil, false
	}

	node := s.Find(strings.TrimPrefix(fields[0], "/"))
	if node == nil {
		return nil, false
	}

	for _, tok := range fields[1:] {
		if node.Secret {
			return node, true
		}

		next := node.Find(tok)
		if next == nil {
			break
		}

		node = next
	}

	return node, node.Secret
}

// Find looks up a top-level node by name, falling back to aliases
// if no exact match is found under the server's ascii casemapping
// (domain.CaseFold): `/CONFIG` and `/config` name the same node.
// Tool-only nodes (registered with `tool:""` but no `cmd:""`) are
// skipped — they are not callable as slash commands.
func (s Set[C]) Find(name string) *Node[C] {
	folded := domain.CaseFold(name)

	for _, node := range s.Commands {
		if node.ToolOnly {
			continue
		}

		if domain.CaseFold(node.Name) == folded {
			return node
		}
	}

	for _, node := range s.Commands {
		if node.ToolOnly {
			continue
		}

		for _, alias := range node.Aliases {
			if domain.CaseFold(alias) == folded {
				return node
			}
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

func toolSchemaForField(field fieldMeta) (map[string]any, error) {
	typ := field.typ
	schema, err := toolSchemaForType(typ)
	if err != nil {
		return nil, err
	}

	if field.optional {
		makeNullable(schema)
	}
	if field.maxItems != nil {
		schema["maxItems"] = *field.maxItems
	}
	if field.typ.Kind() == reflect.Slice && !field.optional {
		schema["minItems"] = 1
	}

	description := field.toolHelp
	if description == "" {
		description = field.help
	}
	if description != "" {
		schema["description"] = description
	}

	return schema, nil
}

type jsonSchemaProvider interface {
	JSONSchema() *invjsonschema.Schema
}

var jsonSchemaProviderType = reflect.TypeFor[jsonSchemaProvider]()

func toolSchemaForType(typ reflect.Type) (map[string]any, error) {
	if schema, ok := schemaFromCustomType(typ); ok {
		return schemaViaCustomType(schema)
	}

	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	switch typ.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil

	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil

	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string"}, nil
		}

		items, err := toolSchemaForType(typ.Elem())
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"type":  "array",
			"items": items,
		}, nil

	case reflect.Array:
		items, err := toolSchemaForType(typ.Elem())
		if err != nil {
			return nil, err
		}

		return map[string]any{
			"type":     "array",
			"items":    items,
			"minItems": typ.Len(),
			"maxItems": typ.Len(),
		}, nil

	case reflect.Struct:
		return schemaViaReflector(typ)

	case reflect.String:
		return map[string]any{"type": "string"}, nil

	default:
		return nil, &UnsupportedToolSchemaTypeError{Type: typ}
	}
}

func schemaFromCustomType(typ reflect.Type) (*invjsonschema.Schema, bool) {
	for typ.Kind() == reflect.Pointer {
		if typ.Implements(jsonSchemaProviderType) {
			value := reflect.New(typ.Elem()).Interface().(jsonSchemaProvider)
			return value.JSONSchema(), true
		}
		typ = typ.Elem()
	}

	if typ.Implements(jsonSchemaProviderType) {
		value := reflect.Zero(typ).Interface().(jsonSchemaProvider)
		return value.JSONSchema(), true
	}
	if reflect.PointerTo(typ).Implements(jsonSchemaProviderType) {
		value := reflect.New(typ).Interface().(jsonSchemaProvider)
		return value.JSONSchema(), true
	}

	return nil, false
}

func schemaViaCustomType(schema *invjsonschema.Schema) (map[string]any, error) {
	out, err := schemaToMap(schema)
	if err != nil {
		return nil, err
	}
	makeStrictObjectSchema(out)

	return out, nil
}

// schemaViaReflector reflects a Go type into a JSON-Schema fragment via
// invopop/jsonschema, including any schema supplied by the type itself.
func schemaViaReflector(typ reflect.Type) (map[string]any, error) {
	reflector := invjsonschema.Reflector{
		DoNotReference: true,
		Mapper: func(typ reflect.Type) *invjsonschema.Schema {
			schema, _ := schemaFromCustomType(typ)
			return schema
		},
	}
	schema := reflector.ReflectFromType(typ)
	out, err := schemaToMap(schema)
	if err != nil {
		return nil, err
	}
	makeStrictObjectSchema(out)

	return out, nil
}

func schemaToMap(schema *invjsonschema.Schema) (map[string]any, error) {
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, &ToolSchemaEncodingError{Err: err}
	}

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, &ToolSchemaEncodingError{Err: err}
	}

	switch value := decoded.(type) {
	case bool:
		return nil, &UnsupportedBooleanToolSchemaError{Value: value}
	case map[string]any:
		if booleanValue, location, ok := booleanSchema(value, nil); ok {
			return nil, &UnsupportedBooleanToolSchemaError{Value: booleanValue, Location: location}
		}

		return value, nil
	default:
		return nil, &ToolSchemaEncodingError{Err: fmt.Errorf("schema encoded as %T", decoded)}
	}
}

func booleanSchema(schema map[string]any, location []string) (bool, []string, bool) {
	var foundValue bool
	var foundLocation []string
	var found bool
	visitSchemaValues(schema, location, func(keyword string, child any, childLocation []string) bool {
		switch child := child.(type) {
		case bool:
			if keyword == "additionalProperties" && !child {
				return true
			}

			foundValue = child
			foundLocation = childLocation
			found = true
		case map[string]any:
			foundValue, foundLocation, found = booleanSchema(child, childLocation)
		}

		return !found
	})

	return foundValue, foundLocation, found
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
// The walk recurses into every schema-valued keyword. Original
// `required` ordering is preserved; newly-added entries are appended
// in alphabetical order so the output is deterministic.
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
		if _, exists := schema["additionalProperties"]; !exists {
			schema["additionalProperties"] = false
		}
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

// recurseStrictChildren applies the strict walk to every nested schema.
func recurseStrictChildren(schema map[string]any) {
	visitSchemaChildren(schema, nil, func(child map[string]any, _ []string) bool {
		makeStrictObjectSchema(child)
		return true
	})
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
	for _, keyword := range []string{"anyOf", "oneOf"} {
		alternatives, ok := schema[keyword].([]any)
		if !ok {
			continue
		}
		for _, alternative := range alternatives {
			if child, ok := alternative.(map[string]any); ok && schemaTypeIncludes(child, "null") {
				return
			}
		}
		schema[keyword] = append(alternatives, map[string]any{"type": "null"})
		return
	}

	t, ok := schema["type"]
	if !ok {
		if len(schema) == 0 {
			return
		}

		original := maps.Clone(schema)
		clear(schema)
		schema["anyOf"] = []any{original, map[string]any{"type": "null"}}
		return
	}

	_, hasEnum := schema["enum"]
	_, hasConst := schema["const"]
	if hasEnum || hasConst {
		original := maps.Clone(schema)
		clear(schema)
		schema["anyOf"] = []any{original, map[string]any{"type": "null"}}
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

func schemaTypeIncludes(schema map[string]any, expected string) bool {
	switch value := schema["type"].(type) {
	case string:
		return value == expected
	case []any:
		for _, item := range value {
			if item == expected {
				return true
			}
		}
	}

	return false
}
