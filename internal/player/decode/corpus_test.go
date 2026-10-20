package decode_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// TestPrivateFormatCorpus walks ~/medialibtest-formats when present and
// reports whether each file decodes or is refused (and why). The files are
// private and must never be committed.
func TestPrivateFormatCorpus(t *testing.T) {
	root := os.Getenv("BRIDGE_FORMAT_CORPUS")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip(err)
		}
		root = filepath.Join(home, "medialibtest-formats")
	}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		t.Skipf("private corpus not present at %s", root)
	}

	var files []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(d.Name()) {
		case ".aif", ".aiff", ".aifc", ".wav", ".WAV", ".flac", ".m4a", ".alac",
			".mp3", ".ogg", ".opus", ".aac":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no audio files in corpus")
	}

	for _, path := range files {
		rel, _ := filepath.Rel(root, path)
		t.Run(rel, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			d, err := decode.Open(f)
			if err != nil {
				if r, ok := decode.AsRefusal(err); ok {
					t.Logf("REFUSED %s: %s", r.Reason, r.Message)
					return
				}
				t.Fatalf("unexpected error: %v", err)
			}
			defer d.Close()
			pcm := d.Format()
			n, err := drain(d)
			if err != nil && err != io.EOF {
				t.Fatalf("decode after open: %v", err)
			}
			t.Logf("DECODED rate=%d ch=%d bits=%d frames=%d samples_read=%d container=%s",
				pcm.RateHz, pcm.Channels, pcm.ValidBits, pcm.Frames, n, pcm.Container)
		})
	}
}

func drain(d decode.Decoder) (int, error) {
	ctx := context.Background()
	buf := make([]int32, 4096)
	total := 0
	for {
		n, err := d.ReadFrames(ctx, buf)
		total += n
		if err == io.EOF {
			return total, io.EOF
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.EOF
		}
	}
}
