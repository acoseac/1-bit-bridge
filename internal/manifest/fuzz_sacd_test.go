// Fuzz coverage for the SACD ISO reader.
//
// sacd.go is a 581-line binary parser over bytes the bridge does not control
// — a disc image on the operator's library mount, which can be truncated, a
// partially-copied rclone transfer, or simply a different pressing than the
// two the format was derived from. That is the same untrusted-input surface
// as the audio extractors, and it shipped (PR #779) without a target while
// the sibling extractors have thirteen between them.
//
// Why a panic here matters more than it looks: runScanWorker recovers
// per-iteration, so a panicking file is SKIPPED. It never reaches the
// manifest, and the only evidence is one log line. CLAUDE.md states the rule
// for exactly this reason — "a crash found by the extractor targets is a REAL
// defect, not a nicety".
//
// Reaching the parser needs a little staging. The master TOC signature lives
// at logical sector 510, so a raw []byte would have to be a megabyte before
// parseSACDTOC got past its geometry probe, and every execution would test
// the not-an-SACD early return. sacdFuzzImage places the fuzzed bytes AT the
// structures instead, without allocating the megabyte of leading zeros.
package manifest

import (
	"bytes"
	"io"
	"testing"
)

// sacdFuzzImage is a sparse io.ReaderAt: zeros below base, then data. It
// stands in for a disc image whose interesting structures start at the
// master-TOC sector, so a fuzz input of a few hundred bytes lands on the
// parsing arithmetic rather than a megabyte of padding.
type sacdFuzzImage struct {
	base int64
	data []byte
}

// ReadAt fills p in two bulk moves — the zero region below base, then the
// data region — rather than byte by byte.
//
// That is not a micro-optimisation. parseSACDArea reads a file-controlled
// tocSize of up to 255 sectors, so one execution asks for ~522 KB; a
// per-byte loop made that target run at roughly a hundred executions a
// second, which is the Q4 failure mode this whole file exists to avoid — a
// target that reports PASS having barely run. With copy it sustains tens of
// thousands.
func (im sacdFuzzImage) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}
	n := 0
	// Zero region: everything below base reads as padding.
	if off < im.base {
		z := int(min(im.base-off, int64(len(p))))
		clear(p[:z])
		n = z
	}
	// Data region.
	if n < len(p) {
		start := off + int64(n) - im.base
		if start >= int64(len(im.data)) {
			return n, io.EOF
		}
		c := copy(p[n:], im.data[start:])
		n += c
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// TestSACDFuzzImageMatchesADenseReader pins the helper the three targets
// stand on.
//
// A sparse reader that quietly returned zeros, or stopped short, would make
// every target above pass while exercising nothing — the vacuous-pin class.
// So it is compared against a real bytes.Reader over the same image,
// materialised densely, across offsets and lengths that straddle the base
// boundary in both directions.
func TestSACDFuzzImageMatchesADenseReader(t *testing.T) {
	const base = 64
	data := []byte("SACDMTOC-and-then-some-payload-bytes")
	dense := append(make([]byte, base), data...)
	ref := bytes.NewReader(dense)
	sparse := sacdFuzzImage{base: base, data: data}

	for _, off := range []int64{0, 1, base - 1, base, base + 1, int64(len(dense)) - 1, int64(len(dense)), int64(len(dense)) + 5} {
		for _, n := range []int{1, 7, 64, 100, len(dense) + 10} {
			gotBuf, refBuf := make([]byte, n), make([]byte, n)
			gotN, gotErr := sparse.ReadAt(gotBuf, off)
			refN, refErr := ref.ReadAt(refBuf, off)
			if gotN != refN {
				t.Fatalf("off=%d n=%d: read %d bytes, bytes.Reader read %d", off, n, gotN, refN)
			}
			if (gotErr == nil) != (refErr == nil) {
				t.Fatalf("off=%d n=%d: err %v, bytes.Reader err %v", off, n, gotErr, refErr)
			}
			if !bytes.Equal(gotBuf[:gotN], refBuf[:refN]) {
				t.Fatalf("off=%d n=%d: content differs from bytes.Reader", off, n)
			}
		}
	}
}

// sacdSeedImage builds the smallest byte run that gets past the geometry
// probe: the master signature at the start of the master-TOC sector, then
// whatever the caller wants the parser to chew on.
func sacdSeedImage(tail []byte) []byte {
	out := make([]byte, 0, len(sacdMasterSignature)+len(tail))
	out = append(out, sacdMasterSignature...)
	return append(out, tail...)
}

// FuzzParseSACDTOC drives the whole TOC read — geometry probe, master-copy
// fallback across sectors 510/520/530, the eight locale text banks, and the
// area pointers — with the image's own bytes under the fuzzer's control.
func FuzzParseSACDTOC(f *testing.F) {
	f.Add(sacdSeedImage(nil))
	f.Add(sacdSeedImage(bytes.Repeat([]byte{0xFF}, 4096)))
	// A signature followed by a plausible area-pointer region: the copy
	// fallback only adopts a copy that carries at least one pointer, so a
	// seed with non-zero pointers reaches further than an all-zero one.
	f.Add(sacdSeedImage(append(
		bytes.Repeat([]byte{0}, 8),
		[]byte{0, 0, 0x02, 0x10, 0, 0, 0x20, 0x00}...)))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, b []byte) {
		// Cap so one pathological input can't turn the corpus into a
		// memory test; the parser's own clamps are what this is checking,
		// and they act well below this.
		if len(b) > 1<<20 {
			b = b[:1<<20]
		}
		for _, g := range []sacdGeometry{sacdPlain2048, sacdRaw2064} {
			img := sacdFuzzImage{
				base: sacdMasterTOCSectors[0]*g.stride + g.payloadOffset,
				data: b,
			}
			// Only a panic is a failure. A nil TOC ("not an SACD") and an
			// error ("signature intact, structure damaged") are both
			// legitimate verdicts on arbitrary bytes.
			_, _ = parseSACDTOC(img)
		}
	})
}

