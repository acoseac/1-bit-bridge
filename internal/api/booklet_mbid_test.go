package api

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestIsValidBookletMBIDRejectsTraversal pins F29 (2026-07-20 review).
//
// BookletPath puts `mbid` in the LEADING position of a filepath.Join, and
// the writer's atomicwrite.WriteBytes runs os.MkdirAll on the parent — so a
// traversing value CREATES its own directories rather than failing. The GET
// handler validated; the write path did not, and two docblocks asserted a
// validation that was never implemented anywhere in the harvest chain.
func TestIsValidBookletMBIDRejectsTraversal(t *testing.T) {
	bad := []string{
		"../../etc/cron.d/evil",
		"..",
		"../sibling",
		"a/b",
		`..\..\windows`,
		"",
		"local-" + strings.Repeat("a", 64),
		"not-a-uuid",
		// Right length and alphabet but wrong shape.
		"0000000000000000000000000000000000000",
		// Trailing newline — a classic anchor-bypass probe.
		"11111111-2222-3333-4444-555555555555\n../evil",
	}
	for _, s := range bad {
		if IsValidBookletMBID(s) {
			t.Errorf("IsValidBookletMBID(%q) = true, want false", s)
		}
	}

	good := []string{
		"11111111-2222-3333-4444-555555555555",
		"AbCdEf01-2345-6789-aBcD-0123456789Ef",
	}
	for _, s := range good {
		if !IsValidBookletMBID(s) {
			t.Errorf("IsValidBookletMBID(%q) = false, want true", s)
		}
	}
}

// TestBookletPathStaysUnderDirForValidMBIDs is the structural companion: for
// every value the validator accepts, the resulting path must stay inside the
// cache dir. This is what makes validate-then-join sufficient.
func TestBookletPathStaysUnderDirForValidMBIDs(t *testing.T) {
	dir := t.TempDir()
	for _, s := range []string{
		"11111111-2222-3333-4444-555555555555",
		"AbCdEf01-2345-6789-aBcD-0123456789Ef",
	} {
		got := BookletPath(dir, s)
		if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
			t.Errorf("BookletPath(%q) = %q, escaped %q", s, got, dir)
		}
		if filepath.Dir(got) != dir {
			t.Errorf("BookletPath(%q) landed in a subdirectory: %q", s, got)
		}
	}
}
