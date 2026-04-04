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
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil, nil
	}

	var metas []fieldMeta

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		if !hasTag(f) {
			continue
		}

		m := fieldMeta{index: i}

		if argName, ok := f.Tag.Lookup("arg"); ok {
			m.isFlag = false
			if argName != "" {
				m.name = argName
			} else {
				m.name = toKebabCase(f.Name)
			}
		} else {
			m.isFlag = true
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
func buildPositionals(fields []fieldMeta, sources map[string]SuggestionSource) []Positional {
	var positionals []Positional

	for _, f := range fields {
		if f.isFlag {
			continue
		}

		p := Positional{
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
func buildFlags(fields []fieldMeta, sources map[string]SuggestionSource) []Flag {
	var flags []Flag

	for _, f := range fields {
		if !f.isFlag {
			continue
		}

		fl := Flag{
			Name:     f.flagName,
			Help:     f.help,
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

// build reflects over a grammar struct and produces a slice of
// Nodes, one per field tagged with `cmd:""`. The grammar must be a
// pointer to a struct. Name derives from the field name
// (kebab-cased) or from a `name:""` tag. Help comes from the
// `help:""` tag.
func build(grammar any) ([]*Node, error) {
	v := reflect.ValueOf(grammar)
	if v.Kind() != reflect.Ptr {
		return nil, fmt.Errorf("grammar must be a pointer to a struct, got %T", grammar)
	}

	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return nil, fmt.Errorf("grammar must be a pointer to a struct, got pointer to %s", v.Kind())
	}

	t := v.Type()
	var nodes []*Node

	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i)

		if !ft.IsExported() {
			continue
		}

		if _, ok := ft.Tag.Lookup("cmd"); !ok {
			continue
		}

		name := ft.Tag.Get("name")
		if name == "" {
			name = toKebabCase(ft.Name)
		}

		help := ft.Tag.Get("help")

		fieldType := ft.Type
		fields, err := resolveFieldMetas(reflect.New(fieldType).Elem().Interface())
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", ft.Name, err)
		}

		node := &Node{
			Name:        name,
			Help:        help,
			Positionals: buildPositionals(fields, nil),
			Flags:       buildFlags(fields, nil),
			factory: func() any {
				return reflect.New(fieldType).Interface()
			},
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
