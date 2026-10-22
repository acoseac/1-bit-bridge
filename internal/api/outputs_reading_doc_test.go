package api

import (
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

// TestEveryReadingCodeIsInTheProtocol: every code a reading's state or its
// verdict can carry, every warning's code and source, the verdict's values
// and its bit-exactness are words the app maps to its own copy, so
// PROTOCOL.md's GET /v1/outputs/{id}/reading section names each of them, in
// backticks.
func TestEveryReadingCodeIsInTheProtocol(t *testing.T) {
	raw, err := os.ReadFile("../../PROTOCOL.md")
	if err != nil {
		t.Fatal(err)
	}
	spec := strings.ReplaceAll(string(raw), "\r\n", "\n")
	const heading = "### `GET /v1/outputs/{id}/reading`"
	start := strings.Index(spec, heading)
	if start < 0 {
		t.Fatalf("PROTOCOL.md has no %s section", heading)
	}
	section := spec[start+len(heading):]
	if end := strings.Index(section, "\n### "); end >= 0 {
		section = section[:end]
	}
	words := append(append(signalpath.Codes(), signalpath.WarningCodes()...), signalpath.WarningSources()...)
	words = append(words, signalpath.FormatPreserved, signalpath.FormatPreservedDoP, signalpath.Withheld, signalpath.NotVerifiable)
	for _, word := range words {
		if !strings.Contains(section, "`"+word+"`") {
			t.Errorf("the reading's section of PROTOCOL.md does not name %s", word)
		}
	}
	for _, gone := range []string{"`bit_exact`", "`bit_exact_dop`"} {
		if strings.Contains(section, gone) {
			t.Errorf("the reading's section of PROTOCOL.md still names %s, which no MPD output's verdict is", gone)
		}
	}
}
