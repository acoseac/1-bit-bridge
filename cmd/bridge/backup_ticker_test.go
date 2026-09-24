package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
)

// TestABackupStoppedMidSnapshotReportsNothing drives the ticker's startup
// snapshot into its VACUUM INTO, cancels the ticker's context there, and
// requires silence: no failure on stderr, no "wrote" line, and no snapshot
// directory left behind. This is the shutdown the ticker meets in practice:
// the startup snapshot starts the moment serve does, and on a large library
// the VACUUM takes a while. Each such shutdown used to put
// `backup (startup): snapshot failed: vacuum manifest db: context canceled`
// in the journal.
//
// The run state still records the pass as over with no new counts, which is
// what sweepFinished(nil) means for a failed pass and a stopped one alike:
// `running` has to clear either way.
func TestABackupStoppedMidSnapshotReportsNothing(t *testing.T) {
	dataDir := t.TempDir()
	src := backup.Sources{DataDir: dataDir, ManifestDB: filepath.Join(dataDir, "bridge.db")}
	backuptest.WriteSource(t, src.ManifestDB)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	// Armed after the drain is registered, so its cleanup runs first and
	// lets a held VACUUM go before the drain waits on the ticker.
	park := backuptest.ParkVacuum(t)

	var stdout, stderr safeBuffer
	status := &sweepStatus[struct{}]{}
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
	}()

	park.Wait(t)
	cancel()
	park.ReleaseUntil(t, done)

	if got := stderr.String(); got != "" {
		t.Errorf("a snapshot stopped by shutdown was reported as a failure:\n%s", got)
	}
	if got := stdout.String(); got != "" {
		t.Errorf("a snapshot stopped by shutdown printed %q; the VACUUM finished before the cancel reached it", got)
	}
	assertNoSnapshotDirs(t, dataDir)
	running, lastStart, _, _, _ := status.snapshot()
	if lastStart.IsZero() {
		t.Error("the startup pass never started, so nothing above was exercised")
	}
	if running {
		t.Error("the run state still says running after the pass was stopped")
	}
}

// TestABackupCancelledBeforeItsPruneReportsNothing is the second half: a
// snapshot that landed, then a prune that starts on a cancelled context. The
// ticker is held on its "wrote" line, after Snapshot returned and before
// PruneContext starts, and cancelled there, so the prune stops at its first
// check. That used to print two failures about a pass that failed at
// nothing: the orphan sweep's "reported a problem ... context canceled" and
// "prune failed: context canceled". The snapshot is still reported, because
// it was written. TestABackupCancelledMidPruneReportsWhatItDeleted is the
// cancel that lands between two deletions.
func TestABackupCancelledBeforeItsPruneReportsNothing(t *testing.T) {
	dataDir := t.TempDir()
	src := backup.Sources{DataDir: dataDir, ManifestDB: filepath.Join(dataDir, "bridge.db")}
	backuptest.WriteSource(t, src.ManifestDB)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	stdout := holdOn("wrote ")
	t.Cleanup(stdout.letGo) // before the drain waits: LIFO

	var stderr safeBuffer
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, stdout, &stderr, nil)
	}()

	stdout.wait(t)
	cancel()
	stdout.letGo()
	waitClosed(t, done, "the backup ticker")

	if got := stderr.String(); got != "" {
		t.Errorf("a prune stopped by shutdown was reported as a failure:\n%s", got)
	}
	if got := stdout.String(); !strings.Contains(got, "backup (startup): wrote ") {
		t.Errorf("stdout = %q, want the snapshot the pass wrote before the cancel", got)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, backup.BackupsDirName))
	if err != nil {
		t.Fatalf("read backups root: %v", err)
	}
	if len(entries) != 1 || !backup.LooksLikeSnapshotDir(filepath.Join(dataDir, backup.BackupsDirName, entries[0].Name())) {
		t.Errorf("backups root holds %v, want the one complete snapshot the pass wrote", entries)
	}
}

