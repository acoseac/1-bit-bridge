package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestArtworkGCRefusesEmptyReferencedSet pins the fail-closed guard.
//
// Every file in the cache misses an empty `known` map, so without the guard
// the walk classifies the entire cache as orphaned and unlinks it. That is not
// recoverable for the scanner-written `local-<sha256>-500.jpg` covers: the
// scanner's mtime skip gate means it never re-reads an unchanged file, so the
// covers stay gone through every subsequent scan.
//
// The docblock on artworkGCCmd records this exact deletion shipping once, via
// a hardcoded DB path that opened an empty store. That fix corrected the path
// and left the shape — an empty answer from the store meaning "delete
// everything" — which is what makes any future wrong-database route
// catastrophic instead of merely wrong.
func TestArtworkGCRefusesEmptyReferencedSet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A populated cache and a store that references nothing — the state a
	// wrong --config, or a run between a root-flip wipe and its rescan,
	// puts the GC in.
	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cached := filepath.Join(artworkDir, "local-"+strings.Repeat("a", 64)+"-500.jpg")
	if err := os.WriteFile(cached, []byte("jpeg bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var so, se bytes.Buffer
	if code := runArtworkGC(ctx, &so, &se, store, artworkDir, false); code == 0 {
		t.Errorf("artwork gc exited 0 against an empty referenced set with files present — "+
			"it treated the whole cache as orphaned.\nstdout:\n%s", so.String())
	}
	if _, err := os.Stat(cached); err != nil {
		t.Errorf("the cached cover was removed despite an empty referenced set: %v\nstderr:\n%s",
			err, se.String())
	}
	if !strings.Contains(se.String(), "refusing") {
		t.Errorf("no refusal message on stderr; an operator needs to know WHY nothing happened:\n%s",
			se.String())
	}
}

// TestArtworkGCEmptyStoreAndEmptyCacheIsClean is the other half, and the
// reason the guard checks the directory rather than refusing on an empty set
// alone: a bridge that has cached no artwork yet is the normal empty case, and
// turning it into a failure would make `artwork gc` exit non-zero on every
// fresh install.
func TestArtworkGCEmptyStoreAndEmptyCacheIsClean(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var so, se bytes.Buffer
	if code := runArtworkGC(ctx, &so, &se, store, filepath.Join(dir, "artwork"), false); code != 0 {
		t.Errorf("artwork gc exited %d on an empty store with no cache directory; "+
			"that is a fresh install, not a fault.\nstderr:\n%s", code, se.String())
	}
}
