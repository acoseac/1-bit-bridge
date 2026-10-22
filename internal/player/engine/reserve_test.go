package engine

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestAFailingReservationWriteRefusesEdits pins a reservation that cannot
// be made durable: every insert and every other token-advancing edit is
// refused with the reason, the rest still works, the write is never retried
// in the background, and the next edit that needs it tries again.
func TestAFailingReservationWriteRefusesEdits(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.restartWith(func() {
		h.files.failReserve = errors.New("read-only file system")
	})
	if ed := h.snap().Edits(); ed.Refused != ReasonReservation {
		t.Fatalf("edits %+v, want refused %s", ed, ReasonReservation)
	}
	_, err := h.eng.InsertAfter(bg, 0, ref("x"), nil)
	h.refused("insert", err, ReasonReservation)
	h.refused("delete", h.eng.Delete(bg, ids[0]), ReasonReservation)
	h.refused("delete all", h.eng.DeleteAll(bg), ReasonReservation)
	h.must("repeat, which advances no token", h.eng.SetRepeat(bg, true))
	h.available()
	h.must("play, which advances no token", h.eng.PlayID(bg, ids[1]))
	if h.snap().Len() != 2 {
		t.Fatal("a refused edit changed the queue")
	}
	h.files.mu.Lock()
	tried := h.files.reserveAttempts
	h.files.mu.Unlock()
	h.clk.Advance(time.Hour)
	h.files.mu.Lock()
	again := h.files.reserveAttempts
	h.files.failReserve = nil
	h.files.mu.Unlock()
	if again != tried {
		t.Fatalf("the reservation was retried in the background: %d attempts, then %d", tried, again)
	}
	if _, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); err != nil {
		t.Fatalf("an insert once the write can land: %v", err)
	}
	if ed := h.snap().Edits(); ed.Refused != "" {
		t.Fatalf("edits %+v once the reservation landed", ed)
	}
}

// TestAQueueSavedWithoutItsRunsReservationLoads pins the file a run whose
// reservation failed writes: its counters are past the reservation, and the
// next start loads it as it was.
func TestAQueueSavedWithoutItsRunsReservationLoads(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.restartWith(func() { h.files.failReserve = errors.New("read-only file system") })
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.restartWith(func() { h.files.failReserve = nil })
	sn := h.snap()
	if got := sn.IDs(); len(got) != 2 || got[0] != ids[0] || !sn.Repeat() {
		t.Fatalf("the queue saved by a run without its reservation loaded as %v, repeat %v", got, sn.Repeat())
	}
	if aside, _ := filepath.Glob(h.queuePath() + ".bad-*"); len(aside) != 0 {
		t.Fatalf("a good queue file was moved aside: %v", aside)
	}
}

// TestAStartWithoutItsReservationPublishesNoToken pins the start's token:
// published only once a durable reservation covers it, and 0, none, until
// then. Two starts in a row whose reservation write fails, or whose bad
// reservation file stays where it is, with no queue write between them,
// publish no token, so never the same one twice; the first start that can
// reserve publishes one past every token an earlier run published.
func TestAStartWithoutItsReservationPublishesNoToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(h *harness)
		mend    func(h *harness)
		refused Reason
	}{
		{"the write fails", func(h *harness) {
			h.files.failReserve = errors.New("read-only file system")
		}, func(h *harness) {
			h.files.failReserve = nil
		}, ReasonReservation},
		{"the bad file stays", func(h *harness) {
			h.must("corrupt", os.WriteFile(h.reservePath(), []byte("not json"), 0o600))
			h.files.renameErr = errors.New("read-only file system")
		}, func(h *harness) {
			h.files.renameErr = nil
		}, ReasonFileStuck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			h.insert("a", "b")
			published := h.snap().Token()
			var tokens []uint32
			h.restartWith(func() { tc.breakIt(h) })
			for run := 0; run < 2; run++ {
				if run > 0 {
					h.restartWith(func() {})
				}
				sn := h.snap()
				if ed := sn.Edits(); ed.Refused != tc.refused {
					t.Fatalf("start %d: edits %+v, want refused %s", run+1, ed, tc.refused)
				}
				tokens = append(tokens, sn.Token())
			}
			if tokens[0] != 0 || tokens[1] != 0 {
				t.Fatalf("two starts with no durable reservation published tokens %v, want none (0)", tokens)
			}
			h.restartWith(func() { tc.mend(h) })
			if tok := h.snap().Token(); tok <= published {
				t.Fatalf("the first start that can reserve published token %d, not past %d", tok, published)
			}
		})
	}
}

