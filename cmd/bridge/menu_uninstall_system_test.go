package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// TestUninstallDoesNotWipeUnderALiveService is the second half of the
// system-install fix, and the half with the teeth.
//
// packaging.Uninstall used to no-op on a sudo install and return nil:
// the two POSIX uninstallers touch only the fixed USER-level path and
// treat a missing file as success. So the menu printed "service
// uninstalled." while the LaunchDaemon stayed registered and running —
// and then offered os.RemoveAll(cfgDir), which is the certs, the tokens
// and the database, out from under a live bridge.
//
// Uninstall now refuses; this pins that the menu acts on the refusal
// rather than carrying on to the wipe. Asserting on the FILES, not on
// the text: a prompt that is worded correctly and deletes anyway is the
// failure this is about.
func TestUninstallDoesNotWipeUnderALiveService(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte("libraryRoots: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Something that must survive: the token store is the file whose
	// loss costs the operator every paired device.
	tokens := filepath.Join(dir, "tokens.json")
	if err := os.WriteFile(tokens, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := uninstallService
	uninstallService = func() (string, error) { return "", packaging.ErrSystemInstallNeedsRoot }
	t.Cleanup(func() { uninstallService = orig })

	var stdout, stderr bytes.Buffer
	// "y" to the uninstall question, then the exact WIPE phrase queued
	// behind it.
	//
	// The second line is what makes this test load-bearing, and the
	// first draft omitted it: with only "y", a regression that offers
	// the wipe anyway reads "" at the phrase prompt, the exact-match
	// check refuses, nothing is deleted, and the file assertions pass
	// while the guard is gone. The negative control caught that — it
	// passed with the guard removed. Queued, the line is consumed ONLY
	// if the wipe is offered, so the files disappear exactly when the
	// guard is missing.
	in := bufio.NewReader(strings.NewReader("y\nWIPE\n"))
	state := menuState{initialized: true, cfgPath: cfgPath, kind: packaging.KindLaunchdSystem}

	actUninstall(context.Background(), in, &stdout, &stderr, state)

	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config survived? %v", err)
	}
	if _, err := os.Stat(tokens); err != nil {
		t.Errorf("the token store was removed under a service that is still running: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the config dir was wiped under a service that is still running: %v", err)
	}

	// And the operator has to be told, or a silently-skipped step reads
	// as the menu being finished.
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "still") {
		t.Errorf("nothing told the operator the service is still installed:\n%s", out)
	}
	if strings.Contains(stdout.String(), "service uninstalled.") {
		t.Error("a refused uninstall was reported as done")
	}
}

// TestUninstallStillWipesWhenTheServiceIsGone is the positive control.
// Without it the guard above passes against a menu that never wipes at
// all, which would quietly break the command's actual purpose.
func TestUninstallStillWipesWhenTheServiceIsGone(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte("libraryRoots: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := uninstallService
	uninstallService = func() (string, error) { return "/tmp/fake.plist", nil }
	t.Cleanup(func() { uninstallService = orig })

	var stdout, stderr bytes.Buffer
	// "y" to uninstall, then the exact phrase at the wipe prompt.
	in := bufio.NewReader(strings.NewReader("y\nWIPE\n"))
	state := menuState{initialized: true, cfgPath: cfgPath, kind: packaging.KindLaunchdUser}

	actUninstall(context.Background(), in, &stdout, &stderr, state)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the wipe did not happen after a successful uninstall; stat err = %v", err)
	}
	if !strings.Contains(stdout.String(), "service uninstalled.") {
		t.Errorf("a successful uninstall was not reported:\n%s", stdout.String())
	}
}
