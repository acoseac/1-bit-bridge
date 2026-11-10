package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A step registered after abandonment has a fresh signal. setAside must
// also ask its hold before the first veto, the fallback, and each cleanup.
func TestSetAsideAsksItsHoldBeforeEachVetoAttempt(t *testing.T) {
	for _, when := range []string{"before the step", "after the creation check", "after the bundle attempt", "after the root attempt"} {
		t.Run(when, func(t *testing.T) {
			dir := t.TempDir()
			h := heldForTest(t)
			h.handOff() // No on-disk marker is needed for this unit fixture.
			bundle, root := filepath.Join(dir, vetoFileName), rootVetoPath(dir)
			attempts, removals := 0, 0
			prevVeto, prevSync, prevRemove := failVeto, syncVetoDir, removeVeto
			t.Cleanup(func() { failVeto, syncVetoDir, removeVeto = prevVeto, prevSync, prevRemove })
			failVeto = func(string) error {
				attempts++
				if when == "after the creation check" {
					// The attempt passed its check but has not opened the file.
					h.abandon()
				}
				return nil
			}
			syncVetoDir = func(path string) error {
				if (when == "after the bundle attempt" && path == bundle) || (when == "after the root attempt" && path == root) {
					h.abandon()
				}
				return errors.New("injected: the veto's directory sync failed")
			}
			removeVeto = func(path string) error { removals++; return os.Remove(path) }
			var err error
			if when == "before the step" {
				h.abandon()
				var answered bool
				err, answered = stepOf(h, time.Now().Add(time.Minute), stepVeto, func(_, given context.Context) error {
					if given.Err() != nil {
						return errors.New("the fixture did not get a fresh step signal")
					}
					return setAside(dir, publication{given: given, held: h.gaveUp})
				}, nil)
				if !answered {
					t.Fatal("the veto step did not answer")
				}
			} else {
				// Isolate the hold's guard: no registered step signal can hide
				// a missing hold check at the fallback or the cleanup.
				err = setAside(dir, publication{given: context.Background(), held: h.gaveUp})
			}
			if !errors.Is(err, ErrRecheckRefused) {
				t.Errorf("setAside = %v, want the recheck refused", err)
			}
			want := map[string]int{"before the step": 0, "after the creation check": 1, "after the bundle attempt": 1, "after the root attempt": 2}[when]
			if attempts != want || removals != 0 {
				t.Errorf("%d veto attempts, %d removals; want %d attempts and no removal", attempts, removals, want)
			}
			for i, path := range []string{bundle, root} {
				_, statErr := os.Stat(path)
				if i < want && statErr != nil {
					t.Errorf("veto %s was not kept: %v", path, statErr)
				}
				if i >= want && !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("unexpected veto %s: %v", path, statErr)
				}
			}
		})
	}
}

func TestRecoveryStepsKeepFreshSignalsAfterTheHoldIsGivenUp(t *testing.T) {
	for _, words := range []string{stepIncomplete, stepReadBack} {
		t.Run(words, func(t *testing.T) {
			h := heldForTest(t)
			h.abandon()
			live, answered := stepOf(h, time.Now().Add(time.Minute), words, func(_, given context.Context) bool {
				return given.Err() == nil
			}, nil)
			if !answered || !live {
				t.Fatalf("recovery step: answered %v, live signal %v", answered, live)
			}
		})
	}
}
