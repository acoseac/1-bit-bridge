package backup

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestAGiveUpOnceTheLocksLetGoBeganWritesNoMarker is backlog B653's fifth
// round's red for a give-up that comes while a hold lets its lock go (the
// review of the round's brief): the release's start is recorded in the
// section that takes the mark and the lock's let-go, so a give-up after it
// raises a mark in this process alone and writes no marker beside a lock
// that is no longer held, whether it comes from inside the let-go or races
// it (run under the race detector). Red on the round before, whose give-up
// in the let-go wrote a marker for the next holder to find.
func TestAGiveUpOnceTheLocksLetGoBeganWritesNoMarker(t *testing.T) {
	t.Run("a give-up inside the let-go", func(t *testing.T) {
		root := t.TempDir()
		h := newHold(context.Background(), "snap", root)
		h.held(func() { h.abandon() }, root, ownerKey(root), "snap")
		h.release()
		h.mu.Lock()
		mark := h.mark
		h.mu.Unlock()
		if mark == nil {
			t.Fatal("the give-up raised no mark")
		}
		mark.mu.Lock()
		marker := mark.marker
		mark.mu.Unlock()
		if marker != "" {
			t.Errorf("a give-up once the let-go had begun writes a marker at %s", marker)
		}
		h.drop()
	})
	t.Run("a give-up racing the let-go", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 300; i++ {
			h := newHold(context.Background(), "snap", root)
			h.held(func() {}, root, ownerKey(root), "snap")
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.abandon()
			}()
			h.release()
			wg.Wait()
			h.mu.Lock()
			mark := h.mark
			h.mu.Unlock()
			if mark != nil {
				mark.mu.Lock()
				stray := mark.marker != "" && !mark.removed
				mark.mu.Unlock()
				if stray {
					t.Fatalf("round %d: a mark raised once the let-go had begun keeps a marker the let-go never removes", i)
				}
			}
			h.drop()
		}
		// A marker written before the let-go removed it is gone with it, and
		// so is the temporary file of its write (backlog B871's round 5).
		if _, err := os.Lstat(filepath.Join(root, stuckMarkerName)); err == nil {
			t.Error("a marker was left beside a lock no hold holds")
		}
		if _, err := os.Lstat(filepath.Join(root, stuckMarkerTmpName)); err == nil {
			t.Error("a marker's temporary file was left beside a lock no hold holds")
		}
	})
}

// TestACallersGiveUpAfterAHandOffMarksNothing: a holder that handed its
// write to a hold of its own (handOff: a stopped verification's worker, whose
// INCOMPLETE abandonPending writes) is given up by its caller, as its caller's
// bound and its own answer land together, while it lets go of what it holds:
// that give-up overran nothing and marks nothing, while a step of the same
// release that overruns its own bound marks the folder all the same. Twenty
// runs under the race detector found the caller's give-up landing there and
// marking the folder at the worker's last step, its wait for the lock, once
// in twenty (backlog B653's fifth round), where the hand-off's quiet ended as
// the release began.
func TestACallersGiveUpAfterAHandOffMarksNothing(t *testing.T) {
	t.Run("the caller's give-up as the holder lets go", func(t *testing.T) {
		h := newHold(context.Background(), "snap", t.TempDir())
		h.handOff()
		h.alsoLetGo(func() { h.abandon() })
		h.release()
		if check, marked := h.marked(); marked {
			t.Errorf("the caller's give-up of a holder letting go after its hand-off marked the folder: %+v", check)
		}
		h.drop()
	})
	t.Run("a step of the release that overruns", func(t *testing.T) {
		prev := releaseBound
		releaseBound = 50 * time.Millisecond
		defer func() { releaseBound = prev }()
		h := newHold(context.Background(), "snap", t.TempDir())
		h.handOff()
		out := make(chan struct{})
		h.alsoLetGo(func() { <-out })
		time.AfterFunc(300*time.Millisecond, func() { close(out) })
		h.release()
		if check, marked := h.marked(); !marked || check.Step != stepOwnership {
			t.Errorf("an ownership's let-go past its bound after a hand-off marked %+v, %v; want the folder marked at it", check, marked)
		}
		h.drop()
	})
}

// TestASpellingIsKeyedAgainstTheDirectoryTheProcessStartedIn is backlog
// B653's fifth round's red for the key a relative spelling of a backups
// folder is marked under (the Claude API's Low on round 4): it is made
// absolute against the working directory read once as the process started
// (workDir), never by os.Getwd at its use, which stats the directory, so a
// key asks no filesystem. Red on the round before, whose filepath.Abs asked
// for the working directory at each use.
func TestASpellingIsKeyedAgainstTheDirectoryTheProcessStartedIn(t *testing.T) {
	start := workDir
	if start == "" {
		t.Skip("the working directory could not be read as the process started")
	}
	t.Chdir(t.TempDir())
	if got, want := spellingKey(filepath.Join("data", "backups")), filepath.Join(start, "data", "backups"); got != want {
		t.Errorf("spellingKey = %q, want %q: the working directory was asked again at the key's use", got, want)
	}
}

// TestAHoldsContextEndsWithIt is backlog B653's fifth round's red for the
// context a hold derives from its caller's (the Claude API's Low on round
// 4): drop, its holder's last act, ends it, so nothing of it outlives the
// hold in its caller's context. Red on the round before, which ended it only
// at a give-up.
func TestAHoldsContextEndsWithIt(t *testing.T) {
	h := newHold(context.Background(), "snap", t.TempDir())
	h.drop()
	if h.ctx.Err() == nil {
		t.Error("the hold's context outlived the hold")
	}
}
