package manifest

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/dhowden/tag"
)

// countingStream is a stream over data that counts the Read calls reaching
// it, as a file counts read(2) calls. ReadAt is bytes.Reader's own and is not
// counted: the guards read by offset.
type countingStream struct {
	*bytes.Reader
	reads int
}

func (c *countingStream) Read(p []byte) (int, error) {
	c.reads++
	return c.Reader.Read(p)
}

// unsynchronisedTag is an ID3v2.4 tag carrying the unsynchronisation flag and
// holding TIT2 "Title" and a TXXX frame whose value is n bytes, none of them
// 0xFF, so the flag changes no byte of it.
func unsynchronisedTag(n int) []byte {
	p := append([]byte{3}, "big\x00"...)
	for i := range n {
		p = append(p, 'a'+byte(i%26))
	}
	body := append(textFrameBytes(4, "TIT2", "Title"), id3v2FrameBytes(4, "TXXX", uint32(len(p)), 0, p)...)
	return id3v2TagBytes(4, 0x80, body)
}

// TestAnUnsynchronisedID3v2TagIsReadAPageAtATime is the regression test for
// backlog B117, on both routes a file takes to dhowden: tag.ReadFrom (an MP3)
// and tag.ReadID3v2Tags at a DSF's metadata pointer. dhowden's
// unsynchroniser reads its stream one byte per Read call, and on a file each
// was a read(2): on main a 1 MiB tag reached its stream in over a million
// reads, and a 10 MB one took seconds to extract. Read a page at a time, a
// tag costs its length over 4096 reads and a few more.
func TestAnUnsynchronisedID3v2TagIsReadAPageAtATime(t *testing.T) {
	const page = 4096
	tg := unsynchronisedTag(1 << 20)
	for _, c := range []struct {
		name    string
		data    []byte
		extract func(io.ReadSeeker, *Track) error
	}{
		{"an MP3, through tag.ReadFrom", append(append([]byte{}, tg...), mp3Audio()...),
			func(rs io.ReadSeeker, tr *Track) error {
				return extractViaDhowdenFromReader(rs, "x.mp3", tr, &ExtractContext{})
			}},
		{"a DSF, through tag.ReadID3v2Tags", dsfWithID3(tg),
			func(rs io.ReadSeeker, tr *Track) error {
				return extractDSFFromReader(rs, "x.dsf", tr, &ExtractContext{})
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &countingStream{Reader: bytes.NewReader(c.data)}
			var tr Track
			if err := c.extract(s, &tr); err != nil {
				t.Fatal(err)
			}
			if tr.Title != "Title" {
				t.Fatalf("premise: the tag was read; Title = %q", tr.Title)
			}
			if limit := len(tg)/page + 16; s.reads > limit {
				t.Errorf("%d reads reached the stream for a %d-byte unsynchronised tag; a page at a time needs at most %d",
					s.reads, len(tg), limit)
			}
		})
	}
}

