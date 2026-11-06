package dsdconv

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// TestTheOfflineChainAgrees holds the converter to the offline faithful
// rendition's Stage A (internal/transcode: ffmpeg's DSD decoder at ×0.5,
// then sox's very-high-quality rate to 176.4 kHz and the 30 kHz / 40 kHz
// sinc) on 5th-order fixtures at DSD64: the 1 kHz tone's level within
// 0.1 dB once the chain's 6.02 dB pre-attenuation is put back, and the
// 50 kHz probe's in-band peak and energy within 1 dB of the chain's. Like
// transcode's measurements it runs only with BRIDGE_DSD_FIXTURE_TESTS=1 and
// both tools on the host, which is not the ordinary suite's to pay for.
func TestTheOfflineChainAgrees(t *testing.T) {
	if os.Getenv("BRIDGE_DSD_FIXTURE_TESTS") != "1" {
		t.Skip("set BRIDGE_DSD_FIXTURE_TESTS=1 to compare with the offline chain")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg on this host")
	}
	sox, err := exec.LookPath("sox")
	if err != nil {
		t.Skip("no sox on this host")
	}
	const rate = 2822400
	p := Plan{176400, 24}
	for _, fx := range []struct {
		name string
		freq float64
	}{{"1 kHz", 1000}, {"50 kHz", 50000}} {
		bits := ciff(rate, tones(rate, 1, fx.freq))
		dir := t.TempDir()
		src := filepath.Join(dir, "fixture.dsf")
		var buf bytes.Buffer
		if err := dsdtone.WriteDSF(&buf, rate, 2, dsdtone.PackBits(bits, true), int64(len(bits))); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(dir, "chain.f64")
		ff := exec.Command(ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error", "-i", src, "-map", "0:a:0", "-af", "volume=0.5", "-f", "f32le", "-")
		sx := exec.Command(sox, "-t", "raw", "-e", "float", "-b", "32", "-L", "-r", "352800", "-c", "2", "-",
			"-t", "raw", "-e", "float", "-b", "64", "-L", "-c", "1", out, "remix", "1",
			"rate", "-v", "-L", "176400", "sinc", "-a", "110", "-t", "10000", "-35000")
		pipe, err := ff.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		sx.Stdin = pipe
		if err := sx.Start(); err != nil {
			t.Fatal(err)
		}
		if err := ff.Run(); err != nil {
			t.Fatalf("ffmpeg: %v", err)
		}
		if err := sx.Wait(); err != nil {
			t.Fatalf("sox: %v", err)
		}
		raw, err := os.ReadFile(out)
		if err != nil || len(raw) == 0 || len(raw)%8 != 0 {
			t.Fatalf("the chain wrote %d bytes (%v)", len(raw), err)
		}
		chain := make([]float64, len(raw)/8)
		for i := range chain {
			chain[i] = 2 * math.Float64frombits(binary.LittleEndian.Uint64(raw[8*i:]))
		}
		got, _ := convert(t, [2][]byte{bits, bits}, rate, p, 4096)
		ours := channel(got, 0, 24)
		from, n := 8820, 32768
		if len(chain) < from+n || len(ours) < from+n {
			t.Fatalf("%d and %d samples, want %d", len(chain), len(ours), from+n)
		}
		if fx.freq == 1000 {
			a := dbfs(toneAmplitude(ours[from:from+35280], 176400, 1000))
			b := dbfs(toneAmplitude(chain[from:from+35280], 176400, 1000))
			t.Logf("1 kHz: the converter %.3f dBFS, the offline chain %.3f dBFS", a, b)
			if math.Abs(a-b) > 0.1 {
				t.Errorf("the 1 kHz tone is %.3f dBFS here and %.3f through the offline chain", a, b)
			}
			continue
		}
		om, binHz := spectrumDBFS(ours, from, n, 176400)
		cm, _ := spectrumDBFS(chain, from, n, 176400)
		op, _ := peakInBand(om, binHz, 20, 20000)
		cp, _ := peakInBand(cm, binHz, 20, 20000)
		oe, ce := bandEnergyDB(om, binHz, 20, 20000), bandEnergyDB(cm, binHz, 20, 20000)
		t.Logf("50 kHz: in band the converter peaks %.2f dBFS with %.2f dB, the offline chain %.2f with %.2f", op, oe, cp, ce)
		if op-cp > 1 || oe-ce > 1 {
			t.Errorf("the converter folds more into the band than the offline chain: peak %+.2f dB, energy %+.2f dB", op-cp, oe-ce)
		}
	}
}
