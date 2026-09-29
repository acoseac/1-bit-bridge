package manifest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/dhowden/tag"
)

// --- ID3v2 builders ----------------------------------------------------------

// id3v2HeaderBytes is a 10-byte ID3v2 header of the given version and flags
// declaring size, in synchsafe bytes.
func id3v2HeaderBytes(version, flags byte, size uint32) []byte {
	h := []byte{'I', 'D', '3', version, 0, flags, 0, 0, 0, 0}
	writeSyncSafeSize(h[6:10], size)
	return h
}

// id3v2TagBytes is an ID3v2 tag whose size field covers body exactly.
func id3v2TagBytes(version, flags byte, body []byte) []byte {
	return append(id3v2HeaderBytes(version, flags, uint32(len(body))), body...)
}

// id3v2FrameBytes is one frame: its id, the size it declares (3 bytes big
// endian in version 2, 4 in version 3, 4 synchsafe in version 4; versions 3
// and 4 then carry two flag bytes, the second, the format flags, being
// fmtFlags) and payload, which need not be declared long.
func id3v2FrameBytes(version byte, id string, declared uint32, fmtFlags byte, payload []byte) []byte {
	out := []byte(id)
	switch version {
	case 2:
		out = append(out, byte(declared>>16), byte(declared>>8), byte(declared))
	case 3:
		out = binary.BigEndian.AppendUint32(out, declared)
		out = append(out, 0, fmtFlags)
	default:
		var s [4]byte
		writeSyncSafeSize(s[:], declared)
		out = append(out, s[:]...)
		out = append(out, 0, fmtFlags)
	}
	return append(out, payload...)
}

// textFrameBytes is a text frame of version holding s, encoded as UTF-8 in
// versions 3 and 4 and as ISO-8859-1 in version 2.
func textFrameBytes(version byte, id, s string) []byte {
	enc := byte(3)
	if version == 2 {
		enc = 0
	}
	p := append([]byte{enc}, s...)
	return id3v2FrameBytes(version, id, uint32(len(p)), 0, p)
}

// repeatedTIT2 is n copies of one 12-byte version 4 TIT2 frame: the shape
// backlog B101 measured.
func repeatedTIT2(n int) []byte { return bytes.Repeat(textFrameBytes(4, "TIT2", "x"), n) }

// repeatedFramesTag is a whole tag of 5,000 copies of one frame: past the
// renaming bound, and on the old code about 190 MB of dhowden's renaming
// strings from a 60 KB tag (the regression test and the fuzz seeds).
func repeatedFramesTag() []byte { return id3v2TagBytes(4, 0, repeatedTIT2(5000)) }

// distinctFrameID is the i-th of a run of 4-byte ids, none repeated.
func distinctFrameID(i int) string {
	b := make([]byte, 4)
	for k := 3; k >= 0; k-- {
		b[k] = byte('!' + i%94)
		i /= 94
	}
	return string(b)
}

// distinctFrames is n version 4 frames of 11 bytes, each id different.
func distinctFrames(n int) []byte {
	out := make([]byte, 0, 11*n)
	for i := range n {
		out = append(out, id3v2FrameBytes(4, distinctFrameID(i), 1, 0, []byte{0})...)
	}
	return out
}

// mp3Audio is one MPEG-1 Layer III frame, what follows a tag in an MP3.
func mp3Audio() []byte { return append([]byte{0xFF, 0xFB, 0x90, 0x64}, make([]byte, 140)...) }

// dsfWithID3 is a DSF container whose metadata pointer names id3, which it
// ends with.
func dsfWithID3(id3 []byte) []byte {
	var fmtChunk [52]byte
	copy(fmtChunk[0:4], "fmt ")
	binary.LittleEndian.PutUint64(fmtChunk[4:12], 52)
	binary.LittleEndian.PutUint32(fmtChunk[12:16], 1)
	binary.LittleEndian.PutUint32(fmtChunk[20:24], 2)
	binary.LittleEndian.PutUint32(fmtChunk[24:28], 2)
	binary.LittleEndian.PutUint32(fmtChunk[28:32], 2822400)
	binary.LittleEndian.PutUint32(fmtChunk[32:36], 1)
	binary.LittleEndian.PutUint64(fmtChunk[36:44], 2822400*5)
	binary.LittleEndian.PutUint32(fmtChunk[44:48], 4096)
	var data [12]byte
	copy(data[0:4], "data")
	binary.LittleEndian.PutUint64(data[4:12], 12)
	var dsd [28]byte
	copy(dsd[0:4], "DSD ")
	binary.LittleEndian.PutUint64(dsd[4:12], 28)
	binary.LittleEndian.PutUint64(dsd[12:20], uint64(28+52+12+len(id3)))
	binary.LittleEndian.PutUint64(dsd[20:28], 28+52+12)
	out := append(dsd[:], fmtChunk[:]...)
	out = append(out, data[:]...)
	return append(out, id3...)
}