// TestABackupCancelledMidPruneReportsWhatItDeleted cancels the prune
// between two deletions, and requires the count of what it removed before it
// stopped: those snapshots are gone, so "pruned N" is a fact about the pass,
// and nothing is reported as a failure. PruneContext's loop has no place to
// hold a goroutine between two removals, only the ctx.Err() check before
// each one, so the pass's context cancels itself there: cancelWhenGone
// cancels on the first Err after the first snapshot the prune deletes has
// gone, which is the check before the second. (CodeRabbit, #998.)
func TestABackupCancelledMidPruneReportsWhatItDeleted(t *testing.T) {
	dataDir := t.TempDir()
	src := backup.Sources{DataDir: dataDir, ManifestDB: filepath.Join(dataDir, "bridge.db")}
	backuptest.WriteSource(t, src.ManifestDB)
	// Three snapshots older than the startup threshold, so the startup
	// pass writes a fourth. With keep 1 the prune removes all three,
	// newest first, so old[0] is the one it deletes first.
	root := filepath.Join(dataDir, backup.BackupsDirName)
	now := time.Now().UTC()
	old := []string{
		writeSnapshotDir(t, root, now.Add(-72*time.Hour)),
		writeSnapshotDir(t, root, now.Add(-96*time.Hour)),
		writeSnapshotDir(t, root, now.Add(-120*time.Hour)),
	}

	base, cancel := context.WithCancel(context.Background())
	ctx := &cancelWhenGone{Context: base, cancel: cancel, path: old[0]}
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	var stdout, stderr safeBuffer
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 1 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, nil)
	}()
	waitClosed(t, done, "the backup ticker")

	if got := stderr.String(); got != "" {
		t.Errorf("a prune stopped by shutdown was reported as a failure:\n%s", got)
	}
	if got := stdout.String(); !strings.Contains(got, "backup (startup): pruned 1 older snapshot(s)") {
		t.Errorf("stdout = %q, want the one snapshot the prune removed before the cancel", got)
	}
	for i, dir := range old {
		_, err := os.Stat(dir)
		if gone := errors.Is(err, os.ErrNotExist); gone != (i == 0) {
			t.Errorf("%s gone = %v, want %v: the prune stops after its first deletion", filepath.Base(dir), gone, i == 0)
		}
	}
}

// cancelWhenGone is a context that cancels itself the first time its Err is
// asked after path has gone. It places a cancel between two filesystem
// operations that have no hook between them, only a ctx.Err() check. Done
// and every other method are the embedded context's own.
type cancelWhenGone struct {
	context.Context
	cancel context.CancelFunc
	path   string
}

// Err cancels the context once path no longer exists, then answers as the
// embedded context does.
func (c *cancelWhenGone) Err() error {
	if _, err := os.Stat(c.path); errors.Is(err, os.ErrNotExist) {
		c.cancel()
	}
	return c.Context.Err()
}

