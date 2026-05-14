package transcode

import (
	"path/filepath"
	"strings"
	"testing"
)

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
// FAT-family character substitutions. Single-character cases assert
// each illegal byte maps to `_` deterministically.
func TestSafeVariantFilenameSanitisesFATIllegalChars(t *testing.T) {
	cases := []struct {
		name    string
		srcBase string
		want    string
	}{
		{"colon", "Bach: BWV 1006.flac", "Bach_ BWV 1006.flac.upscaled-v2-176400-24.flac"},
		{"asterisk", "Track*.flac", "Track_.flac.upscaled-v2-176400-24.flac"},
		{"question", "Track?.flac", "Track_.flac.upscaled-v2-176400-24.flac"},
		{"quote", `Foo"Bar.flac`, "Foo_Bar.flac.upscaled-v2-176400-24.flac"},
		{"lt", "Foo<Bar.flac", "Foo_Bar.flac.upscaled-v2-176400-24.flac"},
		{"gt", "Foo>Bar.flac", "Foo_Bar.flac.upscaled-v2-176400-24.flac"},
		{"pipe", "Foo|Bar.flac", "Foo_Bar.flac.upscaled-v2-176400-24.flac"},
		{"backslash", `Foo\Bar.flac`, "Foo_Bar.flac.upscaled-v2-176400-24.flac"},
		{"all-illegal", `:*?"<>|\.flac`, "________.flac.upscaled-v2-176400-24.flac"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := safeVariantFilename(c.srcBase, "upscaled-v2-176400-24")
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
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