// --- the walk against dhowden -------------------------------------------------

// unboundedID3v2Walk counts everything, so a test can compare what it counts
// with what dhowden stores.
func unboundedID3v2Walk() *id3v2Walk {
	return &id3v2Walk{maxFrames: math.MaxInt, maxLookups: math.MaxUint64, copies: map[string]int{}}
}

// dhowdenFrameCounts is what dhowden stored reading the tag data starts with,
// per frame id, and false when its read failed (it then stores nothing).
func dhowdenFrameCounts(data []byte) (map[string]int, bool) {
	m, err := tag.ReadID3v2Tags(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	idLen := 4
	if m.Format() == tag.ID3v2_2 {
		idLen = 3
	}
	counts := map[string]int{}
	for key := range m.Raw() {
		// A repeat is keyed id_0, id_1, …: every key starts with its id.
		counts[key[:min(idLen, len(key))]]++
	}
	return counts, true
}

// walkDisagreement compares what the walk counts in data with what dhowden
// stores from it, when dhowden reads it. They agree when every id has the same
// count, except that the walk may count one frame more in all: the last,
// which dhowden drops for an id it does not know (validID3Frame). ok is false
// when dhowden's read failed, and there is nothing to compare.
func walkDisagreement(data []byte) (diff string, ok bool) {
	stored, ok := dhowdenFrameCounts(data)
	if !ok {
		return "", false
	}
	w := unboundedID3v2Walk()
	w.tag(bytes.NewReader(data))
	ids := map[string]bool{}
	for id := range w.copies {
		ids[id] = true
	}
	for id := range stored {
		ids[id] = true
	}
	extra := 0
	var diffs []string
	for id := range ids {
		d := w.copies[id] - stored[id]
		if d < 0 || d > 1 {
			diffs = append(diffs, fmt.Sprintf("%q: walk %d, dhowden %d", id, w.copies[id], stored[id]))
		}
		extra += max(d, 0)
	}
	sort.Strings(diffs)
	if extra > 1 {
		diffs = append(diffs, fmt.Sprintf("the walk counts %d frames dhowden does not store", extra))
	}
	return strings.Join(diffs, "; "), true
}

// requireWalkAgreesWithDhowden fails when the walk and dhowden disagree about
// data, or when dhowden cannot read it (the shapes it is given must be read).
func requireWalkAgreesWithDhowden(t *testing.T, data []byte) {
	t.Helper()
	diff, ok := walkDisagreement(data)
	if !ok {
		t.Fatal("dhowden could not read the tag, so the walk has nothing to agree with")
	}
	if diff != "" {
		t.Fatalf("the walk does not count what dhowden stores: %s", diff)
	}
}

// id3v2Shape is a tag the walk must follow as dhowden reads it. hidden builds
// the shape with n copies of a frame dhowden reads in a place a walk that
// followed declared sizes, or skipped the unsynchronisation filter, would not
// look; nil when the shape hides none.
type id3v2Shape struct {
	name   string
	data   []byte
	hidden func(n int) []byte
}

// id3v2Shapes are the ways dhowden's reads of a tag differ from its declared
// layout, each read by dhowden without error.
func id3v2Shapes() []id3v2Shape {
	padding := make([]byte, 64) // a zero frame header: dhowden stops there
	// withPadding declares a tag longer than body by extra (so dhowden's
	// offset, which counts declared sizes, stays under it while it reads
	// frames the declared sizes skip), ending in padding.
	withPadding := func(version, flags byte, body []byte, extra int) []byte {
		body = append(append([]byte{}, body...), padding...)
		return append(id3v2HeaderBytes(version, flags, uint32(len(body)+extra)), body...)
	}
	return []id3v2Shape{
		{name: "version 2.2 frames, one id repeated", data: id3v2TagBytes(2, 0, bytes.Join([][]byte{
			textFrameBytes(2, "TT2", "a"), textFrameBytes(2, "TT2", "b"), textFrameBytes(2, "TP1", "c"), textFrameBytes(2, "TT2", "d"),
		}, nil))},
		{name: "version 2.3 TXXX frames", data: id3v2TagBytes(3, 0, bytes.Join([][]byte{
			id3v2FrameBytes(3, "TXXX", 6, 0, []byte("\x03a\x00b\x00c")),
			id3v2FrameBytes(3, "TXXX", 4, 0, []byte("\x03d\x00e")),
			id3v2FrameBytes(3, "TXXX", 4, 0, []byte("\x03f\x00g")),
			textFrameBytes(3, "TIT2", "t"),
		}, nil))},
		{
			// Version 3 counts the bytes after the 4-byte length and adds
			// only those to its offset.
			name: "version 2.3 with an extended header",
			data: id3v2TagBytes(3, 0x40, append([]byte{0, 0, 0, 6, 0, 0, 0, 0, 0, 0},
				append(textFrameBytes(3, "TIT2", "t"), textFrameBytes(3, "TPE1", "a")...)...)),
		},
		{
			// Version 4 counts the length's own 4 bytes.
			name: "version 2.4 with an extended header",
			data: id3v2TagBytes(4, 0x40, append([]byte{0, 0, 0, 6, 1, 0},
				append(textFrameBytes(4, "TIT2", "t"), textFrameBytes(4, "TIT2", "u")...)...)),
		},
		{
			// A length under 4 wraps below zero: readBytes reads nothing for
			// it, so the frames start right after the length.
			name: "version 2.4 with an extended header whose length wraps",
			data: id3v2TagBytes(4, 0x40, append([]byte{0, 0, 0, 2},
				append(textFrameBytes(4, "TIT2", "t"), textFrameBytes(4, "TIT2", "u")...)...)),
			hidden: func(n int) []byte {
				return id3v2TagBytes(4, 0x40, append([]byte{0, 0, 0, 2}, repeatedTIT2(n)...))
			},
		},
		{
			// Compression in version 3: 4 bytes of decompressed size, and
			// the payload is 4 shorter than declared.
			name: "version 2.3 compression",
			data: id3v2TagBytes(3, 0, bytes.Join([][]byte{
				id3v2FrameBytes(3, "PRIV", 10, 0x80, []byte("\x00\x00\x00\x09owner\x00")),
				textFrameBytes(3, "TIT2", "t"),
			}, nil)),
		},
		{
			// Declaring 2, compressed: 2-4 wraps, readBytes reads nothing,
			// and the next header is read right after the size field.
			name: "version 2.3 compression on a frame declaring under 4",
			data: withPadding(3, 0, bytes.Join([][]byte{
				id3v2FrameBytes(3, "PRIV", 2, 0x80, []byte("\x00\x00\x00\x00")),
				textFrameBytes(3, "TIT2", "t"), textFrameBytes(3, "TIT2", "u"),
			}, nil), 0),
			hidden: func(n int) []byte {
				body := append(id3v2FrameBytes(3, "PRIV", 2, 0x80, []byte("\x00\x00\x00\x00")),
					bytes.Repeat(textFrameBytes(3, "TIT2", "x"), n)...)
				return withPadding(3, 0, body, 0)
			},
		},
		{
			// A data length indicator shorter than the frame: dhowden reads
			// that many bytes, and the frame's declared rest as frames.
			name: "version 2.4 data length indicator shorter than the frame",
			data: func() []byte {
				inner := append(textFrameBytes(4, "TIT2", "t"), textFrameBytes(4, "TIT2", "u")...)
				a := id3v2FrameBytes(4, "PRIV", uint32(4+1+len(inner)), 0x01, append([]byte{0, 0, 0, 1, 0}, inner...))
				return withPadding(4, 0, a, len(inner))
			}(),
			hidden: func(n int) []byte {
				inner := repeatedTIT2(n)
				a := id3v2FrameBytes(4, "PRIV", uint32(4+1+len(inner)), 0x01, append([]byte{0, 0, 0, 1, 0}, inner...))
				return withPadding(4, 0, a, len(inner))
			},
		},
		{
			// Longer than the frame: dhowden reads on into the next frame,
			// which it never sees as one.
			name: "version 2.4 data length indicator longer than the frame",
			data: withPadding(4, 0, bytes.Join([][]byte{
				id3v2FrameBytes(4, "PRIV", 5, 0x01, []byte{0, 0, 0, 13, 0}),
				textFrameBytes(4, "TIT2", "s"),
				textFrameBytes(4, "TPE1", "a"),
			}, nil), 0),
		},
		{
			// Compressed with an indicator in version 4: the indicator is the
			// length dhowden reads.
			name: "version 2.4 compression with a data length indicator",
			data: id3v2TagBytes(4, 0, bytes.Join([][]byte{
				id3v2FrameBytes(4, "PRIV", 8, 0x09, []byte{0, 0, 0, 4, 'a', 'b', 'c', 'd'}),
				textFrameBytes(4, "TIT2", "t"),
			}, nil)),
		},
		{
			// One byte of encryption method, and a payload 1 shorter.
			name: "version 2.3 encryption",
			data: id3v2TagBytes(3, 0, bytes.Join([][]byte{
				id3v2FrameBytes(3, "PRIV", 3, 0x40, []byte{0x80, 'a', 'b'}),
				textFrameBytes(3, "TIT2", "t"),
			}, nil)),
		},
		{
			name: "version 2.4 encryption",
			data: id3v2TagBytes(4, 0, bytes.Join([][]byte{
				id3v2FrameBytes(4, "PRIV", 3, 0x04, []byte{0x80, 'a', 'b'}),
				textFrameBytes(4, "TIT2", "t"),
			}, nil)),
		},
		{
			// Encryption takes 1 from a data length indicator of 0, which
			// wraps: readBytes reads nothing, and the method byte is all the
			// payload dhowden reads. Everywhere else the method byte and the
			// shorter payload add up to the same length.
			name: "version 2.4 encryption with a zero data length indicator",
			data: id3v2TagBytes(4, 0, bytes.Join([][]byte{
				id3v2FrameBytes(4, "PRIV", 5, 0x05, []byte{0, 0, 0, 0, 0x80}),
				textFrameBytes(4, "TIT2", "t"),
			}, nil)),
			hidden: func(n int) []byte {
				return id3v2TagBytes(4, 0, append(id3v2FrameBytes(4, "PRIV", 5, 0x05, []byte{0, 0, 0, 0, 0x80}), repeatedTIT2(n)...))
			},
		},
		{
			// dhowden drops a zero byte that follows an emitted 0xFF, across
			// frame boundaries: every FF 00 in a payload is one byte short of
			// what the declared sizes count.
			name: "version 2.4 unsynchronisation",
			data: id3v2TagBytes(4, 0x80, bytes.Join([][]byte{
				id3v2FrameBytes(4, "TIT2", 3, 0, []byte{3, 0xFF, 0x00, 'x'}),
				id3v2FrameBytes(4, "TIT2", 2, 0, []byte{3, 0xFF, 0x00}),
				id3v2FrameBytes(4, "TPE1", 2, 0, []byte{3, 'a'}),
			}, nil)),
			hidden: func(n int) []byte {
				// Each frame is a byte longer in the file than its declared
				// size, so the padding that ends the read is inside the size.
				frames := bytes.Repeat(id3v2FrameBytes(4, "TIT2", 2, 0, []byte{3, 0xFF, 0x00}), n)
				return id3v2TagBytes(4, 0x80, append(frames, make([]byte, 10)...))
			},
		},
		{
			// dhowden's synchsafe reads do not mask the high bit, so a size
			// byte past 0x7F reaches bits its neighbour holds: this tag's
			// size field reads 16,400, not 16.
			name: "a tag size with the high bit set",
			data: append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 0x80, 0x10},
				append(bytes.Repeat(textFrameBytes(4, "TIT2", "x"), 20), padding...)...),
		},
		{
			name: "padding ends the frames",
			data: id3v2TagBytes(4, 0, bytes.Join([][]byte{
				textFrameBytes(4, "TIT2", "t"), padding, textFrameBytes(4, "TIT2", "u"),
			}, nil)),
		},
		{
			// The last frame always runs 10 bytes past the size field (the
			// offset counts the header, the size does not): dhowden stores it
			// when it knows the id, and drops it otherwise.
			name: "a last frame with an id dhowden does not know",
			data: id3v2TagBytes(4, 0, append(textFrameBytes(4, "TIT2", "t"), id3v2FrameBytes(4, "ZZZZ", 1, 0, []byte{0})...)),
		},
	}
}

