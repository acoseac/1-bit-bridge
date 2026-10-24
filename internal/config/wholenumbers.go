package config

import (
	"bytes"
	"encoding"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// A whole-number setting takes no value that yaml.v3 would change on the
// way in (backlog B528). yaml.v3 v3.0.1 decodes a !!float into an integer
// by dropping what follows the point (decode.go's scalar, int64(resolved)),
// and takes -.inf and 2^63 as whatever the platform's conversion makes of
// them, so scanIntervalSec: 2.5 loaded as 2, the manifest limit's
// requestsPerMinute: 0.5 as 0, which turns the limit off, and
// output.player.openhomePort: 7792.9 bound 7792, with no word said: a value
// nobody wrote. Such a value is refused where a word on its line is: Load
// refuses the file (DecodeFile), and the player block's reader leaves the
// block unread (playerReader.whole). A float the integer holds exactly is
// taken as it is: 30.0, 3e1, and 08, which yaml.v3 resolves as a float.

// DecodeFile decodes raw, a config file's bytes, into cfg as Load does
// before its defaults, its BRIDGE_* overrides, its paths and its checks:
// yaml.v3's decoder with unknown keys refused (the typo catcher), then, once
// that decode succeeds, CheckWholeNumbers over the same bytes. bridge init's
// rewrite reads a config through it, so the two refuse a file alike. An
// error yaml.v3 returns quotes the file's text: print it through
// ValueFreeYAMLError, as Load does.
func DecodeFile(raw []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // typo-catcher: unknown YAML keys fail the load
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	return checkWholeNumbers(reflect.TypeOf(cfg), yaml.NewDecoder(bytes.NewReader(raw)).Decode)
}

// CheckWholeNumbers refuses each whole-number setting that n, decoded into
// into (a pointer, as for Decode), writes as a value yaml.v3 would change on
// the way in: a fraction, an infinity, NaN, or a float outside the setting's
// range (exactInteger). It answers a *yaml.TypeError with a line for each,
// in the words ValueFreeYAMLError gives a float yaml.v3 refuses itself
// ("line 5: cannot unmarshal a !!float value into int"), which quote
// nothing of the value, or nil. It looks at nothing else, so it is asked of
// a node that decodes into into without an error.
//
// yaml.v3 itself maps n onto the settings, decoding it into into's type
// with each whole number in it replaced by a check (shadowOf), so a merge
// key, an alias, an inline field and an encoded key reach the setting they
// reach in the decode, and none other. A map's keys are not looked at: no
// map in the config is keyed by a number.
func CheckWholeNumbers(n *yaml.Node, into any) error {
	return checkWholeNumbers(reflect.TypeOf(into), n.Decode)
}

// checkWholeNumbers is CheckWholeNumbers for a value of type into (or what
// it points to), which decode decodes into.
func checkWholeNumbers(into reflect.Type, decode func(any) error) error {
	if into == nil {
		return nil
	}
	if into.Kind() == reflect.Pointer {
		into = into.Elem()
	}
	shadow, ok := shadowOf(into)
	if !ok {
		return nil
	}
	return decode(reflect.New(shadow).Interface())
}

// exactInteger reports whether yaml.v3 decodes n, a value written for an
// integer of type t, unchanged: anything but a !!float (an integer, a null,
// or a value the decoder refuses itself), or a !!float whose value is whole
// and inside t's range. Any other !!float that the decoder takes, it takes
// changed.
func exactInteger(n *yaml.Node, t reflect.Type) bool {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!float" {
		return true
	}
	var f float64
	if n.Decode(&f) != nil {
		return false
	}
	bits := t.Bits()
	lo, hi := 0.0, math.Ldexp(1, bits)
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		lo, hi = -math.Ldexp(1, bits-1), math.Ldexp(1, bits-1)
	}
	return f == math.Trunc(f) && f >= lo && f < hi
}

// integerCheck stands in for a whole-number setting of type T in a shadow
// (shadowOf). It decodes nothing.
type integerCheck[T any] struct{}

// UnmarshalYAML refuses a value yaml.v3 would decode into T changed. The
// refusal is a *yaml.TypeError, which the decoder adds to its own and goes
// on past, so every such setting is named. yaml.v3 asks it of every value
// but a null, an alias's once the alias is followed, at the line of the
// node it names.
func (*integerCheck[T]) UnmarshalYAML(n *yaml.Node) error {
	t := reflect.TypeFor[T]()
	if exactInteger(n, t) {
		return nil
	}
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: cannot unmarshal a !!float value into %s", n.Line, t)}}
}