// dhowdenBufferCorpus is every kind of stream dhowden reads: the real files in
// testdata (MP3, DSF, AIFF and WAV tags from Picard and ffmpeg; iTunes' and
// Picard's M4A atoms; Ogg FLAC pages), the ID3v2 shapes the guard was built
// from, a FLAC stream, a file holding only an ID3v1 tag (dhowden seeks from
// the end), a DSF (it seeks to the metadata pointer) and unsynchronised tags.
func dhowdenBufferCorpus(t *testing.T) map[string][]byte {
	t.Helper()
	corpus := map[string][]byte{}
	for _, dir := range []string{"id3", "m4a", "ogg"} {
		entries, err := os.ReadDir(filepath.Join("testdata", dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join("testdata", dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			corpus[dir+"/"+e.Name()] = b
		}
	}
	for _, s := range id3v2Shapes() {
		corpus["shape: "+s.name] = s.data
	}
	picture := flacPictureBody("image/jpeg", "cover", 6000, payloadOf(6000))
	corpus["a FLAC stream"] = flacStream(streamInfo(), commentBlock(false, "TITLE=t", "ARTIST=a"),
		flacBlock(true, 6, len(picture), picture))
	id3v1 := append(make([]byte, 300), "TAG"...)
	corpus["an ID3v1 tag alone"] = append(id3v1, make([]byte, 125)...)
	corpus["a DSF"] = dsfWithID3(textFrameBytes(4, "TIT2", "t"))
	corpus["an unsynchronised tag"] = append(unsynchronisedTag(3*dhowdenReadBufferSize+17), mp3Audio()...)
	corpus["a DSF with an unsynchronised tag"] = dsfWithID3(unsynchronisedTag(dhowdenReadBufferSize - 1))
	return corpus
}

// requireSameThroughTheBuffer fails unless read, over data through a
// dhowdenReadBuffer, answers as it does over data bare: the same error, raw map
// and picture, and a stream released where the bare one was left.
func requireSameThroughTheBuffer(t *testing.T, data []byte, read func(io.ReadSeeker) (tag.Metadata, error)) {
	t.Helper()
	bare, under := bytes.NewReader(data), bytes.NewReader(data)
	m1, e1 := read(bare)
	buffered, release := newDhowdenReadBuffer(under)
	m2, e2 := read(buffered)
	release()
	if (e1 == nil) != (e2 == nil) || (e1 != nil && e1.Error() != e2.Error()) {
		t.Fatalf("bare: %v; through the buffer: %v", e1, e2)
	}
	if e1 == nil && (!reflect.DeepEqual(m1.Raw(), m2.Raw()) || !reflect.DeepEqual(m1.Picture(), m2.Picture())) {
		t.Errorf("the tags read through the buffer differ:\nbare     %v\nbuffered %v", m1.Raw(), m2.Raw())
	}
	p1, _ := bare.Seek(0, io.SeekCurrent)
	p2, _ := under.Seek(0, io.SeekCurrent)
	if p1 != p2 {
		t.Errorf("the released stream is at %d, the bare one at %d", p2, p1)
	}
}

// TestDhowdenReadsTheSameThroughTheBuffer pins that the buffer is invisible to
// dhowden: over every kind of stream it reads, tag.ReadFrom (and
// tag.ReadID3v2Tags, where the stream opens with a tag) answers through a
// dhowdenReadBuffer exactly as over the bare stream, which is what keeps the
// guards, which walk the bare stream, seeing what dhowden reads.
func TestDhowdenReadsTheSameThroughTheBuffer(t *testing.T) {
	for name, data := range dhowdenBufferCorpus(t) {
		t.Run(name, func(t *testing.T) {
			requireSameThroughTheBuffer(t, data, tag.ReadFrom)
			if bytes.HasPrefix(data, []byte("ID3")) {
				requireSameThroughTheBuffer(t, data, tag.ReadID3v2Tags)
			}
		})
	}
}

// FuzzDhowdenReadBufferReadsAsItsStreamDoes runs a tape of reads and seeks
// against a dhowdenReadBuffer over a stream and against the same stream bare,
// from the same offset, and requires the two to hold the same bytes in the
// same places: io.ReadFull of n bytes answers the same bytes, count and error
// on both; a single Read through the buffer (which may be short, as any
// reader's may) returns bytes the bare stream holds next, and nothing only at
// its end, with io.EOF; a seek from the start, the current offset or the end
// answers the same offset, or fails on both. Then release must leave the
// stream where the bare one is. Each tape step is two bytes: the operation and
// its argument.
func FuzzDhowdenReadBufferReadsAsItsStreamDoes(f *testing.F) {
	f.Add([]byte("0123456789abcdef"), []byte{0, 5, 3, 0xF5, 0, 11, 4, 0x80, 5, 3, 5, 0}, uint16(3))
	f.Add(make([]byte, 3*dhowdenReadBufferSize+5), []byte{0, 0xFF, 1, 0x10, 5, 200, 3, 0xF0, 0, 0xFF, 2, 7, 1, 0}, uint16(0))
	f.Add(unsynchronisedTag(dhowdenReadBufferSize), []byte{0, 11, 3, 0xF5, 1, 0x40, 4, 0xFF, 5, 200, 3, 0}, uint16(0))
	f.Fuzz(func(t *testing.T, data, tape []byte, start uint16) {
		bare, under := bytes.NewReader(data), bytes.NewReader(data)
		at := int64(start) % int64(len(data)+1)
		if _, err := bare.Seek(at, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := under.Seek(at, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		buffered, release := newDhowdenReadBuffer(under)
		for i := 0; i+1 < len(tape); i += 2 {
			tapeStep(t, i/2, bare, buffered, tape[i]%6, tape[i+1])
		}
		release()
		p1, _ := bare.Seek(0, io.SeekCurrent)
		p2, _ := under.Seek(0, io.SeekCurrent)
		if p1 != p2 {
			t.Fatalf("after release the stream is at %d, the bare one at %d", p2, p1)
		}
	})
}

// tapeStep is one step of the fuzz target's tape, done to both streams: op 0
// and 1 read n and a buffer's length plus n bytes in full, op 5 is one Read of
// up to n bytes, and op 2, 3 and 4 seek from the start, the current offset
// (possibly to before the start) and the end.
func tapeStep(t *testing.T, step int, bare, buffered io.ReadSeeker, op, arg byte) {
	t.Helper()
	switch op {
	case 0, 1:
		n := int(arg)
		if op == 1 {
			n += dhowdenReadBufferSize
		}
		p, q := make([]byte, n), make([]byte, n)
		n1, e1 := io.ReadFull(bare, p)
		n2, e2 := io.ReadFull(buffered, q)
		if n1 != n2 || !bytes.Equal(p[:n1], q[:n2]) || e1 != e2 {
			t.Fatalf("step %d: ReadFull(%d) = %d, %v bare and %d, %v through the buffer", step, n, n1, e1, n2, e2)
		}
	case 5:
		oneRead(t, step, bare, buffered, int(arg))
	default:
		whence := int(op - 2)
		off := int64(int8(arg)) * 97
		if whence == io.SeekStart {
			off = int64(arg) * 97
		}
		o1, e1 := bare.Seek(off, whence)
		o2, e2 := buffered.Seek(off, whence)
		if (e1 == nil) != (e2 == nil) || (e1 == nil && o1 != o2) {
			t.Fatalf("step %d: Seek(%d, %d) = %d, %v bare and %d, %v through the buffer", step, off, whence, o1, e1, o2, e2)
		}
	}
}

// oneRead is tapeStep's single Read of up to n bytes through the buffer,
// checked against the bare stream, which is then moved on by what it
// returned.
func oneRead(t *testing.T, step int, bare, buffered io.Reader, n int) {
	t.Helper()
	q := make([]byte, n)
	n2, e2 := buffered.Read(q)
	if n == 0 {
		if _, e1 := bare.Read(q); e1 != e2 {
			t.Fatalf("step %d: an empty Read answers %v bare and %v through the buffer", step, e1, e2)
		}
		return
	}
	p := make([]byte, n2)
	if n1, _ := io.ReadFull(bare, p); n1 != n2 || !bytes.Equal(p, q[:n2]) {
		t.Fatalf("step %d: a Read through the buffer returned %d bytes the bare stream does not hold there", step, n2)
	}
	if n2 == 0 {
		if nb, eb := bare.Read(make([]byte, 1)); e2 != io.EOF || nb != 0 || eb != io.EOF {
			t.Fatalf("step %d: a Read through the buffer returned nothing (%v) where the bare stream is not at its end", step, e2)
		}
	}
}
