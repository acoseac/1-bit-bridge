//go:build windows

package updater

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Backlog B898 on Windows itself: the predicate over Windows' own errnos, and
// holds made by handles the test opens with share mode 0, so that Windows'
// own ERROR_SHARING_VIOLATION refuses the rename. Each case first requires
// that refusal of a rename of the held file, without which it proves nothing,
// and lets a hold go only from the observer, after a refusal it heard.

// TestOnlyWindowsSharingAndLockViolationsAreAFileInUse: a sharing or a lock
// violation is a hold, wrapped as a rename or an open wraps it; Access is
// denied (a rename onto the mapped running image, a permission, backlog
// B202), and every other failure, never is.
func TestOnlyWindowsSharingAndLockViolationsAreAFileInUse(t *testing.T) {
	for _, tc := range []struct {
		errno windows.Errno
		want  bool
	}{
		{windows.ERROR_SHARING_VIOLATION, true},
		{windows.ERROR_LOCK_VIOLATION, true},
		{windows.ERROR_ACCESS_DENIED, false},
		{windows.ERROR_FILE_NOT_FOUND, false},
		{windows.ERROR_PATH_NOT_FOUND, false},
		{windows.ERROR_NOT_SAME_DEVICE, false},
	} {
		for _, err := range []error{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: tc.errno}, &os.PathError{Op: "open", Path: "a", Err: tc.errno}} {
			if got := inUse(err); got != tc.want {
				t.Errorf("inUse(%v) = %v, want %v", err, got, tc.want)
			}
		}
	}
}

// TestAnAccessDeniedRenameIsAnsweredAtOnceThroughThePredicate: Windows'
// ERROR_ACCESS_DENIED at the rollback's vacate, through the platform's own
// predicate, is one attempt and no wait; its sharing violation is waited out.
func TestAnAccessDeniedRenameIsAnsweredAtOnceThroughThePredicate(t *testing.T) {
	for _, tc := range []struct {
		errno    windows.Errno
		attempts int
	}{
		{windows.ERROR_ACCESS_DENIED, 1},
		{windows.ERROR_SHARING_VIOLATION, len(forwardWaits)},
	} {
		r := newHeldRig(t, "", map[string]*refusals{"dst>aside": {n: -1,
			err: &os.LinkError{Op: "rename", Old: "a", New: "b", Err: tc.errno}}})
		renameInUse = inUse
		err := r.rollback()
		if got := strings.Count(strings.Join(r.calls, " "), "dst>aside"); got != tc.attempts || !errors.Is(err, tc.errno) {
			t.Errorf("%v at the vacate: %d attempts (%v), want %d", tc.errno, got, err, tc.attempts)
		}
		if tc.attempts == 1 && len(r.waits) != 0 {
			t.Errorf("%v was waited for: %v", tc.errno, r.waits)
		}
	}
}

// holdShareNone opens path sharing nothing with any other handle, as a
// scanner may hold a file it reads, and requires that Windows now refuses a
// rename of it with a sharing violation; letGo closes the handle, once, and
// the test's end does too.
func holdShareNone(t *testing.T, path string) (letGo func()) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("hold %s: %v", path, err)
	}
	var closed atomic.Bool
	letGo = func() {
		if closed.CompareAndSwap(false, true) {
			_ = windows.CloseHandle(h)
		}
	}
	t.Cleanup(letGo)
	if err := os.Rename(path, path+".elsewhere"); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("a rename of the held %s = %v, want a sharing violation: the case shows nothing", filepath.Base(path), err)
	}
	return letGo
}

// realHolds stands in for the moves' waits: each recorded, and slept for real
// once the test's own hold is let go (released), so that a scanner of the
// host's holding the file a moment longer is waited out as in production.
func realHolds(t *testing.T, released *atomic.Bool) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	prev := renameWait
	t.Cleanup(func() { renameWait = prev })
	renameWait = func(d time.Duration) {
		waits = append(waits, d)
		if released.Load() {
			time.Sleep(d)
		}
	}
	return &waits
}

// letGoAtRefusalOf lets go of a hold once the observer has heard a refusal of
// path, and counts each refusal of it.
func letGoAtRefusalOf(t *testing.T, path string, letGo func(), released *atomic.Bool) *atomic.Int32 {
	t.Helper()
	var heard atomic.Int32
	t.Cleanup(SetRenameObserverForTest(func(rr RenameRefusal) {
		if rr.From == path && errors.Is(rr.Err, windows.ERROR_SHARING_VIOLATION) {
			heard.Add(1)
			if letGo != nil {
				letGo()
				released.Store(true)
			}
		}
	}))
	return &heard
}