// TestAReservationIsMadeOnlyWithItsDirectoryEntry pins the reservation's
// durability through the engine's own writer, atomicwrite's: a reservation
// whose directory entry could not be fsynced is not made, so nothing it
// covers is handed out, and a crash that rolls the reservation file back
// to its last durable content (what an entry that was never fsynced
// allows) never yields an ID or a token a run handed out before. The
// barrier is the test's on every platform, one that answers and then one
// that fails, so it runs on Windows too, which has none of its own. New's
// probe is the harness's, which passes, so the barrier fails for the writes
// alone, as one that fails now and then does; one that fails every time is
// New's refusal (TestNewRefusesADirectoryThatCannotBeSynced).
func TestAReservationIsMadeOnlyWithItsDirectoryEntry(t *testing.T) {
	var broken atomic.Bool
	errNoSync := errors.New("fsync: input/output error")
	// Registered before the harness, so it is restored after every engine
	// goroutine that could read it has ended.
	prev := atomicwrite.SetSyncParentDirForTest(func(string) error {
		if broken.Load() {
			return errNoSync
		}
		return nil
	})
	t.Cleanup(func() { atomicwrite.SetSyncParentDirForTest(prev) })
	h := newHarness(t)
	h.tweak = func(c *Config) { c.seams.writeReserve = nil }
	h.boot()
	seen := map[uint32]bool{}
	for _, id := range h.insert("a", "b") {
		seen[id] = true
	}
	published := h.snap().Token()
	h.shutdown()
	h.joinAfter(h.eng)
	durable, err := os.ReadFile(h.reservePath())
	h.must("read the reservation file", err)

	broken.Store(true)
	h.boot()
	sn := h.snap()
	published = max(published, sn.Token())
	if ed := sn.Edits(); ed.Refused != ReasonReservation {
		t.Errorf("edits %+v with the reservation's directory entry not durable, want refused %s", ed, ReasonReservation)
	}
	if id, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); err == nil {
		seen[id] = true
		published = max(published, h.snap().Token())
		t.Errorf("an insert took ID %d with the reservation's directory entry not durable", id)
	}
	// A crash: this run's queue writes are lost, and the reservation
	// file's renames roll back to the last durable one.
	h.files.mu.Lock()
	h.files.dropQueue = true
	h.files.mu.Unlock()
	h.shutdown()
	h.joinAfter(h.eng)
	h.must("roll the reservation file back", os.WriteFile(h.reservePath(), durable, 0o600))
	h.files.mu.Lock()
	h.files.dropQueue = false
	h.files.mu.Unlock()
	broken.Store(false)
	h.boot()
	if tok := h.snap().Token(); tok <= published {
		t.Fatalf("after the rollback the token is %d, not past %d, which a run published", tok, published)
	}
	if id := h.insert("y")[0]; seen[id] {
		t.Fatalf("after the rollback ID %d was handed out again", id)
	}
}

// TestNewRefusesADirectoryThatCannotBeSynced pins New's probe of the
// barrier: a reservation directory whose fsync fails on every call, as on a
// mount that cannot sync a directory, is a configuration error that names
// the directory and says where the data directory must go, at every start,
// with nothing read, moved aside or written; once the directory can be
// synced, the queue loads as it was. Without the probe the engine started
// and refused every edit for ever (reservation_not_durable), with nothing
// naming the cause. The barrier is the test's, so it runs on Windows too.
func TestNewRefusesADirectoryThatCannotBeSynced(t *testing.T) {
	var broken atomic.Bool
	errNoSync := errors.New("sync dir: invalid argument")
	// Registered before the harness, so it is restored after every engine
	// goroutine that could read it has ended.
	prev := atomicwrite.SetSyncParentDirForTest(func(string) error {
		if broken.Load() {
			return errNoSync
		}
		return nil
	})
	t.Cleanup(func() { atomicwrite.SetSyncParentDirForTest(prev) })
	h := newHarness(t)
	h.tweak = func(c *Config) { c.seams.writeReserve, c.seams.probe = nil, nil }
	h.boot()
	ids := h.insert("a", "b")
	h.shutdown()
	h.joinAfter(h.eng)
	queue, err := os.ReadFile(h.queuePath())
	h.must("read the queue file", err)
	reserved, err := os.ReadFile(h.reservePath())
	h.must("read the reservation file", err)

	broken.Store(true)
	for start := 1; start <= 2; start++ {
		_, err := New(h.config())
		switch {
		case err == nil:
			t.Fatalf("start %d: New over a directory that cannot be synced built an engine, want a configuration error", start)
		case !errors.Is(err, errNoSync):
			t.Fatalf("start %d: New = %v, want the barrier's failure", start, err)
		case !strings.Contains(err.Error(), h.dir) || !strings.Contains(err.Error(), "a filesystem that can sync a directory"):
			t.Fatalf("start %d: New = %q, want it to name the directory %s and where the data directory must go", start, err, h.dir)
		}
	}
	if got, _ := os.ReadFile(h.queuePath()); !bytes.Equal(got, queue) {
		t.Fatal("a refused start changed the queue file")
	}
	if got, _ := os.ReadFile(h.reservePath()); !bytes.Equal(got, reserved) {
		t.Fatal("a refused start changed the reservation file")
	}
	if aside, _ := filepath.Glob(filepath.Join(h.dir, "*.bad-*")); len(aside) != 0 {
		t.Fatalf("a refused start moved files aside: %v", aside)
	}

	broken.Store(false)
	h.boot()
	if got := h.snap().IDs(); !slices.Equal(got, ids) {
		t.Fatalf("once the directory syncs the queue loads as %v, want %v", got, ids)
	}
}

// TestABadReservationFileIsMovedAside pins the reservation file's own
// checks: moved aside, logged, and the IDs then start past the queue
// file's.
func TestABadReservationFileIsMovedAside(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	h.shutdown()
	h.joinAfter(h.eng)
	h.must("corrupt", os.WriteFile(h.reservePath(), []byte("not json"), 0o600))
	h.boot()
	aside, _ := filepath.Glob(h.reservePath() + ".bad-*")
	if len(aside) != 1 {
		t.Fatalf("moved aside: %v", aside)
	}
	if id := h.insert("c")[0]; id <= ids[1] {
		t.Fatalf("an ID after a bad reservation file is %d, not past the queue's %d", id, ids[1])
	}
	if h.logs.count("moved aside") != 1 {
		t.Fatalf("logs %v", h.logs.all())
	}
}

// restartWith shuts the engine down, runs f, and boots another on the same
// files.
func (h *harness) restartWith(f func()) {
	h.t.Helper()
	old := h.eng
	h.shutdown()
	h.joinAfter(old)
	h.files.mu.Lock()
	f()
	h.files.mu.Unlock()
	h.boot()
}
