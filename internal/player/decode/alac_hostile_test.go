package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

const (
	alac16       = "afconvert_16bit_44100.m4a"
	alac24       = "afconvert_24bit_96000.m4a"
	alac32       = "afconvert_32bit_44100.m4a"
	alacEdit     = "afconvert_16bit_44100_edit.m4a"
	alacMoovLast = "afconvert_24bit_96000_co64_moov_last.m4a"
)

// alacMutation rewrites the 16-bit golden, laid out moov last so a change of
// moov's size moves no audio.
type alacMutation struct {
	name   string
	mutate func(t testing.TB, nodes []*m4aNode) []*m4aNode
	reason decode.Reason
	// msg, when set, must appear in the refusal: the check that fired is
	// the one the case is about.
	msg string
}

func box(t testing.TB, parent *m4aNode, typ string) *m4aNode {
	t.Helper()
	return m4aPath(t, parent.children, typ)
}

func stsd(t testing.TB, nodes []*m4aNode) []byte {
	t.Helper()
	return box(t, m4aSTBL(t, nodes), "stsd").payload
}

func put32At(b []byte, off int, v uint32) { binary.BigEndian.PutUint32(b[off:], v) }

func set32(path ...string) func(off int, v uint32) func(testing.TB, []*m4aNode) []*m4aNode {
	return func(off int, v uint32) func(testing.TB, []*m4aNode) []*m4aNode {
		return func(t testing.TB, nodes []*m4aNode) []*m4aNode {
			put32At(m4aPath(t, nodes, path...).payload, off, v)
			return nodes
		}
	}
}

var (
	stblPath = []string{"moov", "trak", "mdia", "minf", "stbl"}
	inSTBL   = func(typ string) []string { return append(append([]string{}, stblPath...), typ) }
	stszSet  = set32(inSTBL("stsz")...)
	stcoSet  = set32(inSTBL("stco")...)
	stscSet  = set32(inSTBL("stsc")...)
	sttsSet  = set32(inSTBL("stts")...)
	stsdSet  = set32(inSTBL("stsd")...)
	mdhdSet  = set32("moov", "trak", "mdia", "mdhd")
)

func stsdByte(off int, v byte) func(testing.TB, []*m4aNode) []*m4aNode {
	return func(t testing.TB, nodes []*m4aNode) []*m4aNode {
		stsd(t, nodes)[off] = v
		return nodes
	}
}

func stsdTag(off int, tag string) func(testing.TB, []*m4aNode) []*m4aNode {
	return func(t testing.TB, nodes []*m4aNode) []*m4aNode {
		copy(stsd(t, nodes)[off:], tag)
		return nodes
	}
}

// replaceBox swaps the payload of the box at path.
func replaceBox(payload []byte, path ...string) func(testing.TB, []*m4aNode) []*m4aNode {
	return func(t testing.TB, nodes []*m4aNode) []*m4aNode {
		m4aPath(t, nodes, path...).payload = payload
		return nodes
	}
}

// addChild appends a box to the container at path.
func addChild(child *m4aNode, path ...string) func(testing.TB, []*m4aNode) []*m4aNode {
	return func(t testing.TB, nodes []*m4aNode) []*m4aNode {
		p := m4aPath(t, nodes, path...)
		p.children = append(p.children, child)
		return nodes
	}
}

func table(entries ...uint32) []byte {
	out := make([]byte, 4*len(entries))
	for i, e := range entries {
		binary.BigEndian.PutUint32(out[4*i:], e)
	}
	return out
}

func refusedWith(t *testing.T, err error, reason decode.Reason, msg string) {
	t.Helper()
	r, ok := decode.AsRefusal(err)
	if !ok || r.Reason != reason {
		t.Fatalf("got %v, want refusal %s", err, reason)
	}
	if msg != "" && !strings.Contains(r.Message, msg) {
		t.Fatalf("refusal %q does not name %q", r.Message, msg)
	}
}

func openErr(raw []byte) error {
	s, err := decode.Open(bytes.NewReader(raw))
	if err == nil {
		s.Close()
	}
	return err
}

