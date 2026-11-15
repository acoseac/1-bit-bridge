package usbid

import (
	"log/slog"
	"strings"
	"testing"
)

// B882L: Serial.HeldIn, the check that finds a serial in a text in any of its
// spellings, and what it leaves unchanged.

// heldSerial is a serial with a K and an S, the two letters whose
// unicode.SimpleFold orbits hold a third rune: the Kelvin sign (U+212A) and
// the long s (U+017F).
const heldSerial = "SKU-7Q2K-4471"

// kelvinSign and longS are those two runes, as a text from the box may spell
// a serial's K and S with them: unicode.ToUpper leaves the Kelvin sign as it
// is, and unicode.ToLower the long s, so only the whole orbit folds both.
var kelvinSign, longS = string(rune(0x212a)), string(rune(0x017f))

// TestHeldInFindsTheSerialInAnySpelling: the serial as it is, in another case,
// with its K and S spelled by the other runes of their orbits, and with
// anything but letters and digits between its letters and digits, inside a
// longer text, is held.
func TestHeldInFindsTheSerialInAnySpelling(t *testing.T) {
	s := New(heldSerial)
	for _, text := range []string{
		heldSerial,
		"Model " + heldSerial + " Rev B",
		"model sku-7q2k-4471",
		"Sku-7Q2k-4471",
		longS + kelvinSign + "U-7Q2" + kelvinSign + "-4471",
		"s" + kelvinSign + "u7q2k4471",
		"SKU 7Q2K 4471",
		"SKU7Q2K4471",
		"S.K.U/7Q2K_4471",
		"s-k-u-7-q-2-k-4-4-7-1",
		"(SKU) [7Q2K] {4471}",
		"SKU\t7Q2K\u00a04471",
		"XSKU7Q2K4471Y",
	} {
		if !s.HeldIn(text) {
			t.Errorf("HeldIn(%q) is false: the serial is there, in another spelling", text)
		}
	}
}

// TestHeldInDoesNotFindWhatTheTextLacks: a text holding another serial, a part
// of this one, its groups in another order, or a letter of it changed, does
// not hold it, and an absent serial is held in nothing.
func TestHeldInDoesNotFindWhatTheTextLacks(t *testing.T) {
	s := New(heldSerial)
	for _, text := range []string{
		"",
		"GO bar",
		"Model ABC-1234",
		"SKU-7Q2K",
		"7Q2K-4471",
		"SKU 4471",
		"4471-7Q2K-SKU",
		"7Q2K-SKU-4471",
		"SKU-7Q2K-4472",
		"SKU-7Q2X-4471",
		"[redacted]",
	} {
		if s.HeldIn(text) {
			t.Errorf("HeldIn(%q) is true: the text does not hold the serial", text)
		}
	}
	for _, text := range []string{"", "anything", "[none]", heldSerial, "----"} {
		if (Serial{}).HeldIn(text) || New(" \x00\n").HeldIn(text) {
			t.Errorf("an absent serial is held in %q", text)
		}
	}
}

// TestASerialWithNoLetterOrDigitIsHeldInEveryText: such a serial leaves
// nothing to compare, so HeldIn answers true whatever the text, the empty one
// included, and a caller that drops what holds it drops every text (HeldIn's
// doc).
func TestASerialWithNoLetterOrDigitIsHeldInEveryText(t *testing.T) {
	for _, raw := range []string{"----", "#*+/", "\u2013\u2014", "\xff\xfe", ".\x01."} {
		s := New(raw)
		if !s.Present() {
			t.Fatalf("%q is no serial", raw)
		}
		for _, text := range []string{"", "GO bar", "Model ABC-1234", raw} {
			if !s.HeldIn(text) {
				t.Errorf("a serial of %q is not held in %q: with no letter or digit to compare it must be", raw, text)
			}
		}
	}
}

// TestHeldInLeavesTheOtherMethodsAsTheyWere pins what Redact, Label, String,
// MarshalJSON and LogValue answer for a set of serials, with the values main
// gave before HeldIn was added: Redact still replaces the serial's exact
// bytes alone, a copy in another case or with other marks left for HeldIn to
// find.
func TestHeldInLeavesTheOtherMethodsAsTheyWere(t *testing.T) {
	const text = "GO bar GOBAR-7Q2X-91KZ-4471 gobar-7q2x-91kz-4471 GOBAR 7Q2X 91KZ 4471 ---- 1234"
	for _, tc := range []struct {
		serial                             string
		redact, label, str, json, logValue string
	}{
		{testSerial, "GO bar [redacted] gobar-7q2x-91kz-4471 GOBAR 7Q2X 91KZ 4471 ---- 1234", "…4471", "[redacted]", `"[redacted]"`, "[redacted]"},
		{"1234", "GO bar GOBAR-7Q2X-91KZ-4471 gobar-7q2x-91kz-4471 GOBAR 7Q2X 91KZ 4471 ---- [redacted]", "serial set", "[redacted]", `"[redacted]"`, "[redacted]"},
		{"----", "GO bar GOBAR-7Q2X-91KZ-4471 gobar-7q2x-91kz-4471 GOBAR 7Q2X 91KZ 4471 [redacted] 1234", "serial set", "[redacted]", `"[redacted]"`, "[redacted]"},
		{"7Q2X", "GO bar GOBAR-[redacted]-91KZ-4471 gobar-7q2x-91kz-4471 GOBAR [redacted] 91KZ 4471 ---- 1234", "serial set", "[redacted]", `"[redacted]"`, "[redacted]"},
		{"ABCDEFGHIJKL", text, "…IJKL", "[redacted]", `"[redacted]"`, "[redacted]"},
		{"シリアル番号は一二三四五六七八", text, "…五六七八", "[redacted]", `"[redacted]"`, "[redacted]"},
		{"", text, "no serial", "[none]", "null", "[none]"},
	} {
		s := New(tc.serial)
		if got := s.Redact(text); got != tc.redact {
			t.Errorf("Redact of %q = %q, want %q", tc.serial, got, tc.redact)
		}
		if got := s.Label(); got != tc.label {
			t.Errorf("Label of %q = %q, want %q", tc.serial, got, tc.label)
		}
		if got := s.String(); got != tc.str {
			t.Errorf("String of %q = %q, want %q", tc.serial, got, tc.str)
		}
		b, err := s.MarshalJSON()
		if err != nil || string(b) != tc.json {
			t.Errorf("MarshalJSON of %q = %s, %v; want %s", tc.serial, b, err, tc.json)
		}
		if v := s.LogValue(); v.Kind() != slog.KindString || v.String() != tc.logValue {
			t.Errorf("LogValue of %q = %s %q, want the string %q", tc.serial, v.Kind(), v.String(), tc.logValue)
		}
		if strings.Contains(tc.str+tc.json+tc.logValue, tc.serial) && tc.serial != "" {
			t.Errorf("a form of %q holds it", tc.serial)
		}
	}
}
