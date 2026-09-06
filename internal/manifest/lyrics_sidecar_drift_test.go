package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

// statFor builds the TrackStat the skip gate sees for a file whose extraction
// produced ex — the same fields GetTrackStat reads back from the LEFT JOIN.
func statFor(t *testing.T, abs string, ex *extractedLyrics) *TrackStat {
	t.Helper()
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	st := &TrackStat{
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
		ExtractorVersion: ExtractorVersion,
	}
	if ex != nil {
		st.LyricsSource = ex.Source
		st.LyricsSidecarName = ex.SidecarName
		st.LyricsSourceMTimeNS = ex.SourceMTimeNS
		st.LyricsSourceSize = ex.SourceSize
	}
	return st
}

// mp3WithEmbeddedLyrics writes an MP3 carrying a plain USLT — an embedded
// source (`text`, rank 5) that any real sidecar could in principle outrank.
func mp3WithEmbeddedLyrics(t *testing.T, dir, base string) string {
	t.Helper()
	p := filepath.Join(dir, base)
	writeMP3WithFrames(t, p, []id3Frame{usltFrame("", "Embedded first line\nEmbedded second line")})
	return p
}

// TestSidecarDriftConvergesOnAWorthlessSidecar is the regression for the skip
// gate's non-convergent arm.
//
// A sidecar that CANNOT win the pick must not report drift, because the gate
// runs on every scan and its answer never changes: the audio file is re-opened
// and re-parsed forever, invisibly (the re-extract lands on reExtractUnchanged
// → versionStampOnly, so there is not even indexed_at churn to notice). The
// old arm compared the sidecar's EXTENSION rank against the stored source's,
// which says ".lrc beats embedded text" about files that yield no document at
// all, or that resolve to sidecar-txt (rank 6) and lose.
func TestSidecarDriftConvergesOnAWorthlessSidecar(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sidecar string
		body    []byte
	}{
		{"empty .lrc", "song.lrc", nil},
		{"whitespace-only .lrc", "song.lrc", []byte("   \n\t\n  ")},
		{"tagless .lrc demotes to sidecar-txt", "song.lrc", []byte("Just words, no time tags\nAnother line")},
		{"legacy-encoded .lrc", "song.lrc", []byte{0x41, 0xB8, 0xE9, 0xFF, 0xFE}},
		{"oversized .lrc", "song.lrc", bytes.Repeat([]byte("x"), lyrics.MaxBodyBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			abs := mp3WithEmbeddedLyrics(t, dir, "song.mp3")
			if err := os.WriteFile(filepath.Join(dir, tc.sidecar), tc.body, 0o644); err != nil {
				t.Fatal(err)
			}

			ex := extractLyrics(t, abs)
			if ex == nil {
				t.Fatal("fixture broken: no lyrics resolved at all")
			}
			if ex.SidecarName != "" {
				t.Fatalf("fixture broken: the sidecar won the pick (source=%q) — "+
					"this case is about a sidecar that LOSES", ex.Source)
			}

			// Fresh context: a later scan has its own memoized listing.
			st := statFor(t, abs, ex)
			ec := &ExtractContext{SidecarIndex: new(sync.Map)}
			if sidecarLyricsDrifted(abs, st, ec) {
				t.Errorf("skip gate reports drift for a sidecar that cannot win "+
					"(stored source %q): this file re-extracts on every scan, forever", ex.Source)
			}
		})
	}
}

