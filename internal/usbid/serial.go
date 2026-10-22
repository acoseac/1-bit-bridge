// Package usbid holds what names a USB audio unit for the bridge's player
// (B470): its serial, which is kept like a password (Serial), its vendor
// and product ids (ParseHex16), and the anchored path of the port it sits
// on (Port). The configuration stores a player's binding in these types
// and the sysfs reader (internal/alsa) observes cards in them, so the two
// always compare like with like.
package usbid

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Serial is a USB unit's serial number. The bridge treats it as a
// password: it lives in the local config file, which is written 0600, and
// nowhere else, so every way Go prints or encodes a value prints a
// redaction instead. String, GoString, Format (every verb: %v, %+v and %#v
// included), MarshalJSON and LogValue never carry it. The one way out is
// MarshalYAML, which is what writes it to the config file, and
// UnmarshalYAML reads it back. There is deliberately no MarshalText: an
// encoder that takes one would carry the serial.
//
// The value sits behind a pointer, so a Serial that fmt reaches through an
// unexported field, where it cannot call Format, prints as an address and
// never as the serial. A Serial is immutable, so a copy may share it.
//
// The zero Serial is no serial. New trims trailing NULs and whitespace,
// which is how a serial padded in its string descriptor reads, and an
// empty result is no serial, which never matches anything (Equal).
type Serial struct{ v *string }

// The two fixed forms a Serial prints as.
const (
	redacted = "[redacted]"
	none     = "[none]"
)

// New returns raw as a Serial: trailing NULs and whitespace trimmed, and no
// serial at all when nothing is left.
func New(raw string) Serial {
	s := strings.TrimRightFunc(raw, func(r rune) bool { return r == 0 || unicode.IsSpace(r) })
	if s == "" {
		return Serial{}
	}
	return Serial{v: &s}
}

// Present reports whether s is a serial: something was left once New
// trimmed it.
func (s Serial) Present() bool { return s.v != nil }

// Equal reports whether s and t are the same serial, byte for byte. A
// Serial that is not present matches nothing, not even another one that is
// not present: a unit with no serial is not identified by its serial.
func (s Serial) Equal(t Serial) bool {
	return s.v != nil && t.v != nil && *s.v == *t.v
}

// labelMin is the shortest serial whose label shows its tail: a shorter
// one is not shown at all.
const labelMin = 12

// Label is what a person is shown of s: "…" and its last four characters
// when it has twelve or more, "serial set" when it is shorter or its tail
// would not print as written, and "no serial" when it is not present. It
// is the only part of a serial that ever reaches a terminal, a log or an
// admin page.
func (s Serial) Label() string {
	if s.v == nil {
		return "no serial"
	}
	if utf8.RuneCountInString(*s.v) < labelMin {
		return "serial set"
	}
	runes := []rune(*s.v)
	tail := runes[len(runes)-4:]
	for _, r := range tail {
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			return "serial set"
		}
	}
	return "…" + string(tail)
}

// String is the redaction, or "[none]" when s is not present.
func (s Serial) String() string {
	if s.v == nil {
		return none
	}
	return redacted
}

// GoString is %#v's form: the redaction, typed.
func (s Serial) GoString() string { return "usbid.Serial(" + s.String() + ")" }

// Format prints s for every verb, so %x, %q or a width cannot print it
// either: GoString for %#v, String for the rest.
func (s Serial) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = f.Write([]byte(s.GoString()))
		return
	}
	_, _ = f.Write([]byte(s.String()))
}

// MarshalJSON is the redaction as a JSON string, or null when s is not
// present, so a JSON form says whether a unit has a serial and nothing
// more.
func (s Serial) MarshalJSON() ([]byte, error) {
	if s.v == nil {
		return []byte("null"), nil
	}
	return []byte(`"` + redacted + `"`), nil
}

// LogValue is the redaction, for log/slog.
func (s Serial) LogValue() slog.Value { return slog.StringValue(s.String()) }

// IsZero reports whether s is not present, which is what yaml.v3's
// omitempty asks.
func (s Serial) IsZero() bool { return s.v == nil }

// MarshalYAML writes the serial itself: YAML is the config file's
// encoding, the one place the serial is kept.
func (s Serial) MarshalYAML() (any, error) {
	if s.v == nil {
		return nil, nil
	}
	return *s.v, nil
}

// errNotAString is what a serial that is not a YAML string is refused
// with. It never quotes what was there.
var errNotAString = errors.New("a serial is a string")

// UnmarshalYAML reads the serial MarshalYAML wrote, through New.
func (s *Serial) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errNotAString
	}
	var raw string
	if err := n.Decode(&raw); err != nil {
		return errNotAString
	}
	*s = New(raw)
	return nil
}
