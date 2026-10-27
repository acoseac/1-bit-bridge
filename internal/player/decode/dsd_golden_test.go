package decode_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The DSD goldens are written by scripts/player-dsd-goldens/generate.py from
// the DSF and DSDIFF specifications, around a pattern specified there in the
// canonical form (bit 7 earliest). Nothing here derives an expectation from
// the bridge's own decoder.

type dsdFixture struct {
	File            string  `json:"file"`
	Container       string  `json:"container"`
	BitRateHz       int     `json:"bit_rate_hz"`
	Channels        int     `json:"channels"`
	Frames          int64   `json:"frames"`
	BytesPerChannel int64   `json:"bytes_per_channel"`
	SourceBitOrder  string  `json:"source_bit_order"`
	SeekBytes       []int64 `json:"seek_bytes"`
	Refusal         string  `json:"refusal"`
}

type dsdGoldens struct {
	PatternFile  string       `json:"pattern_file"`
	PatternBytes int64        `json:"pattern_bytes_per_channel"`
	Fixtures     []dsdFixture `json:"fixtures"`
	pattern      []byte
}

// -dsd-goldens points every DSD golden test at another directory, such as a
// scratch regeneration (generate.py --out DIR), so a change to the generator
// can be checked without touching the committed fixtures:
//
//	go test ./internal/player/decode -args -dsd-goldens=DIR
var dsdGoldensDir = flag.String("dsd-goldens", filepath.Join("testdata", "dsd-goldens"),
	"directory of the DSD goldens and their manifest")

func dsdGoldenPath(name string) string {
	return filepath.Join(*dsdGoldensDir, name)
}

func loadDSDGoldens(t testing.TB) dsdGoldens {
	t.Helper()
	b, err := os.ReadFile(dsdGoldenPath("manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g dsdGoldens
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.Fixtures) == 0 {
		t.Fatal("empty DSD golden manifest")
	}
	g.pattern, err = os.ReadFile(dsdGoldenPath(g.PatternFile))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(g.pattern)) != 2*g.PatternBytes {
		t.Fatalf("pattern.bin is %d bytes, want %d", len(g.pattern), 2*g.PatternBytes)
	}
	return g
}

// want returns the canonical left and right bytes a fixture must decode to.
func (g dsdGoldens) want(f dsdFixture) [][]byte {
	return [][]byte{
		g.pattern[:f.BytesPerChannel],
		g.pattern[g.PatternBytes : g.PatternBytes+f.BytesPerChannel],
	}
}

func (g dsdGoldens) fixture(t testing.TB, name string) (dsdFixture, []byte) {
	t.Helper()
	for _, f := range g.Fixtures {
		if f.File == name {
			raw, err := os.ReadFile(dsdGoldenPath(name))
			if err != nil {
				t.Fatal(err)
			}
			return f, raw
		}
	}
	t.Fatalf("no DSD golden %s", name)
	return dsdFixture{}, nil
}

// openDSD opens rs and requires a DSDDecoder.
func openDSD(rs io.ReadSeeker) (decode.DSDDecoder, error) {
	s, err := decode.Open(rs)
	if err != nil {
		return nil, err
	}
	d, ok := s.(decode.DSDDecoder)
	if !ok {
		s.Close()
		return nil, fmt.Errorf("opened %T, want a DSDDecoder", s)
	}
	return d, nil
}

func dsdBufs(channels, n int) [][]byte {
	dst := make([][]byte, channels)
	for c := range dst {
		dst[c] = make([]byte, n)
	}
	return dst
}

// readAllDSD reads d to io.EOF with per-channel reads of chunk bytes.
func readAllDSD(t *testing.T, d decode.DSDDecoder, chunk int) [][]byte {
	t.Helper()
	ch := d.Format().Channels
	out := make([][]byte, ch)
	dst := dsdBufs(ch, chunk)
	for {
		n, err := d.ReadDSD(context.Background(), dst)
		for c := range out {
			out[c] = append(out[c], dst[c][:n]...)
		}
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadDSD: n=%d err=%v", n, err)
		}
		if n == 0 {
			t.Fatal("ReadDSD returned 0 without EOF")
		}
	}
}

func requireSameDSD(t *testing.T, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d channels, want %d", len(got), len(want))
	}
	for c := range want {
		if len(got[c]) != len(want[c]) {
			t.Fatalf("channel %d: %d bytes, want %d", c, len(got[c]), len(want[c]))
		}
		if i := firstDiff(got[c], want[c]); i >= 0 {
			t.Fatalf("channel %d byte %d: got %#02x want %#02x", c, i, got[c][i], want[c][i])
		}
	}
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return len(a)
	}
	return -1
}

