package manifest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime/metrics"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/dhowden/tag"
)

// heapAllocated reads the cumulative bytes this process has allocated from
// the heap. A large allocation is counted when it is made, so the difference
// across a call is what the call asked for, whether or not it touched it.
func heapAllocated() uint64 {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// extractionAllocLimit is the most one extraction may allocate from the heap
// for a file of n bytes. The constant covers what the extractors allow
// themselves before the bytes arrive: dhowden's 10 MB readBytes up front and
// the 10 MB its picture budget allows once, the 16 MiB a VORBIS_COMMENT
// block's 24-bit length lets parseVorbisCommentBounded ask for, and the
// 32 MiB cap on an AIFF or WAV ID3 chunk (no one path reaches all of them).
// The per-byte term covers the copies a parse makes of what it reads. A
// length read out of a file that sizes a buffer the file cannot back
// overshoots it by orders of magnitude: a random 32-bit length asks for
// 2 GiB on average.
func extractionAllocLimit(n int) uint64 { return 64<<20 + 64*uint64(n) }

// extractMeasured writes data to a file called name, extracts it with ec,
// and returns the track and the heap the extraction allocated.
func extractMeasured(t testing.TB, name string, data []byte, ec *ExtractContext) (Track, uint64) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var tr Track
	before := heapAllocated()
	_ = ExtractWithContext(p, &tr, ec)
	return tr, heapAllocated() - before
}

// requireBoundedExtraction fails when extracting data allocated more than
// extractionAllocLimit allows, which is what a picture bomb does.
func requireBoundedExtraction(t testing.TB, name string, data []byte, ec *ExtractContext) Track {
	t.Helper()
	tr, got := extractMeasured(t, name, data, ec)
	if limit := extractionAllocLimit(len(data)); got > limit {
		t.Fatalf("extracting a %d-byte %s allocated %d bytes (limit %d): a length read from the file sized a buffer the file cannot back",
			len(data), name, got, limit)
	}
	return tr
}

// --- stream builders ---------------------------------------------------------

// pictureDataLen is what the bomb shapes declare: far past any budget a
// small file has, and small enough that the old code's allocation of it (a
// negative control, run on a host that overcommits) costs nothing but
// address space.
const pictureDataLen = 1 << 30

// flacStream assembles "fLaC" and the given blocks, each a header and body.
func flacStream(blocks ...[]byte) []byte {
	out := []byte("fLaC")
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out
}

// flacBlock is one metadata block: header (last flag, type, the declared
// length) and body. declared is usually len(body); the shapes that desync a
// walker declare something else.
func flacBlock(last bool, typ byte, declared int, body []byte) []byte {
	return append(flacBlockHeader(last, typ, uint32(declared)), body...)
}

// streamInfo is a zeroed STREAMINFO block (not last).
func streamInfo() []byte { return flacBlock(false, 0, 34, make([]byte, 34)) }

// payloadOf is n bytes of picture data the guard never looks inside.
func payloadOf(n int) []byte { return bytes.Repeat([]byte{0xAB}, n) }

// mbpComment is a METADATA_BLOCK_PICTURE comment carrying a picture block
// that declares dataLen and holds payload.
func mbpComment(key string, dataLen uint32, payload []byte) string {
	return key + "=" + base64.StdEncoding.EncodeToString(flacPictureBody("image/jpeg", "", dataLen, payload))
}

