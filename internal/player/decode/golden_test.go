package decode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

type goldenEntry struct {
	File          string  `json:"file"`
	Container     string  `json:"container"`
	RateHz        int     `json:"rate_hz"`
	Channels      int     `json:"channels"`
	ValidBits     int     `json:"valid_bits"`
	Frames        int64   `json:"frames"`
	SamplesSHA256 string  `json:"samples_sha256"`
	SeekFrames    []int64 `json:"seek_frames"`
}

func loadManifest(t *testing.T) []goldenEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "goldens", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []goldenEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("empty golden manifest")
	}
	return entries
}

func TestGoldensDecodeAndHash(t *testing.T) {
	for _, g := range loadManifest(t) {
		g := g
		t.Run(g.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "goldens", g.File))
			if err != nil {
				t.Fatal(err)
			}
			d, err := decode.Open(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			pcm := d.Format()
			if pcm.RateHz != g.RateHz || pcm.Channels != g.Channels || pcm.ValidBits != g.ValidBits {
				t.Fatalf("format %+v, want rate=%d ch=%d bits=%d", pcm, g.RateHz, g.Channels, g.ValidBits)
			}
			if pcm.Frames != g.Frames {
				t.Fatalf("frames %d want %d", pcm.Frames, g.Frames)
			}
			samples := readAll(t, d)
			if int64(len(samples)) != g.Frames*int64(g.Channels) {
				t.Fatalf("got %d samples, want %d", len(samples), g.Frames*int64(g.Channels))
			}
			sum := sha256.New()
			var le [4]byte
			for _, s := range samples {
				binary.LittleEndian.PutUint32(le[:], uint32(s))
				sum.Write(le[:])
			}
			got := hexLower(sum.Sum(nil))
			if got != g.SamplesSHA256 {
				t.Fatalf("samples hash %s, want %s (independent reference)", got, g.SamplesSHA256)
			}
		})
	}
}

func TestGoldensExactSeek(t *testing.T) {
	for _, g := range loadManifest(t) {
		if len(g.SeekFrames) == 0 {
			continue
		}
		g := g
		t.Run(g.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "goldens", g.File))
			if err != nil {
				t.Fatal(err)
			}
			// Full decode once for expected samples at seek points.
			d0, err := decode.Open(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			all := readAll(t, d0)
			d0.Close()

			d, err := decode.Open(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx := context.Background()
			buf := make([]int32, g.Channels)
			for _, frame := range g.SeekFrames {
				if frame >= g.Frames {
					continue
				}
				if err := d.Seek(ctx, frame); err != nil {
					t.Fatalf("seek %d: %v", frame, err)
				}
				n, err := d.ReadFrames(ctx, buf)
				// In-range seeks must return a full frame of samples. A broken
				// seek that yields (0, EOF) must not pass.
				if n != g.Channels {
					t.Fatalf("seek %d: n=%d err=%v, want %d samples", frame, n, err, g.Channels)
				}
				off := int(frame) * g.Channels
				for c := 0; c < n; c++ {
					if buf[c] != all[off+c] {
						t.Fatalf("seek %d ch %d: got %d want %d", frame, c, buf[c], all[off+c])
					}
				}
			}
		})
	}
}

func hexLower(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexdigits[v>>4]
		out[i*2+1] = hexdigits[v&0xf]
	}
	return string(out)
}
