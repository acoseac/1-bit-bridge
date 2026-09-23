package atlasharvest

import "regexp"

// mbidValidPattern matches a MusicBrainz UUID.
//
// A THIRD copy of this anchored pattern, deliberately, on the precedent
// the other two already set: api.mbidPattern and enrich.mbidValidPattern
// are separate because the dependency direction forbids sharing, and each
// says so. The same applies here — this package's imports are fsutil,
// logging, atomicwrite and lyrics; enrich pulls in manifest, so importing
// it to reach one regexp would invert a dependency edge for a constant.
//
// TestHarvestMBIDPatternMatchesEnrich (cmd/bridge, the one package that
// imports both) pins the two against each other so three copies cannot
// drift.
var mbidValidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// isValidMBID reports whether s is a well-formed MusicBrainz UUID.
func isValidMBID(s string) bool { return mbidValidPattern.MatchString(s) }

// IsValidMBID is isValidMBID exported so the cross-package agreement
// guard can compare this copy's BEHAVIOUR against enrich's over a shared
// table — which is the property that matters, rather than the regexp
// source text the two happen to spell the same way today.
func IsValidMBID(s string) bool { return isValidMBID(s) }
