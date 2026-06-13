package transcode

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sha8Marker matches the `~<8 hex chars>.` segment the disambiguation
// path inserts when sanitization touched the basename or the candidate
// exceeded the FS cap. The exact hex isn't asserted (covered by the
// dedicated collision-pair test); structural presence is what callers
// of safeVariantFilename actually depend on.
var sha8Marker = regexp.MustCompile(`~[0-9a-f]{8}\.`)

// TestSafeVariantFilenameDefault covers the normal-case shape:
// `<srcBase>.<variantID>.flac`.
func TestSafeVariantFilenameDefault(t *testing.T) {
	got := safeVariantFilename("01 Love Letters.flac", "upscaled-v2-176400-24")
	want := "01 Love Letters.flac.upscaled-v2-176400-24.flac"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSafeVariantFilenameSanitisesFATIllegalChars covers the
// FAT-family character substitutions. Each illegal byte maps to `_`
// deterministically, AND the output carries the `~<sha8>` disambiguation
// suffix so two raw inputs that sanitize identically stay distinct on
// disk (see TestSafeVariantFilenameDistinguishesSanitizationCollisions
// for the collision-pair contract).
func TestSafeVariantFilenameSanitisesFATIllegalChars(t *testing.T) {
	cases := []struct {
		name            string
		srcBase         string
		wantSanitizedIn string // substring of the sanitized form
	}{
		{"colon", "Bach: BWV 1006.flac", "Bach_ BWV 1006.flac"},
		{"asterisk", "Track*.flac", "Track_.flac"},
		{"question", "Track?.flac", "Track_.flac"},
		{"quote", `Foo"Bar.flac`, "Foo_Bar.flac"},
		{"lt", "Foo<Bar.flac", "Foo_Bar.flac"},
		{"gt", "Foo>Bar.flac", "Foo_Bar.flac"},
		{"pipe", "Foo|Bar.flac", "Foo_Bar.flac"},
		{"backslash", `Foo\Bar.flac`, "Foo_Bar.flac"},
		{"all-illegal", `:*?"<>|\.flac`, "________.flac"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := safeVariantFilename(c.srcBase, "upscaled-v2-176400-24")
			if !strings.Contains(got, c.wantSanitizedIn) {
				t.Errorf("missing sanitized portion %q in %q", c.wantSanitizedIn, got)
			}
			if !sha8Marker.MatchString(got) {
				t.Errorf("missing ~<sha8>. disambiguation marker in %q", got)
			}
			if !strings.HasSuffix(got, ".upscaled-v2-176400-24.flac") {
				t.Errorf("missing variantID + .flac suffix in %q", got)
			}
		})
	}
}

// TestSafeVariantFilenameDistinguishesSanitizationCollisions is the
// load-bearing collision-pair contract. Two raw basenames that differ
// ONLY in FAT-illegal characters MUST produce distinct output
// filenames — otherwise `os.Rename` silently overwrites the earlier
// sidecar with the later one inside the source-mirrored tree. The
// SHA8 must be computed over the RAW (pre-sanitization) bytes; pre-fix
// the hash was over the SANITIZED form and these two inputs collided
// at both the short-path branch AND the over-length branch.
func TestSafeVariantFilenameDistinguishesSanitizationCollisions(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"colon-vs-asterisk", "Disc 1:Track A.flac", "Disc 1*Track A.flac"},
		{"colon-vs-question", "Movement: I.flac", "Movement? I.flac"},
		{"lt-vs-gt", "Foo<Bar.flac", "Foo>Bar.flac"},
		{"pipe-vs-backslash", "X|Y.flac", `X\Y.flac`},
		// Three-way: every illegal char that maps to `_` would
		// otherwise produce the same output.
		{"colon-vs-lt", "A:B.flac", "A<B.flac"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotA := safeVariantFilename(c.a, "upscaled-v2-176400-24")
			gotB := safeVariantFilename(c.b, "upscaled-v2-176400-24")
			if gotA == gotB {
				t.Errorf("collision: %q and %q both produced %q",
					c.a, c.b, gotA)
			}
		})
	}
}

// TestSafeVariantFilenameCleanNameSkipsSuffix confirms the no-op fast
// path: a basename that doesn't trip sanitization AND fits the FS cap
// returns the plain `<srcBase>.<variantID>.flac` shape with NO `~<sha8>`
// suffix. Critical for the source-mirror property — clean names stay
// human-readable.
func TestSafeVariantFilenameCleanNameSkipsSuffix(t *testing.T) {
	got := safeVariantFilename("01 Love Letters.flac", "upscaled-v2-176400-24")
	want := "01 Love Letters.flac.upscaled-v2-176400-24.flac"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if sha8Marker.MatchString(got) {
		t.Errorf("clean name should NOT carry the ~<sha8> marker: %q", got)
	}
}

