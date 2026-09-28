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
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"testing"
	"time"
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
			_, _, _ = parseSACDArea(sacdFuzzImage{base: g.payloadOffset, data: b}, g, 0)
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
		area, ok, err := parseSACDArea(sacdFuzzImage{data: sacdSeedArea()}, sacdPlain2048, 0)
		if err != nil {
			t.Fatalf("seed area TOC: a read failed on an in-memory image: %v", err)
		}
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

// --- The read-fault property ---
//
// The three targets above feed the parser bytes and fail only on a panic. The
// rule #1061 put in (only a COMPLETED read may answer "not an SACD", because
// that answer retires every virtual row of the container at threshold 1, with
// a tombstone to every paired device) was pinned by unit tests alone: the
// targets' reader never fails, so no fuzzed input ever reached a failure path.
// FuzzSACDExpandUnderAReadFault injects one read failure at a fuzzed place and
// asks what it did to the answer.
//
// The property: expanded through a reader that fails where the fault says, an
// image answers exactly what it answers fault-free, or it answers an error. An
// error retires nothing and writes nothing (processSACDISO), so it is always
// safe. A different successful answer is a violation, and so is "not an SACD"
// where the fault-free read found an album.
//
// It holds only for an image that carries ONE answer, which is why the harness
// builds its images with the fixture builder rather than from free bytes. The
// parser falls back from a copy it could not read to the next one, on purpose
// (the doubled-TOC mechanism), so an image whose copies DIFFER can legitimately
// expand from its second copy when the first cannot be read: the three master
// TOC copies, and the area TOC's two copies, are written identically here, and
// every damage the harness applies (sacdFaultDamage) is applied to every copy
// alike. For the same reason the image holds one stereo area and carries the
// master signature under one geometry: an image valid under both geometries,
// or holding two different stereo areas, carries two answers, and which one a
// failed read leaves standing is not a defect. Nor is a reader that reports
// the end of the file early: that is indistinguishable from a truncated image,
// which is structural truth (sacdReadOutcome), so the injected failures are
// ones that say they failed (sacdFaultErrors).

// sacdFaultErrors are the failures the harness injects: a transport error, and
// a short read that reports none (errSACDShortRead's case). Never io.EOF or
// io.ErrUnexpectedEOF; the block comment above says why.
var sacdFaultErrors = []error{sacdReadEIO, errors.New("read: operation timed out"), nil}

// sacdFaultMaxLen bounds a fault's length. 256 KiB covers every probe of both
// geometries at once (about 49 KB), every master copy, and both area copies.
const sacdFaultMaxLen = 1 << 18

// sacdFaultCase is one input to the property, in the fuzz target's argument
// order: how to build the image, and the one read fault.
type sacdFaultCase struct {
	raw, plainDSD, damageFirst bool
	tracks                     uint8  // 1 + tracks%6 tracks
	lengths                    []byte // track i lasts 1 + lengths[i]%250 frames
	albumTitle                 string // capped at 256 bytes
	damage                     uint8  // an index into sacdFaultDamage, mod its length
	cut                        uint32 // non-zero truncates the image to cut%len
	faultFrom, faultLen        uint32 // mapped into the image by fault
	faultN                     uint16 // bytes a failing read hands back before its error
	errKind                    uint8  // an index into sacdFaultErrors, mod its length
}

// add adds the case to f's seed corpus.
func (c sacdFaultCase) add(f *testing.F) {
	f.Add(c.raw, c.plainDSD, c.damageFirst, c.tracks, c.lengths, c.albumTitle,
		c.damage, c.cut, c.faultFrom, c.faultLen, c.faultN, c.errKind)
}

// sacdFaultDamage is the structural damage the harness can apply, each to
// every copy of the structure alike, so that the image still carries one
// answer. Each one past the first sends the parse down a refusal branch.
// sector maps a logical sector to the byte offset of its payload.
var sacdFaultDamage = []func(img []byte, sector func(int64) int64){
	func([]byte, func(int64) int64) {}, // none: the image expands
	// A multichannel area: refused as not stereo.
	func(img []byte, sector func(int64) int64) {
		for _, a := range []int64{fixAreaStart, fixAreaEnd} {
			img[sector(a)+32] = 6
		}
	},
	// A MULCHTOC signature: refused as not a stereo TOC.
	func(img []byte, sector func(int64) int64) {
		for _, a := range []int64{fixAreaStart, fixAreaEnd} {
			copy(img[sector(a):], "MULCHTOC")
		}
	},
	// No tracks.
	func(img []byte, sector func(int64) int64) {
		for _, a := range []int64{fixAreaStart, fixAreaEnd} {
			img[sector(a)+69] = 0
		}
	},
	// A track area that ends where it starts.
	func(img []byte, sector func(int64) int64) {
		for _, a := range []int64{fixAreaStart, fixAreaEnd} {
			h := sector(a)
			copy(img[h+76:h+80], img[h+72:h+76])
		}
	},
	// A broken TRL2 signature.
	func(img []byte, sector func(int64) int64) {
		for _, a := range []int64{fixAreaStart, fixAreaEnd} {
			copy(img[sector(a+2):], "SACDTRLX")
		}
	},
	// A TOC too short to hold the track tables.
	func(img []byte, sector func(int64) int64) {
		for _, a := range []int64{fixAreaStart, fixAreaEnd} {
			h := sector(a)
			img[h+10], img[h+11] = 0, 2
		}
	},
	// A TOC that claims 200 sectors: its reads run to the end of the image.
	func(img []byte, sector func(int64) int64) {
		for _, a := range []int64{fixAreaStart, fixAreaEnd} {
			h := sector(a)
			img[h+10], img[h+11] = 0, 200
		}
	},
	// No area pointer in any master copy: "no readable master TOC copy".
	func(img []byte, sector func(int64) int64) {
		for _, m := range sacdMasterTOCSectors {
			clear(img[sector(m)+64 : sector(m)+80])
		}
	},
}

