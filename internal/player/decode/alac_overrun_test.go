package decode_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	alac "github.com/mycophonic/saprobe-alac"
)

// Packets that saprobe-alac 1b6eb83 decoded wrongly and without an error,
// from Codex's review of #1300 and built beside them. Each is a compressed
// channel pair with the partial flag, so stts can give it the few frames it
// holds, under Apple's 16-bit cookie (pb 40, mb 10, kb 14). The copy of the
// library in third_party/saprobe-alac checks every code against the packet's
// own length (ops/upstream/saprobe-alac.patch); the shifted bytes are refused
// before the library sees the packet.
var (
	// One frame of zero residuals with one shifted byte a sample, 0x12 and
	// 0x34. 1b6eb83 read the shifted bytes and its 16-bit output dropped
	// them: it returned (0, 0) and no error.
	alacShifted16Packet = []byte{0x20, 0x00, 0x14, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x24, 0x68, 0x70}
	// One frame: U is one zero bit, and V opens a nine-bit escape whose 17
	// value bits need 10 more than the packet has.
	alacEscapePastEndPacket = []byte{0x20, 0x00, 0x10, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0x80}
	// One frame: V's code is eight ones that end the packet, so its stop
	// bit is the padding's.
	alacStopBitPastEndPacket = alacPartialPair(1, "0"+"11111111")
	// Sixteen frames: U is a zero and a zero run of 15 (1 0 000), and V a
	// zero and a zero run whose code (1 0 000) has three of its bits past
	// the end.
	alacRunPastEndPacket = alacPartialPair(16, "0"+"10000"+"0"+"10")
)

// alacPartialPair returns a compressed channel pair of frames frames, its
// predictors without coefficients and with a pbFactor of 0 (Codex's
// packets' header), then the entropy-coded bits given, MSB first, padded to a
// byte with zeros.
func alacPartialPair(frames uint32, bits string) []byte {
	var w bitWriter
	w.write(1, 3)  // channel pair
	w.write(0, 4)  // instance tag
	w.write(0, 12) // unused
	w.write(1, 1)  // partial
	w.write(0, 2)  // no shifted bytes
	w.write(0, 1)  // compressed
	w.write(uint64(frames), 32)
	w.write(0, 8) // mixBits
	w.write(0, 8) // mixRes
	for range 2 {
		w.write(0, 8) // mode and denShift
		w.write(0, 8) // pbFactor and the coefficient count
	}
	for _, b := range bits {
		w.write(uint64(b-'0'), 1)
	}
	return w.buf
}

// alacSinglePacket rewrites the 16-bit golden, laid out moov last, as the one
// packet pkt of frames frames: mdat holds it, stsz sizes it, stts and mdhd
// give it its frames, and stsc and stco put it in one chunk.
func alacSinglePacket(t testing.TB, base, pkt []byte, frames uint32) []byte {
	t.Helper()
	nodes := m4aMoovLast(t, base)
	nodes[1].payload = append([]byte(nil), pkt...)
	stbl := m4aSTBL(t, nodes)
	box(t, stbl, "stsz").payload = table(0, 0, 1, uint32(len(pkt)))
	box(t, stbl, "stts").payload = table(0, 1, 1, frames)
	box(t, stbl, "stsc").payload = table(0, 1, 1, 1, 1)
	box(t, stbl, "stco").payload = table(0, 1, uint32(len(nodes[0].encode())+8))
	put32At(m4aPath(t, nodes, "moov", "trak", "mdia", "mdhd").payload, 16, frames)
	return encodeM4A(nodes)
}