// TestABackupThatFailsIsStillReported is the control on the tests above.
// The silence covers a pass the cancel stopped, and a pass that failed on
// its own still says so. Each case fails on a live context: a manifest
// database that is not a SQLite file fails the snapshot, and a snapshot
// directory whose manifest cannot be read is reported by the orphan sweep
// after a snapshot that succeeded.
func TestABackupThatFailsIsStillReported(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dataDir, db string)
		want  string
	}{
		{
			name: "snapshot",
			setup: func(t *testing.T, _, db string) {
				if err := os.WriteFile(db, []byte("not a SQLite database, and nothing like one"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "backup (startup): snapshot failed: vacuum manifest db: ",
		},
		{
			name: "orphan sweep",
			setup: func(t *testing.T, dataDir, db string) {
				backuptest.WriteSource(t, db)
				// A manifest.json that is a DIRECTORY cannot be read, which
				// is not the same as absent: the sweep reports it and keeps
				// the snapshot rather than reaping it.
				bad := filepath.Join(dataDir, backup.BackupsDirName, "2020-01-01T00-00-00Z", backup.ManifestFile)
				if err := os.MkdirAll(bad, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "backup (startup): orphan sweep reported a problem (prune unaffected): ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			src := backup.Sources{DataDir: dataDir, ManifestDB: filepath.Join(dataDir, "bridge.db")}
			tc.setup(t, dataDir, src.ManifestDB)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			drainLoopOnCleanup(t, cancel, done, "the backup ticker")
			var stdout, stderr safeBuffer
			status := &sweepStatus[struct{}]{}
			go func() {
				defer close(done)
				runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
			}()

			// The pass reports before it records its end, so once the end
			// is recorded the report is in stderr.
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.IsZero() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the startup pass did not finish within 10s; stderr=%q", stderr.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			waitClosed(t, done, "the backup ticker")

			if got := stderr.String(); !strings.Contains(got, tc.want) {
				t.Errorf("stderr = %q, want a line starting %q", got, tc.want)
			}
		})
	}
}

// TestWithoutCancellationKeepsOnlyWhatFailed pins the classification
// behind the ticker's silence, one row per claim in its docblock. The
// joined rows use the exact shape PruneContext's delete loop and
// reapOrphans return when a cancel stops them after a genuine failure,
// errors.Join(errors.Join(errs...), ctx.Err()). Driving that through a
// real prune would need a cancel landing between two directories, and
// neither loop has a place to hold it.
//
// Each row names how its pass's context ENDED rather than holding the
// context: nil for a pass that is still live.
func TestWithoutCancellationKeepsOnlyWhatFailed(t *testing.T) {
	removeA := errors.New("remove backups/a: permission denied")
	removeB := errors.New("remove backups/b: permission denied")
	copyFailed := errors.New("copy tokens.json: input/output error")

	for _, tc := range []struct {
		name       string
		ended      error // how the pass's ctx ended: nil, Canceled or DeadlineExceeded
		err        error
		want       string // "" = nil
		wantCancel bool   // what is reported still carries a cancellation
	}{
		{"nil error", context.Canceled, nil, "", false},
		{"the snapshot's cancelled vacuum", context.Canceled,
			fmt.Errorf("vacuum manifest db: %w", context.Canceled), "", false},
		{"a cancellation wrapped twice", context.Canceled,
			fmt.Errorf("snapshot: %w", fmt.Errorf("vacuum manifest db: %w", context.Canceled)), "", false},
		{"a prune stopped before any failure", context.Canceled,
			errors.Join(errors.Join(), context.Canceled), "", false},
		{"a prune stopped after two failures", context.Canceled,
			errors.Join(errors.Join(removeA, removeB), context.Canceled),
			removeA.Error() + "\n" + removeB.Error(), false},
		// A join under a wrapper cannot be rebuilt around what is left
		// without dropping the wrapper's own context, so it is reported
		// whole when it holds a genuine failure, and is quiet only when the
		// cancellation is all it holds.
		{"a wrapped join holding a failure", context.Canceled,
			fmt.Errorf("prune: %w", errors.Join(removeA, context.Canceled)),
			"prune: " + removeA.Error() + "\n" + context.Canceled.Error(), true},
		{"a wrapped join holding only the cancellation", context.Canceled,
			fmt.Errorf("prune: %w", errors.Join(errors.Join(), context.Canceled)), "", false},
		{"a failure with no cancellation in it, in a cancelled pass", context.Canceled,
			copyFailed, copyFailed.Error(), false},
		{"a deadline", context.DeadlineExceeded,
			fmt.Errorf("vacuum manifest db: %w", context.DeadlineExceeded),
			"vacuum manifest db: " + context.DeadlineExceeded.Error(), false},
		// The row above is rejected by the error alone, since a deadline is
		// not context.Canceled. This one reaches the context: a pass whose
		// ctx ran out of time failed, even when the error it carries is a
		// cancellation from somewhere else. Only a CANCELLED ctx is quiet,
		// not one that is merely done.
		{"a deadline, with another context's cancellation in the error", context.DeadlineExceeded,
			fmt.Errorf("vacuum manifest db: %w", context.Canceled),
			"vacuum manifest db: " + context.Canceled.Error(), true},
		{"another context's cancellation while ctx is live", nil,
			fmt.Errorf("vacuum manifest db: %w", context.Canceled),
			"vacuum manifest db: " + context.Canceled.Error(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withoutCancellation(contextThatEnded(t, tc.ended), tc.err)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("withoutCancellation = %q, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("withoutCancellation = nil, want %q", tc.want)
			}
			if got.Error() != tc.want {
				t.Errorf("withoutCancellation = %q, want %q", got, tc.want)
			}
			if errors.Is(got, context.Canceled) != tc.wantCancel {
				t.Errorf("errors.Is(result, context.Canceled) = %v, want %v", !tc.wantCancel, tc.wantCancel)
			}
		})
	}
	cancelled := contextThatEnded(t, context.Canceled)
	// The kept half of a joined error is the SAME errors, not copies of
	// their text.
	got := withoutCancellation(cancelled, errors.Join(errors.Join(removeA, removeB), context.Canceled))
	if !errors.Is(got, removeA) || !errors.Is(got, removeB) {
		t.Errorf("the genuine failures lost their identity: %v", got)
	}
	// And an error with no cancellation in it comes back unchanged.
	if got := withoutCancellation(cancelled, copyFailed); got != copyFailed {
		t.Errorf("withoutCancellation rebuilt an error it had nothing to take out of: %v", got)
	}
	// A lone survivor comes back as itself, not as a join of one, so its
	// own type still answers a type switch. For PruneContext's shape that
	// is the very join of failures it collected.
	errs := errors.Join(removeA, removeB)
	if got := withoutCancellation(cancelled, errors.Join(errs, context.Canceled)); got != errs {
		t.Errorf("the one surviving child came back re-wrapped: %#v", got)
	}
	if got := withoutCancellation(cancelled, errors.Join(copyFailed, context.Canceled)); got != copyFailed {
		t.Errorf("the one surviving error came back re-wrapped: %#v", got)
	}
}

// contextThatEnded returns a context that is live when ended is nil,
// cancelled when it is context.Canceled, and past its deadline when it is
// context.DeadlineExceeded.
func contextThatEnded(t *testing.T, ended error) context.Context {
	t.Helper()
	switch {
	case ended == nil:
		return context.Background()
	case errors.Is(ended, context.Canceled):
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	case errors.Is(ended, context.DeadlineExceeded):
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}
	t.Fatalf("contextThatEnded: no context ends with %v", ended)
	return nil
}

// assertNoSnapshotDirs fails the test if anything is under dataDir's backups
// root. A missing root counts as empty.
func assertNoSnapshotDirs(t *testing.T, dataDir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, backup.BackupsDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read backups root: %v", err)
	}
	for _, e := range entries {
		t.Errorf("a stopped snapshot left %s under the backups root", e.Name())
	}
}

// waitClosed waits for done to close, failing the test after 10s.
func waitClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return within 10s of its cancel", what)
	}
}

