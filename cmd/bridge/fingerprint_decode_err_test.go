package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestNoteDecodeErrCachesOnlyAnUnreadableFile pins what one failed decode
// leaves behind. Only an unreadable file is cached as a miss, and only while
// its budget is still running. A decode that ended after the per-file budget
// is a timeout whatever error carries it: an exit status, which is what
// Windows returns after TerminateProcess, a start error, or ErrUnreadable.
// A cancelled sweep leaves nothing at all, even when that budget has also
// expired, so the shutdown check must come first.
func TestNoteDecodeErrCachesOnlyAnUnreadableFile(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stopExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(stopExpired)
	// Err is still nil. That is the window before the context timer runs.
	late := deadlinePassedContext{}
	unreadable := fmt.Errorf("fpcalc: %w", acoustid.ErrUnreadable)
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		fileCtx    context.Context
		err        error
		wantCached bool
		wantLine   string // the one WARN line it logs, or none
	}{
		{"a cancelled sweep", cancelled, context.Background(), unreadable, false, ""},
		{"a cancelled sweep whose budget already expired", cancelled, expired, errors.New("exit status 1"), false, ""},
		{"a timeout", context.Background(), context.Background(), fmt.Errorf("fpcalc: %w", context.DeadlineExceeded), false, msgFpcalcTimedOut},
		{"an exit after the budget", context.Background(), expired, errors.New("exit status 1"), false, msgFpcalcTimedOut},
		{"a start error after the budget", context.Background(), late, errors.New("exec: canceling Cmd: TerminateProcess"), false, msgFpcalcTimedOut},
		{"an unreadable file after the budget", context.Background(), expired, unreadable, false, msgFpcalcTimedOut},
		{"an unreadable file", context.Background(), context.Background(), unreadable, true, ""},
		{"any other error", context.Background(), context.Background(), errors.New("fpcalc: exit status 3"), false, "fingerprint: decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			s := &fingerprintSweeper{cache: acoustid.NewCache(4)}
			key := acoustid.Key{Path: "Album/01.flac", Size: 1, MTimeNS: 1}
			s.noteDecodeErr(tc.ctx, tc.fileCtx, key, key.Path, tc.err)
			if _, cached := s.cache.Get(key); cached != tc.wantCached {
				t.Errorf("cached = %v, want %v", cached, tc.wantCached)
			}
			for _, msg := range []string{msgFpcalcTimedOut, "fingerprint: decode"} {
				want := 0
				if msg == tc.wantLine {
					want = 1
				}
				if got := rec.Failures(msg); len(got) != want {
					t.Errorf("%q lines = %q, want %d", msg, got, want)
				}
			}
		})
	}
}

// deadlinePassedContext is a per-file budget whose deadline is already past
// and whose Err is still nil: the window before the context timer runs.
type deadlinePassedContext struct{}

func (deadlinePassedContext) Deadline() (time.Time, bool) {
	return time.Now().Add(-time.Second), true
}

func (deadlinePassedContext) Done() <-chan struct{} { return nil }

func (deadlinePassedContext) Err() error { return nil }

func (deadlinePassedContext) Value(any) any { return nil }
