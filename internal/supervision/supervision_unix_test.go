//go:build !windows

package supervision

import (
	"os"
	"strconv"
	"testing"
)

// Pins the contract that the admin UI relies on: each documented
// supervisor env var, on its own, flips IsSupervised to true; both
// unset returns false. A future refactor that swaps the env-var
// names without updating the UI would silently break the "honest
// confirm dialog" feature this package exists for; this test makes
// such a refactor a build failure.
func TestIsSupervised_envVarMatrix(t *testing.T) {
	// Save + restore so the test is hermetic against the runner's
	// environment (the user's shell may legitimately have one of
	// these set when running the suite).
	saved := map[string]string{
		"XPC_SERVICE_NAME": os.Getenv("XPC_SERVICE_NAME"),
		"INVOCATION_ID":    os.Getenv("INVOCATION_ID"),
		"LISTEN_FDS":       os.Getenv("LISTEN_FDS"),
		"LISTEN_PID":       os.Getenv("LISTEN_PID"),
	}
	t.Cleanup(func() {
		for k, v := range saved {
			if v == "" {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, v)
			}
		}
	})

	myPID := strconv.Itoa(os.Getpid())
	// Derive a non-self PID from os.Getppid() — guaranteed != myPID
	// for any non-init process (and the test harness always has a
	// parent). Beats a hardcoded literal like "99999" which can in
	// principle collide with the test runner's actual PID on a busy
	// system; the collision would silently flip the env-leak guard
	// case to passing-for-the-wrong-reason.
	otherPID := strconv.Itoa(os.Getppid())

	cases := []struct {
		name      string
		xpc       string
		inv       string
		listenFDs string
		listenPID string
		want      bool
	}{
		{"all unset", "", "", "", "", false},
		{"launchd only", "com.acoseac.onebit.bridge", "", "", "", true},
		{"systemd only", "", "abc123def456", "", "", true},
		{"both set", "com.acoseac.onebit.bridge", "abc123def456", "", "", true},
		// macOS sentinel: the entire user session is a launchd
		// subtree, so every shell-spawned process inherits
		// XPC_SERVICE_NAME=0 from its ancestor. "0" means "you
		// have a launchd ancestor but you are NOT a managed
		// job" — must NOT count as supervised. Found post-merge
		// on the local Mac dev fixture: nohup-launched bridge
		// had XPC=0 inherited from zsh, the prior detection
		// treated it as supervised, and the admin UI promised
		// an auto-relaunch that wasn't going to happen.
		{"launchd zero sentinel", "0", "", "", "", false},
		// Whitespace-only is not a real Label (real launchd
		// Labels are reverse-DNS strings). Treat as
		// unsupervised — matches the package doc's
		// "false negatives fine, false positives not" stance
		// against an accidental shell `export
		// XPC_SERVICE_NAME=" "` falsely promoting the process
		// to "managed". The trim happens in the production
		// code so " " === "".
		{"launchd whitespace only", " ", "", "", "", false},
		{"launchd whitespace plus zero", "  0  ", "", "", "", false},
		// systemd-side analog: INVOCATION_ID has no documented
		// sentinel — it's either unset or a UUID. We don't
		// trim or filter "0" there; if a future systemd
		// version starts using a sentinel we'll learn it the
		// honest way.
		{"systemd literal zero (still set)", "", "0", "", "", true},
		// systemd socket activation: LISTEN_FDS is set when a
		// .socket unit hands fds to the bridge. Per sd_listen_fds(3)
		// LISTEN_PID MUST equal getpid() — the env can leak from a
		// parent that exec()'d us. Both gates required.
		{"systemd socket activation (LISTEN_FDS + matching PID)", "", "", "3", myPID, true},
		// Env-leak guard: parent was socket-activated, exec()'d us,
		// LISTEN_PID stayed pointing at the parent. Promoting us to
		// supervised would lie to the admin UI about auto-relaunch.
		{"LISTEN_FDS set but LISTEN_PID is parent", "", "", "3", otherPID, false},
		// sd_listen_fds(3) sentinel: LISTEN_FDS=0 means "no fds
		// passed" — same shape as the XPC=0 launchd sentinel. Even
		// with a matching LISTEN_PID, the process was not actually
		// socket-activated and the admin UI must NOT promise
		// auto-relaunch.
		{"LISTEN_FDS zero sentinel (matching PID)", "", "", "0", myPID, false},
		// Non-numeric LISTEN_FDS is malformed per spec; treat as
		// unsupervised. Real systemd always sets a small positive
		// integer.
		{"LISTEN_FDS non-numeric (matching PID)", "", "", "garbage", myPID, false},
		// LISTEN_FDS without LISTEN_PID is malformed per spec —
		// treat conservatively (false). Real systemd always sets
		// the pair together.
		{"LISTEN_FDS set, LISTEN_PID unset", "", "", "3", "", false},
		// Empty LISTEN_FDS is "no fds inherited" → not socket-
		// activated. LISTEN_PID alone proves nothing.
		{"LISTEN_FDS empty, LISTEN_PID matches", "", "", "", myPID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.xpc == "" {
				_ = os.Unsetenv("XPC_SERVICE_NAME")
			} else {
				_ = os.Setenv("XPC_SERVICE_NAME", tc.xpc)
			}
			if tc.inv == "" {
				_ = os.Unsetenv("INVOCATION_ID")
			} else {
				_ = os.Setenv("INVOCATION_ID", tc.inv)
			}
			if tc.listenFDs == "" {
				_ = os.Unsetenv("LISTEN_FDS")
			} else {
				_ = os.Setenv("LISTEN_FDS", tc.listenFDs)
			}
			if tc.listenPID == "" {
				_ = os.Unsetenv("LISTEN_PID")
			} else {
				_ = os.Setenv("LISTEN_PID", tc.listenPID)
			}
			if got := IsSupervised(); got != tc.want {
				t.Fatalf("IsSupervised() = %v, want %v (XPC=%q INVOCATION=%q LISTEN_FDS=%q LISTEN_PID=%q)",
					got, tc.want, tc.xpc, tc.inv, tc.listenFDs, tc.listenPID)
			}
		})
	}
}