// A packet whose last code ends past the packet is a truncated_or_corrupt
// refusal that returns none of it, and the refusal is the library's own
// overrun error, not the recover's: an escape's value, a code's stop bit and
// a zero run's count, each in V, whose bits nothing reads after.
func TestALACCodesPastThePacketAreRefused(t *testing.T) {
	if got := alacPartialPair(1, "0"+"111111111"+"0000000"); !bytes.Equal(got, alacEscapePastEndPacket) {
		t.Fatalf("the builder writes % x for Codex's escape packet, want % x", got, alacEscapePastEndPacket)
	}
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	for _, c := range []struct {
		name   string
		pkt    []byte
		frames uint32
	}{
		{"an escape's value past the end", alacEscapePastEndPacket, 1},
		{"a stop bit past the end", alacStopBitPastEndPacket, 1},
		{"a zero run's count past the end", alacRunPastEndPacket, 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, err := openPCM(bytes.NewReader(alacSinglePacket(t, base, c.pkt, c.frames)))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			buf := make([]int32, 2*4096)
			n, err := d.ReadFrames(context.Background(), buf)
			if n != 0 {
				t.Fatalf("read %d samples of a packet whose last code ends past it: %v", n, buf[:n])
			}
			refusedWith(t, err, decode.ReasonTruncated, "bitstream overrun")
			if p := decode.ALACLibraryPanics(d); p != 0 {
				t.Fatalf("the library panicked %d times; the recover is the last resort", p)
			}
		})
	}
}

// A compressed 16-bit packet that shifts bytes out is refused before the
// library sees it: its 16-bit output has no place for them, and no encoder
// shifts a 16-bit sample (Apple's and FFmpeg's shift only above 16 bits).
func TestALACShiftedBytesIn16BitPacketRefused(t *testing.T) {
	g := loadALACGoldens(t)
	_, base := g.fixture(t, alac16)
	d, err := openPCM(bytes.NewReader(alacSinglePacket(t, base, alacShifted16Packet, 1)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	buf := make([]int32, 2*16)
	n, err := d.ReadFrames(context.Background(), buf)
	if n != 0 {
		t.Fatalf("read %d samples of a 16-bit packet whose shifted bytes are dropped: %v", n, buf[:n])
	}
	refusedWith(t, err, decode.ReasonTruncated, "16-bit")
}

// The copy of saprobe-alac the bridge builds refuses each packet with its
// own overrun error, past every check the bridge makes, so the 32-bit pair
// the header check refuses reaches it too. 1b6eb83 panicked on the first
// three, reading past its padding, and decoded the other three from it.
func TestSaprobeALACRefusesCodesPastThePacket(t *testing.T) {
	config := func(depth uint8) alac.PacketConfig {
		return alac.PacketConfig{FrameLength: 4096, BitDepth: depth, NumChannels: 2,
			PB: 40, MB: 10, KB: 14, MaxRun: 255, SampleRate: 44100}
	}
	for _, c := range []struct {
		name  string
		depth uint8
		pkt   []byte
	}{
		{"the panic packet, 16-bit", 16, alacPanicPacket},
		{"the panic packet, 24-bit", 24, alacPanicPacket},
		{"the panic packet, a 32-bit pair with no shifted byte", 32, alacPanicPacket},
		{"an escape's value past the end", 16, alacEscapePastEndPacket},
		{"a stop bit past the end", 16, alacStopBitPastEndPacket},
		{"a zero run's count past the end", 16, alacRunPastEndPacket},
	} {
		lib, err := alac.NewPacketDecoder(config(c.depth))
		if err != nil {
			t.Fatal(err)
		}
		var out []byte
		var panicked any
		func() {
			defer func() { panicked = recover() }()
			out, err = lib.DecodePacket(c.pkt)
		}()
		switch {
		case panicked != nil:
			t.Errorf("%s: the library panicked: %v", c.name, panicked)
		case err == nil:
			t.Errorf("%s: decoded %d bytes and no error", c.name, len(out))
		case !errors.Is(err, alac.ErrDecode) || !strings.Contains(err.Error(), "bitstream overrun"):
			t.Errorf("%s: got %v, want the library's bitstream overrun", c.name, err)
		}
	}
}
