package transcode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestJobSpecRenderScratchBytes pins the ONE scratch derivation the
// coordinator's pre-flight and the sweeper's budget share: int32 at the
// TARGET rate over the duration, stereo when channels are unknown,
// size-derived at the NOMINAL DSD rate when the manifest carries no
// duration, and 0 for a PCM job.
func TestJobSpecRenderScratchBytes(t *testing.T) {
	pcm := JobSpec{SourceSampleRate: 96000, TargetSampleRate: 48000, SourceDurationSec: 600, SourceChannels: 2}
	if got := pcm.RenderScratchBytes(); got != 0 {
		t.Errorf("PCM job RenderScratchBytes = %d, want 0 (no intermediate)", got)
	}

	known := JobSpec{SourceIsDSD: true, SourceSampleRate: 2822400, TargetSampleRate: 176400,
		SourceChannels: 2, SourceDurationSec: 3600}
	if got, want := known.RenderScratchBytes(), TempBytesForRender(2, 176400, 3600); got != want {
		t.Errorf("known duration: RenderScratchBytes = %d, want %d", got, want)
	}
	if got := known.RenderScratchBytes(); got != 4*2*176400*3600 {
		t.Errorf("known duration: %d bytes, want the 5.08 GB an hour of 176.4 kHz int32 stereo holds", got)
	}

	// No manifest duration: derive it from the size at the NOMINAL DSD
	// rate (1 GB of DSD64 stereo is ~1417 s), then size at the TARGET.
	sized := JobSpec{SourceIsDSD: true, SourceSize: 1_000_000_000, SourceSampleRate: 2822400, TargetSampleRate: 44100}
	wantSized := TempBytesForRender(2, 44100, dsdSizeDerivedDurationSec(1_000_000_000, 2822400, 2))
	if got := sized.RenderScratchBytes(); got != wantSized {
		t.Errorf("size-derived: RenderScratchBytes = %d, want %d", got, wantSized)
	}
	if got := sized.RenderScratchBytes(); got <= 0 || got > 4*2*44100*1500 {
		t.Errorf("size-derived: %d bytes is not in the ~1417 s × 44.1 kHz stereo int32 range", got)
	}

	// Channels unknown → stereo; known → honoured.
	unknownCh := JobSpec{SourceIsDSD: true, SourceDurationSec: 10, TargetSampleRate: 44100}
	if got, want := unknownCh.RenderScratchBytes(), TempBytesForRender(2, 44100, 10); got != want {
		t.Errorf("unknown channels: %d, want the stereo estimate %d", got, want)
	}
	six := JobSpec{SourceIsDSD: true, SourceDurationSec: 10, TargetSampleRate: 44100, SourceChannels: 6}
	if got, want := six.RenderScratchBytes(), TempBytesForRender(6, 44100, 10); got != want {
		t.Errorf("6 channels: %d, want %d", got, want)
	}
	if six.RenderScratchBytes() <= unknownCh.RenderScratchBytes() {
		t.Error("a 6-channel source must hold more scratch than a stereo one")
	}
}

// TestExportedScratchHelpersMirrorTheInternals: the exported wrappers the
// sweeper and `--gc` call are the internals at the standard purge age —
// a stale scratch goes, a fresh one stays, and the directory is the one
// the chain writes into.
func TestExportedScratchHelpersMirrorTheInternals(t *testing.T) {
	tempDir := t.TempDir()
	if got, want := RenderScratchDir(tempDir), renderScratchDir(tempDir); got != want {
		t.Fatalf("RenderScratchDir = %q, want the chain's %q", got, want)
	}
	dir := RenderScratchDir(tempDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	touch := func(path string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	stale := filepath.Join(dir, "aaaa0001.stageA.sox")
	fresh := filepath.Join(dir, "aaaa0002.stageA.sox")
	touch(stale, renderScratchMaxAge+time.Hour)
	touch(fresh, time.Hour)

	n, err := PurgeStaleRenderScratch(tempDir)
	if err != nil {
		t.Fatalf("PurgeStaleRenderScratch: %v", err)
	}
	if n != 1 {
		t.Errorf("removed %d, want exactly the one stale scratch", n)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale scratch must be gone (stat err=%v)", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the fresh scratch must survive: %v", err)
	}
	// An empty tempDir means the OS temp dir — never an error, and never
	// a walk of anything but the bridge-owned subdirectory.
	if _, err := PurgeStaleRenderScratch(""); err != nil {
		t.Errorf("PurgeStaleRenderScratch(\"\") = %v, want nil (OS temp dir)", err)
	}
}
