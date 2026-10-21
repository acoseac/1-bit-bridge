package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	alac "github.com/mycophonic/saprobe-alac"
)

// openALACErr opens raw with the ALAC demuxer alone, past the sniffer.
func openALACErr(raw []byte) error {
	d, err := decode.OpenALAC(bytes.NewReader(raw))
	if err == nil {
		d.Close()
	}
	return err
}

// The demuxer's own refusals of files the sniffer refuses first in Open: a
// sample entry the sniffer cannot read, a file with no moov, a box bomb
// ahead of the track, and top-level boxes that do not fit the file.
func TestALACDemuxerAlone(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	const huge = math.MaxUint32
	cases := []alacMutation{
		{"stsd entry size huge", stsdSet(stsdEntrySize, huge), decode.ReasonTruncated, "sample entry"},
		{"stsd entry size 0", stsdSet(stsdEntrySize, 0), decode.ReasonTruncated, "sample entry"},
		{"stsd fLaC", stsdTag(stsdEntryType, "fLaC"), decode.ReasonUnsupportedComp, "fLaC"},
		{"stsd version 1", stsdByte(0, 1), decode.ReasonUnsupportedLayout, "stsd version"},
		{"alac box of 30 bytes", stsdSet(stsdAlacBoxSize, 30), decode.ReasonUnsupportedLayout, "alac box"},
		{"a box of 7 bytes in the sample entry", stsdSet(stsdAlacBoxSize, 7), decode.ReasonUnsupportedLayout, "sample entry"},
		{"no moov", func(t testing.TB, n []*m4aNode) []*m4aNode { return n[:2] }, decode.ReasonTruncated, "moov"},
		{"a box bomb ahead of the track", func(t testing.TB, n []*m4aNode) []*m4aNode {
			moov := m4aPath(t, n, "moov")
			bomb := make([]*m4aNode, 100000)
			for i := range bomb {
				bomb[i] = &m4aNode{typ: "free"}
			}
			moov.children = append(bomb, moov.children...)
			return n
		}, decode.ReasonUnsupportedLayout, "boxes"},
		{"mdat first", func(t testing.TB, n []*m4aNode) []*m4aNode {
			return []*m4aNode{n[1], n[0], n[2]}
		}, decode.ReasonUnknownContainer, "ftyp"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			refusedWith(t, openALACErr(encodeM4A(c.mutate(t, m4aMoovLast(t, base)))), c.reason, c.msg)
		})
	}

	raw := encodeM4A(m4aMoovLast(t, base))
	// The box headers inside moov, which the sniffer refuses first in Open.
	at := bytes.Index(raw, []byte("stsz")) - 4
	for _, c := range []struct {
		name   string
		raw    []byte
		reason decode.Reason
		msg    string
	}{
		{"a box past its parent", putBE32(raw, at, 1<<20), decode.ReasonUnsupportedLayout, "past its parent"},
		{"a box of size 0 inside a container", putBE32(raw, at, 0), decode.ReasonUnsupportedLayout, "size 0"},
		{"a box below its header", putBE32(raw, at, 7), decode.ReasonUnsupportedLayout, "smaller than its header"},
		{"an extended size of 2^63", putBE64(putBE32(raw, at, 1), at+8, 1<<63), decode.ReasonUnsupportedLayout, "addressable"},
		{"an extended size below its header", putBE64(putBE32(raw, at, 1), at+8, 15), decode.ReasonUnsupportedLayout, "smaller than its header"},
	} {
		refusedWith(t, openALACErr(c.raw), c.reason, c.msg)
	}
	// A top-level box other than mdat that runs past the file's end is a
	// file cut short; a size of 0 runs to the end; an extended size the
	// file cannot address is refused before it is added to anything.
	free := func(size uint32) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b, size)
		copy(b[4:], "free")
		return b
	}
	refusedWith(t, openALACErr(append(append([]byte{}, raw...), free(1000)...)), decode.ReasonTruncated, "end of the file")
	if err := openALACErr(append(append([]byte{}, raw...), free(0)...)); err != nil {
		t.Fatalf("a last box of size 0: %v", err)
	}
	big := append(append([]byte{}, raw...), 0, 0, 0, 1, 'f', 'r', 'e', 'e', 0x80, 0, 0, 0, 0, 0, 0, 0)
	refusedWith(t, openALACErr(big), decode.ReasonUnsupportedLayout, "addressable")
	// Fewer than eight bytes after the last box end the top-level walk.
	if err := openALACErr(append(append([]byte{}, raw...), 1, 2, 3)); err != nil {
		t.Fatalf("three trailing bytes: %v", err)
	}
}