// Every table count, size and offset that cannot be honoured is a typed
// refusal at open: never a panic, an allocation of the declared size, or a
// packet read from outside mdat. Codex's B470 evaluation found each of the
// count, size, cookie and stsd cases making saprobe-alac's container path
// panic, allocate tens of MiB from a 50 KB file, or loop for seconds.
func TestALACAbsurdTablesRefused(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	const huge = math.MaxUint32
	cases := []alacMutation{
		{"stsz count 2^32-1", stszSet(8, huge), decode.ReasonTruncated, "stsz"},
		{"stsz count 0", stszSet(8, 0), decode.ReasonTruncated, "no packets"},
		{"stsz constant size with a huge count", func(t testing.TB, n []*m4aNode) []*m4aNode {
			return stszSet(8, huge)(t, stszSet(4, 1)(t, n))
		}, decode.ReasonUnsupportedLayout, "packets"},
		{"stsz constant size, the cap's count", func(t testing.TB, n []*m4aNode) []*m4aNode {
			return stszSet(8, 1<<20)(t, stszSet(4, 1)(t, n))
		}, decode.ReasonTruncated, "mdat"},
		{"stco count 2^32-1", stcoSet(4, huge), decode.ReasonTruncated, "stco"},
		{"stco count 0", stcoSet(4, 0), decode.ReasonTruncated, "stco"},
		{"stco offset outside mdat", stcoSet(8, 1), decode.ReasonTruncated, "mdat"},
		{"stco offset past the file", stcoSet(8, huge), decode.ReasonTruncated, "mdat"},
		// Counts the packets could hold but the box does not: only the
		// count-against-box check refuses these, where a huge count is
		// refused by the count-against-packets checks first.
		{"stco count past its box", stcoSet(4, 2), decode.ReasonTruncated, "entries"},
		{"stts count past its box", sttsSet(4, 3), decode.ReasonTruncated, "entries"},
		{"stsc count 2^32-1", stscSet(4, huge), decode.ReasonTruncated, "stsc"},
		{"stsc first chunk 2", stscSet(8, 2), decode.ReasonTruncated, "stsc"},
		{"stsc samples per chunk 0", stscSet(12, 0), decode.ReasonTruncated, "stsc"},
		{"stsc three packets in the chunk", stscSet(12, 3), decode.ReasonTruncated, "stsc"},
		{"stsc five packets in the chunk", stscSet(12, 5), decode.ReasonTruncated, "stsc"},
		{"stsc description index 2", stscSet(16, 2), decode.ReasonUnsupportedLayout, "description"},
		{"stts count 2^32-1", sttsSet(4, huge), decode.ReasonTruncated, "stts"},
		{"stts run of 2^32-1 packets", sttsSet(8, huge), decode.ReasonTruncated, "stts"},
		{"stts delta 0", sttsSet(12, 0), decode.ReasonTruncated, "stts"},
		{"stts delta above the frame length", sttsSet(12, 4097), decode.ReasonTruncated, "stts"},
		{"stts covers too few packets", sttsSet(8, 2), decode.ReasonTruncated, "stts"},
		{"mdhd duration disagrees with stts", mdhdSet(16, 12424), decode.ReasonTruncated, "mdhd"},
		{"mdhd timescale differs from the sample rate", mdhdSet(12, 48000), decode.ReasonUnsupportedLayout, "timescale"},
		{"stsd two entries", stsdSet(stsdEntryCount, 2), decode.ReasonUnsupportedLayout, "entries"},
		{"stsd 2^32-1 entries", stsdSet(stsdEntryCount, huge), decode.ReasonUnsupportedLayout, "entries"},
		// The sniffer sees no ALAC entry in these four, so Open refuses
		// them before the demuxer runs; TestALACDemuxerAlone holds the
		// demuxer's own refusals of them.
		{"stsd entry size huge", stsdSet(stsdEntrySize, huge), decode.ReasonUnknownContainer, ""},
		{"stsd entry size 0", stsdSet(stsdEntrySize, 0), decode.ReasonUnknownContainer, ""},
		{"stsd mp4a", stsdTag(stsdEntryType, "mp4a"), decode.ReasonLossy, "AAC"},
		{"stsd fLaC", stsdTag(stsdEntryType, "fLaC"), decode.ReasonUnknownContainer, ""},
		{"QuickTime sound description version 1", stsdByte(stsdSoundVersion+1, 1), decode.ReasonUnsupportedLayout, "version"},
		{"data reference index 2", stsdByte(stsdDataRef+1, 2), decode.ReasonUnsupportedLayout, "data reference"},
		{"no alac box in the entry", stsdTag(stsdAlacBoxType, "xxxx"), decode.ReasonUnsupportedLayout, "alac box"},
		{"alac box of 37 bytes", stsdSet(stsdAlacBoxSize, 37), decode.ReasonTruncated, "alac box"},
		{"alac box version 1", stsdByte(stsdAlacFullBox, 1), decode.ReasonUnsupportedLayout, "alac box"},
		{"sample entry of one channel", stsdByte(stsdChannels+1, 1), decode.ReasonUnsupportedChannels, ""},
		{"cookie frame length 0", stsdSet(cookieFrameLength, 0), decode.ReasonUnsupportedLayout, "frame length"},
		{"cookie frame length 31", stsdSet(cookieFrameLength, 31), decode.ReasonUnsupportedLayout, "frame length"},
		{"cookie frame length 65537", stsdSet(cookieFrameLength, 65537), decode.ReasonUnsupportedLayout, "frame length"},
		{"cookie frame length 4194304", stsdSet(cookieFrameLength, 4<<20), decode.ReasonUnsupportedLayout, "frame length"},
		{"cookie compatible version 1", stsdByte(cookieCompatible, 1), decode.ReasonUnsupportedLayout, "compatible version"},
		{"cookie 8-bit", stsdByte(cookieBitDepth, 8), decode.ReasonUnsupportedBits, ""},
		{"cookie 20-bit", stsdByte(cookieBitDepth, 20), decode.ReasonUnsupportedBits, ""},
		{"cookie 33-bit", stsdByte(cookieBitDepth, 33), decode.ReasonUnsupportedBits, ""},
		{"cookie kb 32", stsdByte(cookieKB, 32), decode.ReasonUnsupportedLayout, "kb"},
		{"cookie 0 channels", stsdByte(cookieChannels, 0), decode.ReasonUnsupportedChannels, ""},
		{"cookie 1 channel", stsdByte(cookieChannels, 1), decode.ReasonUnsupportedChannels, ""},
		{"cookie 9 channels", stsdByte(cookieChannels, 9), decode.ReasonUnsupportedChannels, ""},
		{"cookie max frame bytes past the format's largest packet", stsdSet(cookieMaxFrame, 4096*10+2), decode.ReasonTruncated, "max frame bytes"},
		{"cookie max frame bytes below a packet", stsdSet(cookieMaxFrame, 16000), decode.ReasonTruncated, "max frame bytes"},
		{"cookie average bit rate past the format's largest", stsdSet(cookieAvgBitRate, huge), decode.ReasonTruncated, "bit rate"},
		{"cookie sample rate 0", stsdSet(cookieSampleRate, 0), decode.ReasonUnsupportedLayout, ""},
		{"cookie sample rate 768001", stsdSet(cookieSampleRate, 768001), decode.ReasonUnsupportedLayout, ""},
		{"stz2 for stsz", func(t testing.TB, n []*m4aNode) []*m4aNode {
			box(t, m4aSTBL(t, n), "stsz").typ = "stz2"
			return n
		}, decode.ReasonUnsupportedLayout, "stz2"},
		{"composition offsets", addChild(&m4aNode{typ: "ctts", payload: table(0, 1, 4, 0)}, stblPath...), decode.ReasonUnsupportedLayout, "ctts"},
		{"a second stts", addChild(&m4aNode{typ: "stts", payload: table(0, 1, 4, 4096)}, stblPath...), decode.ReasonUnsupportedLayout, "stts"},
		{"external data reference", func(t testing.TB, n []*m4aNode) []*m4aNode {
			dref := m4aPath(t, n, "moov", "trak", "mdia", "minf", "dinf", "dref").payload
			dref[len(dref)-1] = 0 // the url box's self-contained flag
			return n
		}, decode.ReasonUnsupportedLayout, "data reference"},
		{"no data reference", func(t testing.TB, n []*m4aNode) []*m4aNode {
			m4aPath(t, n, "moov", "trak", "mdia", "minf", "dinf").typ = "xxxx"
			return n
		}, decode.ReasonUnsupportedLayout, "data reference"},
		{"two sound tracks", func(t testing.TB, n []*m4aNode) []*m4aNode {
			moov := m4aPath(t, n, "moov")
			moov.children = append(moov.children, m4aPath(t, n, "moov", "trak"))
			return n
		}, decode.ReasonUnsupportedLayout, "audio tracks"},
		{"no sound track", func(t testing.TB, n []*m4aNode) []*m4aNode {
			copy(m4aPath(t, n, "moov", "trak", "mdia", "hdlr").payload[8:], "vide")
			return n
		}, decode.ReasonUnsupportedLayout, "audio track"},
		{"fragmented: mvex in moov", addChild(&m4aNode{typ: "mvex", payload: nil}, "moov"), decode.ReasonUnsupportedLayout, "fragmented"},
		{"fragmented: a moof", func(t testing.TB, n []*m4aNode) []*m4aNode {
			return append(n, &m4aNode{typ: "moof"})
		}, decode.ReasonUnsupportedLayout, "fragmented"},
		{"a second moov", func(t testing.TB, n []*m4aNode) []*m4aNode {
			return append(n, m4aPath(t, n, "moov"))
		}, decode.ReasonUnsupportedLayout, "moov"},
		{"no moov", func(t testing.TB, n []*m4aNode) []*m4aNode {
			return n[:2]
		}, decode.ReasonUnknownContainer, ""},
		// After the track, where the sniffer has already found ALAC: its
		// own box cap hides a track behind a bomb.
		{"a box bomb in moov", func(t testing.TB, n []*m4aNode) []*m4aNode {
			moov := m4aPath(t, n, "moov")
			for i := 0; i < 100000; i++ {
				moov.children = append(moov.children, &m4aNode{typ: "free"})
			}
			return n
		}, decode.ReasonUnsupportedLayout, "boxes"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			raw := encodeM4A(c.mutate(t, m4aMoovLast(t, base)))
			refusedWith(t, openErr(raw), c.reason, c.msg)
		})
	}
}

