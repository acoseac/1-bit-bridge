package decode_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestADecoderGivesDSDExactlyForADSDContainer cross-checks
// format.Container.IsDSD against the decoders on every committed golden:
// content of a container IsDSD names opens a DSDDecoder, content of any
// other a PCM Decoder, and the goldens open every container format.Containers
// names (B470 PR 5b: the player's codecs leave out the DSD containers by
// IsDSD, so it must say what the decoders do).
func TestADecoderGivesDSDExactlyForADSDContainer(t *testing.T) {
	seen := map[format.Container]bool{}
	for _, dir := range []string{"goldens", "alac-goldens", "dsd-goldens"} {
		entries, err := os.ReadDir(filepath.Join("testdata", dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			switch filepath.Ext(e.Name()) {
			case ".json", ".md", ".bin":
				continue
			}
			f, err := os.Open(filepath.Join("testdata", dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			src, err := decode.Open(f)
			if err != nil {
				_ = f.Close()
				continue // a golden of a refusal (DST, six channels, AAC, …)
			}
			var c format.Container
			switch d := src.(type) {
			case decode.Decoder:
				c = d.Format().Container
				if c.IsDSD() {
					t.Errorf("%s/%s: a PCM decoder of %s, which IsDSD names DSD", dir, e.Name(), c)
				}
			case decode.DSDDecoder:
				c = d.Format().Container
				if !c.IsDSD() {
					t.Errorf("%s/%s: a DSD decoder of %s, which IsDSD does not name DSD", dir, e.Name(), c)
				}
			default:
				t.Errorf("%s/%s: a source that is neither decoder (%T)", dir, e.Name(), src)
			}
			seen[c] = true
			_ = src.Close()
			_ = f.Close()
		}
	}
	for _, c := range format.Containers() {
		if !seen[c] {
			t.Errorf("no golden opens container %s: the cross-check does not cover it", c)
		}
	}
	if len(format.Containers()) == 0 {
		t.Error("format.Containers() is empty")
	}
	for c := range seen {
		if !slices.Contains(format.Containers(), c) {
			t.Errorf("a golden opens container %s, which format.Containers() does not name", c)
		}
	}
}
