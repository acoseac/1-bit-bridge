package manifest

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The two allocation-bomb regressions (2026-07-20 review, F20 + F21).
//
// Both upstream tag parsers allocate from an unvalidated 32-bit length read
// straight out of the file, BEFORE the read that would fail. The resulting
// request is below Go's maxAlloc, so `makeslice` does not panic — the
// runtime throws "out of memory", which does not unwind defers, so the
// scanner's per-iteration recover() cannot catch it. The process dies, and
// because the startup scan re-runs on every boot and re-encounters the file
// before it is ever indexed, the bridge crash-loops with no log line naming
// the culprit.
//
// These tests must therefore assert on the GUARD, not on surviving the
// allocation: if a guard regresses, the test process itself would be killed
// rather than failing cleanly.

// flacBlockHeader builds a 4-byte FLAC metadata block header:
// 1 bit last-block flag, 7 bits type, 24 bits body length.
func flacBlockHeader(last bool, blockType byte, length uint32) []byte {
	b := make([]byte, 4)
	b[0] = blockType & 0x7F
	if last {
		b[0] |= 0x80
	}
	b[1] = byte(length >> 16)
	b[2] = byte(length >> 8)
	b[3] = byte(length)
	return b
}

// TestParseVorbisCommentBoundedRefusesHugeVendorLength pins F20's first
// allocation site: mewkiz's readString does `make([]byte, n)` from the raw
// vendor length before any read. A 4-byte body claiming 0xFFFFFFFF would
// request 4 GiB.
func TestParseVorbisCommentBoundedRefusesHugeVendorLength(t *testing.T) {
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(0xFFFFFFFF))

	_, err := parseVorbisCommentBounded(bytes.NewReader(body.Bytes()), int64(body.Len()))
	if err == nil {
		t.Fatal("vendor length beyond the block: want error, got nil")
	}
}

// TestParseVorbisCommentBoundedRefusesHugeTagCount pins F20's SECOND and
// worse site: `make([][2]string, x)` is pointer-bearing (32 B/elem, and the
// GC must scan it), so 0xFFFFFFFF is a ~137 GiB request from a 12-byte file.
func TestParseVorbisCommentBoundedRefusesHugeTagCount(t *testing.T) {
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(0)) // vendor length 0
	_ = binary.Write(&body, binary.LittleEndian, uint32(0xFFFFFFFF))

	_, err := parseVorbisCommentBounded(bytes.NewReader(body.Bytes()), int64(body.Len()))
	if err == nil {
		t.Fatal("tag count beyond the block: want error, got nil")
	}
}

// TestParseVorbisCommentBoundedReadsRealTags is the guard against the fix
// being over-strict — the normal path must still round-trip.
func TestParseVorbisCommentBoundedReadsRealTags(t *testing.T) {
	var body bytes.Buffer
	vendor := "reference libFLAC"
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(vendor)))
	body.WriteString(vendor)
	tags := []string{"ARTIST=Abdullah Ibrahim", "ARTIST=Ekaya", "ALBUM=Water from an Ancient Well"}
	_ = binary.Write(&body, binary.LittleEndian, uint32(len(tags)))
	for _, tg := range tags {
		_ = binary.Write(&body, binary.LittleEndian, uint32(len(tg)))
		body.WriteString(tg)
	}

	got, err := parseVorbisCommentBounded(bytes.NewReader(body.Bytes()), int64(body.Len()))
	if err != nil {
		t.Fatalf("well-formed block: unexpected error %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(tags) = %d, want 3", len(got))
	}
	if got[0][0] != "ARTIST" || got[0][1] != "Abdullah Ibrahim" {
		t.Fatalf("first tag = %q=%q, want ARTIST=Abdullah Ibrahim", got[0][0], got[0][1])
	}
	if got[2][1] != "Water from an Ancient Well" {
		t.Fatalf("third tag value = %q", got[2][1])
	}
}

