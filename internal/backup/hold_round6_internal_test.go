package backup

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldForTest is a hold of a backups folder of the test's own, its lock taken
// as far as the hold knows (held: an unlock that lets nothing go), let go and
// dropped at the test's end.
func heldForTest(t *testing.T) *hold {
	t.Helper()
	root := t.TempDir()
	h := newHold(context.Background(), "20260101T000000Z", root)
	h.held(func() {}, root, ownerKey(root), "20260101T000000Z")
	t.Cleanup(func() {
		h.release()
		h.drop()
	})
	return h
}

// TestTheVerdictsWriteIsAdmittedInOneSectionWithTheGiveUp is backlog B653's
// sixth round's red for the admission of a verdict's write (Codex's P1 on
// round 5, and both reviews of the round's brief): the check of the hold's
// give-up and the registration of the write's step are one section of the
// hold's lock (publishOf), so a give-up comes either before it, and the write
// is not admitted and never runs (its holder writes its INCOMPLETE in a step
// of its own), or after it, and ends the signal of the write it registered,
// which the write asks before its rename and before each removal of a veto.
// The second case parks a give-up against the section, which must wait for
// the section to end: one that came between a check and a registration made
// apart would find no step of the write's to end, and the write would run on a
// signal no give-up ends. Red on the round before, whose write ran on a fresh
// signal whatever the hold's give-up said.
func TestTheVerdictsWriteIsAdmittedInOneSectionWithTheGiveUp(t *testing.T) {
	t.Run("a give-up before the registration", func(t *testing.T) {
		h := heldForTest(t)
		h.abandon()
		ran := false
		_, admitted, answered := publishOf(h, time.Now().Add(time.Minute), func(context.Context) bool {
			ran = true
			return true
		})
		if admitted || !answered || ran {
			t.Errorf("a write after its hold's give-up: admitted %v, answered %v, ran %v; want it refused at once, never run", admitted, answered, ran)
		}
		h.mu.Lock()
		at := h.at
		h.mu.Unlock()
		if at == stepWrite {
			t.Error("the write refused was registered as the hold's step")
		}
	})
	t.Run("the give-up is read under the admission lock", func(t *testing.T) {
		h := heldForTest(t)
		onAdmissionLocked = func() {
			// Let abandonment take the actual lock after its acquisition,
			// before any protected state is read. A check moved immediately
			// before h.mu.Lock has already sampled false and must fail here.
			h.mu.Unlock()
			h.abandon()
			h.mu.Lock()
		}
		t.Cleanup(func() { onAdmissionLocked = nil })
		ran := false
		_, admitted, answered := publishOf(h, time.Now().Add(time.Minute), func(context.Context) bool {
			ran = true
			return true
		})
		if admitted || !answered || ran {
			t.Errorf("the check sampled before the admission lock: admitted %v, answered %v, ran %v", admitted, answered, ran)
		}
	})
	t.Run("a give-up during the registration", func(t *testing.T) {
		h := heldForTest(t)
		gaveUp := make(chan struct{})
		var once sync.Once
		onAdmission = func() {
			once.Do(func() {
				go func() {
					h.abandon()
					close(gaveUp)
				}()
				// Observe the abandonment actually blocked on this lock;
				// elapsed time alone does not establish that it tried it.
				deadline := time.Now().Add(5 * time.Second)
				blocked := false
				for time.Now().Before(deadline) {
					buf := make([]byte, 1<<20)
					n := runtime.Stack(buf, true)
					for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
						if strings.Contains(stack, "[sync.Mutex.Lock]") && strings.Contains(stack, "backup.(*hold).abandon(") &&
							strings.Contains(stack, "TestTheVerdictsWriteIsAdmittedInOneSectionWithTheGiveUp") {
							blocked = true
						}
					}
					if blocked {
						break
					}
					select {
					case <-gaveUp:
						t.Error("abandonment acquired the lock before the write's registration")
						return
					default:
					}
					runtime.Gosched()
				}
				if !blocked {
					t.Error("abandonment never reached the admission lock")
				}
			})
		}
		t.Cleanup(func() { onAdmission = nil })
		heard, admitted, answered := publishOf(h, time.Now().Add(time.Minute), func(given context.Context) bool {
			select {
			case <-given.Done():
				return true
			case <-time.After(5 * time.Second):
				return false
			}
		})
		select {
		case <-gaveUp:
		case <-time.After(10 * time.Second):
			t.Fatal("the give-up parked against the admission never ran: no section admitted the write")
		}
		if !admitted || !answered {
			t.Fatalf("the write: admitted %v, answered %v; want it admitted, the give-up coming after its registration", admitted, answered)
		}
		if !heard {
			t.Error("the write's signal stayed live through a give-up that came as it was admitted: the give-up found no step of the write's to end")
		}
	})
}

// TestNoVetoRemovalBeginsOnceItsHoldIsGivenUp is backlog B653's sixth round's
// red for the removals a write begins over a record it leaves as it is
// (Codex's P1 on round 5, the Claude API's Low, both reviews of the round's
// brief): each removal checks whether the write's hold was given up,
// though the write's own step was not (an INCOMPLETE's write, begun after the
// give-up, is handed a signal no earlier give-up ended), and asks again if
// the write was given up during the veto's lookup (vetoedAt's Lstat can
// wait on the folder): the give-up is asked again right before each removal,
// after the lookup. Red on the round before, which asked the write's signal
// alone, and once, before the lookup.
func TestNoVetoRemovalBeginsOnceItsHoldIsGivenUp(t *testing.T) {
	for _, tc := range []string{"its hold given up", "given up during the veto's lookup"} {
		t.Run(tc, func(t *testing.T) {
			dir, v := unchangedRecord(t)
			veto := filepath.Join(dir, vetoFileName)
			given, giveUp := context.WithCancel(context.Background())
			defer giveUp()
			held := func() bool { return false }
			switch tc {
			case "its hold given up":
				held = func() bool { return true }
			case "given up during the veto's lookup":
				var once sync.Once
				prev := lookupVeto
				lookupVeto = func(p string) (os.FileInfo, error) {
					if p == veto {
						once.Do(giveUp)
					}
					return os.Lstat(p)
				}
				t.Cleanup(func() { lookupVeto = prev })
			}
			if _, written, err := publishOver(dir, v, given, held); err != nil || written {
				t.Fatalf("publishVerdict = written %v, %v; want nothing written: the test did not reach the unchanged record", written, err)
			}
			if _, err := os.Lstat(veto); err != nil {
				t.Errorf("%s, the veto was removed: %v", tc, err)
			}
		})
	}
}