// image builds the case's image with the fixture builder, which writes every
// TOC copy identically, then damages every copy alike and truncates.
func (c sacdFaultCase) image(t *testing.T) []byte {
	t.Helper()
	tracks := make([]sacdFixtureTrack, 1+int(c.tracks%6))
	start := 0
	for i := range tracks {
		dur := 150
		if i < len(c.lengths) {
			dur = 1 + int(c.lengths[i]%250)
		}
		tracks[i] = sacdFixtureTrack{startFrame: start, duration: dur, title: fmt.Sprintf("T%d", i+1)}
		start += dur
	}
	title := c.albumTitle
	if len(title) > 256 {
		title = title[:256]
	}
	img := buildSACDImage(t, tracks, sacdFixtureOptions{
		raw2064: c.raw, plainDSD: c.plainDSD, damageFirst: c.damageFirst, albumTitle: title,
	})
	g := sacdPlain2048
	if c.raw {
		g = sacdRaw2064
	}
	sector := func(lsn int64) int64 { return lsn*g.stride + g.payloadOffset }
	sacdFaultDamage[int(c.damage)%len(sacdFaultDamage)](img, sector)
	if c.cut != 0 {
		img = img[:int(c.cut%uint32(len(img)))]
	}
	return img
}

// fault maps the case's fault into an image of imgLen bytes, or a little past
// its end, where a read stops short.
//
// The seeds are what give the fuzzer its reach, not this mapping. Go's
// mutator changes one argument per step and walks an integer by at most 100,
// so a fault can wander only from where a seed put one. With the DST probe's
// failure dropped again (the #1061 defect) and that probe's own seed left
// out, the fuzzer found the violation from the other seeds in under a second;
// from a single seed whose fault touched no read it ran 468,005 inputs in
// 90 s without finding it, and a mapping that starts every fault at a
// sector's first byte or its payload did no better in another 90 s.
func (c sacdFaultCase) fault(imgLen int) sacdFault {
	from := int64(c.faultFrom) % (int64(imgLen) + 4096)
	return sacdFault{
		from: from,
		to:   from + 1 + int64(c.faultLen%sacdFaultMaxLen),
		n:    int(c.faultN),
		err:  sacdFaultErrors[int(c.errKind)%len(sacdFaultErrors)],
	}
}

// sacdFaultViolation expands img fault-free and through a reader that fails
// where fault says, and returns what is wrong with the faulted answer ("" when
// nothing is), and whether any read reached the fault.
func sacdFaultViolation(img []byte, fault sacdFault) (violation string, reached bool) {
	const rel = "Music/Album.iso"
	mtime := time.Unix(1700000000, 0).UTC()
	want, wantErr := expandSACD(bytes.NewReader(img), rel, int64(len(img)), mtime)
	r := &sacdFaultyImage{img: img, faults: []sacdFault{fault}}
	got, gotErr := expandSACD(r, rel, int64(len(img)), mtime)
	reached = r.failed > 0
	switch {
	case gotErr != nil:
		return "", reached // an error retires nothing and writes nothing
	case wantErr != nil:
		return fmt.Sprintf("the fault-free read failed (%v) and the faulted one answered %d tracks",
			wantErr, len(got)), reached
	case want != nil && got == nil:
		return fmt.Sprintf("a read that did not complete answered \"not an SACD\" for an album of %d tracks",
			len(want)), reached
	case !reflect.DeepEqual(got, want):
		return fmt.Sprintf("a read that did not complete changed the answer: %d tracks, fault-free %d",
			len(got), len(want)), reached
	}
	return "", reached
}

