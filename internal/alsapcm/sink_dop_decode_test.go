package alsapcm_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDoPBlockBoundary(t *testing.T) {
	raw, e := os.ReadFile(filepath.Join("..", "player", "decode", "testdata", "dsd-goldens", "lsb_dsd64_partial_block.dsf"))
	if e != nil {
		t.Fatal(e)
	}
	binary.LittleEndian.PutUint64(raw[64:72], 32769)
	src, e := decode.Open(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	defer src.Close()
	d := src.(decode.DSDDecoder)
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(10)
	k.Free()
	s, e := k.OpenNativeSink(context.Background(), alsapcm.DSDConfig{Kind: format.KindDoP, Endpoint: alsapcm.FakeEndpoint(), Format: format.S32LE, BitRate: 2822400, Pause: alsapcm.PauseKernel})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Release(context.Background(), alsapcm.Immediate)
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	planes := [][]byte{make([]byte, 777), make([]byte, 777)}
	remaining := 32769
	total := 0
	for {
		n, re := d.ReadDSD(context.Background(), planes)
		bits := min(n*8, remaining)
		if bits > 0 {
			got, we := s.WriteDSD(context.Background(), 1, planes[0][:n], planes[1][:n], 0, bits)
			if we != nil || got != bits {
				t.Fatal(got, we)
			}
			remaining -= bits
		}
		total += n
		if re == io.EOF {
			break
		}
		if re != nil {
			t.Fatal(re)
		}
	}
	if total != 4097 || remaining != 0 {
		t.Fatal("container padding leaked", total, remaining)
	}
	if e = s.Finish(context.Background()); e != nil {
		t.Fatal(e)
	}
	if s.Position().Played != 32769 || len(k.Given()) != 2049*8 {
		t.Fatal("logical/padded lengths", s.Position(), len(k.Given()))
	}
	pattern, e := os.ReadFile(filepath.Join("..", "player", "decode", "testdata", "dsd-goldens", "pattern.bin"))
	if e != nil {
		t.Fatal(e)
	}
	got := k.Given()
	for frame := 0; frame < 2049; frame++ {
		for ch := 0; ch < 2; ch++ {
			var word uint16
			for j := 0; j < 16; j++ {
				at := frame*16 + j
				v := (byte(0x69) >> uint(7-j%8)) & 1
				if at < 32769 {
					v = (pattern[ch*13288+at/8] >> uint(7-at%8)) & 1
				}
				word |= uint16(v) << uint(15-j)
			}
			off := frame*8 + ch*4
			if got[off+1] != byte(word) || got[off+2] != byte(word>>8) {
				t.Fatalf("frame %d channel %d payload", frame, ch)
			}
		}
	}
	k.CheckClosedEach(t)
}
