//go:build windows

package fsutil

import "testing"

// TestStripVerbatimPrefixGivesAnOrdinaryPath: GetFinalPathNameByHandle
// answers with the `\\?\` prefix, which ResolveLinks takes off so the answer
// compares with the drive-letter and UNC spellings everything else uses. A
// volume GUID path, or anything else unexpected, is an error, and
// resolveLinks then gives EvalSymlinks's answer instead.
func TestStripVerbatimPrefixGivesAnOrdinaryPath(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{`\\?\C:\Users\x\Music`, `C:\Users\x\Music`, true},
		{`\\?\C:\`, `C:\`, true},
		{`\\?\UNC\nas\share\music`, `\\nas\share\music`, true},
		{`\\?\Volume{0b2cd1a1-0000-0000-0000-100000000000}\music`, "", false},
		{`C:\Users\x\Music`, "", false},
	} {
		got, err := stripVerbatimPrefix(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("stripVerbatimPrefix(%s) = %q, %v; want %q, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}
