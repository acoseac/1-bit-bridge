package transcode

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestSoxArgsTmpPathIsUniquePerCall pins the mechanism behind F2: two jobs
// holding the SAME JobSpec must not derive the same temp path.
//
// They legitimately can hold the same spec. `DropInflight` frees a dedup key
// for a job that is still RUNNING — DELETE /v1/upscale/variants calls it
// precisely so a re-submit isn't coalesced against a worker about to write the
// sidecar the caller means to delete — and the delete also removes the
// track_variants row, so finalizeAndEnqueue's LookupVariant no longer refuses.
// With the default worker count (min(NumCPU-1, 4) >= 2 on any real host) job B
// then starts while job A's sox is still writing.
//
// The FINAL path must stay a pure function of the spec (it is the published
// variant, and track_variants.sidecar_path has to keep matching it across
// restarts) — only the temp path carries the token.
func TestSoxArgsTmpPathIsUniquePerCall(t *testing.T) {
	j := JobSpec{
		SourceAbsPath:    "/lib/Music/Album/01.flac",
		SourceLibraryRel: "Music/Album/01.flac",
		TargetSampleRate: 192000,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        "/tmp/transcoded",
	}

	const n = 64
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		args, _, finalPath, tmpPath := j.SoxArgs()
		if finalPath != j.SidecarPath() {
			t.Fatalf("call %d: finalPath = %q, want the deterministic SidecarPath %q "+
				"(the published variant path must NOT carry the temp token)",
				i, finalPath, j.SidecarPath())
		}
		if seen[tmpPath] {
			t.Fatalf("call %d: tmpPath %q repeated — two workers holding this spec "+
				"concurrently would share one temp file, and RunSox opens every job by "+
				"os.Remove-ing it (the later starter unlinks the earlier's in-progress "+
				"output, then the earlier renames the later's partial file into place)",
				i, tmpPath)
		}
		seen[tmpPath] = true
		// The temp path must still be the exact sox output argument — RunSox
		// renames what sox wrote, with no re-derivation of its own.
		if args[6] != tmpPath {
			t.Fatalf("call %d: sox output arg %q != returned tmpPath %q", i, args[6], tmpPath)
		}
		if !strings.HasPrefix(tmpPath, finalPath+".") || !strings.HasSuffix(tmpPath, sidecarTmpSuffix) {
			t.Fatalf("call %d: tmpPath %q is not %q + .<token> + %q",
				i, tmpPath, finalPath, sidecarTmpSuffix)
		}
	}
}

// TestSidecarTmpTokenIsFixedWidth pins the assumption sidecarTmpReserve (and
// therefore fsBasenameCap) is a compile-time constant on: the token is always
// exactly sidecarTmpTokenHexLen hex digits, so the temp basename always adds
// exactly sidecarTmpReserve bytes. A variable-width token (e.g. %x of a uint32,
// which drops leading zeros) would silently under-reserve the 255-byte budget.
func TestSidecarTmpTokenIsFixedWidth(t *testing.T) {
	for i := 0; i < 512; i++ {
		tok := nextSidecarTmpToken()
		if len(tok) != sidecarTmpTokenHexLen {
			t.Fatalf("token %q has len %d, want exactly %d", tok, len(tok), sidecarTmpTokenHexLen)
		}
		for _, r := range tok {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("token %q carries non-hex rune %q", tok, r)
			}
		}
	}
	if want := 1 + sidecarTmpTokenHexLen + len(sidecarTmpSuffix); sidecarTmpReserve != want {
		t.Fatalf("sidecarTmpReserve = %d, want %d — the reserve must cover "+
			`"." + token + sidecarTmpSuffix`, sidecarTmpReserve, want)
	}
}

// TestSoxArgsTmpBasenameFitsFilesystemCap is the end-to-end budget pin: a
// pathological source basename must still produce a TEMP basename within the
// 255-byte ext4 / NTFS / exFAT cap, because that is the name sox is actually
// told to create. safeVariantFilename's reserve is what buys the headroom; this
// asserts it against the real SoxArgs output rather than the helper in
// isolation, so a future change to the temp shape that forgets to grow
// sidecarTmpReserve fails here.
func TestSoxArgsTmpBasenameFitsFilesystemCap(t *testing.T) {
	cases := []string{
		strings.Repeat("x", 400) + ".flac",
		strings.Repeat("ř", 200) + ".flac",                // multi-byte: 400 bytes of runes
		strings.Repeat("y", 223) + ".flac",                // lands at exactly 255 with no reserve
		"Bach: BWV " + strings.Repeat("z", 300) + ".flac", // FAT-sanitised AND over-cap
	}
	for _, base := range cases {
		j := JobSpec{
			SourceAbsPath:    "/lib/" + base,
			SourceLibraryRel: "Music/Album/" + base,
			TargetSampleRate: 192000,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        "/tmp/transcoded",
		}
		_, _, finalPath, tmpPath := j.SoxArgs()
		if n := len(filepath.Base(tmpPath)); n > 255 {
			t.Errorf("temp basename is %d bytes (> 255) for source %q…: %q",
				n, base[:20], filepath.Base(tmpPath))
		}
		if n := len(filepath.Base(finalPath)); n > 255 {
			t.Errorf("final basename is %d bytes (> 255) for source %q…: %q",
				n, base[:20], filepath.Base(finalPath))
		}
	}
}
