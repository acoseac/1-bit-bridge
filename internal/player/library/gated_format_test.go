package library

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// wavpackHead and apeHead are the first bytes of a WavPack file (a block's
// "wvpk" header, version 0x410) and of a Monkey's Audio file (the "MAC "
// descriptor, version 3990), padded: what the decoders inspect.
func wavpackHead() []byte {
	b := make([]byte, 4096)
	copy(b, "wvpk")
	binary.LittleEndian.PutUint32(b[4:], 4096-8)
	binary.LittleEndian.PutUint16(b[8:], 0x410)
	return b
}

func apeHead() []byte {
	b := make([]byte, 4096)
	copy(b, "MAC ")
	binary.LittleEndian.PutUint16(b[4:], 3990)
	return b
}

// TestTheGatedFormatsAreRefusedWithTheDecodersReason: a WavPack or Monkey's
// Audio file is refused at the queue's check (backlog B489), at the Insert's
// membership check and at an open, before any file is opened, with the
// decoders' own reason for content they cannot open, unknown_container, which
// is what the content of such a file gets from them: the player decodes
// neither format. By the path, whatever its case.
func TestTheGatedFormatsAreRefusedWithTheDecodersReason(t *testing.T) {
	if ReasonGatedFormat != engine.Reason(decode.ReasonUnknownContainer) {
		t.Fatalf("ReasonGatedFormat is %q, the decoders' reason %q", ReasonGatedFormat, decode.ReasonUnknownContainer)
	}
	for _, tc := range []struct {
		what string
		data []byte
	}{{"WavPack", wavpackHead()}, {"Monkey's Audio", apeHead()}} {
		_, err := decode.Open(bytes.NewReader(tc.data))
		r, ok := decode.AsRefusal(err)
		if !ok || r.Reason != decode.ReasonUnknownContainer {
			t.Errorf("the decoders open a %s file's content as %v, want the refusal %s", tc.what, err, decode.ReasonUnknownContainer)
		}
	}

	f := newLibraryFixture(t)
	opened := 0
	f.r.cfg.Seams = &Seams{BeforeOpen: func() { opened++ }}
	for _, tc := range []struct {
		rel  string
		data []byte
	}{
		{"Artist/Album/01 a.wv", wavpackHead()},
		{"Artist/Album/02 b.WV", wavpackHead()},
		{"Artist/Album/03 c.ape", apeHead()},
	} {
		ref := f.add(tc.rel, tc.data)
		if got := member(t, f.r, ref.String()); got != ReasonGatedFormat {
			t.Errorf("Member of %s: %q, want %q", tc.rel, got, ReasonGatedFormat)
		}
		c, err := f.open(ref)
		f.refusedWith(tc.rel, c, err, ReasonGatedFormat)
	}
	if opened != 0 {
		t.Errorf("the queue's check opened %d files of a format it refuses", opened)
	}
	// A FLAC beside them is a member.
	flac := f.add("Artist/Album/04 d.flac", []byte("fLaC"))
	if got := member(t, f.r, flac.String()); got != "" {
		t.Errorf("a FLAC beside them: %s", got)
	}
}