// TestTheID3v2WalkCountsWhatDhowdenStores pins the walk to dhowden's reads of
// every shape: the same frames, per id.
func TestTheID3v2WalkCountsWhatDhowdenStores(t *testing.T) {
	for _, s := range id3v2Shapes() {
		t.Run(s.name, func(t *testing.T) {
			requireWalkAgreesWithDhowden(t, s.data)
			if s.hidden != nil {
				requireWalkAgreesWithDhowden(t, s.hidden(40))
			}
		})
	}
}

// TestTheID3v2GuardFindsRepeatsWhereDhowdenReadsThem pins the dangerous
// direction: frames dhowden reads where the declared layout says there are
// none are counted, so a pile of repeats there is refused. Each shape reads
// its repeats as frames (TestTheID3v2WalkCountsWhatDhowdenStores, at 40).
func TestTheID3v2GuardFindsRepeatsWhereDhowdenReadsThem(t *testing.T) {
	for _, s := range id3v2Shapes() {
		if s.hidden == nil {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			if ok, _ := id3v2TagWithinBudget(bytes.NewReader(s.hidden(5000))); ok {
				t.Fatal("the guard passed 5,000 copies of a frame dhowden reads: it walked a different layout than dhowden's")
			}
		})
	}
}

// TestTheID3v2GuardPassesRepeatsDhowdenNeverReads pins the other direction: a
// pile of repeats dhowden does not read as frames is no reason to refuse the
// tag, and dhowden reads the tag's own frames as usual.
func TestTheID3v2GuardPassesRepeatsDhowdenNeverReads(t *testing.T) {
	pile := repeatedTIT2(5000)
	title := textFrameBytes(4, "TIT2", "t")
	cases := map[string][]byte{
		// dhowden's offset reaches the tag's size before the stream ends.
		"after the tag's size": append(id3v2TagBytes(4, 0, title), pile...),
		// A zero frame header ends dhowden's read.
		"after padding": id3v2TagBytes(4, 0, bytes.Join([][]byte{title, make([]byte, 10), pile}, nil)),
		// A frame's payload is one blob to dhowden, whatever it holds.
		"inside a frame's payload": id3v2TagBytes(4, 0, append(title, id3v2FrameBytes(4, "PRIV", uint32(len(pile)), 0, pile)...)),
		// The frame's declared size takes dhowden's offset past the tag's
		// size, while its data length indicator reads one byte: dhowden
		// stops, and never reaches the pile the stream holds next.
		"after a frame whose declared size ends the tag": id3v2TagBytes(4, 0, bytes.Join([][]byte{
			title,
			id3v2FrameBytes(4, "PRIV", uint32(4+1+len(pile)), 0x01, []byte{0, 0, 0, 1, 0}),
			pile,
		}, nil)),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if ok, refusal := id3v2TagWithinBudget(bytes.NewReader(data)); !ok {
				t.Fatalf("refused (%+v) a tag dhowden reads only a frame or two of", refusal)
			}
			requireWalkAgreesWithDhowden(t, data)
		})
	}
	// tag.ReadFrom reads an "ID3" stream whose bytes 4 to 8 say ftyp as MP4,
	// and a stream in a version dhowden refuses as nothing.
	for name, data := range map[string][]byte{
		"an ID3 stream read as MP4":       append([]byte("ID3\x04ftyp\x00\x00\x00"), repeatedTIT2(5000)...),
		"a version dhowden does not read": append([]byte("ID3\x05\x00\x00\x00\x03\x00\x00"), repeatedTIT2(5000)...),
	} {
		if ok, refusal := dhowdenID3v2WithinBudget(bytes.NewReader(data)); !ok {
			t.Errorf("%s: refused (%+v)", name, refusal)
		}
	}
}

