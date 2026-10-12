package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// gcIdentityVariant is a rendition id the variants-dir probe counts
// (looksLikeVariantSidecar) and LocateSidecar finds at its canonical path.
const gcIdentityVariant = "upscaled-v2-176400-24"

// The variants directory's identity is what an unmount changes, and a
// directory that still holds a rendition, or that this run's forward sweep
// already emptied, looks healthy to the checks that only ask whether it
// holds a rendition. These tests stand the unmount in the way the watcher
// tests do: rename the directory aside and make another at the same path,
// from inside the sweep. They do not compare the kept identity with
// anything before that swap (on Windows os.SameFile reads a lazy identity
// and would pin it to the directory that is there then).

func gcClearDirHooks(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		gcAfterClassifiedRow = nil
		gcBeforeReverseDirCheck = nil
	})
}

func gcIdentityStore(t *testing.T) *manifest.Store {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func gcSeedVariant(t *testing.T, store *manifest.Store, dir, source string, write bool) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p := transcode.VariantSidecarPath(dir, source, gcIdentityVariant)
	if write {
		writeFixtureFile(t, p, 50)
	}
	if err := store.UpsertVariant(ctx, manifest.VariantRow{
		SourcePath: source, VariantID: gcIdentityVariant, SidecarPath: p, Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
}

func gcVariantCount(t *testing.T, store *manifest.Store) int {
	t.Helper()
	rows, err := store.AllVariants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func gcUnmountVolume(t *testing.T, dir string) string {
	t.Helper()
	volume := dir + ".volume"
	if err := atomicwrite.RenameWithRetry(dir, volume); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return volume
}

func gcRemountVolume(t *testing.T, dir, volume string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := atomicwrite.RenameWithRetry(volume, dir); err != nil {
		t.Fatal(err)
	}
}

func gcRunIdentity(t *testing.T, store *manifest.Store, dir string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rc := runGC(context.Background(), &stdout, &stderr, store, dir, t.TempDir(), gcOptions{maxDeletePercent: 20})
	return rc, stdout.String(), stderr.String()
}

func gcRequireKept(t *testing.T, store *manifest.Store, want int, stderr string) {
	t.Helper()
	if got := gcVariantCount(t, store); got != want {
		t.Fatalf("%d variant rows, want %d\n%s", got, want, stderr)
	}
}

func TestRunGCRefusesASweepWhoseVolumeIsUnmountedDuringClassification(t *testing.T) {
	gcClearDirHooks(t)
	dir := t.TempDir()
	store := gcIdentityStore(t)
	const n = 8
	for i := 0; i < n; i++ {
		gcSeedVariant(t, store, dir, fmt.Sprintf("Artist/Present/%02d.flac", i), true)
	}
	// Eight present rows, and the swap lands after the first is classified,
	// so seven read as missing. Seven is under the relocation floor of ten,
	// and the replacement holds a rendition, so the health re-check before
	// the reverse sweep would let those seven rows go.
	var volume string
	seen := 0
	gcAfterClassifiedRow = func(string, integrity.SidecarVerdict) {
		seen++
		if seen != 1 {
			return
		}
		volume = gcUnmountVolume(t, dir)
		writeFixtureFile(t, transcode.VariantSidecarPath(dir, "Other/Album/01.flac", gcIdentityVariant), 50)
	}

	rc, stdout, stderr := gcRunIdentity(t, store, dir)
	gcRequireKept(t, store, n, stderr)
	if rc == 0 {
		t.Fatalf("sweep exited 0 after the volume went\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	for _, want := range []string{
		"no longer the directory this sweep began on",
		"Nothing was unlinked and no row was removed",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	decoy := transcode.VariantSidecarPath(dir, "Other/Album/01.flac", gcIdentityVariant)
	if _, err := os.Stat(decoy); err != nil {
		t.Errorf("replacement's rendition: %v", err)
	}
	for i := 0; i < n; i++ {
		source := fmt.Sprintf("Artist/Present/%02d.flac", i)
		p := transcode.VariantSidecarPath(volume, source, gcIdentityVariant)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file on the volume %s: %v", source, err)
		}
	}
	if seen != 1 {
		t.Errorf("classified %d rows after the directory changed, want 1", seen)
	}
}

func TestRunGCRefusesASweepWhoseVolumeWentAndCameBackDuringClassification(t *testing.T) {
	gcClearDirHooks(t)
	dir := t.TempDir()
	store := gcIdentityStore(t)
	const n = 8
	for i := 0; i < n; i++ {
		gcSeedVariant(t, store, dir, fmt.Sprintf("Artist/Present/%02d.flac", i), true)
	}
	// The directory is the one the sweep began on again by the time
	// classification ends, so a check only at the reverse sweep cannot see
	// the rows classified while it was away. The hook runs after a row is
	// accepted, and a missing row is accepted only while the directory is
	// still that one, so with the check the remount never runs.
	var volume string
	seen := 0
	gcAfterClassifiedRow = func(string, integrity.SidecarVerdict) {
		seen++
		switch seen {
		case 1:
			volume = gcUnmountVolume(t, dir)
		case 5:
			gcRemountVolume(t, dir, volume)
			volume = ""
		}
	}

	rc, stdout, stderr := gcRunIdentity(t, store, dir)
	gcRequireKept(t, store, n, stderr)
	if rc == 0 {
		t.Fatalf("sweep exited 0 after the volume went and came back\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	if !strings.Contains(stderr, "no longer the directory this sweep began on") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if volume == "" {
		t.Fatal("the volume was remounted; the check at the end of classification would have matched")
	}
	for i := 0; i < n; i++ {
		source := fmt.Sprintf("Artist/Present/%02d.flac", i)
		p := transcode.VariantSidecarPath(volume, source, gcIdentityVariant)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file on the volume %s: %v", source, err)
		}
	}
	if seen != 1 {
		t.Errorf("classified %d rows after the directory changed, want 1", seen)
	}
}

func TestRunGCRefusesAnEmptyExceptionOnADirectoryItDidNotClassify(t *testing.T) {
	gcClearDirHooks(t)
	dir := t.TempDir()
	store := gcIdentityStore(t)
	const present, missing = 5, 4
	for i := 0; i < present; i++ {
		gcSeedVariant(t, store, dir, fmt.Sprintf("Artist/Here/%02d.flac", i), true)
	}
	for i := 0; i < missing; i++ {
		gcSeedVariant(t, store, dir, fmt.Sprintf("Artist/Gone/%02d.flac", i), false)
	}
	// Four missing rows are under the relocation floor. The orphan is a
	// rendition, older than the forward sweep's grace, so that sweep
	// unlinks it and the Empty exception (this run removed a rendition)
	// would then explain an empty directory put at the path afterwards.
	orphan := transcode.VariantSidecarPath(dir, "Artist/Orphan/01.flac", gcIdentityVariant)
	writeFixtureFile(t, orphan, 80)
	ageFiles(t, orphan)
	var volume string
	gcBeforeReverseDirCheck = func() {
		volume = gcUnmountVolume(t, dir)
	}

	rc, stdout, stderr := gcRunIdentity(t, store, dir)
	gcRequireKept(t, store, present+missing, stderr)
	if rc == 0 {
		t.Fatalf("sweep exited 0 over a directory it did not classify\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	for _, want := range []string{
		"no longer the directory this sweep began on",
		"Nothing further was unlinked and no row was removed",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	for i := 0; i < present; i++ {
		source := fmt.Sprintf("Artist/Here/%02d.flac", i)
		p := transcode.VariantSidecarPath(volume, source, gcIdentityVariant)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file on the volume %s: %v", source, err)
		}
	}
}
