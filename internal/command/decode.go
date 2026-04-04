package command

import (
	"encoding"
	"fmt"
	"math/bits"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// DecodeError is returned when a raw value cannot be decoded into
// the target type.
type DecodeError struct {
	Value    string
	Expected string
	Err      error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("expected %s but got %q", e.Expected, e.Value)
}

func (e *DecodeError) Unwrap() error {
	return e.Err
}

// NoDecoderError is returned when the registry has no decoder for a
// type.
type NoDecoderError struct {
	Type reflect.Type
}

func (e *NoDecoderError) Error() string {
	return fmt.Sprintf("no decoder for type %s", e.Type)
}

// Decoder populates a reflect.Value from a raw string.
type Decoder interface {
	Decode(raw string, target reflect.Value) error
}

// DecoderFunc is a single function that implements Decoder.
type DecoderFunc func(raw string, target reflect.Value) error

// Decode implements Decoder.
func (f DecoderFunc) Decode(raw string, target reflect.Value) error {
	return f(raw, target)
}

// FieldDecoder may be implemented by field types that need custom
// parsing. This is checked first during resolution, before any
// registered types or kind defaults.
type FieldDecoder interface {
	Decode(raw string) error
}

var (
	fieldDecoderType    = reflect.TypeOf((*FieldDecoder)(nil)).Elem()
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// Registry maps types and kinds to Decoders. Resolution order:
//  1. FieldDecoder interface on the type (or pointer to it)
//  2. Exact type match
//  3. encoding.TextUnmarshaler interface
//  4. Kind fallback (including slice)
type Registry struct {
	types map[reflect.Type]Decoder
	kinds map[reflect.Kind]Decoder
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		types: map[reflect.Type]Decoder{},
		kinds: map[reflect.Kind]Decoder{},
	}
}

// RegisterType registers a Decoder for an exact reflect.Type.
func (r *Registry) RegisterType(typ reflect.Type, d Decoder) *Registry {
	r.types[typ] = d
	return r
}

// RegisterKind registers a Decoder for a reflect.Kind.
func (r *Registry) RegisterKind(kind reflect.Kind, d Decoder) *Registry {
	r.kinds[kind] = d
	return r
}

// ForType resolves a Decoder for the given type.
func (r *Registry) ForType(typ reflect.Type) Decoder {
	for _, impl := range []reflect.Type{typ, reflect.PointerTo(typ)} {
		if impl.Implements(fieldDecoderType) {
			return &fieldDecoderAdapter{}
		}
	}

	if d, ok := r.types[typ]; ok {
		return d
	}

	for _, impl := range []reflect.Type{typ, reflect.PointerTo(typ)} {
		if impl.Implements(textUnmarshalerType) {
			return &textUnmarshalerAdapter{}
		}
	}

	switch typ.Kind() {
	case reflect.Slice:
		return r.sliceDecoder(typ)
	case reflect.Map:
		return r.mapDecoder(typ)
	case reflect.Ptr:
		return r.ptrDecoder(typ)
	}

	if d, ok := r.kinds[typ.Kind()]; ok {
		return d
	}

	return nil
}

// RegisterDefaults registers decoders for all primitive Go kinds.
func (r *Registry) RegisterDefaults() *Registry {
	return r.
		RegisterKind(reflect.String, stringDecoder()).
		RegisterKind(reflect.Bool, boolDecoder()).
		RegisterKind(reflect.Int, intDecoder(bits.UintSize)).
		RegisterKind(reflect.Int8, intDecoder(8)).
		RegisterKind(reflect.Int16, intDecoder(16)).
		RegisterKind(reflect.Int32, intDecoder(32)).
		RegisterKind(reflect.Int64, intDecoder(64)).
		RegisterKind(reflect.Uint, uintDecoder(bits.UintSize)).
		RegisterKind(reflect.Uint8, uintDecoder(8)).
		RegisterKind(reflect.Uint16, uintDecoder(16)).
		RegisterKind(reflect.Uint32, uintDecoder(32)).
		RegisterKind(reflect.Uint64, uintDecoder(64)).
		RegisterKind(reflect.Float32, floatDecoder(32)).
		RegisterKind(reflect.Float64, floatDecoder(64)).
		RegisterType(reflect.TypeOf(time.Duration(0)), durationDecoder()).
		RegisterType(reflect.TypeOf(time.Time{}), timeDecoder()).
		RegisterType(reflect.TypeOf(&url.URL{}), urlDecoder())
}

type fieldDecoderAdapter struct{}

func (a *fieldDecoderAdapter) Decode(raw string, target reflect.Value) error {
	var fd FieldDecoder

	if target.Type().Implements(fieldDecoderType) {
		fd = target.Interface().(FieldDecoder)
	} else {
		fd = target.Addr().Interface().(FieldDecoder)
	}

	return fd.Decode(raw)
}

func stringDecoder() DecoderFunc {
	return func(raw string, target reflect.Value) error {
		target.SetString(raw)
		return nil
	}
}

func boolDecoder() DecoderFunc {
	return func(raw string, target reflect.Value) error {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return &DecodeError{Value: raw, Expected: "bool", Err: err}
		}

		target.SetBool(b)
		return nil
	}
}

