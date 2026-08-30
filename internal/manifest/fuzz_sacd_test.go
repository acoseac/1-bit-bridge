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
	"path"
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

// sacdSeedMasterUnit builds a master TOC unit that actually SURVIVES
// parseSACDTOC's validation, rather than one that merely starts with the
// right eight bytes.
//
// The distinction matters, and the first version of this file got it
// wrong: the area pointers live at offsets 64/68 and 72/76 of the master
// sector, and a candidate is adopted only when `start > 0 && end > start`.
// A seed whose pointer bytes sit anywhere else is rejected at
// `len(cand) == 0` and reaches exactly as far as an all-zero buffer — so
// it looked like a deeper seed and contributed nothing.
func sacdSeedMasterUnit() []byte {
	unit := make([]byte, 2*sacdSectorPayload)
	copy(unit, sacdMasterSignature)
	putU16 := func(off int, v uint16) { unit[off], unit[off+1] = byte(v>>8), byte(v) }
	putU32 := func(off int, v uint32) {
		unit[off], unit[off+1] = byte(v>>24), byte(v>>16)
		unit[off+2], unit[off+3] = byte(v>>8), byte(v)
	}
	putU16(18, 1)     // album sequence
	putU32(64, 0x10)  // area 1 start
	putU32(68, 0x200) // area 1 end — start > 0 && end > start, so adopted
	putU16(120, 2001) // year
	// Second sector: the first locale text bank, so the album-title and
	// artist pointer slots get walked too.
	bank := unit[sacdSectorPayload:]
	copy(bank, []byte("SACDText"))
	bank[16], bank[17] = 0, 64 // album-title slot -> offset 64
	copy(bank[64:], []byte("Seed Album\x00"))
	return unit
}

// sacdSeedArea builds a stereo area TOC that reaches the track table and
// the TTxt bank walk — the deepest arithmetic in the file.
//
// Every gate on the way there has to be satisfied, and the first version
// of this seed cleared none past the third: channels == 2, then
// 1 <= trackCount <= 255, then `trackAreaEnd > trackAreaStart` — both were
// left zero, so 0 <= 0 returned early and the track table was never
// reached at all. Past that: the TRL1/TRL2 sector signatures, each track's
// start LSN falling inside [trackAreaStart, trackAreaEnd), and start and
// duration timecodes whose seconds < 60 and frames < 75 with a non-zero
// duration.
func sacdSeedArea() []byte {
	const tocSectors = 4 // the 3-sector minimum + one TTxt sector to walk
	d := make([]byte, tocSectors*sacdSectorPayload)
	putU16 := func(off int, v uint16) { d[off], d[off+1] = byte(v>>8), byte(v) }
	putU32 := func(off int, v uint32) {
		d[off], d[off+1] = byte(v>>24), byte(v>>16)
		d[off+2], d[off+3] = byte(v>>8), byte(v)
	}
	copy(d, sacdStereoSignature)
	putU16(10, tocSectors) // tocSize
	d[32] = 2              // channels — stereo, or parseSACDArea refuses
	d[68] = 0              // trackOffset
	d[69] = 1              // trackCount
	putU32(72, 0x10)       // trackAreaStart
	putU32(76, 0x200)      // trackAreaEnd — must exceed start

	trl1 := sacdSectorPayload
	trl2 := 2 * sacdSectorPayload
	copy(d[trl1:], sacdTRL1Signature)
	copy(d[trl2:], sacdTRL2Signature)
	putU32(trl1+8, 0x20) // track 1 start LSN, inside [start, end)
	// Start 00:00:00 and a three-minute duration, both read as
	// (minutes, seconds, frames) and validated at 75 fps.
	d[trl2+8], d[trl2+9], d[trl2+10] = 0, 0, 0
	d[trl2+8+1020], d[trl2+8+1021], d[trl2+8+1022] = 3, 0, 0

	// Fourth sector: a TTxt bank with one item, so the pointer-following
	// text walk runs instead of being skipped for want of a sector.
	base := 3 * sacdSectorPayload
	copy(d[base:], sacdTTxtSignature)
	putU16(base+8, 32) // track 1's item pointer -> offset 32 within the bank
	d[base+32] = 1     // one item
	d[base+36] = 0x01  // type 0x01 = title
	copy(d[base+37:], []byte("Seed Title\x00"))
	return d
}

// FuzzParseSACDTOC drives the whole TOC read — geometry probe, master-copy
// fallback across sectors 510/520/530, the eight locale text banks, and the
// area pointers — with the image's own bytes under the fuzzer's control.
func FuzzParseSACDTOC(f *testing.F) {
	f.Add(sacdSeedMasterUnit()) // survives validation — see the builder
	f.Add(sacdSeedImage(nil))
	f.Add(sacdSeedImage(bytes.Repeat([]byte{0xFF}, 4096)))
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
	f.Add(sacdSeedArea()) // reaches the track table and TTxt walk
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

// TestSACDFuzzSeedsReachTheParser keeps the two structural seeds honest.
//
// A seed is only worth its bytes if it gets PAST the validation gates; one
// that is rejected early is indistinguishable from the empty seed and
// quietly narrows the corpus the fuzzer starts from. Both seeds in this
// file were exactly that when first written — the master unit put its area
// pointers at the wrong offset, and the area seed left trackAreaStart /
// trackAreaEnd zero, so `trackAreaEnd <= trackAreaStart` returned before
// the track table. Nothing failed; the targets simply explored less.
//
// Asserting the parse SUCCEEDS is what makes that visible, and it is why
// this is a test rather than a comment.
func TestSACDFuzzSeedsReachTheParser(t *testing.T) {
	t.Run("master unit parses to a TOC", func(t *testing.T) {
		img := sacdFuzzImage{base: sacdMasterTOCSectors[0] * sacdPlain2048.stride, data: sacdSeedMasterUnit()}
		toc, err := parseSACDTOC(img)
		if err != nil {
			t.Fatalf("seed master unit failed to parse: %v", err)
		}
		if toc == nil {
			t.Fatal("seed master unit produced no TOC — it is rejected at the " +
				"area-pointer gate and explores no more than the empty seed")
		}
	})

	t.Run("area seed parses to a stereo area with a track", func(t *testing.T) {
		area, ok := parseSACDArea(sacdFuzzImage{data: sacdSeedArea()}, sacdPlain2048, 0)
		if !ok {
			t.Fatal("seed area TOC was refused — it never reaches the track " +
				"table or the TTxt bank walk, which is the arithmetic this " +
				"target exists to exercise")
		}
		if len(area.tracks) != 1 {
			t.Fatalf("seed area produced %d tracks, want 1", len(area.tracks))
		}
		if got := area.tracks[0].title; got != "Seed Title" {
			t.Errorf("seed area track title = %q, want %q — the TTxt bank walk "+
				"did not run", got, "Seed Title")
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
		// The INDEX half of the property. Checking only the container
		// would accept a renderer that maps an accepted index to a
		// different accepted one — 7 rendering as "08.dff" round-trips
		// its container perfectly and is still wrong, and on a
		// multi-track disc it is wrong in the way that matters: two
		// tracks colliding on one path, or a row whose path names
		// another track.
		_, file := path.Split(p)
		gotIndex, ok := parseSACDVirtualIndex(file)
		if !ok {
			t.Fatalf("rendered %q but parseSACDVirtualIndex refused its file component %q",
				p, file)
		}
		if gotIndex != index {
			t.Fatalf("index round-trip: rendered %q for index %d, parsed back %d",
				p, index, gotIndex)
		}
	})
}