// Box headers that cannot be honoured: a size below its header, an extended
// size past what is addressable, a box past its parent, and a size of 0
// (to the end of the file), which only a top-level box may have. The
// sniffer refuses most of these before the demuxer does, with the reason
// the demuxer gives.
func TestALACBoxSizesRefused(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	stszAt := func(t testing.TB) ([]byte, int) {
		raw := encodeM4A(m4aMoovLast(t, base))
		i := bytes.Index(raw, []byte("stsz"))
		if i < 4 {
			t.Fatal("no stsz")
		}
		return raw, i - 4
	}
	cases := []struct {
		name   string
		patch  func(raw []byte, at int) []byte
		reason decode.Reason
	}{
		{"size below its header", func(raw []byte, at int) []byte { return putBE32(raw, at, 7) }, decode.ReasonUnsupportedLayout},
		{"size 0 inside a container", func(raw []byte, at int) []byte { return putBE32(raw, at, 0) }, decode.ReasonUnsupportedLayout},
		{"size past its parent", func(raw []byte, at int) []byte { return putBE32(raw, at, 1<<20) }, decode.ReasonUnsupportedLayout},
		{"extended size 2^63", func(raw []byte, at int) []byte {
			// size 1 and a 64-bit size where the box's first payload bytes were.
			out := putBE32(raw, at, 1)
			return putBE64(out, at+8, 1<<63)
		}, decode.ReasonUnsupportedLayout},
		{"extended size 2^64-1", func(raw []byte, at int) []byte {
			out := putBE32(raw, at, 1)
			return putBE64(out, at+8, math.MaxUint64)
		}, decode.ReasonUnsupportedLayout},
		{"extended size below its header", func(raw []byte, at int) []byte {
			out := putBE32(raw, at, 1)
			return putBE64(out, at+8, 15)
		}, decode.ReasonUnsupportedLayout},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			raw, at := stszAt(t)
			refusedWith(t, openErr(c.patch(raw, at)), c.reason, "")
		})
	}
}

