package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// A test waits on serve for an EVENT: a boot milestone, an exit, a line
// printed. It gives up only where the harness would end the run anyway, at
// the test binary's deadline (-test.timeout), less serveWaitReserve for the
// cleanups a failure runs, and then reports what serve was doing.
//
// A fixed bound there was a guess about disk writes. Neither serve's boot
// nor its teardown has a bound of its own that a starved host keeps to: the
// boot migrates and writes the store, and the teardown's store close
// checkpoints and syncs it. On a Windows host with 24 writers syncing to its
// disk (B63, 2026-09-29) the store close took up to 16.4 s and a boot more
// than 30 s, and the old bounds failed 12 of 24 runs of the three tests CI
// had failed: "no statement compared a key within 10s", "serve never
// reached the tsnet start within 30s", "runServe did not return". These
// waits failed none of 24. A bound that stays is derived from the thing it
// bounds: a grace (the drains-together order check), never a guess at a
// disk.
//
// A wait that reaches the deadline means serve never got there, which a
// hang, the thing these waits exist to catch, does. To make one fail
// sooner while debugging it, run with a shorter -timeout.

// serveWaitReserve is the part of the test binary's deadline a wait on serve
// leaves unspent: the drain and the directory removals a failure runs next,
// and the report.
const serveWaitReserve = 30 * time.Second

// serveDrainReserve is the part a drain leaves unspent (serveDrainGiveUp). It
// is smaller than serveWaitReserve, so a drain that follows a wait which gave
// up still has the time between the two to see serve exit, once the cleanups
// registered after it have released what the test held serve on. With one
// reserve for both, the drain's give-up had passed by then, and it reported
// a second failure about a serve that was on its way out.
const serveDrainReserve = 10 * time.Second

// serveGiveUpTime is when a wait on serve gives up, or the zero time when the
// test binary runs with no deadline, and it never does.
func serveGiveUpTime(t *testing.T) time.Time {
	return serveDeadlineLess(t, serveWaitReserve)
}

// serveGiveUp fires when a wait on serve gives up (serveGiveUpTime), at once
// if that has passed, and never when the test binary has no deadline.
func serveGiveUp(t *testing.T) <-chan time.Time {
	return firesAt(serveGiveUpTime(t))
}

// serveDrainGiveUp is serveGiveUp for a drain, which runs in a cleanup: it
// fires serveDrainReserve before the deadline.
func serveDrainGiveUp(t *testing.T) <-chan time.Time {
	return firesAt(serveDeadlineLess(t, serveDrainReserve))
}

func serveDeadlineLess(t *testing.T, reserve time.Duration) time.Time {
	deadline, ok := t.Deadline()
	if !ok {
		return time.Time{}
	}
	return deadline.Add(-reserve)
}

// firesAt fires at the instant at, at once if that has passed, and never for
// the zero time.
func firesAt(at time.Time) <-chan time.Time {
	if at.IsZero() {
		return nil
	}
	return time.After(time.Until(at))
}

// serveStacks is the stack of every goroutine running in runServe or started
// by it, for a failure that says what serve was doing when a wait gave up.
func serveStacks() string {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var b strings.Builder
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "/cmd/bridge.runServe") {
			b.WriteString(g)
			b.WriteString("\n\n")
		}
	}
	if b.Len() == 0 {
		return "(no goroutine of serve's is running)"
	}
	return b.String()
}