// rollbackFiles lays out a binary and its backup in a directory of their own.
func rollbackFiles(t *testing.T) (dst, bak string) {
	t.Helper()
	dir := t.TempDir()
	dst, bak = filepath.Join(dir, "bridge.exe"), filepath.Join(dir, "bridge.exe.bak")
	for p, body := range map[string]string{dst: "RUNNING", bak: "KNOWN-GOOD"} {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dst, bak
}

// TestARollbackWaitsOutAHandleThatSharesNoDeletion: another handle holds the
// backup sharing nothing, as a scanner holds the executable a start has just
// run; the rollback's probe hears the refusal with the binary in place, and
// once the hold is let go the backup is put back.
func TestARollbackWaitsOutAHandleThatSharesNoDeletion(t *testing.T) {
	dst, bak := rollbackFiles(t)
	var released atomic.Bool
	letGo := holdShareNone(t, bak)
	realHolds(t, &released)
	heard := letGoAtRefusalOf(t, bak, letGo, &released)
	if err := replaceRunningBinary(dst, bak); err != nil {
		t.Fatalf("the rollback = %v, want the backup put back once its holder let it go", err)
	}
	if heard.Load() < 1 {
		t.Error("the rollback never heard the backup refused")
	}
	if got := readBytes(t, dst); got != "KNOWN-GOOD" {
		t.Errorf("the binary's place holds %q, want the backup", got)
	}
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		t.Errorf("the backup is still there: %v", err)
	}
	if m, _ := filepath.Glob(dst + ".rollback-*"); len(m) != 0 {
		t.Errorf("asides left: %v", m)
	}
}

// TestARollbackHeldThroughItsWaitMovesNothing: the backup held through the
// whole forward schedule, the rollback moves nothing, the hold its answer.
func TestARollbackHeldThroughItsWaitMovesNothing(t *testing.T) {
	dst, bak := rollbackFiles(t)
	var released atomic.Bool
	holdShareNone(t, bak)
	waits := realHolds(t, &released)
	heard := letGoAtRefusalOf(t, bak, nil, &released)
	err := replaceRunningBinary(dst, bak)
	var swapErr *SwapError
	if !errors.As(err, &swapErr) || swapErr.Outcome != NothingMoved || !swapErr.InUse || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("the rollback = %v, want nothing moved for the hold", err)
	}
	if int(heard.Load()) != len(forwardWaits) || waitsTotal(*waits) != ForwardWait() {
		t.Errorf("heard %d refusals over %v, want %d over %s", heard.Load(), *waits, len(forwardWaits), ForwardWait())
	}
	if readBytes(t, dst) != "RUNNING" || readBytes(t, bak) != "KNOWN-GOOD" {
		t.Error("a file moved")
	}
}

// TestAPutBackHeldThroughItsWaitSaysWhereBothFilesAre: a hold that comes as
// the place is made, and on the aside as it is put back, as a new holder
// between a probe and its rename would: Windows' own sharing violation refuses
// both, the binary is missing, and the error names the rename that puts it
// back.
func TestAPutBackHeldThroughItsWaitSaysWhereBothFilesAre(t *testing.T) {
	dst, bak := rollbackFiles(t)
	var released atomic.Bool
	realHolds(t, &released)
	prev := renameFunc
	t.Cleanup(func() { renameFunc = prev })
	held := map[string]bool{}
	renameFunc = func(from, to string) error {
		if to == dst && !held[from] {
			held[from] = true
			holdShareNone(t, from)
		}
		return os.Rename(from, to)
	}
	err := replaceRunningBinary(dst, bak)
	var swapErr *SwapError
	if !errors.As(err, &swapErr) || swapErr.Outcome != BinaryMissing || !errors.Is(err, ErrBinaryMissing) {
		t.Fatalf("the rollback = %v, want the binary missing", err)
	}
	asides, _ := filepath.Glob(dst + ".rollback-*")
	if len(asides) != 1 || readBytes(t, asides[0]) != "RUNNING" || readBytes(t, bak) != "KNOWN-GOOD" {
		t.Fatalf("asides %v and the backup: want the binary aside and the backup where it was", asides)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("the binary's place: %v, want nothing there", err)
	}
	if want := "rename " + asides[0] + " to " + dst; swapErr.Recover != want {
		t.Errorf("Recover = %q, want %q", swapErr.Recover, want)
	}
}