// oggCRC is the Ogg page checksum: CRC-32 with polynomial 0x04c11db7, no
// reflection, no final XOR, over the page with its CRC field zeroed.
func oggCRC(page []byte) uint32 {
	var crc uint32
	for _, v := range page {
		crc ^= uint32(v) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// oggStream lays packets out as Ogg pages of one stream, splitting each
// packet into pages of at most pageData bytes (a multiple of 255 keeps a
// page's segments whole), with valid CRCs, so dhowden reads it.
func oggStream(pageData int, packets ...[]byte) []byte {
	pageData = max(255, pageData/255*255)
	var out []byte
	seq := uint32(0)
	for _, pkt := range packets {
		rest := pkt
		continued := false
		for {
			chunk := rest[:min(len(rest), pageData)]
			rest = rest[len(chunk):]
			var segs []byte
			for n := len(chunk); ; n -= 255 {
				if n < 255 {
					if len(rest) == 0 { // the packet ends on this page
						segs = append(segs, byte(n))
					} else if n > 0 {
						panic("oggStream: chunk not a multiple of 255")
					}
					break
				}
				segs = append(segs, 255)
			}
			var flags byte
			if continued {
				flags |= 0x1
			}
			if seq == 0 {
				flags |= 0x2
			}
			h := []byte("OggS")
			h = append(h, 0, flags)
			h = append(h, make([]byte, 8)...)
			h = binary.LittleEndian.AppendUint32(h, 0x1b99) // serial
			h = binary.LittleEndian.AppendUint32(h, seq)
			h = append(h, 0, 0, 0, 0) // CRC, filled below
			h = append(h, byte(len(segs)))
			h = append(h, segs...)
			page := append(h, chunk...)
			binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
			out = append(out, page...)
			seq++
			continued = true
			if len(rest) == 0 {
				break
			}
		}
	}
	return out
}

// vorbisIdent is a Vorbis identification header packet (its fields do not
// matter to dhowden, which looks only for the comment packet).
func vorbisIdent() []byte { return append([]byte("\x01vorbis"), make([]byte, 23)...) }

// vorbisCommentPacket is a Vorbis comment header packet.
func vorbisCommentPacket(comments ...string) []byte {
	p := append([]byte("\x03vorbis"), flacVorbisCommentBody(comments...)...)
	return append(p, 1) // framing bit
}

// opusCommentPacket is an Opus comment header packet.
func opusCommentPacket(comments ...string) []byte {
	return append([]byte("OpusTags"), flacVorbisCommentBody(comments...)...)
}

// pictureBombShapes are the ways the old guard let dhowden reach
// `make([]byte, dataLen)` with dataLen far past the bytes present.
func pictureBombShapes() []struct {
	name string // shape
	file string // name the file is written under; the extension routes the extractor
	data []byte
} {
	bomb := flacPictureBody("image/jpeg", "", pictureDataLen, nil)
	emptyComments := flacVorbisCommentBody()
	return []struct {
		name string
		file string
		data []byte
	}{
		{
			// The saved crasher's shape: the old guard could not read a
			// picture's fields inside a block declaring 0 bytes, and passed
			// it; dhowden reads them from the bytes that follow.
			"a PICTURE block declaring length 0", "x.flac",
			flacStream(flacBlock(true, 6, 0, bomb)),
		},
		{
			"a PICTURE block too short for its own fields", "x.flac",
			flacStream(streamInfo(), flacBlock(true, 6, 10, bomb)),
		},
		{
			// dhowden reads a VORBIS_COMMENT by its contents, not its declared
			// length, and takes the next block header from where they end.
			"a PICTURE header in the unused tail of a VORBIS_COMMENT block", "x.flac",
			flacStream(streamInfo(), flacBlock(false, 4, len(emptyComments)+4+len(bomb),
				append(append(emptyComments, flacBlockHeader(true, 6, uint32(len(bomb)))...), bomb...))),
		},
		{
			"a PICTURE header in the unused tail of a PICTURE block", "x.flac",
			flacStream(streamInfo(), flacBlock(false, 6, len(flacPictureBody("image/jpeg", "", 0, nil))+4+len(bomb),
				append(append(flacPictureBody("image/jpeg", "", 0, nil), flacBlockHeader(true, 6, uint32(len(bomb)))...), bomb...))),
		},
		{
			"a METADATA_BLOCK_PICTURE comment in a FLAC stream", "x.flac",
			flacStream(streamInfo(), func() []byte {
				body := flacVorbisCommentBody("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil))
				return flacBlock(true, 4, len(body), body)
			}()),
		},
		{
			"a METADATA_BLOCK_PICTURE key in mixed case", "x.flac",
			flacStream(streamInfo(), func() []byte {
				body := flacVorbisCommentBody(mbpComment("Metadata_Block_Picture", pictureDataLen, nil))
				return flacBlock(true, 4, len(body), body)
			}()),
		},
		{
			// strings.ToLower turns U+212A KELVIN SIGN into 'k', so dhowden
			// decodes this key too.
			"a METADATA_BLOCK_PICTURE key spelled with a KELVIN SIGN", "x.flac",
			flacStream(streamInfo(), func() []byte {
				body := flacVorbisCommentBody(mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil))
				return flacBlock(true, 4, len(body), body)
			}()),
		},
		{
			// dhowden's comment map outlives the block, so the one value is
			// decoded again at the end of every later VORBIS_COMMENT block:
			// 9 MB is within its up-front allowance once, not 40 times.
			"a METADATA_BLOCK_PICTURE decoded again at each later VORBIS_COMMENT block", "x.flac",
			func() []byte {
				body := flacVorbisCommentBody("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", 9<<20, nil))
				blocks := [][]byte{streamInfo(), flacBlock(false, 4, len(body), body)}
				for i := range 40 {
					blocks = append(blocks, flacBlock(i == 39, 4, len(emptyComments), emptyComments))
				}
				return flacStream(blocks...)
			}(),
		},
		{
			"a METADATA_BLOCK_PICTURE in an Ogg Vorbis comment packet", "x.ogg",
			oggStream(65025, vorbisIdent(), vorbisCommentPacket("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil))),
		},
		{
			"a METADATA_BLOCK_PICTURE in an Opus comment packet", "x.oga",
			oggStream(65025, append([]byte("OpusHead"), make([]byte, 11)...), opusCommentPacket("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil))),
		},
		{
			// The comment packet split across pages: the walk has to join
			// them the way dhowden's demuxer does.
			"a METADATA_BLOCK_PICTURE in an Ogg comment packet spanning pages", "x.ogg",
			oggStream(255, vorbisIdent(), vorbisCommentPacket("TITLE=t", "COMMENT="+strings.Repeat("x", 600), mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil))),
		},
		{
			// tag.ReadFrom picks its parser by the first bytes, never the
			// extension.
			"a FLAC bomb named .mp3", "x.mp3",
			flacStream(flacBlock(true, 6, 0, bomb)),
		},
		{
			"a FLAC bomb named .m4a", "x.m4a",
			flacStream(flacBlock(true, 6, 0, bomb)),
		},
		{
			"an Ogg bomb named .flac", "x.flac",
			oggStream(65025, vorbisIdent(), vorbisCommentPacket(mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil))),
		},
	}
}

// TestNoPictureMakesAnExtractionAllocateBeyondTheFile is the regression test
// for backlog B99: every way the old FLAC-only guard let dhowden's
// readPictureBlock allocate a buffer far past the bytes present. On the old
// code each of these allocated 1 GiB here (and up to 4 GiB with a larger
// field); the nightly FuzzExtractFLAC runner died of it.
func TestNoPictureMakesAnExtractionAllocateBeyondTheFile(t *testing.T) {
	for _, c := range pictureBombShapes() {
		t.Run(c.name, func(t *testing.T) {
			requireBoundedExtraction(t, c.file, c.data, &ExtractContext{})
		})
	}
}

// TestThePictureGuardRefusesEveryBombShape pins the guard itself on the same
// shapes, so a regression names the guard rather than an allocation count.
func TestThePictureGuardRefusesEveryBombShape(t *testing.T) {
	for _, c := range pictureBombShapes() {
		t.Run(c.name, func(t *testing.T) {
			if ok, refusal := dhowdenPicturesWithinBudget(bytes.NewReader(c.data)); ok {
				t.Fatalf("guard passed a stream whose picture asks dhowden for %d bytes", pictureDataLen)
			} else if refusal.Budget != pictureBudget(int64(len(c.data))) || refusal.Declared <= 0 {
				t.Fatalf("refusal %+v does not carry the file's budget and the declared size", refusal)
			}
		})
	}
}

// TestPicturesDhowdenCanReadStillReachIt is the over-strictness guard: real
// pictures of every kind keep their tags and their cover, and a picture
// truncated within dhowden's own up-front allowance keeps its file's tags (as
// it always did: dhowden ignores a METADATA_BLOCK_PICTURE it cannot read).
func TestPicturesDhowdenCanReadStillReachIt(t *testing.T) {
	cover := encodeSolidImage(t, 16, 16, 90)
	audio := make([]byte, 64<<10) // the file's frames, which dhowden never reads
	cases := []struct {
		name, file string
		data       []byte
		wantCover  bool
	}{
		{
			"a FLAC PICTURE block", "x.flac",
			flacStream(streamInfo(),
				func() []byte {
					body := flacVorbisCommentBody("TITLE=t")
					return flacBlock(false, 4, len(body), body)
				}(),
				func() []byte {
					body := flacPictureBody("image/jpeg", "cover", uint32(len(cover)), cover)
					return append(flacBlock(true, 6, len(body), body), audio...)
				}()),
			true,
		},
		{
			"a METADATA_BLOCK_PICTURE in a FLAC stream", "x.flac",
			flacStream(streamInfo(), func() []byte {
				body := flacVorbisCommentBody("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover))
				return append(flacBlock(true, 4, len(body), body), audio...)
			}()),
			true,
		},
		{
			"a METADATA_BLOCK_PICTURE in an Ogg Vorbis stream", "x.ogg",
			oggStream(65025, vorbisIdent(), vorbisCommentPacket("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover))),
			true,
		},
		{
			"a METADATA_BLOCK_PICTURE in an Opus stream", "x.oga",
			oggStream(65025, append([]byte("OpusHead"), make([]byte, 11)...), opusCommentPacket("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover))),
			true,
		},
		{
			"a METADATA_BLOCK_PICTURE across Ogg pages", "x.ogg",
			oggStream(510, vorbisIdent(), vorbisCommentPacket("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover))),
			true,
		},
		{
			// Declares 5 MB it does not hold: within dhowden's 10 MB up-front
			// allowance, so it is read (and fails) as before.
			"a METADATA_BLOCK_PICTURE truncated within dhowden's allowance", "x.flac",
			flacStream(streamInfo(), func() []byte {
				body := flacVorbisCommentBody("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", 5<<20, cover))
				return append(flacBlock(true, 4, len(body), body), audio...)
			}()),
			false,
		},
		{
			// Two VORBIS_COMMENT blocks are out of spec; in a file whose audio
			// holds most of its bytes, decoding the picture twice stays well
			// inside the budget.
			"a METADATA_BLOCK_PICTURE before a second VORBIS_COMMENT block", "x.flac",
			func() []byte {
				first := flacVorbisCommentBody("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover))
				second := flacVorbisCommentBody("ALBUM=a")
				return flacStream(streamInfo(), flacBlock(false, 4, len(first), first),
					append(flacBlock(true, 4, len(second), second), audio...))
			}(),
			true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if ok, refusal := dhowdenPicturesWithinBudget(bytes.NewReader(c.data)); !ok {
				t.Fatalf("guard refused a file dhowden reads within bounds: %+v", refusal)
			}
			tr := requireBoundedExtraction(t, c.file, c.data, &ExtractContext{ArtworkCacheDir: t.TempDir()})
			if tr.Title != "t" {
				t.Errorf("Title = %q, want the tag dhowden reads (t)", tr.Title)
			}
			if gotCover := strings.HasPrefix(tr.ArtworkMBID, "local-"); gotCover != c.wantCover {
				t.Errorf("embedded cover extracted = %v (ArtworkMBID %q), want %v", gotCover, tr.ArtworkMBID, c.wantCover)
			}
		})
	}
}

// TestThePictureGuardDoesNotReadAPicturePayload pins that the guard reads a
// picture's header and never its payload: the single-open FLAC path exists
// because a 5-25 MiB cover crossing the wire twice per track halved scanner
// throughput on NAS mounts, and dhowden reads the payload right after. For a
// METADATA_BLOCK_PICTURE that means decoding only the base64 of its header;
// for an Ogg stream it also means page headers and segment tables, never the
// segment data it does not need.
func TestThePictureGuardDoesNotReadAPicturePayload(t *testing.T) {
	cover := payloadOf(4 << 20)
	cases := []struct {
		name   string
		data   []byte
		budget int64
	}{
		{
			// Magic, two block headers, the picture's fields: well under 1 KiB.
			"a FLAC PICTURE block",
			flacStream(streamInfo(), func() []byte {
				body := flacPictureBody("image/jpeg", "cover", uint32(len(cover)), cover)
				return flacBlock(true, 6, len(body), body)
			}()),
			1 << 10,
		},
		{
			"a METADATA_BLOCK_PICTURE in a FLAC stream",
			flacStream(streamInfo(), func() []byte {
				body := flacVorbisCommentBody("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover))
				return flacBlock(true, 4, len(body), body)
			}()),
			8 << 10, // the header's base64, read by the decoder in blocks
		},
		{
			// 5.6 MB of base64 spans 86 pages: their headers and segment
			// tables (282 bytes each) are what the walk must read to find
			// where the packet ends.
			"a METADATA_BLOCK_PICTURE in an Ogg stream",
			oggStream(65025, vorbisIdent(), vorbisCommentPacket("TITLE=t", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(cover)), cover))),
			64 << 10,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs := &countingReadSeeker{rs: bytes.NewReader(c.data)}
			if ok, refusal := dhowdenPicturesWithinBudget(rs); !ok {
				t.Fatalf("guard refused a well-formed %d-byte cover: %+v", len(cover), refusal)
			}
			if rs.read > c.budget {
				t.Errorf("guard read %d bytes of a %d-byte file (budget %d): the picture payload is being read",
					rs.read, len(c.data), c.budget)
			}
		})
	}
}

// TestThePictureGuardFollowsDhowdenPastAPicture pins the other half: after a
// picture the walk goes on from where dhowden's reads end, so a bomb in a
// later block is still reached. A walk that lost its place would read on
// from the wrong bytes and pass it.
func TestThePictureGuardFollowsDhowdenPastAPicture(t *testing.T) {
	cover := payloadOf(4096)
	good := flacPictureBody("image/jpeg", "cover", uint32(len(cover)), cover)
	bad := flacPictureBody("image/jpeg", "", pictureDataLen, nil)
	data := flacStream(flacBlock(false, 6, len(good), good), flacBlock(true, 6, len(bad), bad))
	if ok, _ := dhowdenPicturesWithinBudget(bytes.NewReader(data)); ok {
		t.Fatal("the walk did not reach the second PICTURE block: it lost its place after the first")
	}
}

// TestThePictureGuardPassesWhatDhowdenReadsNoPictureFrom pins the fail-open
// half: streams dhowden reads no picture from (or cannot read at all) are
// not refused.
func TestThePictureGuardPassesWhatDhowdenReadsNoPictureFrom(t *testing.T) {
	bomb := flacPictureBody("image/jpeg", "", pictureDataLen, nil)
	for name, in := range map[string][]byte{
		"empty":        {},
		"under 11":     []byte("fLaC\x86"),
		"an ID3v2 tag": append([]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), flacStream(flacBlock(true, 6, 0, bomb))...),
		"truncated":    append([]byte("fLaC"), 0x86, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00),
		// A picture after the LAST block is never read by dhowden.
		"a picture after the last block": append(flacStream(flacBlock(true, 0, 34, make([]byte, 34))), flacBlock(true, 6, len(bomb), bomb)...),
		// An unknown block is skipped by its declared length, bomb and all.
		"a picture inside an unknown block": flacStream(flacBlock(true, 9, len(bomb), bomb)),
		// A comment with no '=' ends dhowden's read of the stream.
		"a picture after a comment with no '='": flacStream(func() []byte {
			body := flacVorbisCommentBody("NOEQUALS", mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil))
			return flacBlock(true, 4, len(body), body)
		}()),
		// An Ogg stream whose comment packet is never completed.
		"an unfinished Ogg comment packet": func() []byte {
			s := oggStream(255, vorbisIdent(), vorbisCommentPacket(mbpComment("METADATA_BLOCK_PICTURE", pictureDataLen, nil)))
			return s[:len(s)-100]
		}(),
	} {
		if ok, refusal := dhowdenPicturesWithinBudget(bytes.NewReader(in)); !ok {
			t.Errorf("%s: refused (%+v), want passed", name, refusal)
		}
	}
}

// TestThePictureGuardLeavesTheReaderWhereItFoundIt pins the call site's
// premise: tag.ReadFrom reads from the reader's current offset.
func TestThePictureGuardLeavesTheReaderWhereItFoundIt(t *testing.T) {
	data := append([]byte("xxxx"), flacStream(streamInfo(), flacBlock(true, 6, 0, flacPictureBody("image/jpeg", "", pictureDataLen, nil)))...)
	r := bytes.NewReader(data)
	if _, err := r.Seek(4, 0); err != nil {
		t.Fatal(err)
	}
	if ok, _ := dhowdenPicturesWithinBudget(r); ok {
		t.Fatal("the guard did not walk the stream from the reader's offset")
	}
	if pos, _ := r.Seek(0, 1); pos != 4 {
		t.Fatalf("reader left at %d, want 4", pos)
	}
}

// TestDhowdenStillAllocatesAPictureBeforeReadingIt is the premise the guard
// rests on. The day dhowden bounds readPictureBlock's allocation itself this
// fails, and the guard (not its fuzz seeds) can go.
func TestDhowdenStillAllocatesAPictureBeforeReadingIt(t *testing.T) {
	const declared = 64 << 20
	data := flacStream(flacBlock(true, 6, 0, flacPictureBody("image/jpeg", "", declared, nil)))
	before := heapAllocated()
	_, _ = tag.ReadFrom(bytes.NewReader(data))
	if got := heapAllocated() - before; got < declared {
		t.Fatalf("dhowden allocated %d bytes for a %d-byte file whose picture declares %d: it no longer allocates before reading, so the picture guard may be retired",
			got, len(data), declared)
	}
}

// TestNoRuneLongerThanThreeBytesLowersIntoThePictureKey pins mbpKeyScan's
// premise: strings.ToLower reaches a letter of the METADATA_BLOCK_PICTURE key
// from no rune longer than 3 bytes, so a key that lowers to it is at most
// three times its length.
func TestNoRuneLongerThanThreeBytesLowersIntoThePictureKey(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !utf8.ValidRune(r) || !strings.ContainsRune(mbpKeyName, unicode.ToLower(r)) {
			continue
		}
		if n := utf8.RuneLen(r); n > 3 {
			t.Errorf("%U (%d bytes) lowers to %q", r, n, unicode.ToLower(r))
		}
	}
}