// TestSafeVariantFilenameNFCvsNFD pins the Unicode-normalisation
// invariant: two strings that render the same glyph but differ in
// composition form (NFC `é` U+00E9 vs NFD `e` + combining acute U+0065
// U+0301) MUST produce distinct output filenames. Pre-fix this was
// implicitly guaranteed because sanitiseForFAT touched only ASCII bytes;
// a future refactor that adds Unicode normalisation INSIDE
// sanitiseForFAT (collapsing both to NFC) would re-introduce a silent
// collision. This test will fail under such a refactor.
//
// Note: today both inputs are "clean" (no FAT-illegal chars), so they
// take the short-path branch. The contract this test enforces is
// "distinct raw bytes → distinct output filenames" — even if a future
// sanitiseForFAT normalises the visible form, the raw-byte SHA8 path
// MUST kick in for either of the normalised inputs to stay distinct
// from the other.
func TestSafeVariantFilenameNFCvsNFD(t *testing.T) {
	nfc := "Café.flac"  // "Café.flac" — 2-byte é
	nfd := "Café.flac" // "Café.flac" — 'e' + combining acute, 3 bytes total for the glyph
	if nfc == nfd {
		t.Fatal("test fixtures are byte-identical; check the string literals")
	}
	gotNFC := safeVariantFilename(nfc, "upscaled-v2-176400-24")
	gotNFD := safeVariantFilename(nfd, "upscaled-v2-176400-24")
	if gotNFC == gotNFD {
		t.Errorf("NFC vs NFD collision: %q and %q both produced %q",
			nfc, nfd, gotNFC)
	}
}

// TestSafeVariantFilenameDeterministic confirms the substitution
// is stable across runs — the DB lookup contract requires it
// (same source must produce same filename so `track_variants.sidecar_path`
// stays coherent across restarts).
func TestSafeVariantFilenameDeterministic(t *testing.T) {
	a := safeVariantFilename("Tr:ack.flac", "upscaled-v2-176400-24")
	b := safeVariantFilename("Tr:ack.flac", "upscaled-v2-176400-24")
	if a != b {
		t.Errorf("non-deterministic: %q != %q", a, b)
	}
}

// TestSafeVariantFilenameOverLength validates the middle-truncate +
// SHA8 path for basenames that would push the total over the 255-byte
// filesystem cap (ext4, NTFS, exFAT). The output:
//   - stays ≤ 255 bytes;
//   - ends in ".flac";
//   - retains the variantID for collision separation;
//   - retains the SHA8 of the original for uniqueness.
func TestSafeVariantFilenameOverLength(t *testing.T) {
	long := strings.Repeat("x", 300) + ".flac"
	got := safeVariantFilename(long, "upscaled-v2-176400-24")
	if len(got) > 255 {
		t.Errorf("filename length %d exceeds 255-byte cap: %q", len(got), got)
	}
	if !strings.HasSuffix(got, ".flac") {
		t.Errorf("want .flac suffix, got %q", got)
	}
	if !strings.Contains(got, "upscaled-v2-176400-24") {
		t.Errorf("want variantID in filename, got %q", got)
	}
	// The middle-truncate marker — "~<sha8>." — must be present so
	// re-runs with the same input produce the same filename.
	if !strings.Contains(got, "~") {
		t.Errorf("over-length filename should carry the ~<sha8> marker: %q", got)
	}
}

// TestSafeVariantFilenameReservesTmpSuffix pins the temp-file budget:
// the output basename plus sidecarTmpSuffix must fit the 255-byte
// filesystem cap, because sox writes to `<sidecar>.tmp` before the
// atomic rename. Pre-fix the budget was a bare 255, so an input that
// produced a 255-byte name made the temp target 259 bytes —
// ENAMETOOLONG on every common filesystem, exactly for the
// long-classical-filename inputs the sanitizer exists to handle.
func TestSafeVariantFilenameReservesTmpSuffix(t *testing.T) {
	const variantID = "upscaled-v2-176400-24"
	cases := []struct {
		name    string
		srcBase string
	}{
		// Clean name sized so the candidate landed at exactly 255
		// bytes under the pre-fix budget: len(srcBase) + len(".") +
		// len(variantID) + len(".flac") = 228 + 1 + 21 + 5 = 255.
		{"exact-pre-fix-cap", strings.Repeat("x", 223) + ".flac"},
		{"well-over-cap", strings.Repeat("x", 300) + ".flac"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := safeVariantFilename(c.srcBase, variantID)
			if len(got)+len(sidecarTmpSuffix) > 255 {
				t.Errorf("len(%q) = %d; +%q overflows the 255-byte basename cap",
					got, len(got), sidecarTmpSuffix)
			}
		})
	}
}

