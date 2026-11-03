//go:build !windows

package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAStopWhileTheHoldWaitsIsTheInstallsAnswer: the hold of a candidate the
// settings check refused, or one that predates the check (for the
// auto-installer), waits for the install marker's lock, and a stop in that
// wait is the install's answer: the context's error, never the refusal's,
// with nothing recorded (backlog B692's round 2). The auto-installer reads a
// stop as no failure.
func TestAStopWhileTheHoldWaitsIsTheInstallsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		check     ConfigCheck
		automatic bool
	}{
		{"a refused candidate", ConfigCheck{Verdict: ConfigRefused, Reasons: []string{"line 1: a whole number is written with a leading zero"}}, false},
		{"a candidate that predates the check", ConfigCheck{Verdict: ConfigCheckUnsupported}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fix := newInstallFixture(t, "0.2.3")
			dir, live := installLayout(t)
			cfg := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(cfg, []byte("scanIntervalSec: 010\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			holdStateLock(t, dir, withinTheTest)
			waits := signalWaits(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			installed, _ := installIn(t, ctx, installingUpdater(t, fix, "0.2.1"), InstallOptions{
				DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier, ConfigPath: cfg, Automatic: tc.automatic,
				ConfigCheck: func(context.Context, string, string) ConfigCheck { return tc.check },
			}, cancel)
			select {
			case <-waits:
			case err := <-installed:
				t.Fatalf("the hold did not wait for the marker's lock another process held: %v", err)
			case <-time.After(withinTheTest):
				t.Fatal("the hold never waited for the marker's lock")
			}
			cancel()
			err := <-installed
			if _, held := RefusalReason(err); !errors.Is(err, context.Canceled) || held ||
				errors.Is(err, ErrConfigRefused) || errors.Is(err, ErrConfigCheckUnsupported) {
				t.Fatalf("an install stopped while its hold waited = %v, want the stop and not the refusal", err)
			}
			if st, err := LoadState(dir); err != nil || st.RejectedVersion != "" {
				t.Errorf("the marker = %+v (%v), want no hold recorded", st, err)
			}
			if got := readFile(t, live); got != "bridge-binary-0.2.1" {
				t.Errorf("the binary is %q, want it as it was", got)
			}
		})
	}
}