// TestTheID3v2GuardHoldsAtItsBounds pins both bounds where they fall, and that
// the renaming bound is on the SUM of the lookups: a bound per id would let
// many ids each repeat up to it, and their lookups add up.
func TestTheID3v2GuardHoldsAtItsBounds(t *testing.T) {
	twoIDs := func(n int) []byte {
		return append(bytes.Repeat(textFrameBytes(4, "TIT2", "x"), n), bytes.Repeat(textFrameBytes(4, "TPE1", "x"), n)...)
	}
	cases := []struct {
		name string
		body []byte
		want id3v2Refusal // zero: passed
	}{
		{"2,048 copies of one frame", repeatedTIT2(2048), id3v2Refusal{}},
		{"2,049 copies of one frame", repeatedTIT2(2049),
			id3v2Refusal{What: id3v2RepeatsPastBound, Frame: "TIT2", Copies: 2049, Frames: 2049, Lookups: 2049 * 2048 / 2}},
		{"1,448 copies of each of two frames", twoIDs(1448), id3v2Refusal{}},
		{"1,449 copies of each of two frames", twoIDs(1449),
			id3v2Refusal{What: id3v2RepeatsPastBound, Frame: "TPE1", Copies: 1449, Frames: 2 * 1449, Lookups: 1449 * 1448}},
		{"65,536 distinct frames", distinctFrames(65536), id3v2Refusal{}},
		{"65,537 distinct frames", distinctFrames(65537),
			id3v2Refusal{What: id3v2FramesPastBound, Frame: distinctFrameID(65536), Copies: 1, Frames: 65537}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := id3v2TagBytes(4, 0, c.body)
			ok, refusal := id3v2TagWithinBudget(bytes.NewReader(data))
			if ok != (c.want.What == "") || refusal != c.want {
				t.Fatalf("id3v2TagWithinBudget = %v, %+v; want %v, %+v", ok, refusal, c.want.What == "", c.want)
			}
		})
	}
}