// sacdFaultSeeds put a fault on each read #1061 made fail closed, so the seed
// corpus alone, which every `go test` runs, pins all three: the probes for the
// master signature (under both geometries, and as a short read with no error),
// both copies of the area TOC, and the DST probe. The last two are faults a
// copy survives, and must change nothing.
var sacdFaultSeeds = []struct {
	name     string
	c        sacdFaultCase
	wantFail bool // the faulted expansion must answer an error
}{
	{"every master-signature probe, plain", sacdFaultCase{
		faultFrom: 510 * 2048, faultLen: 530*2064 + 20 - 510*2048,
	}, true},
	{"every master-signature probe, raw", sacdFaultCase{
		raw: true, faultFrom: 510 * 2048, faultLen: 530*2064 + 20 - 510*2048,
	}, true},
	{"every master-signature probe, short with no error", sacdFaultCase{
		faultFrom: 510 * 2048, faultLen: 530*2064 + 20 - 510*2048, faultN: 3, errKind: 2,
	}, true},
	{"both copies of the area TOC", sacdFaultCase{
		faultFrom: fixAreaStart * 2048, faultLen: (fixAreaEnd + fixTOCSectors - fixAreaStart) * 2048,
	}, true},
	{"the DST probe", sacdFaultCase{
		tracks: 2, faultFrom: fixAudioStart * 2048,
	}, true},
	{"the first master copy, which the second replaces", sacdFaultCase{
		faultFrom: 510 * 2048, faultLen: 2048 - 1,
	}, false},
	{"the area TOC's first copy, which its second replaces", sacdFaultCase{
		faultFrom: fixAreaStart * 2048, faultLen: fixTOCSectors*2048 - 1,
	}, false},
}

// FuzzSACDExpandUnderAReadFault is a PROPERTY target: an image expanded
// through one failing read answers what it answers fault-free, or an error.
// The block comment above states the property, and why the harness builds
// the image rather than taking free bytes.
func FuzzSACDExpandUnderAReadFault(f *testing.F) {
	for _, s := range sacdFaultSeeds {
		s.c.add(f)
	}
	f.Fuzz(func(t *testing.T, raw, plainDSD, damageFirst bool, tracks uint8, lengths []byte,
		albumTitle string, damage uint8, cut, faultFrom, faultLen uint32, faultN uint16, errKind uint8) {
		c := sacdFaultCase{raw, plainDSD, damageFirst, tracks, lengths, albumTitle,
			damage, cut, faultFrom, faultLen, faultN, errKind}
		img := c.image(t)
		fault := c.fault(len(img))
		if v, _ := sacdFaultViolation(img, fault); v != "" {
			t.Fatalf("%s (fault [%d, %d) n=%d err=%v)", v, fault.from, fault.to, fault.n, fault.err)
		}
	})
}

// TestSACDFaultSeedsReachTheFailure keeps the seeds honest. A seed whose fault
// no read reaches pins nothing, and a seed meant to exercise a read #1061 made
// fail closed must answer an error there: passing because the answer came back
// unchanged would mean the fault never landed on the read it names.
func TestSACDFaultSeedsReachTheFailure(t *testing.T) {
	for _, s := range sacdFaultSeeds {
		t.Run(s.name, func(t *testing.T) {
			img := s.c.image(t)
			if tracks, err := expandSACD(bytes.NewReader(img), "Music/Album.iso", 1, time.Unix(1, 0)); err != nil || len(tracks) == 0 {
				t.Fatalf("the seed image does not expand fault-free: %d tracks, err %v", len(tracks), err)
			}
			fault := s.c.fault(len(img))
			v, reached := sacdFaultViolation(img, fault)
			if !reached {
				t.Fatalf("no read reached the fault [%d, %d)", fault.from, fault.to)
			}
			if v != "" {
				t.Fatal(v)
			}
			r := &sacdFaultyImage{img: img, faults: []sacdFault{fault}}
			if _, err := expandSACD(r, "Music/Album.iso", 1, time.Unix(1, 0)); (err != nil) != s.wantFail {
				t.Fatalf("the faulted expansion answered err %v; an error is wanted: %v", err, s.wantFail)
			}
		})
	}
}

// TestSACDFaultPropertySeesALyingReader is the property's positive control:
// the check can fail. A reader that reports the end of the file early is
// indistinguishable from a truncated image, so the parser answers "not an
// SACD" through it, and sacdFaultViolation must say so. If this passes with no
// violation, the property checks nothing.
func TestSACDFaultPropertySeesALyingReader(t *testing.T) {
	img := sacdFaultCase{}.image(t)
	lie := sacdFault{from: 510 * 2048, to: 530*2064 + 20, err: io.EOF}
	v, reached := sacdFaultViolation(img, lie)
	if !reached {
		t.Fatal("no read reached the lying fault")
	}
	if v == "" {
		t.Fatal("a reader that lies about the end of the file answered \"not an SACD\" and the property saw nothing")
	}
}
