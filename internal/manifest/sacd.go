package manifest

// SACD ISO expansion — TOC layer only. A scanned `.iso` SACD rip yields one
// VIRTUAL track row per stereo track at "<iso>/st/NN.dff"; the container
// itself gets NO row. The bridge never demuxes audio server-side: clients
// fetch the CONTAINER's bytes over the ordinary ranged-read endpoint and
// demux the track's sector window themselves (the iOS materializer), so
// `/v1/read` on a virtual path 404s cleanly by design and the DLNA file
// handler's own not-found path covers renderers.
//
// The virtual-path grammar is a PINNED cross-repo wire contract
// (PROTOCOL.md "SACD ISO expansion"; the iOS mirror is
// `SACDVirtualPath.swift`): `st/` stereo (the only minted area in v1),
// `mc/` reserved-recognized-never-minted, index `%02d` for 1–99 and
// unpadded for 100–255, container suffix `.iso` case-insensitive. Both
// sides must parse the SAME shapes or deletion-pass membership and client
// classification drift.
//
// Clean-room provenance (binding — docs/SACDISOFeasibility.md §2.2 in the
// iOS repo): every structural fact below descends from own-bytes analysis
// of two real operator-supplied ISOs from different authoring toolchains,
// plus expired Philips/Sony patents (US6370090B1 — the doubled-TOC
// mechanism) and the public Sony/Philips "Super Audio CD — A Technical
// Overview" (2001). The GPL SACD lineage (sacd-ripper / sacd_extract /
// foo_input_sacd — code AND prose) was never read; the leaked System
// Description draft stays unopened. Format corners are resolved
// empirically against real ISOs, never by consulting either. Do not
// change that basis.