// TestSidecarDriftStillFiresWhenTheSidecarWouldWin is the other half: the
// convergence fix must not blind the gate. A real timed .lrc appearing beside
// a track whose stored row came from an embedded tag has to re-extract, or the
// user's explicit override never takes effect.
func TestSidecarDriftStillFiresWhenTheSidecarWouldWin(t *testing.T) {
	dir := t.TempDir()
	abs := mp3WithEmbeddedLyrics(t, dir, "song.mp3")
	ex := extractLyrics(t, abs)
	if ex == nil || ex.SidecarName != "" {
		t.Fatalf("fixture broken: want an embedded-sourced row, got %+v", ex)
	}
	st := statFor(t, abs, ex)

	// The user drops a real .lrc in. The audio file has not changed.
	if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte(lrcBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sidecarLyricsDrifted(abs, st, &ExtractContext{SidecarIndex: new(sync.Map)}) {
		t.Fatal("skip gate missed a winning sidecar: the user's .lrc would never be picked up")
	}

	// And it converges: once re-extracted, the .lrc is the stored source and
	// the gate goes quiet again.
	ex2 := extractLyrics(t, abs)
	if ex2 == nil || ex2.SidecarName != "song.lrc" {
		t.Fatalf("re-extraction did not adopt the sidecar: %+v", ex2)
	}
	if sidecarLyricsDrifted(abs, statFor(t, abs, ex2), &ExtractContext{SidecarIndex: new(sync.Map)}) {
		t.Error("skip gate still reports drift after adopting the sidecar — not converged")
	}
}

// TestWorthlessSidecarThenEditedStillReExtracts pins that the fast-path read is
// not a cache: a sidecar that yielded nothing yesterday and carries real timed
// lyrics today must still re-extract.
func TestWorthlessSidecarThenEditedStillReExtracts(t *testing.T) {
	dir := t.TempDir()
	abs := mp3WithEmbeddedLyrics(t, dir, "song.mp3")
	lrc := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(lrc, []byte("no tags here"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := extractLyrics(t, abs)
	st := statFor(t, abs, ex)
	if sidecarLyricsDrifted(abs, st, &ExtractContext{SidecarIndex: new(sync.Map)}) {
		t.Fatal("fixture broken: the tagless sidecar should not drift")
	}
	if err := os.WriteFile(lrc, []byte(lrcBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sidecarLyricsDrifted(abs, st, &ExtractContext{SidecarIndex: new(sync.Map)}) {
		t.Error("an edited sidecar that now wins the pick did not re-extract")
	}
}

// TestOversizedSYLTFrameIsRefusedBeforeParsing pins the input bound on an
// untrusted SYLT body.
//
// The fixture is the shape that makes the guard OBSERVABLE rather than merely
// prudent: an entry whose text is a bare newline marker is a line break, so it
// renders to nothing at all while still costing 6 raw bytes, one []SYLTEntry
// and a sort slot. A frame padded with those and closed by two real entries
// therefore parses and renders to a perfectly ordinary ~40-byte document —
// Normalize's trailing size check never sees anything to reject — while having
// cost megabytes of entries and an O(n log n) sort to get there, per scan
// worker. Only an input bound catches it. dhowden applies no cap of its own:
// readBytes streams anything past readBytesMaxUpfront through io.CopyN.
func TestOversizedSYLTFrameIsRefusedBeforeParsing(t *testing.T) {
	body := []byte{0, 'e', 'n', 'g', 2, 1, 0} // enc 0, "eng", ms, lyrics, no descriptor
	pad := []byte{'\n', 0, 0, 0, 0, 0}        // a line break at 0 ms: renders nothing
	for len(body) <= 2*lyrics.MaxBodyBytes {
		body = append(body, pad...)
	}
	entries := len(body) / len(pad)
	body = append(body, 'H', 'i', 0, 0, 0, 0x03, 0xE8)                // "Hi"    @ 1000 ms
	body = append(body, 'T', 'h', 'e', 'r', 'e', 0, 0, 0, 0x07, 0xD0) // "There" @ 2000 ms

	if _, ok := syltCandidate(body); ok {
		t.Errorf("a %d-byte SYLT frame (%d entries) was parsed, sorted and rendered; "+
			"the bound is %d bytes", len(body), entries, 2*lyrics.MaxBodyBytes)
	}

	// The guard must not be a blanket refusal: the same frame, unpadded, is a
	// valid document and has to survive.
	small := []byte{0, 'e', 'n', 'g', 2, 1, 0}
	small = append(small, 'H', 'i', 0, 0, 0, 0x03, 0xE8)
	small = append(small, 'T', 'h', 'e', 'r', 'e', 0, 0, 0, 0x07, 0xD0)
	c, ok := syltCandidate(small)
	if !ok {
		t.Fatal("a small, well-formed SYLT frame was refused")
	}
	if c.Doc.Body == "" || !c.Doc.Synced {
		t.Errorf("small frame produced an unusable document: %+v", c.Doc)
	}
}
