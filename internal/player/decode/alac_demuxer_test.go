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
// counts, without allocating anything near their declared sizes.
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