// integerChecks is the integerCheck for each integer kind. A named integer
// type takes its kind's, which names the kind.
var integerChecks = map[reflect.Kind]reflect.Type{
	reflect.Int:     reflect.TypeFor[integerCheck[int]](),
	reflect.Int8:    reflect.TypeFor[integerCheck[int8]](),
	reflect.Int16:   reflect.TypeFor[integerCheck[int16]](),
	reflect.Int32:   reflect.TypeFor[integerCheck[int32]](),
	reflect.Int64:   reflect.TypeFor[integerCheck[int64]](),
	reflect.Uint:    reflect.TypeFor[integerCheck[uint]](),
	reflect.Uint8:   reflect.TypeFor[integerCheck[uint8]](),
	reflect.Uint16:  reflect.TypeFor[integerCheck[uint16]](),
	reflect.Uint32:  reflect.TypeFor[integerCheck[uint32]](),
	reflect.Uint64:  reflect.TypeFor[integerCheck[uint64]](),
	reflect.Uintptr: reflect.TypeFor[integerCheck[uintptr]](),
}

// The types a value reads itself through, and the two yaml.v3 decodes
// otherwise than by their kind.
var (
	yamlUnmarshaler     = reflect.TypeFor[yaml.Unmarshaler]()
	obsoleteUnmarshaler = reflect.TypeFor[interface{ UnmarshalYAML(func(any) error) error }]()
	textUnmarshaler     = reflect.TypeFor[encoding.TextUnmarshaler]()
	yamlNodeType        = reflect.TypeFor[yaml.Node]()
	durationType        = reflect.TypeFor[time.Duration]()
)

// shadowTypes holds shadowOf's answer for each type it was asked of.
var shadowTypes sync.Map // reflect.Type → shadowType

// shadowType is one of shadowOf's answers.
type shadowType struct {
	t  reflect.Type
	ok bool
}

// shadowOf is t with each whole number in it replaced by its integerCheck,
// and ok false, with t as it is, when t holds none. Every struct field it
// rebuilds keeps its name and its tag, and every type that holds no whole
// number is kept as it is, so yaml.v3 maps a document onto the shadow as
// onto t and decodes the same nodes in the same order: the same keys,
// merges, aliases and inline fields, and the same count of aliases against
// its limit. A type that reads its own YAML (yaml.Unmarshaler, the older
// Unmarshaler, encoding.TextUnmarshaler, yaml.Node) is not looked into: it
// takes what it takes, as PlayerConfig does. A time.Duration decodes from
// text alone, so it is no whole number here. An unexported field is one
// yaml.v3 does not decode, and the shadow leaves it out.
func shadowOf(t reflect.Type) (reflect.Type, bool) {
	if s, ok := shadowTypes.Load(t); ok {
		return s.(shadowType).t, s.(shadowType).ok
	}
	s, ok := buildShadow(t, map[reflect.Type]bool{})
	shadowTypes.Store(t, shadowType{s, ok})
	return s, ok
}

// buildShadow is shadowOf's walk. within holds the struct types it is
// inside, so a type that holds itself is looked into once.
func buildShadow(t reflect.Type, within map[reflect.Type]bool) (reflect.Type, bool) {
	p := reflect.PointerTo(t)
	if t == yamlNodeType || p.Implements(yamlUnmarshaler) || p.Implements(obsoleteUnmarshaler) || p.Implements(textUnmarshaler) {
		return t, false
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		e, ok := buildShadow(t.Elem(), within)
		if !ok {
			return t, false
		}
		switch t.Kind() {
		case reflect.Pointer:
			return reflect.PointerTo(e), true
		case reflect.Slice:
			return reflect.SliceOf(e), true
		case reflect.Array:
			return reflect.ArrayOf(t.Len(), e), true
		}
		return reflect.MapOf(t.Key(), e), true
	case reflect.Struct:
		if within[t] {
			return t, false
		}
		within[t] = true
		defer delete(within, t)
		fields := make([]reflect.StructField, 0, t.NumField())
		holds := false
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			ft, ok := buildShadow(f.Type, within)
			holds = holds || ok
			fields = append(fields, reflect.StructField{Name: f.Name, Type: ft, Tag: f.Tag})
		}
		if !holds {
			return t, false
		}
		return reflect.StructOf(fields), true
	}
	if check, ok := integerChecks[t.Kind()]; ok && t != durationType {
		return check, true
	}
	return t, false
}
