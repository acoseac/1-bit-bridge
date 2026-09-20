package integrity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// relocatedRow builds the 2026-09-20 shape for one track: a row whose
// recorded sidecar_path sits under oldDir (a directory that does not
// exist on this host) and whose file sits at its canonical place under
// newDir with exactly `size` bytes. Returns the row and the canonical
// path the probe must find.
func relocatedRow(t *testing.T, oldDir, newDir, source, variantID string, size int) (VariantSnapshot, string) {
	t.Helper()
	canonical := transcode.VariantSidecarPath(newDir, source, variantID)
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return VariantSnapshot{
		SourcePath:  source,
		VariantID:   variantID,
		SidecarPath: transcode.VariantSidecarPath(oldDir, source, variantID),
		SizeBytes:   int64(size),
	}, canonical
}

// TestLocateSidecarVerdicts pins the five verdicts against the disk,
// including the two that the field report showed have to be told apart:
// a file at the canonical place with the recorded size (relocated —
// adopt) and one with a different size (a copy in flight — keep, do not
// adopt, do not delete).
func TestLocateSidecarVerdicts(t *testing.T) {
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants") // never created
	newDir := t.TempDir()

	t.Run("recorded path present", func(t *testing.T) {
		p := filepath.Join(newDir, "present.flac")
		if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
		loc := LocateSidecar(newDir, VariantSnapshot{SourcePath: "A/1.flac", VariantID: "upscaled-v2-96000-24", SidecarPath: p, SizeBytes: 2})
		if loc.Verdict != SidecarPresent {
			t.Fatalf("verdict = %v, want present", loc.Verdict)
		}
	})
	t.Run("relocated with the recorded size", func(t *testing.T) {
		row, canonical := relocatedRow(t, oldDir, newDir, "Artist/Album/01.flac", "upscaled-v2-176400-24", 1234)
		loc := LocateSidecar(newDir, row)
		if loc.Verdict != SidecarRelocated || loc.Canonical != canonical {
			t.Fatalf("got %+v, want relocated at %s", loc, canonical)
		}
	})
	t.Run("canonical file with a different size is mismatched, never relocated", func(t *testing.T) {
		row, canonical := relocatedRow(t, oldDir, newDir, "Artist/Album/02.flac", "upscaled-v2-176400-24", 1234)
		row.SizeBytes = 999_999 // the row describes a bigger file: the copy has not finished
		loc := LocateSidecar(newDir, row)
		if loc.Verdict != SidecarMismatched || loc.Canonical != canonical {
			t.Fatalf("got %+v, want mismatched at %s", loc, canonical)
		}
	})
	t.Run("missing at both locations", func(t *testing.T) {
		row := VariantSnapshot{
			SourcePath: "Artist/Album/03.flac", VariantID: "upscaled-v2-176400-24",
			SidecarPath: transcode.VariantSidecarPath(oldDir, "Artist/Album/03.flac", "upscaled-v2-176400-24"),
			SizeBytes:   10,
		}
		loc := LocateSidecar(newDir, row)
		if loc.Verdict != SidecarMissing {
			t.Fatalf("got %+v, want missing", loc)
		}
		if loc.Canonical == "" {
			t.Errorf("a missing verdict after a probe should still report where it looked")
		}
	})
	t.Run("recorded path IS the canonical path — nowhere else to look", func(t *testing.T) {
		row := VariantSnapshot{
			SourcePath: "Artist/Album/04.flac", VariantID: "upscaled-v2-176400-24",
			SidecarPath: transcode.VariantSidecarPath(newDir, "Artist/Album/04.flac", "upscaled-v2-176400-24"),
			SizeBytes:   10,
		}
		loc := LocateSidecar(newDir, row)
		if loc.Verdict != SidecarMissing || loc.Canonical != "" {
			t.Fatalf("got %+v, want missing with no second location", loc)
		}
	})
	t.Run("no variants dir means no probe (legacy unconditional sweep)", func(t *testing.T) {
		row, _ := relocatedRow(t, oldDir, newDir, "Artist/Album/05.flac", "upscaled-v2-176400-24", 7)
		loc := LocateSidecar("", row)
		if loc.Verdict != SidecarMissing || loc.Canonical != "" {
			t.Fatalf("got %+v, want missing with no probe", loc)
		}
	})
	t.Run("a directory at the canonical path is not a sidecar", func(t *testing.T) {
		row := VariantSnapshot{
			SourcePath: "Artist/Album/06.flac", VariantID: "upscaled-v2-176400-24",
			SidecarPath: transcode.VariantSidecarPath(oldDir, "Artist/Album/06.flac", "upscaled-v2-176400-24"),
			SizeBytes:   0,
		}
		if err := os.MkdirAll(transcode.VariantSidecarPath(newDir, row.SourcePath, row.VariantID), 0o755); err != nil {
			t.Fatal(err)
		}
		if loc := LocateSidecar(newDir, row); loc.Verdict != SidecarMismatched {
			t.Fatalf("got %+v, want mismatched (a directory, whatever its size, is not the file)", loc)
		}
	})
	if runtime.GOOS != "windows" {
		t.Run("a stat failure that is not ENOENT is unknown, never missing", func(t *testing.T) {
			locked := filepath.Join(t.TempDir(), "locked")
			if err := os.MkdirAll(locked, 0o000); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			if os.Geteuid() == 0 {
				t.Skip("root ignores directory modes")
			}
			row := VariantSnapshot{SourcePath: "A/1.flac", VariantID: "upscaled-v2-96000-24",
				SidecarPath: filepath.Join(locked, "x.flac"), SizeBytes: 1}
			if loc := LocateSidecar(newDir, row); loc.Verdict != SidecarUnknown || loc.Err == nil {
				t.Fatalf("got %+v, want unknown with the error", loc)
			}
		})
	}
}