// TestATagTheID3v2GuardPassesMeetsTheAllocationProperty pins what sized the
// renaming bound: a tag at either bound extracts within extractionAllocLimit,
// the property every extractor fuzz target asserts, so the fuzzer never
// reports as a crasher a tag the guard lets dhowden read. dhowden's renaming
// builds a string per lookup, and at 2^23 lookups (the first draft's bound) a
// 49 KB file allocated 127 MB against a limit of 70 MB.
//
// The allocation is asserted without -race only: the race detector turns off
// the runtime's tiny allocator, so each of the renaming's small strings takes a
// slot of its own and the same extraction allocates about twice as much. The
// property is the nightly fuzz job's, which runs without -race, as the macOS
// and Windows legs of the ordinary suite do.
func TestATagTheID3v2GuardPassesMeetsTheAllocationProperty(t *testing.T) {
	for name, body := range map[string][]byte{
		"one frame at the renaming bound": repeatedTIT2(2048),
		"two frames at the renaming bound": append(bytes.Repeat(textFrameBytes(4, "TIT2", "x"), 1448),
			bytes.Repeat(textFrameBytes(4, "TPE1", "x"), 1448)...),
		"frames at the frames bound": distinctFrames(65536),
		// Both at once: one id at the renaming bound among the most frames.
		"both bounds": append(repeatedTIT2(2048), distinctFrames(65536-2048)...),
	} {
		t.Run(name, func(t *testing.T) {
			data := append(id3v2TagBytes(4, 0, body), mp3Audio()...)
			if ok, refusal := id3v2TagWithinBudget(bytes.NewReader(data)); !ok {
				t.Fatalf("refused a tag within both bounds: %+v", refusal)
			}
			if raceBuild {
				return
			}
			_, got := extractMeasured(t, "x.mp3", data, &ExtractContext{})
			if limit := extractionAllocLimit(len(data)); got > limit {
				t.Fatalf("extracting a %d-byte tag within both bounds allocated %d bytes, over the %d the fuzz targets allow",
					len(data), got, limit)
			}
			t.Logf("%d bytes allocated, limit %d", got, extractionAllocLimit(len(data)))
		})
	}
}