import (
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

const (
	sacdSectorPayload   = 2048
	sacdDSDSampleRate   = 2822400.0
	sacdFramesPerSecond = 75
)

// The Master TOC lives at logical sector 510, with full copies of its
// 10-sector unit at 520 and 530 (the doubled-TOC mechanism).
var sacdMasterTOCSectors = []int64{510, 520, 530}

type sacdGeometry struct {
	stride        int64 // bytes per on-disk sector
	payloadOffset int64 // user-payload offset within a sector
}

var (
	sacdPlain2048 = sacdGeometry{stride: 2048, payloadOffset: 0}
	sacdRaw2064   = sacdGeometry{stride: 2064, payloadOffset: 12}
)

type sacdTrackEntry struct {
	index          int // 1-based within the area — the virtual-path index
	displayNumber  int // index + trackOffset (multi-disc sets number continuously)
	startFrame     int
	durationFrames int
	title          string
	performer      string
}

type sacdTOC struct {
	albumTitle    string
	albumArtist   string
	albumSequence int
	year          int
	stereoTracks  []sacdTrackEntry
	stereoIsDST   bool
	hasStereo     bool
}

// --- Virtual-path grammar (the iOS SACDVirtualPath mirror) ---

// sacdVirtualIndexComponent renders a 1-based track index in the pinned
// width convention: "01".."99" then "100".."255". Empty for out-of-range.
func sacdVirtualIndexComponent(index int) string {
	switch {
	case index >= 1 && index <= 99:
		return fmt.Sprintf("%02d", index)
	case index >= 100 && index <= 255:
		return strconv.Itoa(index)
	default:
		return ""
	}
}

// SACDVirtualTrackPath mints the stereo virtual path for one track of the
// container at rel. Empty when the index is out of the format's range.
func SACDVirtualTrackPath(containerRel string, index int) string {
	c := sacdVirtualIndexComponent(index)
	if c == "" {
		return ""
	}
	p := containerRel + "/st/" + c + ".dff"
	// A minted path MUST round-trip through the recognizer.
	//
	// This guarded only the index until 2026-08-31, so a container that
	// the recognizer rejects still produced a path — SACDVirtualTrackPath("", 94)
	// returned "/st/94.dff", which IsSACDVirtualPath then says is not a
	// virtual path at all. The Swift mirror has always had the guard, and
	// its comment states the reason in as many words: "Mirror the parse's
	// container rule exactly so minted paths always round-trip."
	//
	// No production caller reaches it — processSACDISO only ever passes a
	// real `.iso` — so this closes a latent divergence in a pinned
	// cross-repo grammar rather than fixing a live defect. It matters
	// because the deletion pass keys on IsSACDVirtualPath to decide whether
	// a row counts as seen: a path the minter produces and the recognizer
	// disowns is a row nothing can spare.
	//
	// Checking against the recognizer rather than restating its rule is
	// deliberate — a second copy of "must end .iso, non-empty" is a second
	// thing that can drift, and this is the file where drift is the bug.
	if got, ok := SACDVirtualContainer(p); !ok || got != containerRel {
		return ""
	}
	return p
}

// parseSACDVirtualIndex accepts exactly the minted widths: two-digit
// zero-padded 01–99 or unpadded 100–255. Everything else — "0", "00",
// "001", "1", "256" — is not a virtual index.
func parseSACDVirtualIndex(file string) (int, bool) {
	name, ok := strings.CutSuffix(file, ".dff")
	if !ok {
		return 0, false
	}
	for _, r := range name {
		if !unicode.IsDigit(r) {
			return 0, false
		}
	}
	switch len(name) {
	case 2:
		n, err := strconv.Atoi(name)
		if err != nil || n < 1 {
			return 0, false
		}
		return n, true
	case 3:
		if name[0] == '0' {
			return 0, false
		}
		n, err := strconv.Atoi(name)
		if err != nil || n < 100 || n > 255 {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// SACDVirtualContainer returns the container path of a virtual-SHAPED
// track path ("Music/A.iso/st/01.dff" → "Music/A.iso", true) and false
// for everything else. Mirrors the iOS classifier: the deletion pass uses
// it to treat a virtual row as SEEN whenever its container is in the
// walk's seen set — virtual paths themselves never appear in any disk
// walk, so without this every scan counts them missing and reaps them at
// the threshold (the routed-rows hazard, one row-species over).
func SACDVirtualContainer(p string) (string, bool) {
	// Components: …/<container ending .iso>/<st|mc>/<NN.dff>
	rest, file := path.Split(p)
	rest = strings.TrimSuffix(rest, "/")
	if _, ok := parseSACDVirtualIndex(file); !ok {
		return "", false
	}
	container, area := path.Split(rest)
	if area != "st" && area != "mc" {
		return "", false
	}
	container = strings.TrimSuffix(container, "/")
	if container == "" || !strings.EqualFold(path.Ext(container), ".iso") {
		return "", false
	}
	return container, true
}

// IsSACDVirtualPath reports whether p has the virtual-track shape.
func IsSACDVirtualPath(p string) bool {
	_, ok := SACDVirtualContainer(p)
	return ok
}

// --- TOC parsing ---

func sacdReadU16(d []byte, off int) uint16 {
	if off < 0 || off+2 > len(d) {
		return 0
	}
	return uint16(d[off])<<8 | uint16(d[off+1])
}

func sacdReadU32(d []byte, off int) uint32 {
	if off < 0 || off+4 > len(d) {
		return 0
	}
	return uint32(d[off])<<24 | uint32(d[off+1])<<16 | uint32(d[off+2])<<8 | uint32(d[off+3])
}

// sacdTimecodeFrames validates (min, sec, frame) at 75 fps and returns the
// absolute frame count. ok=false on out-of-range seconds/frames.
func sacdTimecodeFrames(minutes, seconds, frames byte) (int, bool) {
	if seconds >= 60 || int(frames) >= sacdFramesPerSecond {
		return 0, false
	}
	return (int(minutes)*60+int(seconds))*sacdFramesPerSecond + int(frames), true
}

// sacdDecodeText best-effort decodes a NUL-terminated byte run: printable
// prefix, trimmed of whitespace + control characters; empty for garbage.
func sacdDecodeText(d []byte) string {
	if i := strings.IndexByte(string(d), 0); i >= 0 {
		d = d[:i]
	}
	s := strings.Map(func(r rune) rune {
		if r == unicode.ReplacementChar || unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(string(d), ""))
	return strings.TrimSpace(s)
}

// errSACDShortRead is the failure a ReadAt reports when it returned fewer
// bytes than asked and no error. That breaks the io.ReaderAt contract, so it
// says nothing about the file, and it is read as a failed read, never as the
// end of the file.
var errSACDShortRead = errors.New("sacd: read returned fewer bytes than asked, and no error")

// sacdReadOutcome classifies one ReadAt that asked for want bytes and got n.
// It is the whole of the rule "only a COMPLETED read may answer": a verdict
// of "not an SACD" retires every virtual row of the container at threshold 1
// (processSACDISO), so a read that failed must never be able to reach one.
//
// full reports that every byte the parse needs arrived. n is checked BEFORE
// err, because io.ReaderAt permits (len(p), io.EOF) at the end of the
// source. A short read that stopped at the end of the source (io.EOF, or
// io.ErrUnexpectedEOF) is a fact about the FILE, so it comes back as
// (false, nil): the bytes are absent, and the parse may conclude from their
// absence (sacdReadSectors' "physical end" rule). Any other short read did
// not complete, and it comes back as the failure: a transport error (EIO,
// ETIMEDOUT or ESTALE from a NAS), or errSACDShortRead.
func sacdReadOutcome(n, want int, err error) (full bool, failure error) {
	if n >= want {
		return true, nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return false, nil
	}
	if err == nil {
		return false, fmt.Errorf("%w (%d of %d bytes)", errSACDShortRead, n, want)
	}
	return false, err
}

// sacdReadSectors reads the payloads of count consecutive logical sectors
// from first. It stops, with what it has and no error, at the end of the
// image (a truncated image is structural truth), and returns an error for a
// sector whose read did not complete (sacdReadOutcome).
func sacdReadSectors(r io.ReaderAt, g sacdGeometry, first int64, count int) ([]byte, error) {
	out := make([]byte, 0, count*sacdSectorPayload)
	buf := make([]byte, g.stride)
	for i := 0; i < count; i++ {
		off := (first + int64(i)) * g.stride
		n, err := r.ReadAt(buf, off)
		full, failure := sacdReadOutcome(n, int(g.payloadOffset)+sacdSectorPayload, err)
		if failure != nil {
			return nil, fmt.Errorf("sacd: read sector %d: %w", first+int64(i), failure)
		}
		if !full {
			break // physical end — structural truth, not a transport error
		}
		out = append(out, buf[g.payloadOffset:g.payloadOffset+sacdSectorPayload]...)
	}
	return out, nil
}

var sacdMasterSignature = []byte("SACDMTOC")

// sacdDetectGeometry finds the geometry whose master-TOC positions carry the
// master signature. It probes every copy position under both geometries, so
// a disc whose FIRST copy is damaged or unreadable still detects: the copies
// exist exactly for this (the doubled-TOC mechanism).
//
// found=false with a nil error is the one answer that means "not an SACD
// image", and it needs every probe to have COMPLETED (all eight bytes, or
// the end of the file) without finding the signature. A probe that failed
// does not stop the rest, since a later copy can still answer, but when none
// answers, the first failure comes back in place of the verdict: an image
// whose probes did not all complete has not been shown to be anything.
func sacdDetectGeometry(r io.ReaderAt) (sacdGeometry, bool, error) {
	var firstFailure error
	probe := make([]byte, len(sacdMasterSignature))
	for _, g := range []sacdGeometry{sacdPlain2048, sacdRaw2064} {
		for _, lsn := range sacdMasterTOCSectors {
			off := lsn*g.stride + g.payloadOffset
			n, err := r.ReadAt(probe, off)
			full, failure := sacdReadOutcome(n, len(probe), err)
			if failure != nil {
				if firstFailure == nil {
					firstFailure = fmt.Errorf("sacd: master TOC probe at byte %d: %w", off, failure)
				}
				continue
			}
			if full && string(probe) == string(sacdMasterSignature) {
				return g, true, nil
			}
		}
	}
	return sacdGeometry{}, false, firstFailure
}

// sacdProbeDST reads the header byte of a stereo area's first audio sector,
// whose bit 0 is the DST flag. A read that stops at the end of the file (a
// truncated image, or a track area placed past its end) answers "not DST",
// which is a fact about the file. A read that fails answers nothing: its
// error comes back.
func sacdProbeDST(r io.ReaderAt, g sacdGeometry, trackAreaStart uint32) (bool, error) {
	probe := make([]byte, 1)
	off := int64(trackAreaStart)*g.stride + g.payloadOffset
	n, err := r.ReadAt(probe, off)
	full, failure := sacdReadOutcome(n, len(probe), err)
	if failure != nil {
		return false, fmt.Errorf("sacd: DST probe at byte %d: %w", off, failure)
	}
	return full && probe[0]&0x01 == 1, nil
}

// parseSACDTOC reads the disc's TOC through an io.ReaderAt.
//
// It returns (nil, nil) only when the image is not a plain SACD AND every
// probe for the master signature completed (sacdDetectGeometry); the scanner
// then retires every virtual row the container had, so that answer must
// never come from a read that failed. A TOC without a stereo DST area comes
// back with hasStereo or stereoIsDST false, which expandSACD turns into the
// same retiring (nil, nil), and so it too rests on completed reads: an
// area TOC whose reads failed, or a DST probe that failed, is an error. An
// error is also what a signature match with no usable master TOC gets (a
// damaged rip). The scanner logs an error and retires nothing.
func parseSACDTOC(r io.ReaderAt) (*sacdTOC, error) {
	geom, found, err := sacdDetectGeometry(r)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil // not an SACD image: every probe completed and none matched
	}

	// Master TOC unit with copy fallback: a copy must parse AND carry at
	// least one area pointer to be adopted — a signature-intact copy with
	// a torn pointer region falls through to the next copy.
	type areaPointer struct{ start, end uint32 }
	var (
		havemaster  bool
		areas       []areaPointer
		toc         sacdTOC
		masterUnitN = 1 + 8 // TOC sector + eight locale text banks
	)
	for _, lsn := range sacdMasterTOCSectors {
		unit, err := sacdReadSectors(r, geom, lsn, masterUnitN)
		if err != nil || len(unit) < sacdSectorPayload {
			continue
		}
		d := unit[:sacdSectorPayload]
		if string(d[:8]) != string(sacdMasterSignature) {
			continue
		}
		a1s, a1e := sacdReadU32(d, 64), sacdReadU32(d, 68)
		a2s, a2e := sacdReadU32(d, 72), sacdReadU32(d, 76)
		var cand []areaPointer
		if a1s > 0 && a1e > a1s {
			cand = append(cand, areaPointer{a1s, a1e})
		}
		if a2s > 0 && a2e > a2s {
			cand = append(cand, areaPointer{a2s, a2e})
		}
		if len(cand) == 0 {
			continue
		}
		toc.albumSequence = int(sacdReadU16(d, 18))
		if y := int(sacdReadU16(d, 120)); y > 0 {
			toc.year = y
		}
		// First locale text bank: u16 pointer slots — 16 album title,
		// 18 album artist, 32/34 the disc-level fallbacks.
		if len(unit) >= 2*sacdSectorPayload {
			bank := unit[sacdSectorPayload : 2*sacdSectorPayload]
			if string(bank[:8]) == "SACDText" {
				str := func(slot int) string {
					off := int(sacdReadU16(bank, slot))
					if off <= 0 || off >= len(bank) {
						return ""
					}
					return sacdDecodeText(bank[off:])
				}
				toc.albumTitle = str(16)
				if toc.albumTitle == "" {
					toc.albumTitle = str(32)
				}
				toc.albumArtist = str(18)
				if toc.albumArtist == "" {
					toc.albumArtist = str(34)
				}
			}
		}
		areas = cand
		havemaster = true
		break
	}
	if !havemaster {
		return nil, fmt.Errorf("sacd: no readable master TOC copy")
	}

	// Area TOCs: adopt the first STEREO area (each pointer tried at both
	// its start and end copy). A copy whose read failed is passed over like
	// a damaged one, since the other copy can still answer, but its error is
	// kept: "this disc has no stereo area" is only true if every read that
	// looked for one completed.
	var areaFailure error
	for _, ap := range areas {
		for _, candidate := range []int64{int64(ap.start), int64(ap.end)} {
			area, ok, err := parseSACDArea(r, geom, candidate)
			if err != nil {
				if areaFailure == nil {
					areaFailure = fmt.Errorf("sacd: area TOC at sector %d: %w", candidate, err)
				}
				continue
			}
			if !ok {
				continue
			}
			isDST, err := sacdProbeDST(r, geom, area.trackAreaStart)
			if err != nil {
				return nil, err
			}
			toc.hasStereo = true
			toc.stereoTracks = area.tracks
			toc.stereoIsDST = isDST
			break
		}
		if toc.hasStereo {
			break
		}
	}
	if !toc.hasStereo && areaFailure != nil {
		return nil, areaFailure
	}
	return &toc, nil
}

type sacdArea struct {
	trackAreaStart uint32
	tracks         []sacdTrackEntry
}

var (
	sacdStereoSignature = []byte("TWOCHTOC")
	sacdTRL1Signature   = []byte("SACDTRL1")
	sacdTRL2Signature   = []byte("SACDTRL2")
	sacdTTxtSignature   = []byte("SACDTTxt")
)

// parseSACDArea reads + validates one STEREO area TOC starting at lsn.
// It answers in three ways, and the caller must keep them apart:
//
//   - (area, true, nil): a stereo area TOC, adopted.
//   - (_, false, nil): a STRUCTURAL refusal, a fact about the bytes: a
//     multichannel area, a damaged structure, a non-TOC sector, or a TOC
//     that lies past the end of the image. The caller falls back to the
//     area's second copy.
//   - (_, false, err): a read of the area TOC did not complete, so the bytes
//     were never seen. The error is only ever a read failure; nothing here
//     is known about the area.
//
// Validation mirrors the iOS reader: trackCount 1–255; start timecodes
// strictly increasing; durations > 0; track i's normative span never
// overlaps track i+1's start.
func parseSACDArea(r io.ReaderAt, g sacdGeometry, lsn int64) (sacdArea, bool, error) {
	var out sacdArea
	header, err := sacdReadSectors(r, g, lsn, 1)
	if err != nil {
		return out, false, err
	}
	if len(header) < sacdSectorPayload {
		return out, false, nil // past the end of the image
	}
	if string(header[:8]) != string(sacdStereoSignature) {
		return out, false, nil // MULCHTOC (recognized, never minted in v1) or junk
	}
	tocSize := int(sacdReadU16(header, 10))
	if tocSize < 3 {
		return out, false, nil
	}
	if tocSize > 255 {
		tocSize = 255
	}
	body, err := sacdReadSectors(r, g, lsn, tocSize)
	if err != nil {
		return out, false, err
	}
	if len(body) < 3*sacdSectorPayload {
		return out, false, nil
	}
	d := body

	channels := int(d[32])
	if channels != 2 {
		return out, false, nil
	}
	trackOffset := int(d[68])
	trackCount := int(d[69])
	if trackCount < 1 || trackCount > 255 {
		return out, false, nil
	}
	trackAreaStart := sacdReadU32(d, 72)
	trackAreaEnd := sacdReadU32(d, 76)
	if trackAreaEnd <= trackAreaStart {
		return out, false, nil
	}

	trl1 := sacdSectorPayload
	if string(d[trl1:trl1+8]) != string(sacdTRL1Signature) {
		return out, false, nil
	}
	trl2 := 2 * sacdSectorPayload
	if string(d[trl2:trl2+8]) != string(sacdTRL2Signature) {
		return out, false, nil
	}

	starts := make([]int, trackCount)
	durations := make([]int, trackCount)
	for i := 0; i < trackCount; i++ {
		s := trl2 + 8 + 4*i
		f, ok := sacdTimecodeFrames(d[s], d[s+1], d[s+2])
		if !ok {
			return out, false, nil
		}
		starts[i] = f
		o := trl2 + 8 + 1020 + 4*i
		dur, ok := sacdTimecodeFrames(d[o], d[o+1], d[o+2])
		if !ok || dur <= 0 {
			return out, false, nil
		}
		durations[i] = dur
		startLSN := sacdReadU32(d, trl1+8+4*i)
		if startLSN < trackAreaStart || startLSN >= trackAreaEnd {
			return out, false, nil
		}
		if i > 0 {
			if starts[i] <= starts[i-1] {
				return out, false, nil
			}
			if starts[i-1]+durations[i-1] > starts[i] {
				return out, false, nil
			}
		}
	}

	// Track text: signature scan over the TOC's remaining sectors.
	// Wholly best-effort — absence or malformation yields empty titles
	// (the filename-stem fallback covers), never a failed parse.
	titles := make([]string, trackCount)
	performers := make([]string, trackCount)
	totalSectors := len(d) / sacdSectorPayload
	for rel := 3; rel < totalSectors; rel++ {
		base := rel * sacdSectorPayload
		if string(d[base:base+8]) != string(sacdTTxtSignature) {
			continue
		}
		bank := d[base:]
		for i := 0; i < trackCount; i++ {
			ptr := int(sacdReadU16(bank, 8+2*i))
			if ptr <= 0 || ptr+4 > len(bank) {
				continue
			}
			itemCount := int(bank[ptr])
			if itemCount <= 0 || itemCount > 16 {
				continue
			}
			cursor := ptr + 4
			for item := 0; item < itemCount; item++ {
				cursor = (cursor + 3) &^ 3 // items are 4-byte aligned
				if cursor+2 > len(bank) {
					break
				}
				typ := bank[cursor]
				textStart := cursor + 1
				nul := strings.IndexByte(string(bank[textStart:]), 0)
				if nul < 0 {
					break
				}
				text := sacdDecodeText(bank[textStart : textStart+nul])
				if typ&0x80 == 0 { // phonetic variants skipped
					if typ == 0x01 && titles[i] == "" {
						titles[i] = text
					}
					if typ == 0x02 && performers[i] == "" {
						performers[i] = text
					}
				}
				cursor = textStart + nul + 1
			}
		}
		break
	}

	out.trackAreaStart = trackAreaStart
	out.tracks = make([]sacdTrackEntry, trackCount)
	for i := 0; i < trackCount; i++ {
		out.tracks[i] = sacdTrackEntry{
			index:          i + 1,
			displayNumber:  i + 1 + trackOffset,
			startFrame:     starts[i],
			durationFrames: durations[i],
			title:          titles[i],
			performer:      performers[i],
		}
	}
	return out, true, nil
}

// --- Expansion ---

// ExpandSACDISO opens the container at absPath and expands it (expandSACD,
// which says what each answer means). The scanner does not come through
// here: processSACDISO opens through its own opener, so it can stat the
// handle it read.
func ExpandSACDISO(absPath, relPath string, size int64, mtime time.Time) ([]*Track, error) {
	f, _, err := fsutil.OpenAsFile(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return expandSACD(f, relPath, size, mtime)
}

// expandSACD parses the container read through r and mints one *Track per
// stereo DST track. It returns (nil, nil) for a non-SACD image, a plain-DSD
// (non-DST) stereo area, or a multichannel-only disc — v1 expands stereo
// DST only, matching the iOS envelope; the scanner then upserts nothing and
// the image stays invisible to clients (its file still lists in the folder
// browser, where the client's own refusal copy covers taps).
//
// (nil, nil) is also the answer on which processSACDISO RETIRES every
// virtual row the container had, at threshold 1 and journaled, so a
// tombstone reaches every paired device. That is why it is given only when
// every read it rests on COMPLETED (sacdReadOutcome): a read that failed,
// the EIO, ETIMEDOUT or ESTALE of a NAS that is still serving the file,
// comes back as an error, and an error retires nothing. A read that stopped
// at the end of the file is an answer: a truncated image yields (nil, nil)
// as before, as the iOS reader's short reads do.
func expandSACD(r io.ReaderAt, relPath string, size int64, mtime time.Time) ([]*Track, error) {
	toc, err := parseSACDTOC(r)
	if err != nil || toc == nil {
		return nil, err
	}
	if !toc.hasStereo || !toc.stereoIsDST {
		return nil, nil
	}

	album := toc.albumTitle
	if album == "" {
		album = strings.TrimSuffix(path.Base(relPath), path.Ext(relPath))
	}
	disc := toc.albumSequence
	if disc < 1 {
		disc = 1
	}

	tracks := make([]*Track, 0, len(toc.stereoTracks))
	for _, te := range toc.stereoTracks {
		vp := SACDVirtualTrackPath(relPath, te.index)
		if vp == "" {
			continue // index past the format ceiling — structurally unreachable
		}
		title := te.title
		if title == "" {
			title = fmt.Sprintf("Track %d", te.displayNumber)
		}
		artist := te.performer
		if artist == "" {
			artist = toc.albumArtist
		}
		// Through the shared gate like every other derived duration
		// (ExtractorVersion 12) — STRUCTURAL here, not a live fix, and
		// worth saying which.
		//
		// `durationFrames` comes from the container's TOC, which is
		// untrusted input this walker parses; but it arrives through
		// sacdTimecodeFrames, whose M:S:F triple is bounded by one
		// byte of minutes with seconds < 60 and frames < 75. The
		// largest value expressible is ~4.3 hours, so no SACD image
		// can forge a duration past the week-long ceiling. The gate is
		// here so the rule "every Duration write passes
		// plausibleDuration" has no exception to remember, and so the
		// day that decoder widens — a four-byte timecode, a different
		// TOC revision — this site does not have to be found again.
		// TestSACDTimecodeCannotExpressAnImplausibleDuration pins the
		// bound the claim rests on.
		//
		// Nil rather than a clamped value: Duration is a pointer and
		// its absence already means "unknown" to every consumer — iOS
		// falls back to the file size, as every pre-v11 row does.
		var durationPtr *float64
		if d := float64(te.durationFrames) / float64(sacdFramesPerSecond); plausibleDuration(d) {
			durationPtr = &d
		}
		rate := sacdDSDSampleRate
		bits := 1
		isDSD := true
		trackNo := te.displayNumber
		discNo := disc
		channels := 2
		t := &Track{
			Path:          vp,
			Size:          size, // the CONTAINER's size — matches the iOS scanner's minting
			ModTime:       mtime,
			Title:         title,
			Artist:        artist,
			AlbumArtist:   toc.albumArtist,
			Album:         album,
			TrackNumber:   &trackNo,
			DiscNumber:    &discNo,
			Duration:      durationPtr,
			SampleRate:    &rate,
			BitsPerSample: &bits,
			IsDSD:         &isDSD,
			Codec:         "DFF",
			Compression:   "DST",
			Channels:      &channels,
		}
		if toc.year > 0 {
			y := toc.year
			t.Year = &y
		}
		tracks = append(tracks, t)
	}
	if len(tracks) == 0 {
		return nil, nil
	}
	return tracks, nil
}
