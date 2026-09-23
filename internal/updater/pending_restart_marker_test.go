package updater

import (
	"testing"
	"time"
)

// These pin swapAwaitingRestart directly, against markers written by
// hand. Untagged on purpose: the predicate is pure marker arithmetic and
// platform-independent, while everything that drives a REAL swap is
// `//go:build !windows` in this package (install_test.go, swap_test.go,
// autoinstall_test.go, rollback_sticks_test.go all are, because the
// Windows swap needs SCM and a different fixture). Keeping these here is
// what stops the guard being untested on the platform whose swap path
// has no hardlink fallback at all.
//
// The marker shape is exactly what Install writes before the swap:
// Status "installing", the target, AttemptedAt now, SwapStarted set by
// the markSwapStarted hook once the destructive step begins.

// TestAnInterruptedInstallDoesNotWedgeTheEngine pins the property the
// whole persisted-marker design rests on.
//
// A crash or power loss after SwapStarted leaves an "installing" marker
// on disk. Keyed naively, the refusal would fire forever and the host
// could never install anything again. It does not, for two reasons that
// are asserted here rather than assumed: the marker expires at
// recencyWindow — the SAME constant DecideBootAction uses for
// BootClearAbandoned, so the engine and the boot path cannot disagree
// about whether a marker is still live — and a serve boot clears it
// outright.
func TestAnInterruptedInstallDoesNotWedgeTheEngine(t *testing.T) {
	dir := t.TempDir()
	upd := New(Options{})
	live := State{
		Status:        "installing",
		TargetVersion: "0.2.0",
		AttemptedAt:   time.Now(),
		SwapStarted:   true,
	}
	if err := SaveState(dir, live); err != nil {
		t.Fatal(err)
	}
	if !upd.swapAwaitingRestart(dir, "0.2.0") {
		t.Fatal("a fresh swapped marker should refuse a repeat install")
	}

	// Age it past the window — the same point at which DecideBootAction
	// calls it abandoned.
	abandoned := live
	abandoned.AttemptedAt = time.Now().Add(-RecencyWindow() - time.Minute)
	if err := SaveState(dir, abandoned); err != nil {
		t.Fatal(err)
	}
	if upd.swapAwaitingRestart(dir, "0.2.0") {
		t.Error("an abandoned marker still refuses installs; the guard is a wedge")
	}
	if got := DecideBootAction(abandoned, "0.1.0", time.Now()); got != BootClearAbandoned {
		t.Errorf("the boot path disagrees about the same marker: DecideBootAction = %v, want BootClearAbandoned", got)
	}
}

// TestSwapAwaitingRestartIgnoresAnArmedButUnswappedMarker pins the
// SwapStarted term.
//
// A marker armed with nothing mutated is reachable — a kill during the
// Windows swap's SCM stop is up to 15 s of exactly that state — and .bak
// then belongs to an EARLIER cycle. Nothing is staged, so nothing should
// be refused; DecideBootAction reads the same state as
// BootClearNotSwapped.
func TestSwapAwaitingRestartIgnoresAnArmedButUnswappedMarker(t *testing.T) {
	dir := t.TempDir()
	upd := New(Options{})
	st := State{
		Status:        "installing",
		TargetVersion: "0.2.0",
		AttemptedAt:   time.Now(),
		SwapStarted:   false,
	}
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
	if upd.swapAwaitingRestart(dir, "0.2.0") {
		t.Error("an armed-but-unswapped marker refused an install; nothing was staged")
	}
	if got := DecideBootAction(st, "0.1.0", time.Now()); got != BootClearNotSwapped {
		t.Errorf("DecideBootAction = %v, want BootClearNotSwapped", got)
	}
}

// TestSwapAwaitingRestartOnlyRefusesTheSameTarget is the positive
// control for the version term, in its pure form: refusing a NEWER
// release would strand the host on a version it has not booted.
func TestSwapAwaitingRestartOnlyRefusesTheSameTarget(t *testing.T) {
	dir := t.TempDir()
	upd := New(Options{})
	if err := SaveState(dir, State{
		Status:        "installing",
		TargetVersion: "0.2.0",
		AttemptedAt:   time.Now(),
		SwapStarted:   true,
	}); err != nil {
		t.Fatal(err)
	}
	if !upd.swapAwaitingRestart(dir, "0.2.0") {
		t.Error("the staged target should be refused")
	}
	if upd.swapAwaitingRestart(dir, "0.3.0") {
		t.Error("a newer release was refused; the host would be stranded on a version it has not booted")
	}
	// Tag normalisation: the marker and the poller disagree about the
	// leading "v" by convention, so the comparison must not.
	if !upd.swapAwaitingRestart(dir, "v0.2.0") {
		t.Error("a v-prefixed tag was treated as a different target")
	}
}
