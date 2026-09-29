package manifest

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FLAC in Ogg (backlog B102, ExtractorVersion 18). dhowden's Ogg reader looks
// only for a Vorbis or Opus comment packet, and an Ogg FLAC stream carries its
// comments as a FLAC VORBIS_COMMENT block in one of its header packets, so
// such a file had no tags and was read to its end to find that out.

// oggFixture reads testdata/ogg/name, made by testdata/gen/ogg_flac_fixtures.sh.
func oggFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ogg", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// extractOggFixture extracts the fixture from a file of the given name, with a
// local artwork cache so an embedded picture is stored.
func extractOggFixture(t *testing.T, fixture, name string) Track {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, oggFixture(t, fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := Track{Path: name, Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(p, &tr, &ExtractContext{ArtworkCacheDir: filepath.Join(dir, "art")}); err != nil {
		t.Fatal(err)
	}
	return tr
}

// intIs reports whether p holds want.
func intIs(p *int, want int) bool { return p != nil && *p == want }

// TestAnOggFLACFileGetsItsTags reads the two real muxers' files through the
// scanner's own entry point. On main both came back with no tag at all.
func TestAnOggFLACFileGetsItsTags(t *testing.T) {
	t.Run("ffmpeg", func(t *testing.T) {
		tr := extractOggFixture(t, "ffmpeg_pages.oga", "ffmpeg_pages.oga")
		if tr.Title != "Ogg Title" || tr.Artist != "Ogg Artist" || tr.Album != "Ogg Album" ||
			tr.AlbumArtist != "Ogg Album Artist" || tr.Genre != "Ambient" {
			t.Errorf("text tags = %q / %q / %q / %q / %q, want the file's", tr.Title, tr.Artist, tr.Album, tr.AlbumArtist, tr.Genre)
		}
		if !intIs(tr.Year, 2019) || !intIs(tr.TrackNumber, 7) || !intIs(tr.DiscNumber, 2) {
			t.Errorf("year / track / disc = %v / %v / %v, want 2019 / 7 / 2", tr.Year, tr.TrackNumber, tr.DiscNumber)
		}
		if tr.MusicBrainzTrackID != "0a1b2c3d-0000-4000-8000-000000000001" {
			t.Errorf("MusicBrainzTrackID = %q", tr.MusicBrainzTrackID)
		}
		if tr.Codec != "OGG" {
			t.Errorf("Codec = %q, want OGG (the .oga branch's, unchanged)", tr.Codec)
		}
	})
	t.Run("libFLAC", func(t *testing.T) {
		tr := extractOggFixture(t, "libflac_picture.oga", "libflac_picture.oga")
		if tr.Title != "Libflac Title" || tr.Album != "Libflac Album" {
			t.Errorf("title / album = %q / %q", tr.Title, tr.Album)
		}
		// Two ARTIST comments: dhowden keeps the last, and the FLAC
		// multi-value pass joins both, as it does for a .flac file.
		if tr.Artist != "First Artist; Second Artist" {
			t.Errorf("Artist = %q, want both ARTIST values joined", tr.Artist)
		}
		if !tr.Compilation || tr.AlbumArtist != "Various Artists" {
			t.Errorf("compilation / album artist = %v / %q, want the COMPILATION=1 synth", tr.Compilation, tr.AlbumArtist)
		}
		if !intIs(tr.Year, 2021) || !intIs(tr.TrackNumber, 3) {
			t.Errorf("year / track = %v / %v, want 2021 / 3", tr.Year, tr.TrackNumber)
		}
		// The PICTURE header packet: a 16x16 JPEG, stored as local art.
		if !strings.HasPrefix(tr.ArtworkMBID, "local-") {
			t.Errorf("ArtworkMBID = %q, want the embedded cover's local- key", tr.ArtworkMBID)
		}
	})
}

// rangeRecorder is a ReadSeeker over data that records the furthest byte any
// read reached, through Read or ReadAt. It exposes nothing else of the
// bytes.Reader, so no reader can go around it (io.Copy's WriterTo, say).
type rangeRecorder struct {
	r        *bytes.Reader
	furthest int64
}

func newRangeRecorder(data []byte) *rangeRecorder { return &rangeRecorder{r: bytes.NewReader(data)} }

func (rr *rangeRecorder) Read(p []byte) (int, error) {
	off, _ := rr.r.Seek(0, io.SeekCurrent)
	n, err := rr.r.Read(p)
	rr.note(off, n)
	return n, err
}

func (rr *rangeRecorder) ReadAt(p []byte, off int64) (int, error) {
	n, err := rr.r.ReadAt(p, off)
	rr.note(off, n)
	return n, err
}

func (rr *rangeRecorder) Seek(off int64, whence int) (int64, error) { return rr.r.Seek(off, whence) }

func (rr *rangeRecorder) note(off int64, n int) {
	if end := off + int64(n); n > 0 && end > rr.furthest {
		rr.furthest = end
	}
}

// oggPageStarts returns the offset of every Ogg page in data, reading the page
// headers as the Ogg framing lays them out.
func oggPageStarts(t testing.TB, data []byte) []int64 {
	t.Helper()
	var starts []int64
	for pos := 0; pos < len(data); {
		if pos+27 > len(data) || string(data[pos:pos+4]) != "OggS" {
			t.Fatalf("no Ogg page at %d", pos)
		}
		n := int(data[pos+26])
		size := 27 + n
		for _, s := range data[pos+27 : pos+27+n] {
			size += int(s)
		}
		starts = append(starts, int64(pos))
		pos += size
	}
	return starts
}

// TestAnOggFLACFileIsReadNoFurtherThanItsHeaderPackets pins the other half of
// B102: extracting an Ogg FLAC file reads the pages that hold its header
// packets and not one byte of an audio page. On main dhowden read every page,
// CRC-checking each, to fail at the end (and the picture guard walked every
// page ahead of it), on every extraction.
func TestAnOggFLACFileIsReadNoFurtherThanItsHeaderPackets(t *testing.T) {
	for _, tc := range []struct {
		fixture    string
		firstAudio int // the page holding the first audio packet
		title      string
	}{
		{"ffmpeg_pages.oga", 2, "Ogg Title"},
		{"libflac_picture.oga", 4, "Libflac Title"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			data := oggFixture(t, tc.fixture)
			starts := oggPageStarts(t, data)
			rr := newRangeRecorder(data)
			tr := Track{Path: tc.fixture, Size: int64(len(data)), ModTime: time.Now()}
			if err := extractViaDhowdenFromReader(rr, tc.fixture, &tr, nil); err != nil {
				t.Fatal(err)
			}
			if tr.Title != tc.title {
				t.Errorf("Title = %q, want %q", tr.Title, tc.title)
			}
			if limit := starts[tc.firstAudio]; rr.furthest > limit {
				t.Errorf("the extraction read up to byte %d of %d; its header packets end at %d, where the audio begins",
					rr.furthest, len(data), limit)
			}
			t.Logf("read up to byte %d of %d (the audio begins at %d)", rr.furthest, len(data), starts[tc.firstAudio])
		})
	}
}

// --- synthesized streams -----------------------------------------------------

// oggFLACMappingPacket is the Ogg FLAC mapping's first packet: 0x7F "FLAC",
// mapping version 1.0, the count of header packets that follow (0: not
// declared), then "fLaC" and a STREAMINFO block, as a .flac file begins.
func oggFLACMappingPacket(declared uint16) []byte {
	p := []byte("\x7fFLAC\x01\x00")
	p = binary.BigEndian.AppendUint16(p, declared)
	return append(append(p, "fLaC"...), streamInfo()...)
}

// oggFLACAudioPacket opens as every FLAC frame does, with the sync code: no
// metadata block header can (it would be type 127, which FLAC forbids).
func oggFLACAudioPacket() []byte { return []byte{0xFF, 0xF8, 0x69, 0x08, 0x00, 0x00, 0x00, 0x00} }

// oggLogicalStream lays packets out as the pages of one logical stream: the
// first packet alone on the first page (flagged BOS), the rest packed
// continuously, pageSegs lacing values to a page (1 to 255), each page flagged
// continued when its first segment carries on a packet begun on an earlier
// page, and the last flagged EOS. The CRCs are valid, so dhowden's own
// demuxer reads the pages too.
func oggLogicalStream(serial uint32, pageSegs int, packets ...[]byte) [][]byte {
	pageSegs = min(max(pageSegs, 1), 255)
	var pages [][]byte
	seq := uint32(0)
	emit := func(flags byte, lacing, data []byte) {
		h := []byte("OggS")
		h = append(h, 0, flags)
		h = append(h, make([]byte, 8)...) // granule position
		h = binary.LittleEndian.AppendUint32(h, serial)
		h = binary.LittleEndian.AppendUint32(h, seq)
		h = append(h, 0, 0, 0, 0) // CRC, filled below
		h = append(h, byte(len(lacing)))
		h = append(h, lacing...)
		page := append(h, data...)
		binary.LittleEndian.PutUint32(page[22:26], oggCRC(page))
		pages = append(pages, page)
		seq++
	}
	lace := func(pkt []byte) []byte {
		return append(bytes.Repeat([]byte{255}, len(pkt)/255), byte(len(pkt)%255))
	}
	emit(0x02, lace(packets[0]), packets[0])
	type segment struct {
		data  []byte
		opens bool // the first segment of its packet
	}
	var segs []segment
	for _, pkt := range packets[1:] {
		off := 0
		for i, v := range lace(pkt) {
			segs = append(segs, segment{pkt[off : off+int(v)], i == 0})
			off += int(v)
		}
	}
	for i := 0; i < len(segs); i += pageSegs {
		chunk := segs[i:min(len(segs), i+pageSegs)]
		var flags byte
		if !chunk[0].opens {
			flags |= 0x01
		}
		if i+pageSegs >= len(segs) {
			flags |= 0x04
		}
		var lacing, data []byte
		for _, s := range chunk {
			lacing = append(lacing, byte(len(s.data)))
			data = append(data, s.data...)
		}
		emit(flags, lacing, data)
	}
	return pages
}

// joinPages concatenates pages into a file.
func joinPages(pages ...[]byte) []byte { return bytes.Join(pages, nil) }

// commentBlock is a VORBIS_COMMENT metadata block.
func commentBlock(last bool, comments ...string) []byte {
	body := flacVorbisCommentBody(comments...)
	return flacBlock(last, 4, len(body), body)
}

// oggFLACFile is an Ogg FLAC stream in pages of pageSegs lacing values: the
// mapping packet declaring declared header packets, the blocks, one packet
// each, and an audio packet.
func oggFLACFile(pageSegs int, declared uint16, blocks ...[]byte) []byte {
	packets := append([][]byte{oggFLACMappingPacket(declared)}, blocks...)
	packets = append(packets, oggFLACAudioPacket())
	return joinPages(oggLogicalStream(0x0f1ac, pageSegs, packets...)...)
}

// extractBytesAs extracts data from a file called name.
func extractBytesAs(t *testing.T, name string, data []byte) Track {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	tr := Track{Path: name, Size: int64(len(data)), ModTime: time.Now()}
	if err := ExtractWithContext(p, &tr, &ExtractContext{}); err != nil {
		t.Fatal(err)
	}
	return tr
}

// TestEveryOggFLACShapeGetsItsTags covers what the two muxers do not write:
// a header count left undeclared, a last block not flagged last, a comment
// packet across pages or sharing one, other blocks ahead of the comment,
// another stream's first page ahead of the FLAC stream's, and an Ogg FLAC
// file under another extension (dhowden picks its reader by the first bytes).
func TestEveryOggFLACShapeGetsItsTags(t *testing.T) {
	long := "COMMENT=" + strings.Repeat("x", 700) // three 255-byte segments and more
	picture := flacPictureBody("image/jpeg", "", uint32(len(minimalJPEG)), minimalJPEG)
	for _, tc := range []struct {
		name string
		file string
		data []byte
	}{
		{"one header packet, declared", "a.oga", oggFLACFile(255, 1, commentBlock(true, "TITLE=Shape"))},
		{"count undeclared, last flagged", "a.oga", oggFLACFile(255, 0,
			commentBlock(false, "TITLE=Shape"), flacBlock(true, 1, 8, make([]byte, 8)))},
		{"count undeclared, nothing flagged last", "a.oga", oggFLACFile(255, 0, commentBlock(false, "TITLE=Shape"))},
		{"declared count met, nothing flagged last", "a.oga", oggFLACFile(255, 2,
			commentBlock(false, "TITLE=Shape"), flacBlock(false, 1, 8, make([]byte, 8)))},
		{"a comment packet across pages", "a.oga", oggFLACFile(1, 1, commentBlock(true, "TITLE=Shape", long))},
		{"a comment packet of whole segments", "a.oga", oggFLACFile(2, 1,
			commentBlock(true, "TITLE=Shape", "COMMENT="+strings.Repeat("y", 510-4-len(flacVorbisCommentBody("TITLE=Shape", "COMMENT="))))),
		},
		{"header packets sharing a page", "a.oga", oggFLACFile(255, 3,
			flacBlock(false, 3, 18, make([]byte, 18)), commentBlock(false, "TITLE=Shape"), flacBlock(true, 1, 4, make([]byte, 4)))},
		{"a PICTURE and a SEEKTABLE ahead of the comment", "a.oga", oggFLACFile(255, 3,
			flacBlock(false, 6, len(picture), picture), flacBlock(false, 3, 18, make([]byte, 18)), commentBlock(true, "TITLE=Shape"))},
		{"a METADATA_BLOCK_PICTURE comment", "a.oga", oggFLACFile(255, 1,
			commentBlock(true, "TITLE=Shape", mbpComment("METADATA_BLOCK_PICTURE", uint32(len(minimalJPEG)), minimalJPEG)))},
		{"no audio: the file ends after the headers", "a.oga", joinPages(oggLogicalStream(7, 255,
			oggFLACMappingPacket(1), commentBlock(true, "TITLE=Shape"))...)},
		{"named .flac", "a.flac", oggFLACFile(255, 1, commentBlock(true, "TITLE=Shape"))},
		{"named .ogg", "a.ogg", oggFLACFile(255, 1, commentBlock(true, "TITLE=Shape"))},
		{"another stream's first page ahead", "a.oga", func() []byte {
			other := oggLogicalStream(1, 255, append([]byte("fishead\x00"), make([]byte, 56)...), []byte("fisbone\x00"))
			flac := oggLogicalStream(2, 255, oggFLACMappingPacket(1), commentBlock(true, "TITLE=Shape"), oggFLACAudioPacket())
			// Every stream's first page comes first, then the rest.
			return joinPages(append([][]byte{other[0], flac[0]}, append(other[1:], flac[1:]...)...)...)
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tr := extractBytesAs(t, tc.file, tc.data); tr.Title != "Shape" {
				t.Errorf("Title = %q, want the comment's", tr.Title)
			}
		})
	}
}

// TestAnOggFLACStreamIsReadNoFurtherThanItsHeaderPackets pins each rule that
// ends the header packets by how far the extraction reads, which is where a
// rule that went missing shows (the join, and so the tags, would come out the
// same): the declared count ends them before the next page, as the last flag
// does; with neither, only the first byte of the first audio packet is read;
// and the stream's last page ends them before another chain's pages.
func TestAnOggFLACStreamIsReadNoFurtherThanItsHeaderPackets(t *testing.T) {
	audio := [][]byte{oggFLACAudioPacket(), oggFLACAudioPacket(), oggFLACAudioPacket()}
	// One lacing value to a page: the mapping on page 0, the comment on page
	// 1, and each audio packet on a page of its own from page 2.
	onePerPage := func(declared uint16, comment []byte) []byte {
		packets := append([][]byte{oggFLACMappingPacket(declared), comment}, audio...)
		return joinPages(oggLogicalStream(9, 1, packets...)...)
	}
	chained := joinPages(append(
		oggLogicalStream(9, 255, oggFLACMappingPacket(0), commentBlock(false, "TITLE=Shape")),
		oggLogicalStream(3, 255, vorbisIdent(), vorbisCommentPacket("TITLE=Vorbis"), []byte{0, 1, 2})...)...)
	for _, tc := range []struct {
		name  string
		data  []byte
		limit func(starts []int64) int64
	}{
		{"the declared count", onePerPage(1, commentBlock(false, "TITLE=Shape")),
			func(s []int64) int64 { return s[2] }},
		{"the last flag", onePerPage(0, commentBlock(true, "TITLE=Shape")),
			func(s []int64) int64 { return s[2] }},
		{"the first audio packet's first byte", onePerPage(0, commentBlock(false, "TITLE=Shape")),
			func(s []int64) int64 { return s[2] + 27 + 1 + 1 }},
		{"the stream's last page", chained,
			func(s []int64) int64 { return s[2] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := newRangeRecorder(tc.data)
			tr := Track{Path: "a.oga", Size: int64(len(tc.data)), ModTime: time.Now()}
			if err := extractViaDhowdenFromReader(rr, "a.oga", &tr, nil); err != nil {
				t.Fatal(err)
			}
			if tr.Title != "Shape" {
				t.Errorf("Title = %q, want the comment's", tr.Title)
			}
			if limit := tc.limit(oggPageStarts(t, tc.data)); rr.furthest > limit {
				t.Errorf("the extraction read up to byte %d of %d, past %d", rr.furthest, len(tc.data), limit)
			}
		})
	}
}

// TestOggFLACHeaderPacketsStopAtTheMappingsOwnMaximum: a stream that declares
// no count and flags no block last is read to maxOggFLACHeaderPackets header
// packets, the most the mapping's count can declare, and no further, within
// the allocation the extractors allow a file of its size.
func TestOggFLACHeaderPacketsStopAtTheMappingsOwnMaximum(t *testing.T) {
	blocks := [][]byte{commentBlock(false, "TITLE=Many")}
	for len(blocks) < maxOggFLACHeaderPackets+100 {
		blocks = append(blocks, flacBlockHeader(false, 1, 0)) // an empty PADDING block
	}
	data := oggFLACFile(255, 0, blocks...)
	md, ok := oggFLACMetadata(bytes.NewReader(data))
	if !ok {
		t.Fatal("oggFLACMetadata declined the stream")
	}
	want := int64(len("fLaC") + len(streamInfo()) + len(blocks[0]) + 4*(maxOggFLACHeaderPackets-1) + len(oggFLACTerminator))
	if md.Size() != want {
		t.Errorf("the join is %d bytes, want %d: %d header packets and the terminator", md.Size(), want, maxOggFLACHeaderPackets)
	}
	if tr := requireBoundedExtraction(t, "many.oga", data, &ExtractContext{}); tr.Title != "Many" {
		t.Errorf("Title = %q, want the comment's", tr.Title)
	}
}

// FuzzOggFLACReadsBackTheMetadataItCarries lays a FLAC stream's metadata out
// as Ogg FLAC pages of any size, with the header count declared or not and
// the last block flagged or not, and reads it back: oggFLACMetadata must
// return exactly "fLaC", the STREAMINFO block, the blocks and
// oggFLACTerminator, however the packets fall across the pages (a packet
// continued over several, several sharing one, one ending on a whole
// 255-byte segment).
func FuzzOggFLACReadsBackTheMetadataItCarries(f *testing.F) {
	f.Add([]byte("\x04\x00\x00\x00test\x01\x00\x00\x00\x07\x00\x00\x00TITLE=t"), []byte{}, bytes.Repeat([]byte{7}, 300), uint8(255), true, true)
	f.Add([]byte{}, bytes.Repeat([]byte{1}, 251), []byte{0}, uint8(1), false, false)
	f.Add(bytes.Repeat([]byte{2}, 506), []byte("x"), bytes.Repeat([]byte{3}, 1020), uint8(2), true, false)
	f.Fuzz(func(t *testing.T, a, b, c []byte, pageSegs uint8, declare, flagLast bool) {
		if len(a)+len(b)+len(c) > 1<<20 {
			t.Skip()
		}
		blocks := [][]byte{
			flacBlock(false, 4, len(a), a),
			flacBlock(false, 2, len(b), b),
			flacBlock(flagLast, 1, len(c), c),
		}
		var declared uint16
		if declare {
			declared = uint16(len(blocks))
		}
		data := oggFLACFile(int(pageSegs), declared, blocks...)
		md, ok := oggFLACMetadata(bytes.NewReader(data))
		if !ok {
			t.Fatalf("declined an Ogg FLAC stream of %d bytes", len(data))
		}
		got, err := io.ReadAll(md)
		if err != nil {
			t.Fatal(err)
		}
		want := append([]byte("fLaC"), streamInfo()...)
		for _, b := range blocks {
			want = append(want, b...)
		}
		want = append(want, oggFLACTerminator[:]...)
		if !bytes.Equal(got, want) {
			t.Fatalf("read back %d bytes, want the %d laid out (pages of %d lacing values, declared %d, last flagged %v)",
				len(got), len(want), pageSegs, declared, flagLast)
		}
	})
}

// TestAnOggStreamDhowdenReadsKeepsItsTags: the Ogg FLAC reader takes only what
// dhowden cannot read. A Vorbis or Opus stream, alone or beside a FLAC one,
// is still dhowden's, which returns the first comment packet it meets.
func TestAnOggStreamDhowdenReadsKeepsItsTags(t *testing.T) {
	vorbis := oggLogicalStream(3, 255, vorbisIdent(), vorbisCommentPacket("TITLE=Vorbis"), []byte{0, 1, 2})
	opus := oggLogicalStream(4, 255, append([]byte("OpusHead"), make([]byte, 11)...), opusCommentPacket("TITLE=Opus"), []byte{0, 1, 2})
	flac := oggLogicalStream(5, 255, oggFLACMappingPacket(1), commentBlock(true, "TITLE=Flac"), oggFLACAudioPacket())
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"Vorbis", joinPages(vorbis...), "Vorbis"},
		{"Opus", joinPages(opus...), "Opus"},
		{"FLAC beside Vorbis", joinPages(append([][]byte{flac[0], vorbis[0]}, append(flac[1:], vorbis[1:]...)...)...), "Vorbis"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tr := extractBytesAs(t, "a.ogg", tc.data); tr.Title != tc.want {
				t.Errorf("Title = %q, want %q", tr.Title, tc.want)
			}
		})
	}
}

