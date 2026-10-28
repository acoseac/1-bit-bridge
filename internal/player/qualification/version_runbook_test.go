package qualification

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

func TestQualificationRunbookVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "ops", "b470-native-qualification.md"))
	if err != nil {
		t.Fatal(err)
	}
	// These are current instructions; historical round/version records are not
	// assertions about the candidate an owner must qualify now.
	for _, tc := range []struct{ name, pattern string }{
		{"header", `QualificationVersion is \*\*([0-9]+)\*\*`},
		{"current", `current version is \*\*([0-9]+)\*\*`},
		{"schema", `QualificationVersion=([0-9]+)`},
		{"invalidation", `Version ([0-9]+) invalidates older evidence`},
		{"rerun", `qualification with version ([0-9]+)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			matches := regexp.MustCompile(tc.pattern).FindAllSubmatch(b, -1)
			if len(matches) != 1 || string(matches[0][1]) != strconv.Itoa(Version) {
				t.Fatalf("runbook %s = %q; want one reference to version %d", tc.name, matches, Version)
			}
		})
	}
}
