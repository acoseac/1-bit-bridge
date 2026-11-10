//go:build !windows

package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// The console's off of update checks against an install of another process,
// and serve's own (backlog B761's round 3): the off is saved under the
// install marker's lock (LockForChecksOff), which an install holds from just
// before its decision until after its swap, so the off lands before the
// decision, which reads it and refuses, or after the swap, never between the
// two; and the locks the save and serve's own install take are taken in one
// order.

// lockWaiter says when a taker of the install marker's lock first finds it
// held and waits, for the rest of the test (SetStateLockWaitingForTest).
func lockWaiter(t *testing.T) <-chan struct{} {
	t.Helper()
	waiting := make(chan struct{})
	var once sync.Once
	t.Cleanup(SetStateLockWaitingForTest(func(string) { once.Do(func() { close(waiting) }) }))
	return waiting
}

// offSaved is what an off's save found as it landed: the binary in place
// then, or the error that kept it from landing.
type offSaved struct {
	binary string
	err    error
}

// saveOffAsTheConsoleDoes saves an off as the console does, on a goroutine of
// its own: the install marker's lock in dir first (LockForChecksOff), then,
// holding it, write, which writes the switch off, and the lock let go. It
// hands back what the save found in place at livePath as it landed.
func saveOffAsTheConsoleDoes(dir, livePath string, write func()) <-chan offSaved {
	saved := make(chan offSaved, 1)
	go func() {
		lock, err := LockForChecksOff(context.Background(), dir)
		if err != nil {
			saved <- offSaved{err: err}
			return
		}
		defer lock.Release()
		bin, _ := os.ReadFile(livePath)
		write()
		saved <- offSaved{binary: string(bin)}
	}()
	return saved
}

func savedOf(t *testing.T, saved <-chan offSaved) offSaved {
	t.Helper()
	select {
	case s := <-saved:
		return s
	case <-time.After(testWait):
		t.Fatal("the off's save never landed")
		return offSaved{}
	}
}

// TestAnOffSavedAfterAnInstallsRereadLandsAfterItsSwap is the window Codex
// found on #1446's second round: `bridge update`, another process, has read
// the switch on at its decision under the install marker's lock (its
// RereadSwitch) and not yet committed, when the console saves an off. The
// save waits for the lock (it lands neither before the commit nor between it
// and the swap) and lands once the swap is done, so the install it could no
// longer stop finishes whole and the off is what the next start reads.
func TestAnOffSavedAfterAnInstallsRereadLandsAfterItsSwap(t *testing.T) {
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
	var reread rereadStandIn
	park := newParked(t)
	opts := s.manual()
	opts.RereadSwitch = reread.read
	opts.holdAfterReread = park.hook
	answered := s.installInBackground(opts)
	waitFor(t, park.reached, "the install past its reread at its decision")

	waiting := lockWaiter(t)
	saved := saveOffAsTheConsoleDoes(s.dir, s.livePath, func() { reread.off.Store(true) })
	select {
	case <-waiting:
	case r := <-saved:
		t.Fatalf("the off's save landed while the install held the install marker's lock between its reread and its commit, "+
			"the binary %q then (%v): the install swaps after an off it read as on", r.binary, r.err)
	case <-time.After(testWait):
		t.Fatal("the off's save never waited for the install marker's lock")
	}
	park.let()
	if err := answerOf(t, answered); err != nil {
		t.Fatalf("the install past its reread answered %v, want its swap", err)
	}
	r := savedOf(t, saved)
	if r.err != nil || r.binary != "bridge-binary-0.2.0" {
		t.Errorf("the off's save = %+v, want it landed once 0.2.0 was swapped in", r)
	}
	if !reread.off.Load() {
		t.Error("the off was not saved")
	}
	if st, err := LoadState(s.dir); err != nil || !st.SwapStarted || st.TargetVersion != "0.2.0" {
		t.Errorf("the install marker = %+v (%v), want 0.2.0 swapped", st, err)
	}
}

