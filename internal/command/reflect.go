package command

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode"
)

// resolveFieldMetas inspects a command struct's tags and builds
// fieldMeta entries. Fields with `arg` are positional; fields
// without `arg` (but with other recognised tags) are flags.
func resolveFieldMetas(cmd any) ([]fieldMeta, error) {
	t := reflect.TypeOf(cmd)
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil, nil
	}

	var metas []fieldMeta

	for i := 0; i < t.NumField(); i++ {
		meta, include, err := resolveFieldMeta(t.Field(i), i)
		if err != nil {
			return nil, err
		}
		if include {
			metas = append(metas, meta)
		}
	}

	cliFields := visibleFields(metas, func(field fieldMeta) bool { return !field.cliHidden })
	if err := validatePassthroughPosition(t, cliFields); err != nil {
		return nil, err
	}

	return metas, nil
}

func resolveCLIFieldMetas(cmd any) ([]fieldMeta, error) {
	fields, err := resolveFieldMetas(cmd)
	if err != nil {
		return nil, err
	}

	return visibleFields(fields, func(field fieldMeta) bool { return !field.cliHidden }), nil
}

func visibleFields(fields []fieldMeta, include func(fieldMeta) bool) []fieldMeta {
	visible := make([]fieldMeta, 0, len(fields))
	for _, field := range fields {
		if include(field) {
			visible = append(visible, field)
		}
	}

	return visible
}

func resolveFieldMeta(field reflect.StructField, index int) (fieldMeta, bool, error) {
	if _, ok := field.Tag.Lookup("cmd"); ok || !hasTag(field) {
		return fieldMeta{}, false, nil
	}

	meta := fieldMeta{
		index:      index,
		typ:        field.Type,
		help:       field.Tag.Get("help"),
		toolHelp:   field.Tag.Get("tool"),
		optional:   hasFieldTag(field, "optional"),
		variadic:   field.Type.Kind() == reflect.Slice,
		cliHidden:  field.Tag.Get("cli") == "-",
		toolHidden: field.Tag.Get("tool") == "-",
		xorGroups:  splitTagValues(field.Tag.Get("xor")),
	}

	if cli, ok := field.Tag.Lookup("cli"); ok && cli != "-" {
		return fieldMeta{}, false, &UnsupportedCLIScopeError{Field: field.Name, Value: cli}
	}
	if _, ok := field.Tag.Lookup("nargs"); ok {
		return fieldMeta{}, false, &UnsupportedNargsTagError{Field: field.Name}
	}

	maximum, err := maximumFor(field)
	if err != nil {
		return fieldMeta{}, false, err
	}
	meta.maxItems = maximum

	if argName, ok := field.Tag.Lookup("arg"); ok {
		meta.name = argName
		if meta.name == "" {
			meta.name = toKebabCase(field.Name)
		}
	} else {
		meta.isFlag = true
		meta.boolFlag = field.Type.Kind() == reflect.Bool
		meta.name = toKebabCase(field.Name)
		meta.flagName = "--" + meta.name
	}

	passthrough, err := passthroughModeFor(field, meta.isFlag)
	if err != nil {
		return fieldMeta{}, false, err
	}
	meta.passthrough = passthrough

	if !meta.cliHidden {
		meta.decoder = defaultRegistry.ForType(field.Type)
		if meta.decoder == nil {
			return fieldMeta{}, false, &NoDecoderError{Type: field.Type}
		}
	}

	return meta, true, nil
}

func maximumFor(field reflect.StructField) (*int, error) {
	raw, ok := field.Tag.Lookup("max")
	if !ok {
		return nil, nil
	}
	if field.Type.Kind() != reflect.Slice {
		return nil, &MaxOnNonSliceError{Field: field.Name, Type: field.Type}
	}

	maximum, err := strconv.Atoi(raw)
	if err != nil || maximum <= 0 {
		return nil, &InvalidMaxTagError{Field: field.Name, Value: raw}
	}

	return &maximum, nil
}

func splitTagValues(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
}

