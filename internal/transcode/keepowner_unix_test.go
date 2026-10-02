//go:build !windows

package transcode

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// fakeSoxWritingInPlace stands in for sox(1): it writes a whole FLAC (the
// rendition the test's 192 kHz / 24-bit job asks for, which Run reads before
// it publishes) to its output argument (the argv entry right after
// `-t flac`) through a shell redirection, which opens the file O_TRUNC and so
// keeps a precreated one, as sox does.
var fakeSoxWritingInPlace = `#!/bin/sh
out=""; prev=""; prev2=""
for a in "$@"; do
  if [ "$prev2" = "-t" ] && [ "$prev" = "flac" ]; then out="$a"; break; fi
  prev2="$prev"; prev="$a"
done
[ -n "$out" ] || { echo "fake sox: no output arg in argv" >&2; exit 1; }
` + shWrites("$out", stereoFLAC(192000, 24)) + "\n"

// TestRunAsRootKeepsTheInstallOwner pins that a job run as root gives the
// album directories it makes (fsutil.MkdirAll) and the sidecar sox writes
// (created before sox runs by createOutput, with the owner of the rendition
// it replaces or of its directory) the install's owner, so a `sudo bridge
// upscale` over a service install leaves renditions the service can
// replace. A stand-in sox writes the sidecar, and what Run publishes is
// what it wrote. Driven through fsutil.SimulateRootForTest, since the
// chown itself needs root; cmd/bridge's TestJobCLIsRunAsRootKeepTheInstallOwner
// runs the real sox as root, which is what shows sox keeps the owner.
func TestRunAsRootKeepsTheInstallOwner(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "sox"), []byte(fakeSoxWritingInPlace), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	src := filepath.Join(t.TempDir(), "01.flac")
	if err := os.WriteFile(src, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "transcoded")
	spec := stampedAsScanned(JobSpec{
		SourceAbsPath:    src,
		SourceLibraryRel: "Artist/Album/01.flac",
		TargetSampleRate: 192000,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        out,
	})
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if _, _, err := RunSox(context.Background(), spec); err != nil {
		t.Fatalf("RunSox: %v", err)
	}
	final := spec.SidecarPath()
	want := []fsutil.OwnerChange{
		{Dst: out, UID: 4242, GID: 4243},
		{Dst: filepath.Join(out, "Artist"), UID: 4242, GID: 4243},
		{Dst: filepath.Join(out, "Artist", "Album"), UID: 4242, GID: 4243},
		{Dst: final, UID: 4242, GID: 4243}, // the sidecar, created for sox
	}
	got := changes()
	if len(got) != len(want) {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
		}
	}
	if b, err := os.ReadFile(final); err != nil || !bytes.Equal(b, stereoFLAC(192000, 24)) {
		t.Fatalf("published %d bytes (%v), want what sox wrote into the precreated file", len(b), err)
	}
}

// TestRunDSDAsRootKeepsTheInstallOwner is TestRunAsRootKeepsTheInstallOwner
// for the DSD chain, which makes one more directory: its Stage A scratch
// under the temp dir (JobSpec.mkdirScratch, fsutil.MkdirAllShared). Which
// owner a scratch directory in a shared temp dir takes (the variants
// directory's) is fsutil's TestCreatingAsRootKeepsTheOwner; this pins that
// the chain asks. The scratch FILE, which the chain creates before Stage A
// (createOutput) so a directory that refuses it fails by type, takes its
// directory's owner. It needs the real toolchain, so CI's race legs skip it.
func TestRunDSDAsRootKeepsTheInstallOwner(t *testing.T) {
	requireDSDToolchain(t)
	root := t.TempDir()
	src := filepath.Join(root, "lib", "Album", "tone.dsf")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := dsdtone.MintDSF(src, dsdtone.Tone{RateHz: 2822400, Seconds: 1, AmplitudeDBFS: -20}); err != nil {
		t.Fatal(err)
	}
	out, tmp := filepath.Join(root, "variants"), filepath.Join(root, "tmp")
	spec := stampedAsScanned(JobSpec{
		SourceAbsPath: src, SourceLibraryRel: "Album/tone.dsf",
		SourceSampleRate: 2822400, SourceIsDSD: true, SourceChannels: 2, SourceDurationSec: 1,
		TargetSampleRate: 44100, TargetBits: 16, Quality: QualityMedium,
		OutputDir: out, TempDir: tmp, Kind: JobKindOptimize,
	})
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []fsutil.OwnerChange{
		{Dst: out, UID: 4242, GID: 4243},
		{Dst: filepath.Join(out, "Album"), UID: 4242, GID: 4243},
		{Dst: tmp, UID: 4242, GID: 4243},
		{Dst: renderScratchDir(tmp), UID: 4242, GID: 4243},
		{Dst: spec.SidecarPath(), UID: 4242, GID: 4243},    // the rendition, created for Stage C
		{Dst: renderScratchDir(tmp), UID: 4242, GID: 4243}, // the Stage A scratch file, its directory's owner
	}
	got := changes()
	if len(got) != len(want) {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
		}
	}
}