// TestSafeVariantFilenameOverLengthDeterministic confirms two
// identical over-length inputs produce the same output (SHA8 of the
// original basename is deterministic).
func TestSafeVariantFilenameOverLengthDeterministic(t *testing.T) {
	long := strings.Repeat("x", 300) + ".flac"
	a := safeVariantFilename(long, "upscaled-v2-176400-24")
	b := safeVariantFilename(long, "upscaled-v2-176400-24")
	if a != b {
		t.Errorf("non-deterministic over-length output: %q != %q", a, b)
	}
}

// TestSafeVariantFilenameOverLengthUTF8Safe pins the rune-boundary
// invariant. Byte-level slicing in middle-truncate could land
// between bytes of a multi-byte rune ("Dvořák" mid-truncated at
// the wrong byte position would corrupt the `ř`). The truncate
// helpers must scan rune boundaries.
//
// Gemini HIGH on PR D1 caught this — pre-fix the byte-slice form
// (s[:half], s[len(s)-half:]) was UTF-8-unsafe.
func TestSafeVariantFilenameOverLengthUTF8Safe(t *testing.T) {
	// Build a long string with embedded multi-byte runes so the
	// middle-truncate has to cross rune boundaries.
	long := strings.Repeat("Dvořák", 60) + ".flac" // ~440 bytes
	got := safeVariantFilename(long, "upscaled-v2-176400-24")
	if len(got) > 255 {
		t.Errorf("filename length %d exceeds 255-byte cap: %q", len(got), got)
	}
	// The output MUST be valid UTF-8 — no half-encoded runes leaking
	// through the truncation.
	if !isValidUTF8(got) {
		t.Errorf("filename has invalid UTF-8 (mid-rune truncation): %q", got)
	}
}

// isValidUTF8 is a test helper that returns true iff s is entirely
// valid UTF-8. Uses `utf8.ValidString` via inline implementation to
// avoid adding a top-level import for one test case.
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD && len(s) > 0 {
			// 0xFFFD is the Unicode replacement char that `for range`
			// emits for invalid sequences — flag it iff the source
			// didn't have a literal U+FFFD.
			// Cheap heuristic: if any rune is 0xFFFD, treat as invalid
			// for the purposes of this test (none of the fixtures
			// contain a real replacement char).
			return false
		}
	}
	return true
}

// TestSidecarPathSourceMirroredLayout pins the v1.4 layout: variant
// sidecars land at <OutputDir>/<libRel-dir>/<filename> so a user
// can `mv variantsDir/* library/` and have the variants slot
// alongside source files.
func TestSidecarPathSourceMirroredLayout(t *testing.T) {
	spec := JobSpec{
		SourceLibraryRel: "Diana Krall/The Look of Love/01 Love Letters.flac",
		OutputDir:        "/tmp/variants",
		TargetSampleRate: 176400,
		TargetBits:       24,
	}
	got := spec.SidecarPath()
	want := filepath.Join(
		"/tmp/variants",
		"Diana Krall",
		"The Look of Love",
		"01 Love Letters.flac.upscaled-v2-176400-24.flac",
	)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSidecarPathRootLevelSource covers the edge case where the
// source is at the library root (no intermediate dirs). Result must
// land directly in OutputDir without a `.` segment.
func TestSidecarPathRootLevelSource(t *testing.T) {
	spec := JobSpec{
		SourceLibraryRel: "RootTrack.flac",
		OutputDir:        "/tmp/variants",
		TargetSampleRate: 192000,
		TargetBits:       24,
	}
	got := spec.SidecarPath()
	want := filepath.Join("/tmp/variants", "RootTrack.flac.upscaled-v2-192000-24.flac")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestSidecarPathPreservesOriginalExtension confirms the basename
// keeps its extension in front of the variantID — the load-bearing
// collision-safety property. `Track1.flac` and `Track1.wav` MUST
// land at distinct sidecar paths even at the same variant target.
func TestSidecarPathPreservesOriginalExtension(t *testing.T) {
	flac := JobSpec{
		SourceLibraryRel: "Album/Track1.flac",
		OutputDir:        "/tmp/variants",
		TargetSampleRate: 176400,
		TargetBits:       24,
	}
	wav := JobSpec{
		SourceLibraryRel: "Album/Track1.wav",
		OutputDir:        "/tmp/variants",
		TargetSampleRate: 176400,
		TargetBits:       24,
	}
	if flac.SidecarPath() == wav.SidecarPath() {
		t.Errorf("same-stem different-ext sources collided: %q",
			flac.SidecarPath())
	}
}

// The sanitiseForFAT alloc-guard test moved to internal/fsutil alongside
// the function (TestSanitiseForFATCleanInputZeroAlloc in
// internal/fsutil/safename_test.go). safeVariantFilename's behaviour
// (which composes the shared helper) is still covered by the cases above.