// A packet count above the cap is refused from the stsz header alone, and
// the absurd counts cost no allocation of their declared size. Through Open
// the bound is the sniffer's: it reads up to 4 MiB of moov to find the
// codec, and the over-cap stsz makes moov 4 MiB. TestALACDemuxerAllocatesLittle
// measures the demuxer alone.
func TestALACHostileTablesAllocateLittle(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	const packets = 1<<20 + 1
	sizes := make([]byte, 12+4*packets)
	binary.BigEndian.PutUint32(sizes[8:], packets)
	overCap := encodeM4A(replaceBox(sizes, inSTBL("stsz")...)(t, m4aMoovLast(t, base)))
	raws := map[string][]byte{
		"stsz above the cap": overCap,
		"stsz 2^32-1":        encodeM4A(stszSet(8, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stco 2^32-1":        encodeM4A(stcoSet(4, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stsc 2^32-1":        encodeM4A(stscSet(4, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stts 2^32-1":        encodeM4A(sttsSet(4, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stsd 2^32-1":        encodeM4A(stsdSet(stsdEntryCount, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stsd entry huge":    encodeM4A(stsdSet(stsdEntrySize, math.MaxUint32)(t, m4aMoovLast(t, base))),
	}
	for name, raw := range raws {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := openErr(raw)
		runtime.ReadMemStats(&after)
		if !decode.IsRefusal(err) {
			t.Fatalf("%s: got %v, want a refusal", name, err)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4<<20+1<<20 {
			t.Fatalf("%s: open allocated %d bytes before refusing", name, alloc)
		}
	}
	refusedWith(t, openErr(overCap), decode.ReasonUnsupportedLayout, "packets")
}

// Edit lists the demuxer does not model are refused with a reason naming
// them; a version-1 (64-bit) edit list is read like a version-0 one.
func TestALACEditListsRefused(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alacEdit)
	elst := func(t testing.TB, n []*m4aNode) []byte {
		return m4aPath(t, n, "moov", "trak", "edts", "elst").payload
	}
	setElst := func(off int, v uint32) func(testing.TB, []*m4aNode) []*m4aNode {
		return func(t testing.TB, n []*m4aNode) []*m4aNode {
			put32At(elst(t, n), off, v)
			return n
		}
	}
	cases := []alacMutation{
		{"two edits", func(t testing.TB, n []*m4aNode) []*m4aNode {
			e := m4aPath(t, n, "moov", "trak", "edts", "elst")
			e.payload = append(e.payload, e.payload[8:20]...)
			put32At(e.payload, 4, 2)
			return n
		}, decode.ReasonUnsupportedLayout, "edits"},
		{"no edits", setElst(4, 0), decode.ReasonUnsupportedLayout, "edits"},
		{"an empty edit", setElst(12, math.MaxUint32), decode.ReasonUnsupportedLayout, "empty edit"},
		{"media time -2", setElst(12, math.MaxUint32-1), decode.ReasonTruncated, "media time"},
		{"rate 2", setElst(16, 2<<16), decode.ReasonUnsupportedLayout, "rate"},
		{"rate 1.5", setElst(16, 1<<16|0x8000), decode.ReasonUnsupportedLayout, "rate"},
		{"zero duration", setElst(8, 0), decode.ReasonUnsupportedLayout, "duration"},
		{"media time at the media's end", setElst(12, 12425), decode.ReasonUnsupportedLayout, "media time"},
		{"no whole frame", func(t testing.TB, n []*m4aNode) []*m4aNode {
			put32At(m4aPath(t, n, "moov", "mvhd").payload, 12, 1_000_000_000)
			return setElst(8, 1)(t, n)
		}, decode.ReasonUnsupportedLayout, "frame"},
		{"elst version 2", func(t testing.TB, n []*m4aNode) []*m4aNode {
			elst(t, n)[0] = 2
			return n
		}, decode.ReasonUnsupportedLayout, "elst"},
		{"elst cut short", func(t testing.TB, n []*m4aNode) []*m4aNode {
			e := m4aPath(t, n, "moov", "trak", "edts", "elst")
			e.payload = e.payload[:12]
			return n
		}, decode.ReasonTruncated, "elst"},
		{"a second elst", func(t testing.TB, n []*m4aNode) []*m4aNode {
			edts := m4aPath(t, n, "moov", "trak", "edts")
			edts.children = append(edts.children, edts.children[0])
			return n
		}, decode.ReasonUnsupportedLayout, "elst"},
		{"a second edts", func(t testing.TB, n []*m4aNode) []*m4aNode {
			trak := m4aPath(t, n, "moov", "trak")
			trak.children = append(trak.children, m4aPath(t, n, "moov", "trak", "edts"))
			return n
		}, decode.ReasonUnsupportedLayout, "edts"},
		{"an edit and no mvhd", func(t testing.TB, n []*m4aNode) []*m4aNode {
			m4aPath(t, n, "moov", "mvhd").typ = "xxxx"
			return n
		}, decode.ReasonUnsupportedLayout, "mvhd"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			raw := encodeM4A(c.mutate(t, m4aMoovLast(t, base)))
			refusedWith(t, openErr(raw), c.reason, c.msg)
		})
	}
}

// A version-1 edit list, 64-bit duration and media time, presents the same
// frames as the golden's version-0 one.
func TestALACEditListVersion1(t *testing.T) {
	g := loadALACGoldens(t)
	f, base := g.fixture(t, alacEdit)
	nodes := m4aMoovLast(t, base)
	e := m4aPath(t, nodes, "moov", "trak", "edts", "elst")
	v0 := e.payload
	dur, mt := binary.BigEndian.Uint32(v0[8:]), int32(binary.BigEndian.Uint32(v0[12:]))
	v1 := make([]byte, 8+20)
	v1[0] = 1
	binary.BigEndian.PutUint32(v1[4:], 1)
	binary.BigEndian.PutUint64(v1[8:], uint64(dur))
	binary.BigEndian.PutUint64(v1[16:], uint64(int64(mt)))
	copy(v1[24:], v0[16:20])
	e.payload = v1
	d, err := openPCM(bytes.NewReader(encodeM4A(nodes)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Format().Frames != f.Frames {
		t.Fatalf("%d frames, want %d", d.Format().Frames, f.Frames)
	}
	requireSameSamples(t, readAllFrames(t, d, 4096), f.want(t))
}

// An edit that runs past the media's end presents the media to its end; an
// edit's start may sit inside any packet.
func TestALACEditPastTheEndClipsAtTheMedia(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alacEdit)
	all := alacPattern(3*4096+137, 16)
	for _, mt := range []uint32{0, 1, 4095, 4096, 12424} {
		nodes := m4aMoovLast(t, base)
		elst := m4aPath(t, nodes, "moov", "trak", "edts", "elst").payload
		put32At(m4aPath(t, nodes, "moov", "mvhd").payload, 12, 44100)
		put32At(elst, 8, 1<<31) // far past the media
		put32At(elst, 12, mt)
		d, err := openPCM(bytes.NewReader(encodeM4A(nodes)))
		if err != nil {
			t.Fatalf("media time %d: %v", mt, err)
		}
		if want := int64(3*4096+137) - int64(mt); d.Format().Frames != want {
			t.Fatalf("media time %d: %d frames, want %d", mt, d.Format().Frames, want)
		}
		requireSameSamples(t, readAllFrames(t, d, 2*999), all[2*mt:])
		d.Close()
	}
}

// A file cut inside a packet returns every whole packet before the cut, then
// a truncated refusal on the same or the next call, and never audio from
// past the cut. A file cut before its moov is refused at open.
func TestALACTruncatedMidPacket(t *testing.T) {
	g := loadALACGoldens(t)
	f, raw := g.fixture(t, alac24)
	offs, sizes := m4aPacketSpans(t, parseM4ANodes(t, raw))
	cut := offs[2] + int64(sizes[2])/2
	d, err := openPCM(bytes.NewReader(raw[:cut]))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	want := f.want(t)
	var got []int32
	buf := make([]int32, 2*1000)
	var last error
	for i := 0; i < 1000; i++ {
		n, err := d.ReadFrames(context.Background(), buf)
		got = append(got, buf[:n]...)
		if err != nil {
			last = err
			break
		}
	}
	if r, ok := decode.AsRefusal(last); !ok || r.Reason != decode.ReasonTruncated {
		t.Fatalf("got %v, want a truncated refusal", last)
	}
	if len(got) != 2*2*4096 {
		t.Fatalf("%d frames before the refusal, want the two whole packets (%d)", len(got)/2, 2*4096)
	}
	requireSameSamples(t, got, want[:len(got)])

	_, moovLast := g.fixture(t, alacMoovLast)
	nodes := parseM4ANodes(t, moovLast)
	if nodes[len(nodes)-1].typ != "moov" {
		t.Fatal("the moov-last golden does not end with moov")
	}
	refusedWith(t, openErr(moovLast[:len(moovLast)-200]), decode.ReasonTruncated, "moov")
}

// Packets whose own header contradicts the container or the cookie are
// refused before the library decodes them: an END or a single-channel
// element first (the library returns a silent frame for the first), a
// partial count that is not stts's, set unused bits, a shift of three
// bytes, a compressed 32-bit pair with no shifted byte, whose 33-bit samples
// the entropy decoder reads past the packet's padding, and a compressed
// 16-bit pair that shifts a byte, which the library's 16-bit output drops.
// The packets before it decode; the refusal comes with them or on the next
// call.
func TestALACCorruptPacketHeadersRefused(t *testing.T) {
	g := loadALACGoldens(t)
	f16, raw16 := g.fixture(t, alac16)
	f32, raw32 := g.fixture(t, alac32)
	// The element header's 23 bits, MSB first, in the top of 24: the tag
	// (3), the instance tag (4), 12 unused bits, then partial, the shift (2)
	// and escape.
	element := func(tag, unused, partial, shift, escape uint32) []byte {
		v := tag<<21 | unused<<5 | partial<<4 | shift<<2 | escape<<1
		return []byte{byte(v >> 16), byte(v >> 8), byte(v), 0, 0, 0, 0, 0, 0, 0}
	}
	// A partial escape channel pair whose 32-bit frame count follows the
	// header's 23 bits.
	partialOf := func(n uint32) []byte {
		v := uint64(1)<<61 | uint64(1)<<44 | uint64(1)<<41 | uint64(n)<<9
		b := make([]byte, 16)
		binary.BigEndian.PutUint64(b, v)
		return b
	}
	cases := []struct {
		name string
		raw  []byte
		f    alacFixture
		pkt  int
		data []byte
		msg  string
	}{
		{"END first", raw16, f16, 1, []byte{0xE0, 0, 0, 0, 0, 0, 0, 0}, "element"},
		{"a single channel first", raw16, f16, 1, element(0, 0, 0, 0, 1), "element"},
		{"unused bits set", raw16, f16, 1, element(1, 1, 0, 0, 1), "unused"},
		{"three shifted bytes", raw16, f16, 1, element(1, 0, 0, 3, 0), "shift"},
		{"a partial count that is not stts's", raw16, f16, 1, partialOf(4095), "frames"},
		{"a partial count of 0", raw16, f16, 1, partialOf(0), "frames"},
		{"a two-byte packet", raw16, f16, 1, []byte{0x20, 0}, "header"},
		{"a compressed 32-bit pair with no shifted byte", raw32, f32, 1, element(1, 0, 0, 0, 0), "33"},
		{"a compressed 16-bit pair that shifts a byte", raw16, f16, 1, element(1, 0, 0, 1, 0), "16-bit"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			d, err := openPCM(bytes.NewReader(m4aReplacePacket(t, c.raw, c.pkt, c.data)))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			buf := make([]int32, 2*3*4096)
			n, err := d.ReadFrames(context.Background(), buf)
			if err == nil {
				t.Fatalf("read %d samples and no error, want the packet refused", n)
			}
			refusedWith(t, err, decode.ReasonTruncated, c.msg)
			if n != 2*4096*c.pkt {
				t.Fatalf("%d frames before the refusal, want %d", n/2, 4096*c.pkt)
			}
			requireSameSamples(t, buf[:n], c.f.want(t)[:n])
		})
	}
}

// saprobe-alac 1b6eb83 panicked on this ten-byte stereo packet under Apple's
// own cookie values (frame length 4096, pb 40, mb 10, kb 14): its last nine
// bits are ones, so the first residual is an escape whose value was read from
// the padding, and the zero run that follows read four bytes from past the
// padded buffer (golomb.go: dynGet's read32bit; ops/upstream/saprobe-alac.md).
// No check of the cookie or the header can tell it apart from a good packet.
// The patched copy in third_party/saprobe-alac refuses it at the escape.
var alacPanicPacket = []byte{0x20, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x01, 0xFF}

// A packet the library refuses with an error is a truncated_or_corrupt
// refusal, never a success and never a short packet, and the same refusal
// again on a retry; after a seek past it the next packets decode.
func TestALACLibraryErrorIsARefusal(t *testing.T) {
	g := loadALACGoldens(t)
	f, raw := g.fixture(t, alac16)
	d, err := openPCM(bytes.NewReader(m4aReplacePacket(t, raw, 0, alacPanicPacket)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	buf := make([]int32, 2*4096)
	n, err := d.ReadFrames(ctx, buf)
	if n != 0 {
		t.Fatalf("read %d samples of the refused packet", n)
	}
	refusedWith(t, err, decode.ReasonTruncated, "packet 0 does not decode")
	// The same packet again: the same refusal, not a different failure.
	if n, err := d.ReadFrames(ctx, buf); n != 0 || !decode.IsRefusal(err) {
		t.Fatalf("second read: n=%d err=%v", n, err)
	}
	if err := d.Seek(ctx, 4096); err != nil {
		t.Fatal(err)
	}
	n, err = d.ReadFrames(ctx, buf)
	if err != nil || n != len(buf) {
		t.Fatalf("read after the seek: n=%d err=%v", n, err)
	}
	requireSameSamples(t, buf, f.want(t)[2*4096:2*4096+len(buf)])
}

// A cancel during a read, or an I/O error after it, leaves the decoder where
// it was: the next read continues with the next frame, and nothing is
// skipped or repeated.
func TestALACCancelThenRetryNoGap(t *testing.T) {
	g := loadALACGoldens(t)
	f, raw := g.fixture(t, alac24)
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelReader{Reader: bytes.NewReader(raw), cancel: cancel}
	d, err := openPCM(r)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first := make([]int32, 2*3000)
	n, err := d.ReadFrames(context.Background(), first)
	if err != nil || n != len(first) {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	r.armed = true
	dst := make([]int32, 2*6000)
	n1, err1 := d.ReadFrames(ctx, dst)
	if !errors.Is(err1, context.Canceled) {
		t.Fatalf("read under cancel: n=%d err=%v, want Canceled", n1, err1)
	}
	// The cancel fires during packet 1's read: the call returns the 1096
	// frames left of packet 0 and decodes nothing of packet 1.
	if n1 != 2*(4096-3000) {
		t.Fatalf("read under cancel returned %d frames, want the %d already decoded", n1/2, 4096-3000)
	}
	r.armed = false
	got := append(append(first[:n:n], dst[:n1]...), readAllFrames(t, d, 4096)...)
	requireSameSamples(t, got, f.want(t))
}

func TestALACIOErrorThenRetryNoGap(t *testing.T) {
	g := loadALACGoldens(t)
	f, raw := g.fixture(t, alac32)
	r := &ioErrorReader{Reader: bytes.NewReader(raw), err: errShortRead}
	d, err := openPCM(r)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first := make([]int32, 2*3000)
	n, err := d.ReadFrames(context.Background(), first)
	if err != nil || n != len(first) {
		t.Fatalf("first read: n=%d err=%v", n, err)
	}
	r.armed, r.reads, r.failAfter = true, 0, 1
	dst := make([]int32, 2*6000)
	n1, err1 := d.ReadFrames(context.Background(), dst)
	if !errors.Is(err1, errShortRead) {
		t.Fatalf("read with an I/O error: n=%d err=%v", n1, err1)
	}
	r.armed = false
	got := append(append(first[:n:n], dst[:n1]...), readAllFrames(t, d, 4096)...)
	requireSameSamples(t, got, f.want(t))
}

// ReadFrames refuses a dst that is not whole stereo frames rather than write
// half a frame, and a cancelled context stops a read and a seek.
func TestALACReadBufferShape(t *testing.T) {
	g := loadALACGoldens(t)
	_, raw := g.fixture(t, alac16)
	d, err := openPCM(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if n, err := d.ReadFrames(context.Background(), make([]int32, 3)); err == nil || n != 0 {
		t.Fatalf("odd dst: n=%d err=%v, want an error", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.ReadFrames(ctx, make([]int32, 4)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
	if err := d.Seek(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled seek: %v", err)
	}
}

// Cover art of more than 4 MiB in moov after the track, and a large free
// box at the end of stbl, are skipped by seeking: the file opens and decodes
// (PR 1a's sniff reads only 4 MiB of moov, and found ALAC before the cut).
func TestALACLargeBoxesAreSkipped(t *testing.T) {
	g := loadALACGoldens(t)
	f, raw := g.fixture(t, alacMoovLast)
	cover := &m4aNode{typ: "udta", payload: bytes.Repeat([]byte{0xA5}, 5<<20)}
	free := &m4aNode{typ: "free", payload: make([]byte, 5<<20)}
	for name, mutate := range map[string]func(testing.TB, []*m4aNode) []*m4aNode{
		"cover after the track":   addChild(cover, "moov"),
		"free at the end of stbl": addChild(free, stblPath...),
	} {
		d, err := openPCM(bytes.NewReader(encodeM4A(mutate(t, m4aMoovLast(t, raw)))))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		requireSameSamples(t, readAllFrames(t, d, 4096), f.want(t))
		d.Close()
	}
}

// An ID3 tag in front of an M4A is skipped by the sniffer, and the M4A's
// offsets stay relative to its own start.
func TestALACAfterID3Prefix(t *testing.T) {
	g := loadALACGoldens(t)
	f, raw := g.fixture(t, alac16)
	tag := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 0}
	d, err := openPCM(bytes.NewReader(append(tag, raw...)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	requireSameSamples(t, readAllFrames(t, d, 4096), f.want(t))
}