func passthroughModeFor(field reflect.StructField, flag bool) (PassthroughMode, error) {
	mode, ok := field.Tag.Lookup("passthrough")
	if !ok {
		return PassthroughModeNone, nil
	}
	if flag {
		return PassthroughModeNone, &PassthroughOnFlagError{Field: field.Name}
	}

	switch mode {
	case "", "all":
		return PassthroughModeAll, nil
	case "partial":
		return PassthroughModePartial, nil
	default:
		return PassthroughModeNone, &UnsupportedPassthroughModeError{Field: field.Name, Mode: mode}
	}
}

func validatePassthroughPosition(structType reflect.Type, metas []fieldMeta) error {
	for index, meta := range metas {
		if meta.passthrough == PassthroughModeNone {
			continue
		}

		for _, following := range metas[index+1:] {
			if !following.isFlag {
				return &PassthroughNotFinalError{Field: structType.Field(meta.index).Name}
			}
		}
	}

	return nil
}

// buildPositionals converts positional fieldMetas into Positional
// values for the completion system.
func buildPositionals[C KindProvider](fields []fieldMeta, sources map[string]SuggestionSource[C]) []Positional[C] {
	var positionals []Positional[C]

	for _, f := range fields {
		if f.isFlag {
			continue
		}

		p := Positional[C]{
			Name:        f.name,
			Help:        f.help,
			Optional:    f.optional,
			Variadic:    f.variadic,
			Passthrough: f.passthrough,
		}

		if sources != nil {
			if src, ok := sources[f.name]; ok {
				p.Source = src
			}
		}

		positionals = append(positionals, p)
	}

	return positionals
}

// buildFlags converts flag fieldMetas into Flag values for the
// completion system.
func buildFlags[C KindProvider](fields []fieldMeta, sources map[string]SuggestionSource[C]) []Flag[C] {
	var flags []Flag[C]

	for _, f := range fields {
		if !f.isFlag {
			continue
		}

		fl := Flag[C]{
			Name:     f.flagName,
			Help:     f.help,
			Boolean:  f.boolFlag,
			Optional: f.optional,
			Variadic: f.variadic,
		}

		if sources != nil {
			if src, ok := sources[f.name]; ok {
				fl.Source = src
			}
		}

		flags = append(flags, fl)
	}

	return flags
}

// hasCmdChildren returns true if the struct type has any exported
// fields tagged with `cmd:""`, indicating it is a group node whose
// fields are subcommands rather than arguments.
func hasCmdChildren(t reflect.Type) bool {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return false
	}

	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}

		if _, ok := f.Tag.Lookup("cmd"); ok {
			return true
		}
	}

	return false
}

// build reflects over a grammar struct and produces a slice of
// Nodes, one per field tagged with `cmd:""`. The grammar must be a
// pointer to a struct. Name derives from the field name
// (kebab-cased) or from a `name:""` tag. Help comes from the
// `help:""` tag.
//
// If a `cmd`-tagged field is itself a struct whose fields carry
// `cmd` tags, the field becomes a branch node that can have both its
// own args/flags and child commands. This recursion works at any
// depth.
func buildNode[C KindProvider](ft reflect.StructField, fieldVal reflect.Value) (*Node[C], error) {
	name := ft.Tag.Get("name")
	if name == "" {
		name = toKebabCase(ft.Name)
	}

	help := ft.Tag.Get("help")

	var aliases []string
	if aliasStr := ft.Tag.Get("aliases"); aliasStr != "" {
		aliases = strings.Split(aliasStr, ",")
	}

	fieldType := ft.Type

	allFields, err := resolveFieldMetas(reflect.New(fieldType).Elem().Interface())
	if err != nil {
		return nil, err
	}
	fields := visibleFields(allFields, func(field fieldMeta) bool { return !field.cliHidden })
	toolFields := visibleFields(allFields, func(field fieldMeta) bool { return !field.toolHidden })

	var sources map[string]SuggestionSource[C]

	if c, ok := fieldVal.Interface().(Completer[C]); ok {
		sources = c.Sources()
	}

	node := &Node[C]{
		Name:                 name,
		Aliases:              aliases,
		Help:                 help,
		RequiredKind:         parseRequiredKind(ft.Tag.Get("kind")),
		RequiredCapabilities: parseRequiredCapabilities(ft.Tag.Get("caps")),
		Tool:                 hasToolTag(ft),
		ToolDesc:             toolDescFromTag(ft),
		Secret:               hasSecretTag(ft),
		Positionals:          buildPositionals[C](fields, sources),
		Flags:                buildFlags[C](fields, sources),
		fields:               fields,
		toolFields:           toolFields,
		factory: func() any {
			return reflect.New(fieldType).Interface()
		},
	}

	if node.Tool {
		runtimeSchema, groups, err := buildToolParameters(toolFields)
		if err != nil {
			return nil, err
		}
		toolSchema, err := providerToolSchema(name, runtimeSchema)
		if err != nil {
			return nil, err
		}

		toolValidator, err := compileToolSchema(name, toolSchema)
		if err != nil {
			return nil, err
		}
		runtimeValidator, err := compileToolSchema(name, runtimeSchema)
		if err != nil {
			return nil, err
		}

		node.toolSchema = toolSchema
		node.toolValidator = toolValidator
		node.runtimeValidator = runtimeValidator
		node.toolXORGroups = groups
	}

	if hasCmdChildren(fieldType) {
		childPtr := reflect.New(fieldType).Interface()

		children, err := build[C](childPtr)
		if err != nil {
			return nil, err
		}

		node.Children = children

		for _, child := range node.Children {
			child.Parent = node
		}
	}

	return node, nil
}

