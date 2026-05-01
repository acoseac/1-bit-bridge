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
