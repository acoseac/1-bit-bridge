package alsapcm

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
				commanded := make(chan struct{})
				s := openTestSinkWith(t, context.Background(), k, cfg, sinkSeams{commanded: func(op string) {
					if op == "drain" {
						close(commanded)
					}
				}})
				n := s.Params().BufferFrames
				// Hold the final first-buffer measurement before the writer can
				// feed buffer two. Drain must be admitted before that next feed:
				// cancellation during a full-buffer write adds an EAGAIN retry.
				measured, resume := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(resume) }) }
				defer release()
				if scenario == "natural_advance" {
					k.atNow(func() bool { return s.run.written == int64(n) }, func() {
						close(measured)
						<-resume
					})
				}
				block := make([]byte, n*(tc.bits/8)*2)
				for i := range block {
					block[i] = byte(i*37 + 11)
				}
				if _, err := s.Write(context.Background(), block); err != nil {
					t.Fatal(err)
				}
				if scenario == "pause_release" {
					// No more producer frames: Pause interrupts only an idle watch.
					await(t, k.when(filledAndIdle(uint64(n))), "first PCM buffer")
					if err := s.Pause(); err != nil {
						t.Fatal(err)
					}
					if err := s.Drop(); err != nil {
						t.Fatal(err)
					}
				} else {
					await(t, measured, "final first-buffer measurement")
					if !s.Next(cfg.Transport).Gapless {
						t.Fatal("compatible advance lost")
					}
					if _, err := s.Write(context.Background(), block); err != nil {
						t.Fatal(err)
					}
					drained := make(chan error, 1)
					go func() { drained <- s.Drain(context.Background()) }()
					await(t, commanded, "Drain admission")
					k.free()
					release()
					if err := <-drained; err != nil {
						t.Fatal(err)
					}
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
					t.Fatalf("PCM trace differs from base: %s: %s", path, pcmTraceDifference(b, want))
				}
			})
		}
	}
}

// pcmTrace retains every ioctl, the committed parameters and every accepted byte.
type pcmTrace struct {
	Calls    []uint32
	HWParams string
	Written  string
}

// pcmTraceDifference diagnoses a failed byte-exact comparison; it never decides
// equality or normalizes a golden. Indices and byte offsets are zero-based.
func pcmTraceDifference(got, want []byte) string {
	var g, w pcmTrace
	if err := json.Unmarshal(got, &g); err != nil {
		return fmt.Sprintf("actual JSON: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		return fmt.Sprintf("golden JSON: %v", err)
	}
	for i := 0; i < min(len(g.Calls), len(w.Calls)); i++ {
		if g.Calls[i] != w.Calls[i] {
			return fmt.Sprintf("Calls[%d]: got %d, want %d (lengths %d/%d)", i, g.Calls[i], w.Calls[i], len(g.Calls), len(w.Calls))
		}
	}
	if len(g.Calls) != len(w.Calls) {
		return fmt.Sprintf("Calls[%d]: length got %d, want %d", min(len(g.Calls), len(w.Calls)), len(g.Calls), len(w.Calls))
	}
	for _, field := range []struct{ name, got, want string }{
		{"HWParams", g.HWParams, w.HWParams}, {"Written", g.Written, w.Written},
	} {
		gb, ge := hex.DecodeString(field.got)
		wb, we := hex.DecodeString(field.want)
		if ge != nil || we != nil {
			return fmt.Sprintf("%s: invalid hex (got %v, want %v)", field.name, ge, we)
		}
		if !bytes.Equal(gb, wb) {
			return pcmByteDifference(field.name, gb, wb)
		}
	}
	return pcmByteDifference("JSON encoding", got, want)
}

// pcmByteDifference names the first changed or missing byte of a field.
func pcmByteDifference(field string, got, want []byte) string {
	for i := 0; i < min(len(got), len(want)); i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("%s byte offset %d: got 0x%02x, want 0x%02x", field, i, got[i], want[i])
		}
	}
	return fmt.Sprintf("%s byte offset %d: length got %d, want %d", field, min(len(got), len(want)), len(got), len(want))
}