// TestAnInstallWaitingBehindAnOffSaveReadsItAndRefuses: the console's save of
// an off holds the install marker's lock when `bridge update` reaches its
// decision; the install waits for the lock, reads the switch again at its
// decision once the save has let it go, finds it off and refuses, nothing
// armed, backed up or swapped.
func TestAnInstallWaitingBehindAnOffSaveReadsItAndRefuses(t *testing.T) {
	s := newSwitchedInstall(t, "0.1.0", "0.2.0", nil)
	var reread rereadStandIn
	lock, err := LockForChecksOff(context.Background(), s.dir)
	if err != nil || !lock.Held() {
		t.Fatalf("the off's save takes the install marker's lock: %+v, %v", lock, err)
	}
	defer lock.Release()
	waiting := lockWaiter(t)
	opts := s.manual()
	opts.RereadSwitch = reread.read
	answered := s.installInBackground(opts)
	select {
	case <-waiting:
	case err := <-answered:
		t.Fatalf("the install answered %v while the off's save held the install marker's lock, its binary %s: "+
			"it did not wait for the save", err, binaryOf(t, s.livePath))
	case <-time.After(testWait):
		t.Fatal("the install never waited for the install marker's lock")
	}
	reread.off.Store(true) // the save's write, under its lock
	lock.Release()
	if err := answerOf(t, answered); !errors.Is(err, ErrUpdateChecksOff) || !strings.Contains(err.Error(), "at the install's decision") {
		t.Errorf("the install that waited behind the off's save answered %v, want ErrUpdateChecksOff at its decision", err)
	}
	s.requireNothingInstalled(t, "0.1.0")
}

func binaryOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// servesSwitch is serve's update check's switch: a live config holder over a
// settings file, its boot read recorded, and an off saved through it as the
// console saves one, after the install marker's lock and with the console's
// own lock (a stand-in of admin's Server.mu) held across the holder's save
// and the updater's notice: the order checksoff.go documents.
type servesSwitch struct {
	path    string
	holder  *config.RuntimeConfig
	console sync.Mutex
}