func build[C KindProvider](grammar any) ([]*Node[C], error) {
	v := reflect.ValueOf(grammar)
	if v.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("grammar must be a pointer to a struct, got %T", grammar)
	}

	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("grammar must be a pointer to a struct, got pointer to %s", v.Kind())
	}

	t := v.Type()
	var nodes []*Node[C]
	seenNames := map[string]string{}

	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i)

		if !ft.IsExported() {
			continue
		}

		_, hasCmd := ft.Tag.Lookup("cmd")
		_, hasTool := ft.Tag.Lookup("tool")
		if !hasCmd && !hasTool {
			continue
		}

		node, err := buildNode[C](ft, v.Field(i))
		if err == nil {
			node.ToolOnly = !hasCmd
		}
		if err != nil {
			return nil, &FieldError{Field: ft.Name, Err: err}
		}

		if owner, ok := seenNames[node.Name]; ok {
			return nil, &DuplicateCommandError{Name: node.Name, ConflictsWith: owner}
		}

		seenNames[node.Name] = node.Name

		for _, alias := range node.Aliases {
			if owner, ok := seenNames[alias]; ok {
				return nil, &AliasCollisionError{Alias: alias, Command: node.Name, ConflictsWith: owner}
			}

			seenNames[alias] = node.Name
		}

		nodes = append(nodes, node)
	}

	return nodes, nil
}

// hasTag returns true if the struct field has at least one recognised
// command tag.
func hasTag(f reflect.StructField) bool {
	for _, key := range []string{"arg", "help", "optional", "nargs", "passthrough", "tool", "cli", "xor", "max"} {
		if _, ok := f.Tag.Lookup(key); ok {
			return true
		}
	}

	return false
}

func hasFieldTag(field reflect.StructField, name string) bool {
	_, ok := field.Tag.Lookup(name)

	return ok
}

func hasToolTag(f reflect.StructField) bool {
	_, ok := f.Tag.Lookup("tool")

	return ok
}

// hasSecretTag reports whether f is marked `secret:""`, the grammar's
// declarative marker for a command whose arguments carry a
// credential.
func hasSecretTag(f reflect.StructField) bool {
	_, ok := f.Tag.Lookup("secret")

	return ok
}

func toolDescFromTag(f reflect.StructField) string {
	v, _ := f.Tag.Lookup("tool")

	return v
}

// toKebabCase converts PascalCase to kebab-case.
func toKebabCase(s string) string {
	var b strings.Builder

	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				prev := rune(s[i-1])
				if unicode.IsLower(prev) {
					b.WriteByte('-')
				} else if i+1 < len(s) && unicode.IsLower(rune(s[i+1])) {
					b.WriteByte('-')
				}
			}

			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}

	return b.String()
}

func toSnakeCase(s string) string {
	return strings.ReplaceAll(toKebabCase(s), "-", "_")
}
