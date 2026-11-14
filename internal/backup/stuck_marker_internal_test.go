package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// markerWrites counts the "writes" a write of the marker at marker says
// (onMarkerWrite) while the test runs.
func markerWrites(t *testing.T, marker string) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	t.Cleanup(SetOnMarkerWriteForTest(func(event, m string) {
		if event == "writes" && m == marker {
			n.Add(1)
		}
	}))
	return &n
}

// requireMarker reads the marker at marker as raw bytes and requires them
// to decode whole as want.
func requireMarker(t *testing.T, marker string, want StuckCheck) {
	t.Helper()
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no marker once its write ended: %v", err)
	}
	var got StuckCheck
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the marker once its write ended does not decode (%q): %v", data, err)
	}
	if got.Snapshot != want.Snapshot || got.Step != want.Step || !got.Since.Equal(want.Since) || got.Restore != want.Restore {
		t.Fatalf("the marker reads %+v, want %+v", got, want)
	}
}

// requireNoEntry requires that nothing stands at p, what its name says.
func requireNoEntry(t *testing.T, p, what string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s stands at %s: %v", what, filepath.Base(p), err)
	}
}

// TestAMarkerReadWhileItIsWrittenIsWholeOrNone is backlog B871's round 5
// (B899: main's CI on macOS read "the marker does not read: unexpected end of
// JSON input"): a read of the marker's raw bytes, made while its write is
// parked between the creation of the file its bytes go to and the bytes
// (afterCreate), meets no marker or the marker whole, never an empty or a
// partial one; and once the write ends the marker stands, whole, said once
// ("writes"), with no temporary file beside it. Red at 445a2eca, whose writer
// made the marker itself, then wrote it.
func TestAMarkerReadWhileItIsWrittenIsWholeOrNone(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, stuckMarkerName)
	check := StuckCheck{Snapshot: "snapshot", Step: "its check", Since: time.Now().UTC().Truncate(time.Second)}
	writes := markerWrites(t, marker)
	parked, letGo := make(chan struct{}), make(chan struct{})
	mark := &stuckMark{marker: marker, ref: dir, check: check}
	mark.afterCreate = func() {
		close(parked)
		<-letGo
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		mark.writeMarker()
	}()
	release := func() {
		select {
		case <-letGo:
		default:
			close(letGo)
		}
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the marker's write never ended once let go")
		}
	}
	t.Cleanup(release)
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the marker's write never created the file its bytes go to")
	}
	switch data, err := os.ReadFile(marker); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		t.Fatalf("a read of the marker while its write was under way: %v", err)
	default:
		var got StuckCheck
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("a read of the marker while its write was under way met %d bytes that do not decode: %v", len(data), err)
		}
	}
	release()
	requireMarker(t, marker, check)
	requireNoEntry(t, filepath.Join(dir, stuckMarkerTmpName), "a temporary file")
	if n := writes.Load(); n != 1 {
		t.Errorf("the write said %q %d times, want once", "writes", n)
	}
}

// TestAMarkerWhoseWriteOrCloseFailsLeavesNothing (backlog B871's round 5): a
// write or a close of the marker's temporary file that fails publishes no
// marker, leaves no temporary file, and says no "writes".
func TestAMarkerWhoseWriteOrCloseFailsLeavesNothing(t *testing.T) {
	for _, step := range []string{"write", "close"} {
		t.Run(step, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, stuckMarkerName)
			writes := markerWrites(t, marker)
			mark := &stuckMark{marker: marker, ref: dir, check: StuckCheck{Snapshot: "snapshot", Since: time.Now().UTC()}}
			failed := 0
			mark.failStep = func(s string) error {
				if s != step {
					return nil
				}
				failed++
				return errors.New("injected: the marker's " + s + " failed")
			}
			mark.writeMarker()
			if failed != 1 {
				t.Fatalf("the injected failure of the %s was met %d times, want once", step, failed)
			}
			requireNoEntry(t, marker, "a marker")
			requireNoEntry(t, filepath.Join(dir, stuckMarkerTmpName), "a temporary file")
			if n := writes.Load(); n != 0 {
				t.Errorf("a write whose %s failed said %q %d times", step, "writes", n)
			}
		})
	}
}

// TestTheMarkersTemporaryFileGoesWithIt (backlog B871's round 5): the
// temporary file a write of the marker left beside it, a process's that
// ended, goes with the marker at a holder's first step (clearStaleMarker) and
// at a hold's let-go (removeMarker).
func TestTheMarkersTemporaryFileGoesWithIt(t *testing.T) {
	for _, by := range []string{"clearStaleMarker", "removeMarker"} {
		t.Run(by, func(t *testing.T) {
			dir := t.TempDir()
			marker, tmp := filepath.Join(dir, stuckMarkerName), filepath.Join(dir, stuckMarkerTmpName)
			for _, p := range []string{marker, tmp} {
				if err := os.WriteFile(p, []byte(`{"snapshot":"ended"`), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if by == "clearStaleMarker" {
				clearStaleMarker(dir)
			} else {
				(&stuckMark{marker: marker, ref: dir}).removeMarker()
			}
			requireNoEntry(t, marker, "a marker")
			requireNoEntry(t, tmp, "a temporary file")
		})
	}
}
