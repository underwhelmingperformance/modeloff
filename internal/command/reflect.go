package command

import (
	"reflect"
	"strconv"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// argTag holds the metadata extracted from struct tags on a single
// command field.
type argTag struct {
	Name       string
	Help       string
	Optional   bool
	FreeForm   bool
	Nargs      *int
	FieldIndex int
}

// parseArgTags reads struct tags from a command value and returns the
// argument metadata in field order. Fields without any recognised
// tags are skipped.
func parseArgTags(cmd Command) []argTag {
	t := reflect.TypeOf(cmd)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil
	}

	var tags []argTag

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)

		if !hasTag(f) {
			continue
		}

		tag := argTag{FieldIndex: i}

		if name, ok := f.Tag.Lookup("arg"); ok && name != "" {
			tag.Name = name
		} else {
			tag.Name = toKebabCase(f.Name)
		}

		tag.Help, _ = f.Tag.Lookup("help")

		if _, ok := f.Tag.Lookup("optional"); ok {
			tag.Optional = true
		}

		if f.Type == reflect.TypeOf([]string{}) {
			tag.FreeForm = true
		}

		if nargsStr, ok := f.Tag.Lookup("nargs"); ok {
			n, err := strconv.Atoi(nargsStr)
			if err == nil {
				tag.Nargs = &n
			}
		}

		tags = append(tags, tag)
	}

	return tags
}

// buildArgSpecs converts parsed tags into ArgSpec values, resolving
// sources by arg name from the provided map.
func buildArgSpecs(tags []argTag, sources map[string]SuggestionSource) []ArgSpec {
	specs := make([]ArgSpec, len(tags))

	for i, tag := range tags {
		specs[i] = ArgSpec{
			Name:     tag.Name,
			Help:     tag.Help,
			Optional: tag.Optional,
			FreeForm: tag.FreeForm,
			Nargs:    tag.Nargs,
		}

		if sources != nil {
			if src, ok := sources[tag.Name]; ok {
				specs[i].Source = src
			}
		}
	}

	return specs
}

// Handle builds a Spec from struct tags on T. The type parameter is
// typically inferred from the handler function. Sources are resolved
// by arg name from the provided map.
func Handle[T Command](name, help, usage string, sources map[string]SuggestionSource, handler func(T) tea.Cmd) Spec {
	var zero T
	tags := parseArgTags(zero)

	return Spec{
		Name:  name,
		Help:  help,
		Usage: usage,
		Args:  buildArgSpecs(tags, sources),
		Handler: func(inv Invocation) tea.Cmd {
			return handler(inv.Parsed.(T))
		},
	}
}

// hasTag returns true if the struct field has at least one recognised
// command tag.
func hasTag(f reflect.StructField) bool {
	for _, key := range []string{"arg", "help", "optional", "nargs", "freeform"} {
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
