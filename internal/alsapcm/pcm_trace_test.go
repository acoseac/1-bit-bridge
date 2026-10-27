package alsapcm

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestPCMTracePins records the base's complete ioctl order, committed HW_PARAMS
// and accepted bytes. These are regression pins, not red-first DSD tests.
func TestPCMTracePins(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rate, bits int
		f          format.ALSAFormat
	}{{"44100_16", 44100, 16, format.S16LE}, {"384000_32", 384000, 32, format.S32LE}} {
		for _, scenario := range []string{"pause_release", "natural_advance"} {
			t.Run(tc.name+"/"+scenario, func(t *testing.T) {
				k := newModel(t)
				cfg := SinkConfig{Endpoint: testEndpoint, Transport: Transport{Kind: format.KindPCM, RateHz: tc.rate, Channels: 2, Format: tc.f, ValidBits: tc.bits}, BufferTime: 40 * time.Millisecond, Pause: PauseKernel}
				s := openTestSink(t, k, cfg)
				n := s.Params().BufferFrames
				block := make([]byte, n*(tc.bits/8)*2)
				for i := range block {
					block[i] = byte(i*37 + 11)
				}
				if _, err := s.Write(context.Background(), block); err != nil {
					t.Fatal(err)
				}
				await(t, k.when(filledAndIdle(uint64(n))), "first PCM buffer")
				if scenario == "pause_release" {
					if err := s.Pause(); err != nil {
						t.Fatal(err)
					}
					if err := s.Drop(); err != nil {
						t.Fatal(err)
					}
				} else {
					if !s.Next(cfg.Transport).Gapless {
						t.Fatal("compatible advance lost")
					}
					if _, err := s.Write(context.Background(), block); err != nil {
						t.Fatal(err)
					}
					k.free()
					drain(t, s)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				var hw string
				k.inspect(func(k *fakeKernel) {
					hw = hex.EncodeToString(unsafe.Slice((*byte)(unsafe.Pointer(k.committed)), int(unsafe.Sizeof(hwParams{}))))
				})
				out := struct {
					Calls    []uint32
					HWParams string
					Written  string
				}{k.calls, hw, hex.EncodeToString(given(k))}
				b, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				b = append(b, '\n')
				path := filepath.Join("testdata", "pcm-traces", tc.name+"_"+scenario+".json")
				if os.Getenv("B470_CAPTURE_PCM_TRACES") == "1" {
					if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, b, 0644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(b, want) {
					t.Fatal(fmt.Sprintf("PCM trace differs from base: %s", path))
				}
			})
		}
	}
}
