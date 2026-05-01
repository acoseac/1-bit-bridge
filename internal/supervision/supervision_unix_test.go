//go:build !windows

package supervision

import (
	"os"
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

	cases := []struct {
		name string
		xpc  string
		inv  string
		want bool
	}{
		{"both unset", "", "", false},
		{"launchd only", "com.acoseac.onebit.bridge", "", true},
		{"systemd only", "", "abc123def456", true},
		{"both set", "com.acoseac.onebit.bridge", "abc123def456", true},
		// macOS sentinel: the entire user session is a launchd
		// subtree, so every shell-spawned process inherits
		// XPC_SERVICE_NAME=0 from its ancestor. "0" means "you
		// have a launchd ancestor but you are NOT a managed
		// job" — must NOT count as supervised. Found post-merge
		// on the local Mac dev fixture: nohup-launched bridge
		// had XPC=0 inherited from zsh, the prior detection
		// treated it as supervised, and the admin UI promised
		// an auto-relaunch that wasn't going to happen.
		{"launchd zero sentinel", "0", "", false},
		// Whitespace-only is not a real Label (real launchd
		// Labels are reverse-DNS strings). Treat as
		// unsupervised — matches the package doc's
		// "false negatives fine, false positives not" stance
		// against an accidental shell `export
		// XPC_SERVICE_NAME=" "` falsely promoting the process
		// to "managed". The trim happens in the production
		// code so " " === "".
		{"launchd whitespace only", " ", "", false},
		{"launchd whitespace plus zero", "  0  ", "", false},
		// systemd-side analog: INVOCATION_ID has no documented
		// sentinel — it's either unset or a UUID. We don't
		// trim or filter "0" there; if a future systemd
		// version starts using a sentinel we'll learn it the
		// honest way.
		{"systemd literal zero (still set)", "", "0", true},
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
			if got := IsSupervised(); got != tc.want {
				t.Fatalf("IsSupervised() = %v, want %v (XPC=%q INVOCATION=%q)",
					got, tc.want, tc.xpc, tc.inv)
			}
		})
	}
}