// TestAHeavilyTaggedFileStillReads is the over-strictness guard: a tag
// carrying more than heavy real tagging does reads its tags. Taggers repeat an
// id a handful of times (TXXX for every custom field, COMM, a scanned booklet's
// APIC pages, Windows Media Player's PRIV, DJ software's GEOB), and a chapter
// is one CHAP frame each; the files on the dev Mac, counted, carried at most 10
// frames and 3 copies of one id (TXXX).
func TestAHeavilyTaggedFileStillReads(t *testing.T) {
	var body []byte
	add := func(n int, frame func(i int) []byte) {
		for i := range n {
			body = append(body, frame(i)...)
		}
	}
	body = append(body, textFrameBytes(4, "TIT2", "t")...)
	add(60, func(i int) []byte {
		p := fmt.Appendf([]byte{3}, "field %d\x00value", i)
		return id3v2FrameBytes(4, "TXXX", uint32(len(p)), 0, p)
	})
	add(6, func(i int) []byte {
		p := fmt.Appendf([]byte{3, 'e', 'n', 'g'}, "iTun%d\x00text", i)
		return id3v2FrameBytes(4, "COMM", uint32(len(p)), 0, p)
	})
	add(30, func(i int) []byte {
		p := fmt.Appendf([]byte("\x03image/jpeg\x00\x05"), "page %d\x00", i)
		p = append(p, payloadOf(64)...)
		return id3v2FrameBytes(4, "APIC", uint32(len(p)), 0, p)
	})
	add(12, func(i int) []byte {
		p := fmt.Appendf(nil, "WM/Owner%d\x00data", i)
		return id3v2FrameBytes(4, "PRIV", uint32(len(p)), 0, p)
	})
	add(8, func(i int) []byte {
		p := fmt.Appendf([]byte{0}, "application/octet-stream\x00\x00Serato %d\x00data", i)
		return id3v2FrameBytes(4, "GEOB", uint32(len(p)), 0, p)
	})
	// A long audiobook's chapters, each its start and end time and offsets.
	add(1000, func(i int) []byte {
		p := append(fmt.Appendf(nil, "ch%d\x00", i), make([]byte, 16)...)
		return id3v2FrameBytes(4, "CHAP", uint32(len(p)), 0, p)
	})
	body = append(body, textFrameBytes(4, "TALB", "a")...)
	data := append(id3v2TagBytes(4, 0, body), mp3Audio()...)
	if ok, refusal := id3v2TagWithinBudget(bytes.NewReader(data)); !ok {
		t.Fatalf("refused a heavily tagged file: %+v", refusal)
	}
	tr := requireBoundedExtraction(t, "x.mp3", data, &ExtractContext{})
	if tr.Title != "t" || tr.Album != "a" {
		t.Errorf("Title, Album = %q, %q; want the tag's (t, a)", tr.Title, tr.Album)
	}
}

