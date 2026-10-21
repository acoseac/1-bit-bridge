package decode_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	alac "github.com/mycophonic/saprobe-alac"
)

// FuzzALACOpen fuzzes the whole path: the sniffer, the M4A demuxer, the
// cookie and packet checks, and saprobe-alac's packet decoder. It is seeded
// with every ALAC golden, the FFmpeg encodes, the refusal fixtures, the file
// whose first packet 1b6eb83 panicked on, and the one-packet files of
// alac_overrun_test.go. For anything that opens as ALAC: the format is a
// shape the player accepts; a read never returns more than asked, more than
// Frames, or a sample outside the valid bits; a clean EOF comes exactly at
// Frames, and any other end is a refusal; seeks to the start, the middle and
// the end succeed and one past the end is refused.
//
// The demuxer then opens the file again with overrunOracle in the library's
// place, which decodes every packet the library decodes twice more with
// bytes appended to it. A decode that reads nothing past the packet's end
// gives the same samples each time, so a difference is a packet decoded from
// past its end, and it fails the target.
//
// A packet the library panicked on is a refusal like any malformed packet.
// With BRIDGE_ALAC_FUZZ_STRICT set it fails the target instead: that is the
// mode that looks for library panics no check prevents
// (ops/upstream/saprobe-alac.md).
func FuzzALACOpen(f *testing.F) {
	strict := os.Getenv("BRIDGE_ALAC_FUZZ_STRICT") != ""
	g := loadALACGoldens(f)
	for _, fx := range g.Fixtures {
		f.Add(readALACGolden(f, fx.File))
	}
	for _, fx := range g.FFmpegFixtures {
		f.Add(readALACGolden(f, fx.File))
	}
	_, base := g.fixture(f, alac16)
	f.Add(m4aReplacePacket(f, base, 0, alacPanicPacket))
	for _, s := range []struct {
		pkt    []byte
		frames uint32
	}{
		{alacShifted16Packet, 1},
		{alacEscapePastEndPacket, 1},
		{alacStopBitPastEndPacket, 1},
		{alacRunPastEndPacket, 16},
	} {
		f.Add(alacSinglePacket(f, base, s.pkt, s.frames))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := decode.Open(bytes.NewReader(raw))
		if err != nil {
			if !decode.IsRefusal(err) {
				t.Fatalf("open: %v, want a refusal", err)
			}
			return
		}
		defer s.Close()
		d, ok := s.(decode.Decoder)
		if !ok || d.Format().Container != format.ContainerALAC {
			return // the mutation made another container
		}
		fm := d.Format()
		if err := fm.Validate(); err != nil {
			t.Fatalf("opened an unacceptable shape: %v", err)
		}
		lo, hi := -(int64(1) << (fm.ValidBits - 1)), int64(1)<<(fm.ValidBits-1)-1
		ctx := context.Background()
		const ask = 2 * 777
		buf := make([]int32, ask)
		total := int64(0)
		var last error
		for i := 0; ; i++ {
			n, err := d.ReadFrames(ctx, buf)
			if n < 0 || n > ask || n%2 != 0 {
				t.Fatalf("read returned %d samples for %d asked", n, ask)
			}
			for _, s := range buf[:n] {
				if int64(s) < lo || int64(s) > hi {
					t.Fatalf("sample %d outside %d valid bits", s, fm.ValidBits)
				}
			}
			total += int64(n / 2)
			if total > fm.Frames {
				t.Fatalf("read %d frames, the stream declares %d", total, fm.Frames)
			}
			if err != nil {
				last = err
				break
			}
			if n == 0 {
				t.Fatal("read returned 0 samples and no error")
			}
			if int64(i) > fm.Frames {
				t.Fatal("read loop did not end")
			}
		}
		if strict && decode.ALACLibraryPanics(d) > 0 {
			t.Fatalf("saprobe-alac panicked on a packet that passed every check: %v", last)
		}
		if errors.Is(last, io.EOF) && total != fm.Frames {
			t.Fatalf("EOF after %d frames, the stream declares %d", total, fm.Frames)
		}
		if !errors.Is(last, io.EOF) && !decode.IsRefusal(last) {
			t.Fatalf("read ended with %v, want EOF or a refusal", last)
		}
		for _, at := range []int64{0, fm.Frames / 2, fm.Frames} {
			if err := d.Seek(ctx, at); err != nil {
				t.Fatalf("seek %d of %d: %v", at, fm.Frames, err)
			}
		}
		if n, err := d.ReadFrames(ctx, buf); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read at the end: n=%d err=%v", n, err)
		}
		if err := d.Seek(ctx, fm.Frames+1); !decode.IsRefusal(err) {
			t.Fatalf("seek past the end: %v, want a refusal", err)
		}
		caught := 0
		od, err := decode.OpenALACWith(bytes.NewReader(raw), overrunOracleFactory(&caught))
		if err != nil {
			return // a prefix Open reads past, which the demuxer alone does not
		}
		defer od.Close()
		for i := int64(0); i <= fm.Frames; i++ {
			if n, err := od.ReadFrames(ctx, buf); err != nil || n == 0 {
				break
			}
		}
		if caught > 0 {
			t.Fatalf("saprobe-alac decoded %d packets from past their end", caught)
		}
	})
}

