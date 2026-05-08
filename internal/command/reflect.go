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
		f := t.Field(i)

		if _, ok := f.Tag.Lookup("cmd"); ok {
			continue
		}

		if !hasTag(f) {
			continue
		}

		m := fieldMeta{index: i, typ: f.Type}

		if argName, ok := f.Tag.Lookup("arg"); ok {
			m.isFlag = false
			if argName != "" {
				m.name = argName
			} else {
				m.name = toKebabCase(f.Name)
			}
		} else {
			m.isFlag = true
			m.boolFlag = f.Type.Kind() == reflect.Bool
			m.name = toKebabCase(f.Name)
			m.flagName = "--" + m.name
		}

		m.help, _ = f.Tag.Lookup("help")

		if _, ok := f.Tag.Lookup("optional"); ok {
			m.optional = true
		}

		m.variadic = f.Type.Kind() == reflect.Slice

		if nargsStr, ok := f.Tag.Lookup("nargs"); ok {
			n, err := strconv.Atoi(nargsStr)
			if err == nil {
				m.nargs = &n
			}
		}

		dec := defaultRegistry.ForType(f.Type)
		if dec == nil {
			return nil, &NoDecoderError{Type: f.Type}
		}

		m.decoder = dec
		metas = append(metas, m)
	}

	return metas, nil
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
			Name:     f.name,
			Help:     f.help,
			Optional: f.optional,
			Variadic: f.variadic,
			Nargs:    f.nargs,
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

	fields, err := resolveFieldMetas(reflect.New(fieldType).Elem().Interface())
	if err != nil {
		return nil, err
	}

	var sources map[string]SuggestionSource[C]

	if c, ok := fieldVal.Interface().(Completer[C]); ok {
		sources = c.Sources()
	}

	node := &Node[C]{
		Name:         name,
		Aliases:      aliases,
		Help:         help,
		RequiredKind: parseRequiredKind(ft.Tag.Get("kind")),
		Tool:         hasToolTag(ft),
		ToolDesc:     toolDescFromTag(ft),
		Positionals:  buildPositionals[C](fields, sources),
		Flags:        buildFlags[C](fields, sources),
		fields:       fields,
		factory: func() any {
			return reflect.New(fieldType).Interface()
		},
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

		if _, ok := ft.Tag.Lookup("cmd"); !ok {
			continue
		}

		node, err := buildNode[C](ft, v.Field(i))
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
	for _, key := range []string{"arg", "help", "optional", "nargs"} {
		if _, ok := f.Tag.Lookup(key); ok {
			return true
		}
	}

	return false
}

func hasToolTag(f reflect.StructField) bool {
	_, ok := f.Tag.Lookup("tool")

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