func newServesSwitch(t *testing.T) *servesSwitch {
	t.Helper()
	t.Setenv("BRIDGE_UPDATE_CHECK_ENABLED", "")
	os.Unsetenv("BRIDGE_UPDATE_CHECK_ENABLED")
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte("libraryRoots:\n  - "+lib+"\ndataDir: "+filepath.Join(dir, "data")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, raw, file, err := config.LoadFileAt(path)
	if err != nil {
		t.Fatalf("the settings do not load: %v", err)
	}
	h := config.NewRuntimeConfig(cfg)
	h.RecordFile(file, raw)
	return &servesSwitch{path: path, holder: h}
}

func (w *servesSwitch) on() bool { return w.holder.Load().Update.EffectiveCheckEnabled() }

// saveOff saves an off in the documented order and tells upd, holding
// inTurn's hold inside the holder's turn where it is set: the marker lock in
// dataDir, then the console's lock, then the holder's turn, then the switch's
// gate (ChecksSwitched), released the other way.
func (w *servesSwitch) saveOff(dataDir string, upd *Updater, inTurn func()) error {
	lock, err := LockForChecksOff(context.Background(), dataDir)
	if err != nil {
		return err
	}
	defer lock.Release()
	w.console.Lock()
	defer w.console.Unlock()
	off := false
	if err := w.holder.UpdateChecked(w.path, func(next *config.Config, _ func() error) error {
		if inTurn != nil {
			inTurn()
		}
		next.Update.CheckEnabled = &off
		return nil
	}); err != nil {
		return err
	}
	upd.ChecksSwitched()
	return nil
}

// TestServesOwnInstallAndAnOffSaveTakeTheLocksInOneOrder: serve's own install
// (its switch the live holder's) and the console's off take the locks in the
// one order checksoff.go documents: the install marker's lock, then the
// console's and the holder's, then the switch's gate. With the install at its
// decision, holding the marker lock, the save waits for it holding nothing
// else, and lands once the swap is done; with the save inside the holder's
// turn, holding the marker lock, the install waits for it holding nothing,
// while a status, the switch and the holder's snapshot still answer, and the
// save's notice stops the waiting install with nothing installed. Neither
// arrival deadlocks.
func TestServesOwnInstallAndAnOffSaveTakeTheLocksInOneOrder(t *testing.T) {
	t.Run("the install at its decision first", func(t *testing.T) {
		w := newServesSwitch(t)
		s := newSwitchedInstall(t, "0.1.0", "0.2.0", func(o *Options, _ *InstallOptions) { o.CheckEnabled = w.on })
		park := newParked(t)
		opts := s.manual()
		opts.holdBeforeDecision = park.hook
		answered := s.installInBackground(opts)
		waitFor(t, park.reached, "serve's install at its decision")

		waiting := lockWaiter(t)
		saved := make(chan error, 1)
		go func() { saved <- w.saveOff(s.dir, s.upd, nil) }()
		select {
		case <-waiting:
		case err := <-saved:
			t.Fatalf("the off's save answered %v while serve's install held the install marker's lock at its decision", err)
		case <-time.After(testWait):
			t.Fatal("the off's save never waited for the install marker's lock")
		}
		// The save waits holding nothing else: the console's lock is free.
		if !w.console.TryLock() {
			t.Fatal("the off's save holds the console's lock while it waits for the install marker's")
		}
		w.console.Unlock()
		park.let()
		if err := answerOf(t, answered); err != nil {
			t.Fatalf("serve's install at its decision answered %v, want its swap", err)
		}
		select {
		case err := <-saved:
			if err != nil {
				t.Fatalf("the off's save = %v, want it saved after the swap", err)
			}
		case <-time.After(testWait):
			t.Fatal("the off's save never landed: the locks deadlocked")
		}
		if w.on() || s.upd.Status().CheckEnabled {
			t.Errorf("after the save the holder reads on %v and the updater's status %v, want both off", w.on(), s.upd.Status().CheckEnabled)
		}
		if got := binaryOf(t, s.livePath); got != "bridge-binary-0.2.0" {
			t.Errorf("the binary = %q, want 0.2.0's", got)
		}
	})
	t.Run("the save inside the holder's turn first", func(t *testing.T) {
		w := newServesSwitch(t)
		s := newSwitchedInstall(t, "0.1.0", "0.2.0", func(o *Options, _ *InstallOptions) { o.CheckEnabled = w.on })
		turn := newParked(t)
		saved := make(chan error, 1)
		go func() { saved <- w.saveOff(s.dir, s.upd, turn.hook) }()
		waitFor(t, turn.reached, "the off's save inside the holder's turn")

		waiting := lockWaiter(t)
		answered := s.installInBackground(s.manual())
		select {
		case <-waiting:
		case err := <-answered:
			t.Fatalf("serve's install answered %v while the off's save held the install marker's lock", err)
		case <-time.After(testWait):
			t.Fatal("serve's install never waited for the install marker's lock")
		}
		// What runs beside them answers: the switch and a snapshot read the
		// holder's published value without its lock, and a status takes no
		// lock the save holds.
		done := make(chan struct{})
		go func() {
			_ = w.on()
			_ = s.upd.Status()
			_ = w.holder.Load()
			close(done)
		}()
		waitFor(t, done, "a status, the switch and the holder's snapshot while the save holds its locks")
		turn.let()
		select {
		case err := <-saved:
			if err != nil {
				t.Fatalf("the off's save = %v, want it saved", err)
			}
		case <-time.After(testWait):
			t.Fatal("the off's save never finished: the locks deadlocked")
		}
		if err := answerOf(t, answered); !errors.Is(err, ErrUpdateChecksOff) {
			t.Errorf("serve's install that waited behind the off's save answered %v, want ErrUpdateChecksOff", err)
		}
		s.requireNothingInstalled(t, "0.1.0")
	})
}
