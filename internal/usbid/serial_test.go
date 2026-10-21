package usbid

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// testSerial is a serial long enough to have a label, with a tail that is
// easy to find in an output that leaked it.
const testSerial = "GOBAR-7Q2X-91KZ-4471"

// leaks reports whether out carries the serial or any eight bytes of it
// in a row: a cut-down copy is a leak too.
func leaks(out string) bool {
	for i := 0; i+8 <= len(testSerial); i++ {
		if strings.Contains(out, testSerial[i:i+8]) {
			return true
		}
	}
	return false
}

// withExported and withUnexported hold a serial the way a DTO or an
// internal struct would: fmt can call Format on the first, and reaches
// the second only by reflection.
type withExported struct {
	Name   string
	Serial Serial
}

type withUnexported struct {
	name   string
	serial Serial
}

// TestSerialNeverPrintsItsValue walks every way Go prints or encodes a
// value: fmt with each verb, on the value, a pointer, a struct field
// fmt can reach and one it cannot, a slice and a map; encoding/json; and
// slog's text and JSON handlers. None may carry the serial.
func TestSerialNeverPrintsItsValue(t *testing.T) {
	s := New(testSerial)
	if !s.Present() {
		t.Fatal("the serial is not present")
	}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%20s", "%-20v", "%T"} {
		outs = append(outs,
			fmt.Sprintf(verb, s),
			fmt.Sprintf(verb, &s),
			fmt.Sprintf(verb, withExported{"a", s}),
			fmt.Sprintf(verb, &withExported{"a", s}),
			fmt.Sprintf(verb, withUnexported{"a", s}),
			fmt.Sprintf(verb, &withUnexported{"a", s}),
			fmt.Sprintf(verb, []Serial{s}),
			fmt.Sprintf(verb, map[string]Serial{"k": s}),
			fmt.Sprintf(verb, []withUnexported{{"a", s}}),
		)
	}
	outs = append(outs, s.String(), s.GoString(), fmt.Sprint(s), fmt.Sprintln(s), errors.New(fmt.Sprint(s)).Error(),
		fmt.Errorf("wrapped: %w", fmt.Errorf("serial %v", s)).Error())
	for _, v := range []any{s, &s, withExported{"a", s}, map[string]Serial{"k": s}, []Serial{s}, withUnexported{"a", s}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%T): %v", v, err)
		}
		outs = append(outs, string(b))
	}
	var text, js bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&text, nil), slog.NewJSONHandler(&js, nil)} {
		l := slog.New(h)
		l.Info("bound", "serial", s, "unit", withExported{"a", s}, "hidden", withUnexported{"a", s}, "ptr", &s)
		l.With("serial", s).Warn("again", slog.Any("list", []Serial{s}), slog.Group("g", "serial", s))
	}
	outs = append(outs, text.String(), js.String())
	for i, out := range outs {
		if leaks(out) {
			t.Errorf("output %d carries the serial: %q", i, out)
		}
	}
	if !strings.Contains(text.String(), "serial=[redacted]") || !strings.Contains(js.String(), `"serial":"[redacted]"`) {
		t.Errorf("the handlers did not print the redaction:\n%s\n%s", text.String(), js.String())
	}
}

// TestSerialJSONSaysOnlyWhetherOneIsSet: a present serial encodes as the
// redaction and an absent one as null, so a JSON form can tell a unit
// with a serial from one without and nothing more.
func TestSerialJSONSaysOnlyWhetherOneIsSet(t *testing.T) {
	b, err := json.Marshal(struct {
		Set, Unset Serial
	}{New(testSerial), Serial{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"Set":"[redacted]","Unset":null}` {
		t.Errorf("json = %s", got)
	}
}

// TestSerialRoundTripsThroughYAML: YAML is the config file's encoding,
// the one place the serial is written whole, and it reads back equal,
// bytes that are not UTF-8 and characters YAML must quote included.
func TestSerialRoundTripsThroughYAML(t *testing.T) {
	type file struct {
		Serial Serial `yaml:"serial,omitempty"`
		Other  string `yaml:"other"`
	}
	for _, raw := range []string{testSerial, "0000000000000000", "a: b # c", "- 12", "'quoted\"", "tab\there", "\xff\xfe-not-utf8", "日本語のシリアル"} {
		b, err := yaml.Marshal(file{Serial: New(raw), Other: "x"})
		if err != nil {
			t.Fatalf("%q: marshal: %v", raw, err)
		}
		var back file
		if err := yaml.Unmarshal(b, &back); err != nil {
			t.Fatalf("%q: unmarshal %s: %v", raw, b, err)
		}
		if !back.Serial.Equal(New(raw)) {
			t.Errorf("%q came back different from %s", raw, b)
		}
	}
	b, err := yaml.Marshal(file{Other: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "serial") {
		t.Errorf("an absent serial was written: %s", b)
	}
	var back file
	if err := yaml.Unmarshal([]byte("serial: [a, b]\nother: x\n"), &back); err == nil {
		t.Error("a serial that is a list was read")
	} else if strings.Contains(err.Error(), "a, b") {
		t.Errorf("the refusal quotes the value: %v", err)
	}
}

// TestSerialComparesAfterTrimmingItsTail: a serial is compared byte for
// byte once trailing NULs and whitespace are trimmed, which is how a
// descriptor padded with them reads; a leading space is the serial's own.
// An empty serial is no serial, and never matches, not even another empty
// one.
func TestSerialComparesAfterTrimmingItsTail(t *testing.T) {
	base := New("ABC123")
	for _, same := range []string{"ABC123", "ABC123\x00", "ABC123\x00\x00\x00", "ABC123 ", "ABC123\n", "ABC123 \x00\t\r\n"} {
		if !New(same).Equal(base) {
			t.Errorf("%q does not match ABC123", same)
		}
	}
	for _, other := range []string{" ABC123", "abc123", "ABC1234", "ABC12", "ABC\x00123"} {
		if New(other).Equal(base) {
			t.Errorf("%q matches ABC123", other)
		}
	}
	for _, empty := range []string{"", "\x00", "\x00\x00", " ", "\n", " \x00\n"} {
		s := New(empty)
		if s.Present() {
			t.Errorf("%q is a serial", empty)
		}
		if s.Equal(New(empty)) || s.Equal(Serial{}) || (Serial{}).Equal(Serial{}) {
			t.Errorf("an empty serial %q matched", empty)
		}
	}
}

// TestSerialLabel: the last four characters of a serial of twelve or
// more, and a fixed label otherwise, so a short serial is never printed
// whole; a tail that does not print is not shown either.
func TestSerialLabel(t *testing.T) {
	for raw, want := range map[string]string{
		testSerial:             "…4471",
		"ABCDEFGHIJKL":         "…IJKL",
		"ABCDEFGHIJK":          "serial set",
		"1234":                 "serial set",
		"ABCDEFGHIJ\x1b[2J":    "serial set",
		"ABCDEFGHIJKLMN​": "serial set",
		"シリアル番号は一二三四五六七八": "…五六七八",
		"":                     "no serial",
	} {
		if got := New(raw).Label(); got != want {
			t.Errorf("Label(%q) = %q, want %q", raw, got, want)
		}
	}
}