// heldWriter is an io.Writer that holds the first write containing a
// marker until the test lets it go. It stops the ticker between two
// statements on the line it prints between them, with no seam in the
// ticker.
type heldWriter struct {
	marker  string
	hold    sync.Once
	held    chan struct{}
	release chan struct{}
	letOnce sync.Once
	buf     safeBuffer
}

// holdOn returns a heldWriter that holds the first write containing marker.
func holdOn(marker string) *heldWriter {
	return &heldWriter{marker: marker, held: make(chan struct{}), release: make(chan struct{})}
}

// Write records p, first holding the writer until letGo if p is the first
// write to contain the marker.
func (w *heldWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.marker) {
		w.hold.Do(func() {
			close(w.held)
			<-w.release
		})
	}
	return w.buf.Write(p)
}

// wait blocks until a write is held, failing the test after 10s.
func (w *heldWriter) wait(t *testing.T) {
	t.Helper()
	select {
	case <-w.held:
	case <-time.After(10 * time.Second):
		t.Fatalf("nothing wrote %q within 10s", w.marker)
	}
}

// letGo releases the held write. Idempotent, because a test calls it inline
// and from a cleanup as well.
func (w *heldWriter) letGo() { w.letOnce.Do(func() { close(w.release) }) }

// String is everything written so far.
func (w *heldWriter) String() string { return w.buf.String() }
