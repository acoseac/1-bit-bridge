package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestARefusedVerificationsLetGoIsCountedUntilItEnds: a verification refused
// before its worker began, at its folder's mark (before any hold) or at the
// slot a check given up holds (once its own hold is dropped), lets go of what
// its capture owned on a goroutine of its own (letGoWithin), answering its
// caller once StopWriteBound has passed whatever that let-go does. The let-go
// is counted (ActiveReleasesForTest) from before it starts until it returns,
// past that bound and past the ownership's unlock and close, so a test that
// drains the holds before it removes their folders can wait for it too
// (backlog B869). Here the let-go releases the capture's ownership, then
// parks; the blocker is let go and dropped while it is parked.
func TestARefusedVerificationsLetGoIsCountedUntilItEnds(t *testing.T) {
	for _, refusal := range []string{"at its folder's mark", "at the slot"} {
		t.Run(refusal, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "snap")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			o, err := takeOwner(dir)
			if err != nil {
				t.Fatal(err)
			}
			// The blocker: a hold given up, on the folder itself or on another
			// folder, its check holding this process's slot.
			on := root
			if refusal == "at the slot" {
				on = t.TempDir()
			}
			blocker := newHold(context.Background(), "blocker", on)
			blocker.held(func() {}, on, ownerKey(on), "blocker")
			if refusal == "at the slot" {
				verifySlot <- struct{}{}
				slotHolder.Store(blocker)
			}
			blocker.giveUp(nil, "a step")
			var blocked sync.Once
			letBlockerGo := func() {
				blocked.Do(func() {
					if refusal == "at the slot" {
						slotHolder.CompareAndSwap(blocker, nil)
						<-verifySlot
					}
					// Its marker removed once written, then its mark dropped.
					blocker.release()
					blocker.drop()
				})
			}
			t.Cleanup(letBlockerGo)
			parked, out := make(chan struct{}), make(chan struct{})
			var unparked sync.Once
			unpark := func() { unparked.Do(func() { close(out) }) }
			t.Cleanup(unpark)
			onEnd := func() {
				o.release()
				close(parked)
				<-out
			}
			_, verr, _ := verifyAndThen(context.Background(), dir, VerifyOptions{BackupsRoot: root}, nil, onEnd)
			if !errors.Is(verr, ErrCheckStillRunning) {
				t.Fatalf("the verification answered %v, want it refused (ErrCheckStillRunning)", verr)
			}
			select {
			case <-parked:
			case <-time.After(30 * time.Second):
				t.Fatal("the refused verification's let-go never ran")
			}
			letBlockerGo()
			for deadline := time.Now().Add(30 * time.Second); ActiveHoldsForTest() != 0; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("%d holds were left once the blocker was dropped", ActiveHoldsForTest())
				}
			}
			if n := ActiveReleasesForTest(); n != 1 {
				t.Fatalf("with the let-go parked past its ownership's release, %d let-goes are counted, want 1", n)
			}
			unpark()
			for deadline := time.Now().Add(30 * time.Second); ActiveReleasesForTest() != 0; time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the let-go returned and %d let-goes are still counted", ActiveReleasesForTest())
				}
			}
		})
	}
}
