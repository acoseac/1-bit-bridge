package manifest

import (
	"bytes"
	"io"
	"testing"
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