func TestDSDGoldensMatchPattern(t *testing.T) {
	g := loadDSDGoldens(t)
	for _, f := range g.Fixtures {
		if f.Refusal != "" {
			continue
		}
		f := f
		t.Run(f.File, func(t *testing.T) {
			_, raw := g.fixture(t, f.File)
			// 4096 is DSF's block; 777 and 1 make every read straddle blocks
			// at a different offset.
			for _, chunk := range []int{4096, 777, 1, 1 << 16} {
				d, err := openDSD(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				got := d.Format()
				want := format.NativeDSD{
					BitRateHz: f.BitRateHz,
					Channels:  f.Channels,
					Container: format.Container(f.Container),
					Frames:    f.Frames,
				}
				if got != want {
					t.Fatalf("format %+v, want %+v", got, want)
				}
				if err := got.Validate(); err != nil {
					t.Fatal(err)
				}
				if got.BytesPerChannel() != f.BytesPerChannel {
					t.Fatalf("BytesPerChannel %d, want %d", got.BytesPerChannel(), f.BytesPerChannel)
				}
				requireSameDSD(t, readAllDSD(t, d, chunk), g.want(f))
				d.Close()
			}
		})
	}
}

func TestDSDGoldensExactSeek(t *testing.T) {
	g := loadDSDGoldens(t)
	for _, f := range g.Fixtures {
		if f.Refusal != "" {
			continue
		}
		f := f
		t.Run(f.File, func(t *testing.T) {
			_, raw := g.fixture(t, f.File)
			want := g.want(f)
			d, err := openDSD(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			ctx := context.Background()
			// Forward, then backward, so a seek that only works from the
			// start or only moves forward fails.
			order := append([]int64{}, f.SeekBytes...)
			for i := len(f.SeekBytes) - 1; i >= 0; i-- {
				order = append(order, f.SeekBytes[i])
			}
			dst := dsdBufs(2, 16)
			for _, pos := range order {
				if err := d.Seek(ctx, pos); err != nil {
					t.Fatalf("seek %d: %v", pos, err)
				}
				n, err := d.ReadDSD(ctx, dst)
				left := f.BytesPerChannel - pos
				if left == 0 {
					if n != 0 || !errors.Is(err, io.EOF) {
						t.Fatalf("seek to the end: n=%d err=%v, want 0, EOF", n, err)
					}
					continue
				}
				wantN := int(min64(16, left))
				// An in-range seek must return data: a broken seek that
				// yields (0, EOF) must not pass.
				if n != wantN {
					t.Fatalf("seek %d: n=%d err=%v, want %d", pos, n, err, wantN)
				}
				if err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("seek %d: %v", pos, err)
				}
				for c := 0; c < 2; c++ {
					if !bytes.Equal(dst[c][:n], want[c][pos:pos+int64(n)]) {
						t.Fatalf("seek %d channel %d: got %x want %x", pos, c, dst[c][:n], want[c][pos:pos+int64(n)])
					}
				}
			}
			if err := d.Seek(ctx, f.BytesPerChannel+1); !decode.IsRefusal(err) {
				t.Fatalf("seek past the end: %v, want a refusal", err)
			}
			if err := d.Seek(ctx, -1); !decode.IsRefusal(err) {
				t.Fatalf("seek before the start: %v, want a refusal", err)
			}
		})
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// One read larger than the stream returns exactly the declared bytes and
// io.EOF: no padding, no metadata, and the next read is (0, EOF).
func TestDSDGoldensEOFAtSampleCount(t *testing.T) {
	g := loadDSDGoldens(t)
	for _, f := range g.Fixtures {
		if f.Refusal != "" {
			continue
		}
		f := f
		t.Run(f.File, func(t *testing.T) {
			_, raw := g.fixture(t, f.File)
			d, err := openDSD(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			dst := dsdBufs(2, int(f.BytesPerChannel)+5000)
			var got [][]byte = [][]byte{nil, nil}
			for {
				n, err := d.ReadDSD(context.Background(), dst)
				for c := range got {
					got[c] = append(got[c], dst[c][:n]...)
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			requireSameDSD(t, got, g.want(f))
			n, err := d.ReadDSD(context.Background(), dst)
			if n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("read after the end: n=%d err=%v", n, err)
			}
		})
	}
}

func TestDSDGoldensRefused(t *testing.T) {
	g := loadDSDGoldens(t)
	seen := 0
	for _, f := range g.Fixtures {
		if f.Refusal == "" {
			continue
		}
		seen++
		_, raw := g.fixture(t, f.File)
		_, err := decode.Open(bytes.NewReader(raw))
		r, ok := decode.AsRefusal(err)
		if !ok || string(r.Reason) != f.Refusal {
			t.Errorf("%s: got %v, want refusal %s", f.File, err, f.Refusal)
		}
	}
	if seen != 5 {
		t.Fatalf("%d refusal fixtures, want 5", seen)
	}
}

// A file cut inside the audio returns the whole bytes before the cut, then a
// truncated refusal; it never reads past the cut as audio.
func TestDSDTruncatedMidBlock(t *testing.T) {
	g := loadDSDGoldens(t)
	cases := []struct {
		file string
		cut  int // file length kept
		// min and max bytes per channel that may come back before the refusal
		min, max int
	}{
		// DSF data starts at 92: block group 0 is [92, 92+8192). Cutting 500
		// bytes into the right channel's block of group 1 leaves group 0
		// whole and 500 bytes of group 1 in both channels.
		{"lsb_dsd64_partial_block.dsf", 92 + 8192 + 4096 + 500, 4096, 4096 + 500},
		// Cutting inside group 1's left block: the right channel has none.
		{"msb_dsd128_partial_block.dsf", 92 + 8192 + 100, 4096, 4096},
		// DFF: an odd cut leaves half a frame.
		{"dsd64.dff", 0, 1, 13287},
	}
	for _, c := range cases {
		c := c
		t.Run(c.file, func(t *testing.T) {
			f, raw := g.fixture(t, c.file)
			cut := c.cut
			if cut == 0 {
				start := dffSoundDataStart(t, raw)
				cut = start + 2*5000 + 1
			}
			reader := bytes.NewReader(raw)
			d, err := openDSD(reader)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			at, _ := reader.Seek(0, io.SeekCurrent)
			reader.Reset(raw[:cut])
			_, _ = reader.Seek(at, io.SeekStart)
			want := g.want(f)
			got := [][]byte{nil, nil}
			dst := dsdBufs(2, 1000)
			var last error
			for i := 0; i < 1000; i++ {
				n, err := d.ReadDSD(context.Background(), dst)
				for ch := range got {
					got[ch] = append(got[ch], dst[ch][:n]...)
				}
				if err != nil {
					last = err
					break
				}
			}
			r, ok := decode.AsRefusal(last)
			if !ok || r.Reason != decode.ReasonTruncated {
				t.Fatalf("got %v, want a truncated refusal", last)
			}
			if len(got[0]) < c.min || len(got[0]) > c.max {
				t.Fatalf("%d bytes before the refusal, want %d..%d", len(got[0]), c.min, c.max)
			}
			for ch := range got {
				if !bytes.Equal(got[ch], want[ch][:len(got[ch])]) {
					t.Fatalf("channel %d: bytes before the cut differ from the pattern at %d", ch, firstDiff(got[ch], want[ch]))
				}
			}
			if c.cut == 0 && len(got[0]) != 5000 {
				t.Fatalf("DFF cut after 5000 frames and a half: got %d bytes", len(got[0]))
			}
		})
	}
}

// dffSoundDataStart returns the body offset of the DSD sound chunk, which the
// generator writes straight after PROP. (A text search would find CMPR's
// "DSD " compression type first.)
func dffSoundDataStart(t testing.TB, raw []byte) int {
	t.Helper()
	prop := bytes.Index(raw, []byte("PROP"))
	if prop < 0 {
		t.Fatal("no PROP chunk")
	}
	size := int(binary.BigEndian.Uint64(raw[prop+4:]))
	next := prop + 12 + size + size&1
	if string(raw[next:next+4]) != "DSD " {
		t.Fatalf("chunk after PROP is %q, want the DSD sound data", raw[next:next+4])
	}
	return next + 12
}

// An ID3 tag in front of a DSF is skipped by the sniffer, and the DSF still
// decodes to the pattern.
func TestDSDAfterID3Prefix(t *testing.T) {
	g := loadDSDGoldens(t)
	f, raw := g.fixture(t, "msb_dsd128_partial_block.dsf")
	tag := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, 0}
	d, err := openDSD(bytes.NewReader(append(tag, raw...)))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	requireSameDSD(t, readAllDSD(t, d, 4096), g.want(f))
}
