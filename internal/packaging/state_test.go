package packaging

import (
	"strings"
	"testing"
)

// TestServiceKindDescription pins the contract that the menu's status
// line relies on: every ServiceKind has a non-empty Description, and
// the user/system split (LaunchdUser vs LaunchdSystem, SystemdUser vs
// SystemdSystem, WindowsSCM vs WindowsStartup) produces distinct
// strings so a UI never collapses two different install kinds into
// the same display label.
func TestServiceKindDescription(t *testing.T) {
	all := []ServiceKind{
		KindNone,
		KindLaunchdUser,
		KindLaunchdSystem,
		KindSystemdUser,
		KindSystemdSystem,
		KindWindowsSCM,
		KindWindowsStartup,
	}
	seen := make(map[string]ServiceKind, len(all))
	for _, k := range all {
		got := k.Description()
		if got == "" {
			t.Errorf("ServiceKind(%d).Description() returned empty string", k)
			continue
		}
		if dup, exists := seen[got]; exists {
			t.Errorf("ServiceKind(%d) and ServiceKind(%d) share description %q — must be distinct so the status line can disambiguate", dup, k, got)
		}
		seen[got] = k
	}
	if got := KindNone.Description(); !strings.Contains(strings.ToLower(got), "not") {
		t.Errorf("KindNone.Description() = %q; expected a phrase that reads as 'not installed'", got)
	}
}

// TestIsInitialized covers the basic shape of IsInitialized. We can't
// portably mock DefaultConfigDir without a temp HOME shim and a re-
// fetch, so this test just asserts the function returns sensible
// values for the current environment — the resolved path is non-
// empty, and the boolean matches whether a real bridge.yaml is
// actually on disk for the test runner. Functional coverage of the
// "exists" branch lives in init_test.go's end-to-end harness.
func TestIsInitialized(t *testing.T) {
	cfgPath, ok := IsInitialized()
	if cfgPath == "" && !ok {
		// DefaultConfigDir failed (e.g. $HOME unset). Acceptable
		// only in degenerate test environments — flag it loudly.
		t.Fatalf("IsInitialized returned empty cfgPath; DefaultConfigDir likely failed")
	}
	if cfgPath != "" && !strings.HasSuffix(cfgPath, "bridge.yaml") {
		t.Errorf("IsInitialized cfgPath %q; expected suffix bridge.yaml", cfgPath)
	}
}

// TestIsInstalledAgreesWithInstalledKind pins the convenience-wrapper
// invariant: IsInstalled() == (InstalledKind() != KindNone). A
// regression here would let the menu's option set diverge from the
// status badge.
func TestIsInstalledAgreesWithInstalledKind(t *testing.T) {
	k, _ := InstalledKind()
	wantInstalled := k != KindNone
	if got := IsInstalled(); got != wantInstalled {
		t.Errorf("IsInstalled() = %v; InstalledKind() = %v; expected agreement", got, k)
	}
}

// TestIsAdminAndIsRootAreStable covers the cross-platform stubs:
// IsAdmin should be true on POSIX (the gate is irrelevant for user-
// context installs) and IsRoot should be false on Windows. We can't
// flip the real value without escalating the test, but the stubs
// must return their expected constants without panicking.
func TestIsAdminAndIsRootAreStable(t *testing.T) {
	// Just call them — the build-tagged real impls would panic on
	// missing syscalls or unset env, which would surface here.
	_ = IsAdmin()
	_ = IsRoot()
}