// TestARenameOntoTheRunningImageIsAnsweredAtOnce: what no wait ends is
// answered after one attempt: a swap's vacate onto a running executable
// (Access is denied, backlog B202) and onto a read-only file, and a rollback
// whose backup is not there.
func TestARenameOntoTheRunningImageIsAnsweredAtOnce(t *testing.T) {
	once := func(t *testing.T, err error, waits []time.Duration, heard int) {
		t.Helper()
		var swapErr *SwapError
		if !errors.As(err, &swapErr) || swapErr.Outcome != NothingMoved || swapErr.InUse || len(waits) != 0 || heard != 1 {
			t.Errorf("err %v, waits %v, %d refusals heard; want one attempt, nothing moved and no wait", err, waits, heard)
		}
	}
	count := func(t *testing.T) *atomic.Int32 {
		var n atomic.Int32
		t.Cleanup(SetRenameObserverForTest(func(RenameRefusal) { n.Add(1) }))
		return &n
	}
	t.Run("a running executable", func(t *testing.T) {
		dir := t.TempDir()
		// The backup's place is bridge.exe, running: the swap of bridge,
		// its backup extension ".exe", vacates onto a mapped image.
		runUntilReady(t, buildSleepingExe(t, dir), dir)
		dst, cand := filepath.Join(dir, "bridge"), filepath.Join(dir, checkStagePrefix+"1.exe")
		writeAll(t, dst, cand)
		var released atomic.Bool
		waits := realHolds(t, &released)
		n := count(t)
		once(t, swapByVacate(dst, cand, ".exe", nil), *waits, int(n.Load()))
	})
	t.Run("a read-only file", func(t *testing.T) {
		dir := t.TempDir()
		dst, cand, bak := filepath.Join(dir, "bridge.exe"), filepath.Join(dir, checkStagePrefix+"1.exe"), filepath.Join(dir, "bridge.exe.bak")
		writeAll(t, dst, cand, bak)
		if err := os.Chmod(bak, 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(bak, 0o644) })
		var released atomic.Bool
		waits := realHolds(t, &released)
		n := count(t)
		once(t, swapByVacate(dst, cand, ".bak", nil), *waits, int(n.Load()))
	})
	t.Run("no backup", func(t *testing.T) {
		dst, bak := rollbackFiles(t)
		if err := os.Remove(bak); err != nil {
			t.Fatal(err)
		}
		var released atomic.Bool
		waits := realHolds(t, &released)
		n := count(t)
		err := replaceRunningBinary(dst, bak)
		once(t, err, *waits, int(n.Load()))
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			t.Errorf("err = %v, want the backup not found", err)
		}
	})
}

// TestAnInstallWaitsForItsCheckedCandidateBeforeItArms: the candidate the
// settings check ran, held: let go after a refusal, the swap arms and lands;
// held throughout, nothing moved and nothing armed.
func TestAnInstallWaitsForItsCheckedCandidateBeforeItArms(t *testing.T) {
	for _, letGoOnRefusal := range []bool{true, false} {
		dir := t.TempDir()
		dst, cand := filepath.Join(dir, "bridge.exe"), filepath.Join(dir, checkStagePrefix+"1.exe")
		writeAll(t, dst, cand)
		var released atomic.Bool
		letGo := holdShareNone(t, cand)
		realHolds(t, &released)
		if !letGoOnRefusal {
			letGo = nil
		}
		heard := letGoAtRefusalOf(t, cand, letGo, &released)
		armed := 0
		err := swapByVacate(dst, cand, ".bak", func() error {
			if heard.Load() < 1 {
				t.Error("the swap armed before it heard the candidate refused")
			}
			armed++
			return nil
		})
		switch {
		case letGoOnRefusal && (err != nil || armed != 1 || readBytes(t, dst) != "C"):
			t.Errorf("let go: the swap = %v, armed %d", err, armed)
		case !letGoOnRefusal && (!errors.Is(err, ErrBinaryInUse) || armed != 0):
			t.Errorf("held throughout: the swap = %v, armed %d; want nothing moved and nothing armed", err, armed)
		}
	}
}

// runUntilReady starts exe (buildSleepingExe's) and waits for its ready file
// in dir, so its image is mapped as a running bridge.exe's is; the test's end
// kills it and joins its Wait.
func runUntilReady(t *testing.T, exe, dir string) {
	t.Helper()
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(exe, ready)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", exe, err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	giveUp := helperReadyGiveUp(t)
	for {
		if _, err := os.Stat(ready); err == nil {
			return
		}
		select {
		case <-exited:
			t.Fatal("the helper exited before it became ready")
		case <-giveUp:
			t.Fatal("the helper never became ready")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func readBytes(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// writeAll writes each path, the candidate as C and every other file as its
// name.
func writeAll(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		body := filepath.Base(p)
		if strings.HasPrefix(body, checkStagePrefix) {
			body = "C"
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}
