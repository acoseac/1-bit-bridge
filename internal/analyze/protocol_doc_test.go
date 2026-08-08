package analyze

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestProtocolDocFormatVersionsMatchTheCode pins PROTOCOL.md's two binary
// header tables against the constants that actually produce those bytes.
//
// **This exists because the doc drifted from the code and nothing caught it.**
// A blind first-match edit bumped the `1BWF` table's version instead of
// `1BSP`'s, leaving BOTH wrong: waveform documented as 2 while emitting 1, and
// spectrum documented as 1 while emitting 2. Every test passed, the gate was
// green, and a client implementing from the spec would have rejected valid
// waveform sidecars and mis-parsed every spectrum blob. Two review bots caught
// it from opposite ends; nothing in the repo would have.
//
// The doc is the wire contract for a second codebase, so a stale version
// number in it is a real defect, not a typo.
func TestProtocolDocFormatVersionsMatchTheCode(t *testing.T) {
	raw, err := os.ReadFile("../../PROTOCOL.md")
	if err != nil {
		t.Skipf("PROTOCOL.md unreadable (%v) — skipping rather than failing a "+
			"consumer of this package built outside the repo", err)
	}
	doc := string(raw)

	for _, c := range []struct {
		magic string
		want  int
	}{
		{"1BWF", waveformFormatVersion},
		{"1BSP", SpectrumSchemaVersion},
	} {
		// Find the table introduced by this magic, then the version row
		// inside it — the two tables are otherwise identically shaped.
		idx := strings.Index(doc, "magic `\""+c.magic+"\"`")
		if idx < 0 {
			idx = strings.Index(doc, "magic `"+c.magic+"`")
		}
		if idx < 0 {
			t.Errorf("%s: no header table found in PROTOCOL.md", c.magic)
			continue
		}
		window := doc[idx:min(idx+400, len(doc))]
		m := regexp.MustCompile(`format version \(` + "`" + `(\d+)` + "`" + `\)`).FindStringSubmatch(window)
		if m == nil {
			t.Errorf("%s: no format-version row in its header table", c.magic)
			continue
		}
		if got := m[1]; got != fmt.Sprint(c.want) {
			t.Errorf("PROTOCOL.md documents %s format version %s, but the code emits %d — "+
				"a client implementing from the spec would reject or mis-parse these bytes",
				c.magic, got, c.want)
		}
	}
}