// TestNoRepeatedID3v2FrameMakesAnExtractionUnbounded is the regression test
// for backlog B101, through every way dhowden reads an ID3v2 tag: tag.ReadFrom
// on a tag at the start of any file and at a "DSD " stream's pointer, and
// ReadID3v2Tags from the DSF extractor and the AIFF and WAV walkers. dhowden's
// renaming allocates a string per lookup, so the allocation property is what
// sees it: on the old code 5,000 copies of one frame allocated about 190 MB
// here, in 0.5 s (16,000 took 6 s and 1.9 GB).
func TestNoRepeatedID3v2FrameMakesAnExtractionUnbounded(t *testing.T) {
	tag4 := repeatedFramesTag()
	for _, c := range []struct{ name, file string }{
		{"an MP3", "x.mp3"},
		{"an ID3v2 file under another extension", "x.ogg"},
		{"a DSF-shaped file under another extension", "y.mp3"},
		{"a DSF", "x.dsf"},
		{"an AIFF ID3 chunk", "x.aiff"},
		{"a WAV id3 chunk", "x.wav"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var data []byte
			switch c.file {
			case "x.mp3", "x.ogg":
				data = append(append([]byte{}, tag4...), mp3Audio()...)
			case "y.mp3", "x.dsf":
				data = dsfWithID3(tag4)
			case "x.aiff":
				data = buildAIFFWithID3(t, tag4)
			case "x.wav":
				data = buildWAVWithID3(t, tag4)
			}
			rec := loggingtest.Record(t)
			tr := requireBoundedExtraction(t, c.file, data, &ExtractContext{})
			if tr.Title != "" {
				t.Errorf("Title = %q: dhowden read the tag", tr.Title)
			}
			lines := rec.Failures(id3v2RefusedMessage)
			if len(lines) != 1 || !strings.Contains(lines[0], "frame=TIT2") || !strings.Contains(lines[0], "copies=2049") {
				t.Errorf("want one refusal naming TIT2 at its 2,049th copy, got %q", lines)
			}
		})
	}
}

// TestTheID3v2GuardDoesNotReadAFramePayload pins that without
// unsynchronisation the walk reads frame headers and skips payloads: a cover
// in an APIC frame is not read twice.
func TestTheID3v2GuardDoesNotReadAFramePayload(t *testing.T) {
	cover := payloadOf(4 << 20)
	apic := append([]byte("\x00image/jpeg\x00\x03\x00"), cover...)
	data := append(id3v2TagBytes(3, 0, append(textFrameBytes(3, "TIT2", "t"), id3v2FrameBytes(3, "APIC", uint32(len(apic)), 0, apic)...)), mp3Audio()...)
	rs := &countingReadSeeker{rs: bytes.NewReader(data)}
	if ok, refusal := dhowdenID3v2WithinBudget(rs); !ok {
		t.Fatalf("refused a tag holding one cover: %+v", refusal)
	}
	if rs.read > 1<<10 {
		t.Errorf("the guard read %d bytes of a %d-byte file: the cover is being read", rs.read, len(data))
	}
}