// TestScanner_V18_OggFLACRowJoinsTheDelta_OtherRowsOnlyStamp is the upgrade,
// end to end: rows a v17 bridge indexed re-extract once. The Ogg FLAC row
// gains its tags and its indexed_at advances, which is what puts it in every
// paired device's delta; an Ogg Vorbis, a FLAC and an MP3 row re-extract
// byte-identical and are only stamped.
func TestScanner_V18_OggFLACRowJoinsTheDelta_OtherRowsOnlyStamp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "flac.oga"), oggFixture(t, "ffmpeg_pages.oga"), 0o644); err != nil {
		t.Fatal(err)
	}
	vorbis := joinPages(oggLogicalStream(3, 255, vorbisIdent(), vorbisCommentPacket("TITLE=Vorbis", "ARTIST=V"), []byte{0, 1, 2})...)
	if err := os.WriteFile(filepath.Join(root, "vorbis.ogg"), vorbis, 0o644); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, filepath.Join(root, "native.flac"), 44100, 16, map[string]string{"TITLE": "Native", "ARTIST": "N"})
	writeMinimalMP3(t, filepath.Join(root, "plain.mp3"), map[string]string{"title": "Plain", "artist": "P"})
	store, sc := newDiscArtScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	// What a v17 bridge left: every row stamped a version behind, and the Ogg
	// FLAC row as the old extractor made it, from the path alone.
	info, err := os.Stat(filepath.Join(root, "flac.oga"))
	if err != nil {
		t.Fatal(err)
	}
	old := Track{Path: "flac.oga", Size: info.Size(), ModTime: info.ModTime().UTC(), Codec: "OGG"}
	fillFromPath(&old, "flac.oga", false)
	oldJSON, err := marshalForStorage(&old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec("UPDATE tracks SET tags_json = ? WHERE path = ?", string(oldJSON), "flac.oga"); err != nil {
		t.Fatalf("munge the Ogg FLAC row: %v", err)
	}
	if _, err := store.db.Exec("UPDATE tracks SET extractor_version = ?", ExtractorVersion-1); err != nil {
		t.Fatalf("munge the stamps: %v", err)
	}
	rows := []string{"flac.oga", "vorbis.ogg", "native.flac", "plain.mp3"}
	before := map[string]int64{}
	for _, rel := range rows {
		before[rel] = trackIndexedAt(t, store, rel)
	}

	scanOnce(t, sc, "v18")

	got, err := store.GetTrack(ctx, "flac.oga")
	if err != nil || got == nil {
		t.Fatalf("GetTrack(flac.oga): err=%v nil=%v", err, got == nil)
	}
	if got.Title != "Ogg Title" || got.Artist != "Ogg Artist" {
		t.Errorf("the Ogg FLAC row's title / artist = %q / %q after the re-extract, want the file's", got.Title, got.Artist)
	}
	if after := trackIndexedAt(t, store, "flac.oga"); after <= before["flac.oga"] {
		t.Errorf("the Ogg FLAC row's indexed_at did not advance (%d -> %d): no device would pull its tags",
			before["flac.oga"], after)
	}
	for _, rel := range rows[1:] {
		if after := trackIndexedAt(t, store, rel); after != before[rel] {
			t.Errorf("%s: indexed_at moved (%d -> %d); the bump must not put an unchanged row in the delta",
				rel, before[rel], after)
		}
	}
	for _, rel := range rows {
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
	}
}
