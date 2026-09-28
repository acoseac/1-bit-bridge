package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestVariantsMoveAsRootKeepsWhatItMovesTheServices pins that `bridge
// variants move` run as root gives every directory it makes the install's
// owner: the --to directory and the one it is made in (fsutil.MkdirAllLike)
// and each album directory beneath (fsutil.MkdirAll). The renamed sidecar
// keeps its owner by itself. Driven through fsutil.SimulateRootForTest,
// since the chown itself needs root; which owner each directory takes is
// fsutil's own tests' question, and cmd/bridge's
// TestJobCLIsRunAsRootKeepTheInstallOwner runs the move as root.
func TestVariantsMoveAsRootKeepsWhatItMovesTheServices(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	base := t.TempDir()
	cfgDir := filepath.Join(base, "install")
	if code, out := loopbackInit(t, cfgDir, testLibrary(t), "Owned"); code != 0 {
		t.Fatalf("bridge init = %d:\n%s", code, out)
	}
	cfg := loadInstallConfig(t, cfgDir)
	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const source, variant = "Artist/Album/01 - Track.flac", "upscaled-v2-176400-24"
	if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	sidecar := transcode.VariantSidecarPath(cfg.Upscale.EffectiveVariantsDir(cfg.DataDir), source, variant)
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte("rendition"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVariant(ctx, manifest.VariantRow{
		SourcePath: source, VariantID: variant, SidecarPath: sidecar, Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 9, SourceMTimeNS: 1, SourceSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	mnt := filepath.Join(base, "mnt")
	to := filepath.Join(mnt, "variants")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	var out, errOut bytes.Buffer
	if code := variantsMoveCmd(ctx, []string{"--config", filepath.Join(cfgDir, "bridge.yaml"),
		"--to", to, "--confirm", "MOVE"}, &out, &errOut); code != 0 {
		t.Fatalf("variants move = %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	var got []string
	for _, c := range changes() {
		if c.UID != 4242 || c.GID != 4243 {
			t.Errorf("%s given to %d:%d, want 4242:4243", c.Dst, c.UID, c.GID)
		}
		got = append(got, c.Dst)
	}
	want := []string{mnt, to, filepath.Join(to, "Artist"), filepath.Join(to, "Artist", "Album")}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("given away %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("given away %v, want exactly %v", got, want)
		}
	}
}

// TestACrossDeviceCopyAsRootKeepsTheMovedFilesOwner pins that the copy a
// move falls back to across filesystems is given the owner of the file it
// replaces, the source it then unlinks (fsutil.KeepOwner, as mv keeps it
// when it copies as root). A same-filesystem move is a rename, which keeps
// the owner by itself.
func TestACrossDeviceCopyAsRootKeepsTheMovedFilesOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src.flac")
	if err := os.WriteFile(src, []byte("rendition"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.flac")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if err := copyAndFsync(src, dst); err != nil {
		t.Fatal(err)
	}
	want := fsutil.OwnerChange{Dst: src, UID: 4242, GID: 4243}
	if got := changes(); len(got) != 1 || got[0] != want {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "rendition" {
		t.Fatalf("copied %q (%v)", b, err)
	}
}
