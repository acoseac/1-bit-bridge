package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSignOutEverywhereRefusesAnInstallWithNoConsole: the sign-out is written
// beside the admin credential, and a loopback install has none, and no
// console session either. The command says so, exits 1, and writes nothing,
// so it does not leave a credential file behind on an install that has never
// had one. A positional argument is refused before anything is read, as
// every admin subcommand refuses one.
func TestSignOutEverywhereRefusesAnInstallWithNoConsole(t *testing.T) {
	cfgDir := t.TempDir()
	if code, out := loopbackInit(t, cfgDir, testLibrary(t), "Loopback"); code != 0 {
		t.Fatalf("bridge init = %d:\n%s", code, out)
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	storePath := filepath.Join(cfgDir, "data", "adminauth.json")

	var out, errOut strings.Builder
	code := adminCmd([]string{"sign-out-everywhere", "--config", cfgPath}, strings.NewReader(""), &out, &errOut)
	if code != 1 || !strings.Contains(errOut.String(), "no admin credentials at "+storePath) {
		t.Errorf("sign-out-everywhere on a loopback install = %d, stderr %q; want 1 naming %s", code, errOut.String(), storePath)
	}
	if _, err := os.Stat(storePath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("sign-out-everywhere on a loopback install left a credential file (stat err=%v)", err)
	}

	out.Reset()
	errOut.Reset()
	if code := adminCmd([]string{"sign-out-everywhere", "--config", cfgPath, "admin"}, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Errorf("sign-out-everywhere with a positional argument = %d, want 2", code)
	}
}

// TestAdminUsageNamesTheSignOuts: `bridge admin` with no subcommand is where
// an operator looks for what the family does, and both ways to sign consoles
// out, with the flag that keeps them, are listed there.
func TestAdminUsageNamesTheSignOuts(t *testing.T) {
	var out, errOut strings.Builder
	if code := adminCmd(nil, strings.NewReader(""), &out, &errOut); code != 2 {
		t.Fatalf("bridge admin = %d, want 2", code)
	}
	for _, want := range []string{"reset-password", "--keep-sessions", "sign-out-everywhere", "login-link"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("bridge admin's usage does not name %q:\n%s", want, errOut.String())
		}
	}
}