// TestParseVorbisCommentBoundedLeavesReaderAtNextBlock pins the positioning
// contract: the caller's walk continues from this reader, so a short read
// would desync it into parsing audio frames as metadata.
func TestParseVorbisCommentBoundedLeavesReaderAtNextBlock(t *testing.T) {
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(0)) // vendor
	_ = binary.Write(&body, binary.LittleEndian, uint32(0)) // zero tags
	body.WriteString("PADDING-WITHIN-BLOCK")                // unread block remainder

	sentinel := "NEXT-BLOCK-HEADER"
	full := append(append([]byte{}, body.Bytes()...), []byte(sentinel)...)

	r := bytes.NewReader(full)
	if _, err := parseVorbisCommentBounded(r, int64(body.Len())); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rest := make([]byte, len(sentinel))
	if _, err := r.Read(rest); err != nil {
		t.Fatalf("reading past the block: %v", err)
	}
	if string(rest) != sentinel {
		t.Fatalf("reader left at %q, want it positioned at %q", rest, sentinel)
	}
}

// TestApplyFLACMultiValueArtistsSurvivesHostileVorbisBlock is the
// end-to-end F20 guard: the exact ~12-byte crafted file from the review.
// Pre-fix this killed the process; post-fix it is a silent no-op.
func TestApplyFLACMultiValueArtistsSurvivesHostileVorbisBlock(t *testing.T) {
	var f bytes.Buffer
	f.WriteString("fLaC")
	var body bytes.Buffer
	_ = binary.Write(&body, binary.LittleEndian, uint32(0))          // vendor length 0
	_ = binary.Write(&body, binary.LittleEndian, uint32(0xFFFFFFFF)) // hostile tag count
	f.Write(flacBlockHeader(true, 4 /* VORBIS_COMMENT */, uint32(body.Len())))
	f.Write(body.Bytes())

	tr := &Track{Artist: "Original", AlbumArtist: "OriginalAlbum"}
	applyFLACMultiValueArtists(bytes.NewReader(f.Bytes()), tr)

	// Must leave the dhowden-populated values untouched rather than dying.
	if tr.Artist != "Original" || tr.AlbumArtist != "OriginalAlbum" {
		t.Fatalf("hostile block mutated the track: %q / %q", tr.Artist, tr.AlbumArtist)
	}
}

// --- F21: PICTURE block pre-validation ------------------------------------

// flacPictureBody builds a PICTURE block body with a caller-chosen dataLen,
// so a test can declare a payload far larger than the block.
func flacPictureBody(mime, desc string, dataLen uint32, payload []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(3)) // picture type: front cover
	_ = binary.Write(&b, binary.BigEndian, uint32(len(mime)))
	b.WriteString(mime)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(desc)))
	b.WriteString(desc)
	for i := 0; i < 4; i++ { // width, height, depth, colors
		_ = binary.Write(&b, binary.BigEndian, uint32(0))
	}
	_ = binary.Write(&b, binary.BigEndian, dataLen)
	b.Write(payload)
	return b.Bytes()
}

// TestFLACPictureBlocksSaneRejectsOversizedDataLen pins F21: dhowden's
// readPictureBlock allocates `make([]byte, dataLen)` from this field before
// the read that would fail, and maxArtworkBytes is checked only AFTER the
// allocation, so it is a policy filter rather than a bound.
func TestFLACPictureBlocksSaneRejectsOversizedDataLen(t *testing.T) {
	body := flacPictureBody("image/jpeg", "", 0xFFFFFFFF, []byte{0xFF, 0xD8, 0xFF})
	var f bytes.Buffer
	f.WriteString("fLaC")
	f.Write(flacBlockHeader(true, 6 /* PICTURE */, uint32(len(body))))
	f.Write(body)

	if flacPictureBlocksSane(bytes.NewReader(f.Bytes())) {
		t.Fatal("picture declaring 4 GiB inside a small block: want not-sane, got sane")
	}
}

// TestFLACPictureBlocksSaneAcceptsRealPicture is the over-strictness guard —
// a genuine embedded cover must still reach dhowden.
func TestFLACPictureBlocksSaneAcceptsRealPicture(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 512)
	body := flacPictureBody("image/jpeg", "cover", uint32(len(payload)), payload)
	var f bytes.Buffer
	f.WriteString("fLaC")
	f.Write(flacBlockHeader(true, 6, uint32(len(body))))
	f.Write(body)

	if !flacPictureBlocksSane(bytes.NewReader(f.Bytes())) {
		t.Fatal("well-formed picture block rejected")
	}
}

