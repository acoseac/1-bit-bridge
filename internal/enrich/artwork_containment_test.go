package enrich

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jpegBody is a minimal body that clears the signature check, so a test
// about CONTAINMENT fails for containment reasons and nothing else.
func jpegBody() *bytes.Reader {
	return bytes.NewReader(append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{0x00}, 32)...))
}

// TestWriteArtworkRefusesAPathOutsideTheCacheDir pins the containment
// layer on its own: a path that escapes the root is refused BEFORE the
// os.MkdirAll that would otherwise create its parents.
//
// The escape is spelled with an explicit traversal rather than an
// unrelated absolute path because that is the shape the defect had — an
// upstream-chosen MBID reaching ArtworkCachePath's filepath.Join, which
// Cleans, so `../../x` resolves outside and MkdirAll then builds the way
// there.
func TestWriteArtworkRefusesAPathOutsideTheCacheDir(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "artwork")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "..", "..", "escaped", "cover-500.jpg")

	err := writeArtworkAtomicStream(root, outside, jpegBody(), MaxCoverArtBytes)
	if err == nil {
		t.Fatal("escaping path was accepted; the containment layer is not load-bearing")
	}
	if !strings.Contains(err.Error(), "outside the cache directory") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	// The whole point: nothing was created on the way to the target.
	if _, statErr := os.Stat(filepath.Join(base, "escaped")); !os.IsNotExist(statErr) {
		t.Fatalf("MkdirAll ran for a refused path: %v", statErr)
	}
}

// TestWriteArtworkAcceptsAPathInsideTheCacheDir is the positive control.
// Without it the test above passes against a helper that refuses
// everything, which would be a worse bug than the one being fixed.
func TestWriteArtworkAcceptsAPathInsideTheCacheDir(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "0a0b0c0d-0e0f-1011-1213-141516171819-500.jpg")
	if err := writeArtworkAtomicStream(root, inside, jpegBody(), MaxCoverArtBytes); err != nil {
		t.Fatalf("a path inside the cache dir was refused: %v", err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("accepted write did not land: %v", err)
	}
}

// TestWriteArtworkRefusesBytesThatAreNotAnImage pins the signature layer
// separately from containment: an in-bounds path carrying an arbitrary
// payload is still refused, and leaves no temp file behind.
func TestWriteArtworkRefusesBytesThatAreNotAnImage(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "cover-500.jpg")

	err := writeArtworkAtomicStream(root, dst, bytes.NewReader([]byte("#!/bin/sh\nrm -rf /\n")), MaxCoverArtBytes)
	if err == nil {
		t.Fatal("non-image body was accepted; the signature layer is not load-bearing")
	}
	if !strings.Contains(err.Error(), "not a recognised image") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatal("a refused body landed at the destination")
	}
	// The staging temp must not survive either — a refused payload left
	// in the cache dir is the same arbitrary-bytes-on-disk outcome one
	// rename short of the real thing.
	ents, rerr := os.ReadDir(root)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Fatalf("refused write left %d file(s) behind: %v", len(ents), ents)
	}
}

// TestWriteArtworkAcceptsPNGDeliberately records a decision rather than a
// preference. This path's contract is JPEG (the file is named .jpg and
// /v1/artwork serves it as image/jpeg), and internal/manifest's scaler
// TRANSCODES PNG rather than storing it for exactly that reason — but CAA
// can serve PNG, those covers render today, and dropping them inside a
// security fix would be a regression bought for nothing. If the
// transcode-or-refuse decision is ever taken, this test is the one to
// change, on purpose.
func TestWriteArtworkAcceptsPNGDeliberately(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "cover-500.jpg")
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, bytes.Repeat([]byte{0x00}, 32)...)
	if err := writeArtworkAtomicStream(root, dst, bytes.NewReader(png), MaxCoverArtBytes); err != nil {
		t.Fatalf("PNG was refused: %v", err)
	}
}
