package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestNoteDecodeErrCachesOnlyAnUnreadableFile pins what one failed decode
// leaves behind. Only an unreadable file is cached as a miss: a timeout may
// be a hung mount, and any other error says nothing about the file. A
// cancelled sweep leaves nothing at all, even for an error that would
// otherwise be cached, so the shutdown check must come first.
func TestNoteDecodeErrCachesOnlyAnUnreadableFile(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	unreadable := fmt.Errorf("fpcalc: %w", acoustid.ErrUnreadable)
	for _, tc := range []struct {
		name       string
		ctx        context.Context
		err        error
		wantCached bool
		wantLine   string // the one WARN line it logs, or none
	}{
		{"a cancelled sweep", cancelled, unreadable, false, ""},
		{"a timeout", context.Background(), fmt.Errorf("fpcalc: %w", context.DeadlineExceeded), false, msgFpcalcTimedOut},
		{"an unreadable file", context.Background(), unreadable, true, ""},
		{"any other error", context.Background(), errors.New("fpcalc: exit status 3"), false, "fingerprint: decode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			s := &fingerprintSweeper{cache: acoustid.NewCache(4)}
			key := acoustid.Key{Path: "Album/01.flac", Size: 1, MTimeNS: 1}
			s.noteDecodeErr(tc.ctx, key, key.Path, tc.err)
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
