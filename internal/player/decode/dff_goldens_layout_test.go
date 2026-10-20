package decode_test

import (
	"encoding/binary"
	"strings"
	"testing"
)

// These tests read the committed DFF goldens' bytes and check what no
// decoding test can see: the demuxer skips COMT and MANF by their sizes, so
// a generator that lays them out wrongly, or drops MANF, still decodes to the
// pattern (round 3: reverting round 2's COMT padding left the suite green).
// They never call the generator. To check a generator change, regenerate
// into a scratch directory and point them at it:
//
//	python3 scripts/player-dsd-goldens/generate.py --out DIR
//	go test ./internal/player/decode -args -dsd-goldens=DIR

// dffGoldens returns every DFF golden, accepted and refused, with its bytes.
func dffGoldens(t *testing.T) map[string][]byte {
	t.Helper()
	g := loadDSDGoldens(t)
	out := map[string][]byte{}
	for _, f := range g.Fixtures {
		if strings.HasSuffix(f.File, ".dff") {
			_, raw := g.fixture(t, f.File)
			out[f.File] = raw
		}
	}
	if len(out) != 5 {
		t.Fatalf("%d DFF goldens, want 5", len(out))
	}
	return out
}

// DSDIFF 1.5 §3.6: COMT is numComments, then each comment's 14-byte header
// (a 6-byte time stamp, cmtType, cmtRef, a 4-byte count), count bytes of
// text, and a pad byte after an odd-length text that the body's size counts
// and count does not. Each golden carries one comment of 23 bytes, so its
// COMT body is 2 + 14 + 23 + 1 = 40 bytes and the chunk needs no pad.
func TestDFFGoldensCOMTFollowsSection36(t *testing.T) {
	const text = "B470 golden, odd length"
	for name, raw := range dffGoldens(t) {
		var comt []byte
		for _, c := range dffTopChunks(t, raw) {
			if string(c[0:4]) == "COMT" {
				comt = c
			}
		}
		if comt == nil {
			t.Errorf("%s: no COMT chunk", name)
			continue
		}
		size := binary.BigEndian.Uint64(comt[4:12])
		body := comt[12:]
		if size > uint64(len(body)) || size < 16 {
			t.Errorf("%s: COMT declares %d bytes, %d follow its header", name, size, len(body))
			continue
		}
		body = body[:size]
		if n := binary.BigEndian.Uint16(body[0:2]); n != 1 {
			t.Errorf("%s: COMT holds %d comments, want 1", name, n)
			continue
		}
		count := uint64(binary.BigEndian.Uint32(body[12:16]))
		if count != uint64(len(text)) || count%2 != 1 {
			t.Errorf("%s: comment count %d, want the odd %d", name, count, len(text))
			continue
		}
		if want := 2 + 14 + count + count&1; size != want {
			t.Errorf("%s: COMT declares %d bytes; its one comment of %d bytes and its pad need %d", name, size, count, want)
			continue
		}
		if got := string(body[16 : 16+count]); got != text {
			t.Errorf("%s: comment text %q, want %q", name, got, text)
		}
		if pad := body[16+count]; pad != 0 {
			t.Errorf("%s: the byte after the comment's text is %#02x, want the 0x00 pad", name, pad)
		}
		if len(comt) != 12+int(size) {
			t.Errorf("%s: an even COMT carries no pad byte of its own; %d bytes follow its header", name, len(comt)-12)
		}
	}
}

// DSDIFF 1.5 §2.3 puts a pad byte after an odd body, uncounted by its size.
// The demuxer has two walks that must skip it before the sound data: the
// form's (every golden's 7-byte MANF before PROP) and PROP's (the 19-byte
// CMPR of "not compressed", in every golden but dst.dff, whose "DST Encoded"
// CMPR is 16 bytes). With no odd chunk, a walk that forgets the pad passes.
func TestDFFGoldensGiveTheWalksAnOddChunk(t *testing.T) {
	for name, raw := range dffGoldens(t) {
		var prop []byte
		oddBeforeProp := false
		for _, c := range dffTopChunks(t, raw) {
			if string(c[0:4]) == "PROP" {
				prop = c
				break
			}
			if binary.BigEndian.Uint64(c[4:12])%2 == 1 {
				oddBeforeProp = true
			}
		}
		if prop == nil {
			t.Errorf("%s: no PROP chunk", name)
			continue
		}
		if !oddBeforeProp {
			t.Errorf("%s: no odd chunk before PROP for the form's walk to pad past", name)
		}
		oddInProp := false
		// PROP's 12-byte header and "SND " are the form's 16-byte prefix in
		// shape, so the same splitter reads its sub-chunks.
		for _, c := range dffTopChunks(t, prop) {
			if binary.BigEndian.Uint64(c[4:12])%2 == 1 {
				oddInProp = true
			}
		}
		if want := name != "dst.dff"; oddInProp != want {
			t.Errorf("%s: an odd sub-chunk in PROP is %v, want %v", name, oddInProp, want)
		}
	}
}