// The demuxer refuses an over-cap stsz from its header, and the absurd
// counts, without allocating anything near their declared sizes; and a file
// at the packet cap opens holding no more than its seek index.
func TestALACDemuxerAllocatesLittle(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	const packets = 1<<20 + 1
	sizes := make([]byte, 12+4*packets)
	binary.BigEndian.PutUint32(sizes[8:], packets)
	raws := map[string][]byte{
		"stsz above the cap": encodeM4A(replaceBox(sizes, inSTBL("stsz")...)(t, m4aMoovLast(t, base))),
		"stsz 2^32-1":        encodeM4A(stszSet(8, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stsz constant, the cap's count": encodeM4A(func(t testing.TB, n []*m4aNode) []*m4aNode {
			return stszSet(8, 1<<20)(t, stszSet(4, 1)(t, n))
		}(t, m4aMoovLast(t, base))),
		"stco 2^32-1":     encodeM4A(stcoSet(4, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stsc 2^32-1":     encodeM4A(stscSet(4, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stts 2^32-1":     encodeM4A(sttsSet(4, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stsd 2^32-1":     encodeM4A(stsdSet(stsdEntryCount, math.MaxUint32)(t, m4aMoovLast(t, base))),
		"stsd entry huge": encodeM4A(stsdSet(stsdEntrySize, math.MaxUint32)(t, m4aMoovLast(t, base))),
	}
	for name, raw := range raws {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := openALACErr(raw)
		runtime.ReadMemStats(&after)
		if !decode.IsRefusal(err) {
			t.Fatalf("%s: got %v, want a refusal", name, err)
		}
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 256<<10 {
			t.Fatalf("%s: the demuxer allocated %d bytes before refusing", name, alloc)
		}
	}

	// The seek index is twelve bytes a packet and 32 a stts run, runs of one
	// duration merged, and stsc is read in step with the chunk offsets, never
	// held. At the cap's 2^20 one-byte packets, stts listing a run a packet
	// and stsc a run a chunk opened holding 44 MiB and 28 MiB (#1300's second
	// round), and durations that alternate past the run cap are refused.
	const n = 1 << 20
	ones, alternate, stscEach := make([][2]uint32, n), make([][2]uint32, n), make([][3]uint32, n)
	for p := range ones {
		ones[p] = [2]uint32{1, 1}
		alternate[p] = [2]uint32{1, 1 + uint32(p%2)}
		stscEach[p] = [3]uint32{uint32(p + 1), 1, 1}
	}
	oneChunk := [][3]uint32{{1, n, 1}}
	for _, c := range []struct {
		name   string
		raw    []byte
		reason decode.Reason // empty when the file opens
	}{
		{"stts a run a packet", alacOneBytePackets(t, base, n, ones, oneChunk, 1), ""},
		{"stsc a run a chunk", alacOneBytePackets(t, base, n, [][2]uint32{{n, 1}}, stscEach, n), ""},
		{"stts durations that alternate", alacOneBytePackets(t, base, n, alternate, oneChunk, 1), decode.ReasonUnsupportedLayout},
	} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := openALACErr(c.raw)
		runtime.ReadMemStats(&after)
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 12*n+1<<20 {
			t.Errorf("%s: the demuxer allocated %d bytes, past twelve a packet and 1 MiB", c.name, alloc)
		}
		if c.reason == "" && err != nil {
			t.Errorf("%s: %v", c.name, err)
		} else if c.reason != "" {
			refusedWith(t, err, c.reason, "stts")
		}
	}
}

// alacOneBytePackets rewrites the 16-bit golden, laid out moov last, as n
// one-byte packets filling an mdat of n bytes: stsz a constant size of 1,
// stts and stsc the entries given, one offset for each of the chunks, at the
// packets stsc puts before it, and mdhd's duration stts's frames. An open
// reads none of the packets.
func alacOneBytePackets(t testing.TB, base []byte, n uint32, stts [][2]uint32, stsc [][3]uint32, chunks uint32) []byte {
	t.Helper()
	nodes := m4aMoovLast(t, base)
	nodes[1].payload = make([]byte, n)
	stbl := m4aSTBL(t, nodes)
	box(t, stbl, "stsz").payload = table(0, 1, n)
	tts := make([]byte, 8+8*len(stts))
	put32At(tts, 4, uint32(len(stts)))
	var frames uint32
	for i, e := range stts {
		put32At(tts, 8+8*i, e[0])
		put32At(tts, 12+8*i, e[1])
		frames += e[0] * e[1]
	}
	box(t, stbl, "stts").payload = tts
	sc := make([]byte, 8+12*len(stsc))
	put32At(sc, 4, uint32(len(stsc)))
	for i, e := range stsc {
		put32At(sc, 8+12*i, e[0])
		put32At(sc, 12+12*i, e[1])
		put32At(sc, 16+12*i, e[2])
	}
	box(t, stbl, "stsc").payload = sc
	co := make([]byte, 8+4*int(chunks))
	put32At(co, 4, chunks)
	at, r := uint32(len(nodes[0].encode())+8), 0 // mdat's payload
	for c := uint32(1); c <= chunks; c++ {
		for r+1 < len(stsc) && stsc[r+1][0] <= c {
			r++
		}
		put32At(co, 4+4*int(c), at)
		at += stsc[r][1]
	}
	box(t, stbl, "stco").payload = co
	put32At(m4aPath(t, nodes, "moov", "trak", "mdia", "mdhd").payload, 16, frames)
	return encodeM4A(nodes)
}

// stts entries of one duration side by side are one run: every golden, its
// stts rewritten as an entry a packet, decodes whole and from each seek frame
// as the pattern. Runs of differing durations are capped at 1024, where every
// golden, corpus file and FFmpeg encode has one or two: 1024 open, and 1025
// are refused.
func TestALACSTTSRunsMergeAndCap(t *testing.T) {
	g := loadALACGoldens(t)
	ctx := context.Background()
	for _, f := range g.decoded() {
		f := f
		t.Run(f.File, func(t *testing.T) {
			nodes := m4aMoovLast(t, readALACGolden(t, f.File))
			stts := box(t, m4aSTBL(t, nodes), "stts")
			each := []uint32{0, 0}
			for i, k := 0, int(binary.BigEndian.Uint32(stts.payload[4:])); i < k; i++ {
				count, delta := binary.BigEndian.Uint32(stts.payload[8+8*i:]), binary.BigEndian.Uint32(stts.payload[12+8*i:])
				for ; count > 0; count-- {
					each = append(each, 1, delta)
				}
			}
			each[1] = uint32(len(each)/2 - 1)
			stts.payload = table(each...)
			raw := encodeM4A(nodes)
			want := f.want(t)
			for _, frame := range append([]int64{0}, f.SeekFrames...) {
				d, err := openPCM(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				if d.Format().Frames != f.Frames {
					t.Fatalf("%d frames, want %d", d.Format().Frames, f.Frames)
				}
				if err := d.Seek(ctx, frame); err != nil {
					t.Fatalf("seek %d: %v", frame, err)
				}
				requireSameSamples(t, readAllFrames(t, d, 2*777), want[2*frame:])
				d.Close()
			}
		})
	}

	_, base := g.fixture(t, alac16)
	const n = 4096
	for _, c := range []struct {
		runs   int
		reason decode.Reason
	}{{1024, ""}, {1025, decode.ReasonUnsupportedLayout}} {
		stts := make([][2]uint32, n)
		for p := range stts {
			stts[p] = [2]uint32{1, 1 + uint32(min(p, c.runs-1)%2)}
		}
		err := openALACErr(alacOneBytePackets(t, base, n, stts, [][3]uint32{{1, n, 1}}, 1))
		if c.reason == "" && err != nil {
			t.Errorf("%d runs: %v", c.runs, err)
		} else if c.reason != "" {
			refusedWith(t, err, c.reason, "1024 runs")
		}
	}
}

// packetsSpy stands in for saprobe-alac: it counts its construction and
// decodes, and panics on the decode numbered panicOn (from 1; 0 never).
type packetsSpy struct {
	made    *int
	decodes int
	panicOn int
	lib     *alac.PacketDecoder
}

func (s *packetsSpy) DecodePacket(p []byte) ([]byte, error) {
	s.decodes++
	if s.decodes == s.panicOn {
		var table []int
		_ = table[s.decodes] // the library's kind of panic: an index out of range
	}
	return s.lib.DecodePacket(p)
}

func spyFactory(made *int, panicOn int) func(alac.PacketConfig) (decode.ALACPackets, error) {
	return func(cfg alac.PacketConfig) (decode.ALACPackets, error) {
		*made++
		lib, err := alac.NewPacketDecoder(cfg)
		if err != nil {
			return nil, err
		}
		return &packetsSpy{made: made, panicOn: panicOn, lib: lib}, nil
	}
}

// A malformed cookie is refused before the library is constructed: the
// cases saprobe-alac panics or over-allocates on (Codex's B470 robustness
// run: channel counts of 0 and 9, frame lengths of 0 and 4,194,304, a rate
// of 0) and the others the validation refuses. The golden itself constructs
// the library once.
func TestALACMalformedCookieNeverReachesTheLibrary(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	made := 0
	d, err := decode.OpenALACWith(bytes.NewReader(base), spyFactory(&made, 0))
	if err != nil || made != 1 {
		t.Fatalf("the golden: err=%v, library made %d times, want once", err, made)
	}
	d.Close()
	for name, mutate := range map[string]func(testing.TB, []*m4aNode) []*m4aNode{
		"0 channels":           stsdByte(cookieChannels, 0),
		"9 channels":           stsdByte(cookieChannels, 9),
		"frame length 0":       stsdSet(cookieFrameLength, 0),
		"frame length 4194304": stsdSet(cookieFrameLength, 4<<20),
		"sample rate 0":        stsdSet(cookieSampleRate, 0),
		"compatible version 1": stsdByte(cookieCompatible, 1),
		"20 bits":              stsdByte(cookieBitDepth, 20),
		"kb 200":               stsdByte(cookieKB, 200),
		"max frame bytes huge": stsdSet(cookieMaxFrame, math.MaxUint32),
		"the alac box cut":     stsdSet(stsdAlacBoxSize, 20),
	} {
		made := 0
		_, err := decode.OpenALACWith(bytes.NewReader(encodeM4A(mutate(t, m4aMoovLast(t, base)))), spyFactory(&made, 0))
		if !decode.IsRefusal(err) {
			t.Errorf("%s: got %v, want a refusal", name, err)
		}
		if made != 0 {
			t.Errorf("%s: the library was constructed %d times before the refusal", name, made)
		}
	}
}

// A panic in the packet decoder is a truncated_or_corrupt refusal that
// returns nothing of that packet; the samples before it stand, and after a
// seek the next packet decodes with a newly constructed decoder.
func TestALACInjectedPanicIsARefusal(t *testing.T) {
	g := loadALACGoldens(t)
	f, raw := g.fixture(t, alac24)
	made := 0
	d, err := decode.OpenALACWith(bytes.NewReader(raw), spyFactory(&made, 2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	want := f.want(t)
	ctx := context.Background()
	buf := make([]int32, 2*3*4096)
	n, err := d.ReadFrames(ctx, buf)
	refusedWith(t, err, decode.ReasonTruncated, "packet 1")
	if n != 2*4096 {
		t.Fatalf("%d frames before the refusal, want packet 0's 4096", n/2)
	}
	requireSameSamples(t, buf[:n], want[:n])
	if p := decode.ALACLibraryPanics(d); p != 1 {
		t.Fatalf("%d panics counted, want 1", p)
	}
	if err := d.Seek(ctx, 2*4096); err != nil {
		t.Fatal(err)
	}
	n, err = d.ReadFrames(ctx, buf[:2*4096])
	if err != nil || n != 2*4096 {
		t.Fatalf("after the seek: n=%d err=%v", n, err)
	}
	requireSameSamples(t, buf[:n], want[2*2*4096:2*3*4096])
	if made != 2 {
		t.Fatalf("library made %d times, want 2: once at open, once after the panic", made)
	}
}

// The real library's panic on alacPanicPacket is counted as a panic, so the
// fuzz targets can tell it from any other refusal.
func TestALACRealLibraryPanicIsCounted(t *testing.T) {
	g := loadALACGoldens(t)
	_, raw := g.fixture(t, alac16)
	d, err := openPCM(bytes.NewReader(m4aReplacePacket(t, raw, 0, alacPanicPacket)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.ReadFrames(context.Background(), make([]int32, 2*16)); !decode.IsRefusal(err) {
		t.Fatalf("got %v, want a refusal", err)
	}
	if p := decode.ALACLibraryPanics(d); p != 1 {
		t.Fatalf("%d panics counted, want 1", p)
	}
}

// A file that ends inside moov is cut there for the sniffer the way its
// 4 MiB cap cuts a large moov: the codec found before the cut decides. An
// AAC file cut after its stsd is refused as lossy, not as a layout the
// sniffer could not walk.
func TestSniffKeepsTheCodecWhenTheFileEndsInsideMoov(t *testing.T) {
	g := loadALACGoldens(t)
	_, aac := g.fixture(t, "afconvert_aac_44100.m4a")
	stsd := bytes.Index(aac, []byte("stsd"))
	moov := bytes.Index(aac, []byte("moov"))
	if stsd < 0 || moov < 0 || stsd < moov {
		t.Fatal("the AAC golden has no moov before its stsd")
	}
	end := moov - 4 + int(binary.BigEndian.Uint32(aac[moov-4:]))
	cut := stsd + 200
	if cut >= end {
		t.Fatalf("cut at %d is not inside moov (ends at %d)", cut, end)
	}
	_, err := decode.Open(bytes.NewReader(aac[:cut]))
	refusedWith(t, err, decode.ReasonLossy, "AAC")
}