// overrunOracle stands in for saprobe-alac and decodes every packet the
// library decodes twice more, with eight bytes of 0xFF and then of 0x55
// appended to it. The library pads a packet with zero bytes, and takes the
// appended ones as part of the packet: a decode that reads nothing past the
// packet's end decodes the same samples each time, and one whose last code
// ran into the padding reads those bits differently, or fails.
type overrunOracle struct {
	lib    *alac.PacketDecoder
	caught *int
}

func (o *overrunOracle) DecodePacket(p []byte) ([]byte, error) {
	out, err := o.lib.DecodePacket(p)
	if err != nil {
		return nil, err
	}
	out = bytes.Clone(out)
	for _, fill := range []byte{0xFF, 0x55} {
		again, err := o.lib.DecodePacket(append(bytes.Clone(p), bytes.Repeat([]byte{fill}, 8)...))
		if err != nil || !bytes.Equal(again, out) {
			*o.caught++
			break
		}
	}
	return out, nil
}

func overrunOracleFactory(caught *int) func(alac.PacketConfig) (decode.ALACPackets, error) {
	return func(cfg alac.PacketConfig) (decode.ALACPackets, error) {
		lib, err := alac.NewPacketDecoder(cfg)
		if err != nil {
			return nil, err
		}
		return &overrunOracle{lib: lib, caught: caught}, nil
	}
}

