package decode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// alacReference is one entry of the qualification.json Codex's B470 ALAC
// evaluation wrote: FFmpeg's decode of a file, packed little-endian at the
// stream's depth, as a byte count and a SHA-256, and the same for the suffix
// FFmpeg decodes from each seek frame.
type alacReference struct {
	File   string `json:"file"`
	Format struct {
		SampleRate int `json:"SampleRate"`
		BitDepth   int `json:"BitDepth"`
		Channels   int `json:"Channels"`
	} `json:"format"`
	Frames    int64       `json:"frames"`
	Reference *alacDigest `json:"reference"`
	Seeks     []struct {
		Frame     int64      `json:"Frame"`
		Reference alacDigest `json:"Reference"`
	} `json:"seeks"`
}

type alacDigest struct {
	Bytes  int64  `json:"Bytes"`
	SHA256 string `json:"SHA256"`
}

// TestALACReferenceCorpus checks the ALAC decoder against FFmpeg's decodes
// of files that stay private, the PR 1a corpus convention: the owner's ALAC
// files under ~/medialibtest-formats/alac (BRIDGE_FORMAT_CORPUS moves the
// root) and the FFmpeg encodes and container variants of Codex's evaluation.
// The reference file is BRIDGE_ALAC_REFERENCE, by default that evaluation's
// results/qualification.json; the test skips without it. Every stereo file
// must decode to FFmpeg's bytes whole and from every seek frame; the mono
// and six-channel encodes are refused for their channels, the AAC control
// as lossy, and the variant whose stts contradicts its packets as corrupt.
func TestALACReferenceCorpus(t *testing.T) {
	home, _ := os.UserHomeDir()
	refPath := os.Getenv("BRIDGE_ALAC_REFERENCE")
	if refPath == "" {
		refPath = filepath.Join(home, "onebit", "briefs", "b470-alac-eval", "results", "qualification.json")
	}
	corpus := os.Getenv("BRIDGE_FORMAT_CORPUS")
	if corpus == "" {
		corpus = filepath.Join(home, "medialibtest-formats")
	}
	b, err := os.ReadFile(refPath)
	if err != nil {
		t.Skipf("no ALAC reference file: %v", err)
	}
	var refs []alacReference
	if err := json.Unmarshal(b, &refs); err != nil {
		t.Fatal(err)
	}
	evalDir := filepath.Dir(filepath.Dir(refPath))
	for _, r := range refs {
		r := r
		path := filepath.Join(evalDir, r.File)
		if rel, ok := strings.CutPrefix(r.File, "corpus/"); ok {
			path = filepath.Join(corpus, "alac", rel)
		}
		t.Run(r.File, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Skipf("not present: %v", err)
			}
			s, err := decode.Open(bytes.NewReader(raw))
			switch {
			case r.Reference == nil:
				refusedWith(t, err, decode.ReasonLossy, "")
				return
			case r.Format.Channels != 2:
				refusedWith(t, err, decode.ReasonUnsupportedChannels, "")
				return
			case strings.Contains(r.File, "stts2048"):
				refusedWith(t, err, decode.ReasonTruncated, "")
				t.Logf("refused: %v", err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			d := s.(decode.Decoder)
			defer d.Close()
			fm := d.Format()
			if fm.RateHz != r.Format.SampleRate || fm.ValidBits != r.Format.BitDepth || fm.Frames != r.Frames {
				t.Fatalf("format %+v, FFmpeg %+v and %d frames", fm, r.Format, r.Frames)
			}
			requireDigest(t, "whole file", packLE(readAllFrames(t, d, 1<<16), fm.ValidBits), *r.Reference)
			for _, sk := range r.Seeks {
				if err := d.Seek(context.Background(), sk.Frame); err != nil {
					t.Fatalf("seek %d: %v", sk.Frame, err)
				}
				requireDigest(t, fmt.Sprintf("from frame %d", sk.Frame),
					packLE(readAllFrames(t, d, 1<<16), fm.ValidBits), sk.Reference)
			}
			t.Logf("%d frames and %d seeks match FFmpeg", r.Frames, len(r.Seeks))
		})
	}
}

func requireDigest(t *testing.T, what string, pcm []byte, want alacDigest) {
	t.Helper()
	sum := sha256.Sum256(pcm)
	if int64(len(pcm)) != want.Bytes || hexLower(sum[:]) != want.SHA256 {
		t.Fatalf("%s: %d bytes hashing to %x; FFmpeg's are %d bytes, %s", what, len(pcm), sum, want.Bytes, want.SHA256)
	}
}