// TestFLACPictureBlocksSaneFailsOpenOnNonFLAC pins the documented
// fail-open contract: inputs that never reach the picture path at all must
// not be treated as hostile.
func TestFLACPictureBlocksSaneFailsOpenOnNonFLAC(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":     {},
		"not_flac":  []byte("ID3\x04\x00\x00\x00\x00\x00\x00junk"),
		"truncated": append([]byte("fLaC"), 0x86, 0xFF),
	} {
		if !flacPictureBlocksSane(bytes.NewReader(in)) {
			t.Errorf("%s: want fail-open (true), got false", name)
		}
	}
}

// TestExtractHostilePictureFLACStillIndexes is the end-to-end guard: a file
// with a bomb PICTURE block must still index (path-derived metadata), not
// take the process down and not abort the scan.
func TestExtractHostilePictureFLACStillIndexes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Artist", "Album")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "01 Track.flac")

	body := flacPictureBody("image/jpeg", "", 0xFFFFFFFF, []byte{0xFF, 0xD8, 0xFF})
	var f bytes.Buffer
	f.WriteString("fLaC")
	f.Write(flacBlockHeader(true, 6, uint32(len(body))))
	f.Write(body)
	if err := os.WriteFile(file, f.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	var tr Track
	// Must return without killing the process. An error is acceptable (the
	// file is genuinely corrupt); a dead process is not.
	_ = ExtractWithContext(file, &tr, nil)
}

// countingReadSeeker wraps a ReadSeeker and tallies bytes actually read,
// so a test can distinguish "seeked past the payload" from "read it".
type countingReadSeeker struct {
	rs   io.ReadSeeker
	read int64
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := c.rs.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingReadSeeker) Seek(off int64, whence int) (int64, error) {
	return c.rs.Seek(off, whence)
}

// TestFLACPictureBlocksSaneDoesNotReadThePayload pins that the preflight
// walk SEEKS past a validated PICTURE payload instead of draining it.
//
// flacPictureBodySane needs ~30 bytes of fixed header fields to judge the
// geometry, but the original implementation deferred an
// io.Copy(io.Discard, lr) that pulled the whole block body through — for a
// real cover that is 5–25 MiB over the wire. The caller then Seek(0)s and
// hands the same file to dhowden, which reads the payload AGAIN: exactly
// the per-track double read the single-open FLAC path was built to
// eliminate (extractors.go's `.flac` branch). On a NAS-mounted library
// that halved scanner throughput.
//
// A 4 MiB payload is far above any plausible header-read, so the bound
// here fails loudly if a drain ever comes back.
func TestFLACPictureBlocksSaneDoesNotReadThePayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 4<<20)
	body := flacPictureBody("image/jpeg", "cover", uint32(len(payload)), payload)
	var f bytes.Buffer
	f.WriteString("fLaC")
	f.Write(flacBlockHeader(true, 6 /* PICTURE */, uint32(len(body))))
	f.Write(body)

	c := &countingReadSeeker{rs: bytes.NewReader(f.Bytes())}
	if !flacPictureBlocksSane(c) {
		t.Fatal("well-formed picture block rejected")
	}
	// magic + block header + the picture header fields — comfortably
	// under 1 KiB. The payload itself must never be transferred.
	const budget = 1 << 10
	if c.read > budget {
		t.Errorf("preflight read %d bytes for a %d-byte payload (budget %d) — "+
			"the PICTURE body is being drained instead of seeked past, "+
			"reintroducing the per-track double read",
			c.read, len(payload), budget)
	}
}