// TestCanonicalSidecarPathIsTheWriterLayout pins the equivalence the
// whole relocation design rests on: the path the probe looks at is the
// path the pool writes (JobSpec.SidecarPath) — for every job kind, for
// a FAT-illegal name and for an over-long one. A layout change that
// reached the writer and not this probe would silently turn every
// relocation back into a deletion.
func TestCanonicalSidecarPathIsTheWriterLayout(t *testing.T) {
	const dir = "/srv/variants"
	specs := []transcode.JobSpec{
		{OutputDir: dir, SourceLibraryRel: "Artist/Album/Song.flac", TargetSampleRate: 192000, TargetBits: 24},
		{OutputDir: dir, SourceLibraryRel: `Artist/Album/Track: A? "Live".flac`, TargetSampleRate: 96000, TargetBits: 24},
		{OutputDir: dir, SourceLibraryRel: "Artist/Album/" + strings.Repeat("x", 300) + ".flac", TargetSampleRate: 44100, TargetBits: 16, Kind: transcode.JobKindOptimize},
		{OutputDir: dir, SourceLibraryRel: "root/Artist/Disc 1/01.dsf", TargetSampleRate: 176400, TargetBits: 24, Kind: transcode.JobKindPCMRender},
		{OutputDir: dir, SourceLibraryRel: "Song.dsf", TargetSampleRate: 44100, TargetBits: 16, Kind: transcode.JobKindOptimize, SourceIsDSD: true},
	}
	for _, spec := range specs {
		row := VariantSnapshot{SourcePath: spec.SourceLibraryRel, VariantID: spec.VariantID()}
		if got, want := CanonicalSidecarPath(dir, row), spec.SidecarPath(); got != want {
			t.Errorf("%q/%s: probe looks at %q, writer lands at %q", spec.SourceLibraryRel, row.VariantID, got, want)
		}
	}
	if got := CanonicalSidecarPath(dir, VariantSnapshot{SidecarPath: "/old/x.flac"}); got != "" {
		t.Errorf("a row with no source identity has no canonical path, got %q", got)
	}
}