// FuzzALACCookie fuzzes the cookie validation alone. Any bytes it accepts
// are 24 long and inside every documented range, it changes no field, and
// saprobe-alac builds a decoder from the configuration that decodes a
// one-frame escape packet, the extreme samples at that depth, exactly.
func FuzzALACCookie(f *testing.F) {
	g := loadALACGoldens(f)
	for _, fx := range g.Fixtures {
		stsd := m4aPath(f, m4aSTBL(f, parseM4ANodes(f, readALACGolden(f, fx.File))).children, "stsd").payload
		f.Add(append([]byte(nil), stsd[stsdCookie:stsdCookie+24]...))
	}
	f.Fuzz(func(t *testing.T, cookie []byte) {
		cfg, err := decode.ParseALACCookie(cookie)
		if err != nil {
			if !decode.IsRefusal(err) {
				t.Fatalf("got %v, want a refusal", err)
			}
			return
		}
		be32 := func(o int) uint32 {
			return uint32(cookie[o])<<24 | uint32(cookie[o+1])<<16 | uint32(cookie[o+2])<<8 | uint32(cookie[o+3])
		}
		want := alac.PacketConfig{
			FrameLength: be32(0), BitDepth: cookie[5], PB: cookie[6], MB: cookie[7], KB: cookie[8],
			NumChannels: cookie[9], MaxRun: uint16(cookie[10])<<8 | uint16(cookie[11]),
			MaxFrameBytes: be32(12), AvgBitRate: be32(16), SampleRate: be32(20),
		}
		switch {
		case len(cookie) != 24, cookie[4] != 0:
			t.Fatalf("accepted a %d-byte cookie of compatible version %d", len(cookie), cookie[4])
		case cfg != want:
			t.Fatalf("validation changed the configuration: %+v, the cookie says %+v", cfg, want)
		case cfg.NumChannels != 2, cfg.BitDepth != 16 && cfg.BitDepth != 24 && cfg.BitDepth != 32:
			t.Fatalf("accepted %d channels of %d bits", cfg.NumChannels, cfg.BitDepth)
		case cfg.FrameLength < 32 || cfg.FrameLength > 1<<16, cfg.KB > 31:
			t.Fatalf("accepted a frame length of %d and kb %d", cfg.FrameLength, cfg.KB)
		case cfg.SampleRate == 0 || cfg.SampleRate > 768000:
			t.Fatalf("accepted a rate of %d", cfg.SampleRate)
		case uint64(cfg.MaxFrameBytes) > uint64(cfg.FrameLength)*10+1:
			t.Fatalf("accepted max frame bytes %d for %d frames", cfg.MaxFrameBytes, cfg.FrameLength)
		}
		lib, err := alac.NewPacketDecoder(cfg)
		if err != nil {
			t.Fatalf("saprobe-alac refused an accepted configuration: %v", err)
		}
		low, high := -(int64(1) << (cfg.BitDepth - 1)), int64(1)<<(cfg.BitDepth-1)-1
		out, err := lib.DecodePacket(escapeFrame(int(cfg.BitDepth), low, high))
		if err != nil {
			t.Fatalf("one-frame escape packet: %v", err)
		}
		bps := int(cfg.BitDepth) / 8
		if len(out) != 2*bps {
			t.Fatalf("one-frame escape packet decoded %d bytes, want %d", len(out), 2*bps)
		}
		if got := leSample(out[:bps]); got != low {
			t.Fatalf("left %d, want %d", got, low)
		}
		if got := leSample(out[bps:]); got != high {
			t.Fatalf("right %d, want %d", got, high)
		}
	})
}

// escapeFrame is a stereo packet of one escape (uncompressed) frame: a
// channel pair with the partial flag and a count of 1, then left and right
// at bits each, then END.
func escapeFrame(bits int, left, right int64) []byte {
	var w bitWriter
	w.write(1, 3)  // channel pair
	w.write(0, 4)  // instance tag
	w.write(0, 12) // unused
	w.write(1, 1)  // partial
	w.write(0, 2)  // no shifted bytes
	w.write(1, 1)  // escape
	w.write(1, 32) // one frame
	w.write(uint64(left)&(1<<bits-1), uint(bits))
	w.write(uint64(right)&(1<<bits-1), uint(bits))
	w.write(7, 3) // END
	return w.buf
}

// bitWriter appends bit fields MSB-first, the way ALAC lays a packet out.
type bitWriter struct {
	buf  []byte
	nbit uint
}

func (w *bitWriter) write(v uint64, bits uint) {
	for i := bits; i > 0; i-- {
		if w.nbit%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if v>>(i-1)&1 != 0 {
			w.buf[len(w.buf)-1] |= 1 << (7 - w.nbit%8)
		}
		w.nbit++
	}
}

// leSample reads a little-endian signed sample of len(b) bytes.
func leSample(b []byte) int64 {
	var v int64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | int64(b[i])
	}
	shift := 64 - 8*uint(len(b))
	return v << shift >> shift
}
