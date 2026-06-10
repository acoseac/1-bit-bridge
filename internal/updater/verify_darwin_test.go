//go:build darwin

package updater

import (
	"errors"
	"testing"
)

// TestNotarizationFlagUnsupported_PinsClassification locks the
// fallback gate: ONLY a codesign complaint about the
// --check-notarization option itself may route to the --strict-only
// fallback. Anything else (real signature failures, notarization
// rejections, unrelated option errors) must surface directly — an
// over-broad match would re-open the notarization-bypass Gemini
// security-high flagged on PR #374.
func TestNotarizationFlagUnsupported_PinsClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"flag rejected (invalid option)",
			errors.New(`codesign [--verify --strict --check-notarization /x]: codesign: invalid option --check-notarization: exit status 2`), true},
		{"flag rejected (unrecognized option)",
			errors.New(`codesign: unrecognized option '--check-notarization'`), true},
		{"flag rejected (unknown option, uppercase)",
			errors.New(`CODESIGN: UNKNOWN OPTION --CHECK-NOTARIZATION`), true},
		{"real signature failure",
			errors.New(`codesign [...]: /x: invalid signature (code or signature have been modified): exit status 1`), false},
		{"not notarized verdict (must NOT fall back)",
			errors.New(`codesign [...]: /x: code has no notarization ticket. --check-notarization failed`), false},
		{"unrelated invalid option without the anchor",
			errors.New(`codesign: invalid option --frobnicate`), false},
	}
	for _, c := range cases {
		if got := notarizationFlagUnsupported(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v (err=%v)", c.name, got, c.want, c.err)
		}
	}
}
