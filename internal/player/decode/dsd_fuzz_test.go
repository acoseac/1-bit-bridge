package decode_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

func FuzzDSFOpen(f *testing.F) { fuzzDSD(f, ".dsf") }

func FuzzDFFOpen(f *testing.F) { fuzzDSD(f, ".dff") }

// fuzzDSD seeds with every DSD golden of one container and checks, for any
// input that opens as DSD: the format is a shape the player accepts; a read
// never returns more than asked, more than BytesPerChannel, or more audio
// than the input holds; a clean EOF comes exactly at BytesPerChannel; and
// seeks to the start and the end succeed while one past the end is refused.
func fuzzDSD(f *testing.F, ext string) {
	entries, err := os.ReadDir(filepath.Join("testdata", "dsd-goldens"))
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ext) {
			raw, err := os.ReadFile(dsdGoldenPath(e.Name()))
			if err != nil {
				f.Fatal(err)
			}
			f.Add(raw)
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := decode.Open(bytes.NewReader(raw))
		if err != nil {
			return
		}
		defer s.Close()
		d, ok := s.(decode.DSDDecoder)
		if !ok {
			return // the mutation made another container
		}
		fm := d.Format()
		if err := fm.Validate(); err != nil {
			t.Fatalf("opened an unacceptable shape: %v", err)
		}
		ctx := context.Background()
		const ask = 777
		dst := dsdBufs(fm.Channels, ask)
		total := int64(0)
		var last error
		for i := 0; ; i++ {
			n, err := d.ReadDSD(ctx, dst)
			if n < 0 || n > ask {
				t.Fatalf("read returned %d bytes for %d asked", n, ask)
			}
			total += int64(n)
			if total > fm.BytesPerChannel() {
				t.Fatalf("read %d bytes per channel, the stream declares %d", total, fm.BytesPerChannel())
			}
			if total*int64(fm.Channels) > int64(len(raw)) {
				t.Fatalf("read %d bytes per channel from a %d-byte input", total, len(raw))
			}
			if err != nil {
				last = err
				break
			}
			if n == 0 {
				t.Fatal("read returned 0 bytes and no error")
			}
			if i > len(raw) {
				t.Fatal("read loop did not end")
			}
		}
		if errors.Is(last, io.EOF) && total != fm.BytesPerChannel() {
			t.Fatalf("EOF after %d bytes per channel, the stream declares %d", total, fm.BytesPerChannel())
		}
		if !errors.Is(last, io.EOF) && !decode.IsRefusal(last) {
			t.Fatalf("read ended with %v, want EOF or a refusal", last)
		}
		if err := d.Seek(ctx, 0); err != nil {
			t.Fatalf("seek to the start: %v", err)
		}
		if err := d.Seek(ctx, fm.BytesPerChannel()); err != nil {
			t.Fatalf("seek to the end: %v", err)
		}
		if n, err := d.ReadDSD(ctx, dst); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read at the end: n=%d err=%v", n, err)
		}
		if err := d.Seek(ctx, fm.BytesPerChannel()+1); !decode.IsRefusal(err) {
			t.Fatalf("seek past the end: %v, want a refusal", err)
		}
	})
}