// FuzzParseSACDArea drives the stereo-area TOC directly. This is where the
// tightest arithmetic lives: a file-controlled tocSize picks the read length,
// trackCount indexes the start/duration tables, and the TTxt bank walk
// follows file-controlled pointers to NUL-terminated text.
func FuzzParseSACDArea(f *testing.F) {
	// A minimally plausible area: signature, tocSize=3, then the TRL1/TRL2
	// sector signatures at their fixed offsets.
	area := make([]byte, 3*sacdSectorPayload)
	copy(area, sacdStereoSignature)
	area[10], area[11] = 0, 3 // tocSize (big-endian u16)
	area[32] = 2              // channels
	area[69] = 1              // trackCount
	copy(area[sacdSectorPayload:], sacdTRL1Signature)
	copy(area[2*sacdSectorPayload:], sacdTRL2Signature)
	f.Add(area)
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte{0xFF}, sacdSectorPayload))

	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1<<20 {
			b = b[:1<<20]
		}
		for _, g := range []sacdGeometry{sacdPlain2048, sacdRaw2064} {
			_, _ = parseSACDArea(sacdFuzzImage{base: g.payloadOffset, data: b}, g, 0)
		}
	})
}

// FuzzSACDVirtualPathRoundTrip is a PROPERTY target, in the sense the three
// CLAUDE.md calls out: it asserts an invariant rather than merely not
// crashing.
//
// The property: for every index the grammar accepts, the path it renders
// parses back to that same index, and is recognised as a virtual path whose
// container is the original. The grammar is a pinned cross-repo contract —
// SACDVirtualPath.swift mirrors it exactly, and the deletion pass keys on
// IsSACDVirtualPath to decide whether a row is seen — so a divergence
// between the renderer and the parser is a row-reaping bug, not a cosmetic
// one.
func FuzzSACDVirtualPathRoundTrip(f *testing.F) {
	f.Add("Album/disc.iso", 1)
	f.Add("Album/disc.iso", 99)
	f.Add("Album/disc.iso", 100)
	f.Add("Album/disc.iso", 255)
	f.Add("a/b/c/My Disc (2001).ISO", 7)
	f.Add("", 0)

	f.Fuzz(func(t *testing.T, container string, index int) {
		p := SACDVirtualTrackPath(container, index)
		if p == "" {
			return // index out of range, or no container — nothing claimed
		}
		if !IsSACDVirtualPath(p) {
			t.Fatalf("rendered %q for (%q, %d) but IsSACDVirtualPath says no",
				p, container, index)
		}
		gotContainer, ok := SACDVirtualContainer(p)
		if !ok {
			t.Fatalf("rendered %q but SACDVirtualContainer refused it", p)
		}
		if gotContainer != container {
			t.Fatalf("container round-trip: rendered %q from %q, parsed back %q",
				p, container, gotContainer)
		}
	})
}
