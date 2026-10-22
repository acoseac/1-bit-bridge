package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
