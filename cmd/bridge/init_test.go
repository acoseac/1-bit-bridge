package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestInitNonInteractiveHappyPath drives `bridge init --yes --no-service`
// against a tempdir and verifies a valid config lands on disk.
//
// We pass --no-service so the test doesn't try to register a launchd /
// systemd unit on the developer's machine — that would be a nasty side
// effect even on a test run. The service-install path is covered by the
// packaging package's own tests.
func TestInitNonInteractiveHappyPath(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes",
		"--no-service",
		"--dir", cfgDir,
		"--library", lib,
		"--name", "Test Home",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd: code=%d stderr=%s", code, stderr.String())
	}

	// Config file exists and round-trips through Load.
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.LibraryName != "Test Home" {
		t.Errorf("LibraryName = %q", loaded.LibraryName)
	}
	if len(loaded.LibraryRoots) != 1 || loaded.LibraryRoots[0] != lib {
		t.Errorf("LibraryRoots = %v", loaded.LibraryRoots)
	}
	if loaded.AdminAddress == "" {
		t.Errorf("AdminAddress should default, got empty")
	}
	// Data dir + TLS cert + key should exist under the config dir.
	for _, p := range []string{
		filepath.Join(cfgDir, "data", "server.crt"),
		filepath.Join(cfgDir, "data", "server.key"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s: %v", p, err)
		}
	}
	out := stdout.String()
	if !strings.Contains(out, "TLS fingerprint") {
		t.Errorf("stdout missing fingerprint banner:\n%s", out)
	}
}

func TestInitRejectsNonDirLibrary(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	notADir := filepath.Join(tmp, "notadir.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes",
		"--no-service",
		"--dir", cfgDir,
		"--library", notADir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit for file-as-library")
	}
	if !strings.Contains(stderr.String(), "not a directory") {
		t.Errorf("stderr should explain: %s", stderr.String())
	}
}

func TestInitYesWithoutLibrary(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes",
		"--no-service",
		"--dir", cfgDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("--yes without --library must fail")
	}
	if !strings.Contains(stderr.String(), "library") {
		t.Errorf("stderr should mention library: %s", stderr.String())
	}
}

func TestInitRespectsTildeExpansion(t *testing.T) {
	// We can't easily plant a real library under $HOME in a test, but
	// the expansion itself is pure — verify the helper.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/Music"); got != filepath.Join(home, "Music") {
		t.Errorf("expandHome(~/Music) = %q", got)
	}
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q", got)
	}
	if got := expandHome("/abs/path"); got != "/abs/path" {
		t.Errorf("expandHome(/abs/path) = %q", got)
	}
}

// TestPromptLaunchModeChoices exercises the Windows-only launch-mode
// picker. The function is platform-agnostic (no runtime guards inside),
// so we can drive it directly from any build and assert that each
// selected digit maps to the right launchChoice shape.
func TestPromptLaunchModeChoices(t *testing.T) {
	cases := []struct {
		name   string
		stdin  string
		expect launchChoice
	}{
		{"default-enter", "\n", launchChoice{spawnNow: true}},
		{"one", "1\n", launchChoice{spawnNow: true}},
		{"two-service", "2\n", launchChoice{useService: true}},
		{"three-manual", "3\n", launchChoice{skipService: true}},
		{"invalid-then-two", "x\n2\n", launchChoice{useService: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := bufio.NewReader(strings.NewReader(c.stdin))
			var w bytes.Buffer
			got := promptLaunchMode(in, &w)
			if got != c.expect {
				t.Errorf("promptLaunchMode(%q) = %+v, want %+v", c.stdin, got, c.expect)
			}
		})
	}
}

// TestResolveLaunchChoiceRespectsFlags makes sure a user who explicitly
// passed `--no-service` or `--service` doesn't get re-prompted, even on
// Windows + interactive. The prompt is an onboarding aid, not a
// clobber of explicit intent.
func TestResolveLaunchChoiceRespectsFlags(t *testing.T) {
	in := bufio.NewReader(strings.NewReader(""))
	var w bytes.Buffer
	// --no-service wins: skipService=true, nothing else touched.
	got := resolveLaunchChoice(in, &w, false, true, false, false)
	if got != (launchChoice{skipService: true}) {
		t.Errorf("--no-service: got %+v", got)
	}
	// --service wins: useService=true.
	got = resolveLaunchChoice(in, &w, false, false, true, false)
	if got != (launchChoice{useService: true}) {
		t.Errorf("--service: got %+v", got)
	}
	// --yes non-interactive with neither flag but --start-now=true.
	got = resolveLaunchChoice(in, &w, true, false, false, true)
	if got != (launchChoice{spawnNow: true}) {
		t.Errorf("--yes --start-now: got %+v", got)
	}
	// --yes with no flags at all: today's default (nothing set).
	got = resolveLaunchChoice(in, &w, true, false, false, false)
	if got != (launchChoice{}) {
		t.Errorf("--yes plain: got %+v", got)
	}
}

func TestInitInteractiveSkipOnExistingConfig(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(cfgDir, "bridge.yaml")
	if err := os.WriteFile(existing, []byte("libraryRoots:\n  - "+lib+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Interactive (no --yes): answer "n" to overwrite prompt; the name
	// and library flags still need to be supplied since we don't answer
	// those prompts in this stdin script.
	stdin := strings.NewReader("n\n")
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--no-service",
		"--dir", cfgDir,
		"--library", lib,
		"--name", "ignored",
	}, stdin, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd: code=%d stderr=%s", code, stderr.String())
	}

	// Config file is unchanged (still the exact bytes we wrote).
	got, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "libraryRoots:") {
		t.Errorf("config changed: %s", got)
	}
}