// TestTreeHoldsVariantSidecars pins the relocation guard's evidence:
// sidecars in either on-disk layout count, junk `.flac` files and
// anything under a dot-directory do not, and an unreadable tree is an
// error the caller fails closed on rather than a "no".
func TestTreeHoldsVariantSidecars(t *testing.T) {
	write := func(t *testing.T, dir, rel string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		files []string
		want  bool
	}{
		{"empty tree", nil, false},
		{"junk flac only", []string{"decoy.flac", "Song.optimized-Mix.flac"}, false},
		{"source-mirrored sidecar", []string{"Artist/Album/01.flac.upscaled-v2-176400-24.flac"}, true},
		{"legacy hash-flat sidecar", []string{"abc123-upscaled-v2-176400-24.flac"}, true},
		{"dsd rendition", []string{"Artist/01.dsf.pcm-v1-176400-24.flac"}, true},
		{"hashed disambiguation form", []string{"Artist/Long..name~a1b2c3d4.optimized-v1-44100-16.flac"}, true},
		{"sidecars only inside .Trashes do not count", []string{".Trashes/501/01.flac.upscaled-v2-176400-24.flac"}, false},
		{"a dot-dir at the root is still pruned, a dot-file is a file", []string{".hidden/x.upscaled-v1-96000-24.flac", "a.flac"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				write(t, dir, f)
			}
			got, err := TreeHoldsVariantSidecars(dir)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Errorf("TreeHoldsVariantSidecars = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("missing dir is an error, not a no", func(t *testing.T) {
		if _, err := TreeHoldsVariantSidecars(filepath.Join(t.TempDir(), "gone")); err == nil {
			t.Fatal("want an error for a directory that cannot be read")
		}
	})
	t.Run("empty path is refused", func(t *testing.T) {
		if _, err := TreeHoldsVariantSidecars(""); err == nil {
			t.Fatal("want an error for an empty directory path — never walk the working directory")
		}
	})
}

// TestMassDeleteRefusal pins the guard's three conditions and both
// escape hatches (100 disables, the floor).
func TestMassDeleteRefusal(t *testing.T) {
	full := t.TempDir()
	if err := os.WriteFile(filepath.Join(full, "01.flac.upscaled-v2-176400-24.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	junk := t.TempDir()
	if err := os.WriteFile(filepath.Join(junk, "decoy.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name           string
		dir            string
		missing, total int
		percent        int
		wantRefuse     bool
	}{
		{"the incident: everything missing, tree full", full, 10248, 10248, 20, true},
		{"over the threshold with sidecars present", full, 21, 100, 20, true},
		{"exactly at the threshold proceeds", full, 20, 100, 20, false},
		{"under the threshold proceeds", full, 19, 100, 20, false},
		{"over the threshold but the tree holds no sidecars", junk, 100, 100, 20, false},
		{"below the floor is never refused", full, 9, 9, 20, false},
		{"at the floor is refused", full, 10, 10, 20, true},
		{"100 disables the guard", full, 10248, 10248, 100, false},
		{"0 refuses any mass deletion while sidecars remain", full, 10, 10000, 0, true},
		{"nothing missing", full, 0, 100, 20, false},
		{"unreadable tree fails closed", filepath.Join(t.TempDir(), "gone"), 50, 100, 20, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := MassDeleteRefusal(tc.dir, tc.missing, tc.total, tc.percent)
			if (reason != "") != tc.wantRefuse {
				t.Fatalf("refusal = %q, want refuse=%v", reason, tc.wantRefuse)
			}
			if tc.wantRefuse && !strings.Contains(reason, "of") {
				t.Errorf("reason should carry the counts: %q", reason)
			}
		})
	}
}

// TestKnownSidecarSetCarriesBothSpellings pins the forward sweeps'
// contract: a relocated row's file under the walked tree is KNOWN,
// so neither the background orphan sweep nor `bridge upscale --gc`
// can unlink a moved catalog; a row with no source identity
// contributes only its recorded path; the keys are case-folded.
func TestKnownSidecarSetCarriesBothSpellings(t *testing.T) {
	rows := []VariantSnapshot{
		{SourcePath: "Artist/Album/01.flac", VariantID: "upscaled-v2-176400-24", SidecarPath: "/mnt/Old/Artist/Album/01.flac.upscaled-v2-176400-24.flac"},
		{SidecarPath: "/mnt/old/legacy-abc.flac"},
	}
	known := KnownSidecarSet("/srv/new", rows)
	for _, want := range []string{
		"/mnt/old/artist/album/01.flac.upscaled-v2-176400-24.flac",
		strings.ToLower(transcode.VariantSidecarPath("/srv/new", "Artist/Album/01.flac", "upscaled-v2-176400-24")),
		"/mnt/old/legacy-abc.flac",
	} {
		if _, ok := known[want]; !ok {
			t.Errorf("known set lacks %q; have %v", want, known)
		}
	}
	if len(known) != 3 {
		t.Errorf("known set has %d entries, want 3 (two spellings for the identified row, one for the bare path)", len(known))
	}
}