// TestFLACPictureBlocksSaneLeavesWalkAlignedAfterPicture pins the other
// half of the seek contract: skipping the payload must land the reader
// exactly on the NEXT block header, or every block after a picture is
// misparsed and the walk bails fail-open (silently disabling the guard).
func TestFLACPictureBlocksSaneLeavesWalkAlignedAfterPicture(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 4096)
	pic := flacPictureBody("image/jpeg", "cover", uint32(len(payload)), payload)

	var f bytes.Buffer
	f.WriteString("fLaC")
	// PICTURE first (not last), then a second PICTURE that is positively
	// inconsistent. The walk can only reach the bad block if it re-aligned
	// correctly after the good one — so `false` here proves alignment.
	f.Write(flacBlockHeader(false, 6, uint32(len(pic))))
	f.Write(pic)
	bad := flacPictureBody("image/jpeg", "", 0xFFFFFFFF, []byte{0xFF, 0xD8, 0xFF})
	f.Write(flacBlockHeader(true, 6, uint32(len(bad))))
	f.Write(bad)

	if flacPictureBlocksSane(bytes.NewReader(f.Bytes())) {
		t.Fatal("walk did not reach the second (oversized) PICTURE block — " +
			"the reader is misaligned after skipping the first payload")
	}
}

// flacVorbisCommentBody builds a minimal VORBIS_COMMENT block body:
// a vendor string then a tag count then that many `KEY=value` entries,
// all little-endian (unlike PICTURE's big-endian fields).
func flacVorbisCommentBody(tags ...string) []byte {
	var b bytes.Buffer
	const vendor = "test"
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(vendor)))
	b.WriteString(vendor)
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(tags)))
	for _, t := range tags {
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(t)))
		b.WriteString(t)
	}
	return b.Bytes()
}

// TestApplyFLACMultiValueArtistsDoesNotReadPayloadsBeforeTheComment is
// the sibling of TestFLACPictureBlocksSaneDoesNotReadThePayload, on the
// walk that did not have it.
//
// applyFLACMultiValueArtists is the THIRD pass over the same *os.File
// (format, then dhowden, then this one), and it used block.Skip() for
// every non-Vorbis block. Skip checks whether the body reader is an
// io.Seeker; meta.New wraps it in a plain io.LimitReader and
// *io.LimitedReader never is, so Skip always falls to
// io.Copy(io.Discard, …) and reads every byte off the file. On a NAS
// mount that is the embedded cover crossing the wire a second time —
// the per-track double read the single-open path exists to eliminate,
// and the exact defect extractFLACFormatFromReader's docblock records
// being fixed for the STREAMINFO walk in #165.
//
// PICTURE is placed BEFORE VORBIS_COMMENT deliberately. The walk
// returns as soon as it finds the comment block, so the canonical
// flac/metaflac layout (STREAMINFO, SEEKTABLE, VORBIS_COMMENT, PICTURE)
// never reaches a picture at all — which is why this was latent, and
// why a fixture in that order would prove nothing.
func TestApplyFLACMultiValueArtistsDoesNotReadPayloadsBeforeTheComment(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 4<<20)
	pic := flacPictureBody("image/jpeg", "cover", uint32(len(payload)), payload)
	comment := flacVorbisCommentBody("ARTIST=Abdullah Ibrahim", "ARTIST=Ekaya")

	var f bytes.Buffer
	f.WriteString("fLaC")
	f.Write(flacBlockHeader(false, 6 /* PICTURE */, uint32(len(pic))))
	f.Write(pic)
	f.Write(flacBlockHeader(true, 4 /* VORBIS_COMMENT */, uint32(len(comment))))
	f.Write(comment)

	c := &countingReadSeeker{rs: bytes.NewReader(f.Bytes())}
	var tr Track
	applyFLACMultiValueArtists(c, &tr)

	// The walk must still have done its job — otherwise "reads few
	// bytes" is satisfied by a walk that gave up at the picture.
	if tr.Artist != "Abdullah Ibrahim; Ekaya" {
		t.Fatalf("multi-value artists = %q, want %q (the walk did not reach the comment block past the picture)",
			tr.Artist, "Abdullah Ibrahim; Ekaya")
	}

	// magic + two block headers + the comment body — comfortably under
	// 1 KiB. The 4 MiB picture payload must never be transferred.
	const budget = 1 << 10
	if c.read > budget {
		t.Errorf("the walk read %d bytes for a %d-byte picture payload (budget %d) — "+
			"the PICTURE body is being drained instead of seeked past, "+
			"reintroducing the per-track double read on NAS-mounted libraries",
			c.read, len(payload), budget)
	}
}