func intDecoder(bitSize int) DecoderFunc {
	return func(raw string, target reflect.Value) error {
		n, err := strconv.ParseInt(raw, 0, bitSize)
		if err != nil {
			return &DecodeError{Value: raw, Expected: fmt.Sprintf("int%d", bitSize), Err: err}
		}

		target.SetInt(n)
		return nil
	}
}

func uintDecoder(bitSize int) DecoderFunc {
	return func(raw string, target reflect.Value) error {
		n, err := strconv.ParseUint(raw, 0, bitSize)
		if err != nil {
			return &DecodeError{Value: raw, Expected: fmt.Sprintf("uint%d", bitSize), Err: err}
		}

		target.SetUint(n)
		return nil
	}
}

func floatDecoder(bitSize int) DecoderFunc {
	return func(raw string, target reflect.Value) error {
		n, err := strconv.ParseFloat(raw, bitSize)
		if err != nil {
			return &DecodeError{Value: raw, Expected: fmt.Sprintf("float%d", bitSize), Err: err}
		}

		target.SetFloat(n)
		return nil
	}
}

func durationDecoder() DecoderFunc {
	return func(raw string, target reflect.Value) error {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return &DecodeError{Value: raw, Expected: "duration", Err: err}
		}

		target.Set(reflect.ValueOf(d))
		return nil
	}
}

type textUnmarshalerAdapter struct{}

func (a *textUnmarshalerAdapter) Decode(raw string, target reflect.Value) error {
	var um encoding.TextUnmarshaler

	if target.Type().Implements(textUnmarshalerType) {
		um = target.Interface().(encoding.TextUnmarshaler)
	} else {
		um = target.Addr().Interface().(encoding.TextUnmarshaler)
	}

	if err := um.UnmarshalText([]byte(raw)); err != nil {
		return &DecodeError{Value: raw, Expected: "text", Err: err}
	}

	return nil
}

func timeDecoder() DecoderFunc {
	return func(raw string, target reflect.Value) error {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return &DecodeError{Value: raw, Expected: "time (RFC3339)", Err: err}
		}

		target.Set(reflect.ValueOf(t))
		return nil
	}
}

func urlDecoder() DecoderFunc {
	return func(raw string, target reflect.Value) error {
		u, err := url.Parse(raw)
		if err != nil {
			return &DecodeError{Value: raw, Expected: "url", Err: err}
		}

		target.Set(reflect.ValueOf(u))
		return nil
	}
}

// sliceDecoder returns a Decoder that appends one decoded element per
// call. Returns nil if no decoder exists for the element type.
func (r *Registry) sliceDecoder(typ reflect.Type) Decoder {
	elemDec := r.ForType(typ.Elem())
	if elemDec == nil {
		return nil
	}

	elem := typ.Elem()

	return DecoderFunc(func(raw string, target reflect.Value) error {
		el := reflect.New(elem).Elem()

		if err := elemDec.Decode(raw, el); err != nil {
			return err
		}

		target.Set(reflect.Append(target, el))
		return nil
	})
}

// mapDecoder returns a Decoder that parses "key=value" pairs and
// inserts them into the map. Returns nil if no decoder exists for
// the key or value types.
func (r *Registry) mapDecoder(typ reflect.Type) Decoder {
	keyDec := r.ForType(typ.Key())
	if keyDec == nil {
		return nil
	}

	valDec := r.ForType(typ.Elem())
	if valDec == nil {
		return nil
	}

	keyType := typ.Key()
	valType := typ.Elem()

	return DecoderFunc(func(raw string, target reflect.Value) error {
		parts := strings.SplitN(raw, "=", 2)
		if len(parts) != 2 {
			return &DecodeError{Value: raw, Expected: "key=value", Err: fmt.Errorf("missing '='")}
		}

		k := reflect.New(keyType).Elem()
		if err := keyDec.Decode(parts[0], k); err != nil {
			return err
		}

		v := reflect.New(valType).Elem()
		if err := valDec.Decode(parts[1], v); err != nil {
			return err
		}

		if target.IsNil() {
			target.Set(reflect.MakeMap(typ))
		}

		target.SetMapIndex(k, v)
		return nil
	})
}

// ptrDecoder returns a Decoder that allocates the pointer target and
// decodes into it. Returns nil if no decoder exists for the element
// type.
func (r *Registry) ptrDecoder(typ reflect.Type) Decoder {
	elemDec := r.ForType(typ.Elem())
	if elemDec == nil {
		return nil
	}

	elem := typ.Elem()

	return DecoderFunc(func(raw string, target reflect.Value) error {
		el := reflect.New(elem).Elem()

		if err := elemDec.Decode(raw, el); err != nil {
			return err
		}

		target.Set(el.Addr())
		return nil
	})
}