// TestTheID3v2GuardLeavesTheReaderWhereItFoundIt pins the call sites'
// premise: dhowden reads from the reader's current offset.
func TestTheID3v2GuardLeavesTheReaderWhereItFoundIt(t *testing.T) {
	data := append([]byte("xxxx"), id3v2TagBytes(4, 0, repeatedTIT2(5000))...)
	for name, guard := range map[string]func(*countingReadSeeker) bool{
		"dhowdenID3v2WithinBudget": func(rs *countingReadSeeker) bool { ok, _ := dhowdenID3v2WithinBudget(rs); return ok },
		"id3v2TagWithinBudget":     func(rs *countingReadSeeker) bool { ok, _ := id3v2TagWithinBudget(rs); return ok },
	} {
		// countingReadSeeker has no ReadAt: the walk seeks to read, and has to
		// put the reader back.
		rs := &countingReadSeeker{rs: bytes.NewReader(data)}
		if _, err := rs.Seek(4, 0); err != nil {
			t.Fatal(err)
		}
		if guard(rs) {
			t.Errorf("%s did not walk the tag from the reader's offset", name)
		}
		if pos, _ := rs.Seek(0, 1); pos != 4 {
			t.Errorf("%s left the reader at %d, want 4", name, pos)
		}
	}
}

// TestDhowdenStillRenamesRepeatedID3v2FramesOneLookupAtATime is the premise
// the guard's renaming bound rests on. The day dhowden stops counting up from
// id_0 for every repeat this fails, and the bound (not the frames bound, nor
// the seeds) can go.
func TestDhowdenStillRenamesRepeatedID3v2FramesOneLookupAtATime(t *testing.T) {
	data := id3v2TagBytes(4, 0, repeatedTIT2(2000))
	before := heapAllocated()
	if _, err := tag.ReadID3v2Tags(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// 1,999,000 lookups, each building a string: about 29 MB measured. A
	// renaming that did not count up from id_0 would allocate a few hundred KB.
	if got := heapAllocated() - before; got < 10<<20 {
		t.Fatalf("dhowden allocated %d bytes renaming 2,000 copies of one frame: it no longer renames them one lookup at a time, so the renaming bound may be retired", got)
	}
}

// TestEveryDhowdenReadIsGuarded pins the call sites: every call of a dhowden
// reader in this package's production code sits in a function that asks the
// guards for what that reader reads, and a reader with no guard is not called
// at all. A new call site that skipped them would hand dhowden a crafted tag.
func TestEveryDhowdenReadIsGuarded(t *testing.T) {
	guardsFor := map[string][]string{
		"ReadFrom":      {"dhowdenPicturesWithinBudget", "dhowdenID3v2WithinBudget"},
		"ReadID3v2Tags": {"id3v2TagWithinBudget"},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		// A name beginning with "." or "_" is no source of the package's (the
		// go tool ignores it; emacs's `.#extractors.go` lock is one), decided
		// before the file is opened.
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkg := ""
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/dhowden/tag" {
				pkg = "tag"
				if imp.Name != nil {
					pkg = imp.Name.Name
				}
			}
		}
		if pkg == "" {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			called := map[string]bool{}
			var reads []*ast.SelectorExpr
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					called[fun.Name] = true
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok && x.Name == pkg && strings.HasPrefix(fun.Sel.Name, "Read") {
						reads = append(reads, fun)
					}
				}
				return true
			})
			for _, r := range reads {
				checked++
				guards, known := guardsFor[r.Sel.Name]
				if !known {
					t.Errorf("%s: %s calls %s.%s, which no dhowden guard walks", fset.Position(r.Pos()), fn.Name.Name, pkg, r.Sel.Name)
					continue
				}
				for _, g := range guards {
					if !called[g] {
						t.Errorf("%s: %s calls %s.%s without %s", fset.Position(r.Pos()), fn.Name.Name, pkg, r.Sel.Name, g)
					}
				}
			}
		}
	}
	if checked < 3 {
		t.Fatalf("found %d dhowden reads, want the 3 this package makes: the sweep is not reading what it should", checked)
	}
}

// FuzzID3v2WalkAgreesWithDhowden fuzzes the walk against dhowden itself:
// wherever dhowden reads a tag, the walk counts what dhowden stores, per id,
// save the last frame (TestTheID3v2WalkCountsWhatDhowdenStores says which).
// A walk that followed declared sizes, the header flags or the filter
// differently from dhowden counts other frames, and this is where that shows.
func FuzzID3v2WalkAgreesWithDhowden(f *testing.F) {
	for _, s := range id3v2Shapes() {
		f.Add(s.data)
		if s.hidden != nil {
			f.Add(s.hidden(3))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if diff, ok := walkDisagreement(data); ok && diff != "" {
			t.Fatalf("the walk does not count what dhowden stores: %s", diff)
		}
		// And the bounded walk the guards run never fails where it cannot read.
		_, _ = id3v2TagWithinBudget(bytes.NewReader(data))
		_, _ = dhowdenID3v2WithinBudget(bytes.NewReader(data))
	})
}
