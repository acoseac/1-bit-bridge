//go:build !windows

package transcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeSoxScript stands in for sox(1). It writes a payload to its output
// argument and coordinates through barrier files so the two concurrent RunSox
// calls below interleave DETERMINISTICALLY rather than by sleep.
//
// The two invocations get byte-identical argv (they hold the same JobSpec —
// that is the whole point), so they cannot be told apart by their arguments.
// They self-assign roles instead: `mkdir` is atomic, so exactly one invocation
// wins the claim and becomes A.
//
//	A: write "AAAA" -> signal a-wrote -> wait b-wrote -> exit
//	B: wait a-wrote -> write "BBBB"   -> signal b-wrote -> wait b-may-exit -> exit
//
// A therefore cannot exit (and RunSox(A) cannot rename) until B has written,
// which is what makes the pre-fix theft reproducible on every run.
//
// The output path is the argv entry right after `-t flac`.
const fakeSoxScript = `#!/bin/sh
state="$FAKE_SOX_STATE"
out=""; prev=""; prev2=""
for a in "$@"; do
  if [ "$prev2" = "-t" ] && [ "$prev" = "flac" ]; then out="$a"; break; fi
  prev2="$prev"; prev="$a"
done
[ -n "$out" ] || { echo "fake sox: no output arg in argv" >&2; exit 1; }

waitfor() {
  i=0
  while [ ! -e "$1" ]; do
    i=$((i+1))
    [ "$i" -gt 3000 ] && { echo "fake sox: timeout waiting for $1" >&2; exit 1; }
    sleep 0.02
  done
}

if mkdir "$state/claim-a" 2>/dev/null; then
  printf 'AAAA' > "$out"
  : > "$state/a-wrote"
  waitfor "$state/b-wrote"
else
  waitfor "$state/a-wrote"
  printf 'BBBB' > "$out"
  : > "$state/b-wrote"
  waitfor "$state/b-may-exit"
fi
exit 0
`

// TestRunSoxConcurrentSameSpecDoesNotStealTmp is the F2 harm pin.
//
// Two RunSox calls hold the SAME JobSpec — the state `DropInflight` +
// re-submit legitimately produces: DELETE /v1/upscale/variants calls
// DropInflight explicitly so a re-submit isn't coalesced against a worker
// that is still running, and the same delete removes the track_variants row
// so finalizeAndEnqueue's LookupVariant no longer refuses. With the default
// worker count (min(NumCPU-1, 4) >= 2 on any real host) job B starts while
// job A's sox is still writing. `TestDropInflightThenCompletionDoesNotRelease
// TheResubmission` already establishes that concurrency as the ACCEPTED
// state — so the shared temp path, not the overlap, is the bug.
//
// On a deterministic shared temp path:
//
//	A: RunSox os.Remove(tmp); sox-A writes "AAAA"
//	B: RunSox os.Remove(tmp)          <-- unlinks A's in-progress output
//	B: sox-B writes "BBBB" to the same path (new inode)
//	A: sox-A exits first (it started first); RunSox(A) renames tmp -> final
//
// so RunSox(A) publishes B's file, stats it, fsyncs it, and returns success —
// and its caller commits a track_variants row for it. The assertion is
// therefore on A's OWN success path: the bytes A published must be A's.
// (In production B is still mid-write, so the published file is truncated;
// the barriers make B's write complete-but-wrong, which is the same defect
// rendered deterministic.)
//
// POSIX-only: on Windows both the unlink and the re-open fail with a sharing
// violation and B fails cleanly, so there is nothing to pin.
func TestRunSoxConcurrentSameSpecDoesNotStealTmp(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "sox"), []byte(fakeSoxScript), 0o755); err != nil {
		t.Fatalf("write fake sox: %v", err)
	}
	state := t.TempDir()
	t.Setenv("FAKE_SOX_STATE", state)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "01.flac")
	if err := os.WriteFile(src, []byte("source"), 0o644); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	spec := JobSpec{
		SourceAbsPath:    src,
		SourceLibraryRel: "Music/Album/01.flac",
		TargetSampleRate: 192000,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	finalPath := spec.SidecarPath()

	// Both goroutines run the identical spec; the script decides who is A.
	res := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := RunSox(context.Background(), spec)
			res <- err
		}()
	}

	// A's RunSox returns first — it is the only one that can, since B's sox is
	// parked on b-may-exit.
	var errA error
	select {
	case errA = <-res:
	case <-time.After(90 * time.Second):
		t.Fatal("timed out waiting for the first RunSox to return")
	}
	if errA != nil {
		t.Fatalf("RunSox(A): %v", errA)
	}

	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read published sidecar: %v", err)
	}
	if string(got) != "AAAA" {
		t.Errorf("published sidecar = %q, want %q — RunSox returned success and its "+
			"caller commits a track_variants row for this path, but the bytes are the "+
			"OTHER concurrent job's: on a shared deterministic temp path B unlinked A's "+
			"in-progress output and A renamed B's file into place", got, "AAAA")
	}

	// Release B and drain it. Post-fix B renames its OWN temp and succeeds
	// (last writer wins on the final path, and both files are complete);
	// pre-fix its temp was already renamed away by A, so its rename fails and
	// the job is counted failed even though the variant exists.
	if err := os.WriteFile(filepath.Join(state, "b-may-exit"), nil, 0o644); err != nil {
		t.Fatalf("release B: %v", err)
	}
	select {
	case errB := <-res:
		if errB != nil {
			t.Errorf("RunSox(B) = %v, want nil — B wrote a complete sidecar, so it must "+
				"not be counted a failed job (pre-fix its temp had been renamed away)", errB)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("timed out waiting for the second RunSox to return")
	}
}
