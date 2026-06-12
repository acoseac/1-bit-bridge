package doctor

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestPlatformCheck — we only ship darwin/linux/windows × amd64/arm64.
// The host we run on should always match.
func TestPlatformCheck(t *testing.T) {
	c := checkPlatform(Deps{})
	if c.Status != OK {
		t.Errorf("platform %s/%s should be ok, got %v: %s",
			runtime.GOOS, runtime.GOARCH, c.Status, c.Summary)
	}
}

func TestConfigDirCheck_Writable(t *testing.T) {
	dir := t.TempDir()
	c := checkConfigDir(Deps{ConfigDir: dir})
	if c.Status != OK {
		t.Errorf("writable temp dir: %v %s", c.Status, c.Hint)
	}
}

func TestConfigDirCheck_ReadOnlyFails(t *testing.T) {
	// Create a dir, then chmod it so WriteFile probe fails. Skip on
	// Windows since chmod 0o500 doesn't block writes there (ACLs, not
	// mode bits).
	if runtime.GOOS == "windows" {
		t.Skip("mode bits don't gate writes on Windows")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "readonly")
	if err := os.MkdirAll(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	c := checkConfigDir(Deps{ConfigDir: ro})
	if c.Status != Fail {
		t.Errorf("read-only dir should fail, got %v", c.Status)
	}
}

func TestTLSCertCheck_Absent(t *testing.T) {
	// Fresh DataDir — no cert exists yet. That's ok (init mints).
	c := checkTLSCert(Deps{DataDir: t.TempDir()})
	if c.Status != OK {
		t.Errorf("absent cert should be ok (init mints), got %v %s", c.Status, c.Summary)
	}
}

func TestTLSCertCheck_PartialStateFails(t *testing.T) {
	// A cert without its key is a load-bearing error state — init
	// can't regenerate safely because doing so would rotate the cert
	// and break every pinned client.
	//
	// servertls.DefaultPaths() returns (<dataDir>/server.crt,
	// <dataDir>/server.key), so we write only the .crt and expect
	// fail.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.crt"), []byte("stub"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := checkTLSCert(Deps{DataDir: dir})
	if c.Status != Fail {
		t.Errorf("partial cert state should fail, got %v %s", c.Status, c.Summary)
	}
}

func TestPortCheck_FreePasses(t *testing.T) {
	// Find a free port, tell doctor about it, expect ok.
	port := mustFreePort(t)
	c := checkPort("port-test", port, "")
	if c.Status != OK {
		t.Errorf("free port %d: %v %s", port, c.Status, c.Summary)
	}
}

func TestPortCheck_BusyFailsWithoutOwnPID(t *testing.T) {
	// Bind the port from the test, then ask doctor about it. With no
	// OwnPIDFile the binder is "someone else" and doctor fails.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	addr := lis.Addr().(*net.TCPAddr)
	c := checkPort("port-test", addr.Port, "")
	if c.Status != Fail {
		t.Errorf("bound port %d without own-pidfile: got %v, want fail", addr.Port, c.Status)
	}
}

func TestPortCheck_OwnPIDMatches(t *testing.T) {
	// Bind from this process, write our own pid to a pidfile, and
	// assert doctor treats the bind as "us" → ok. lsof needs to see
	// our PID as the listener, which works on darwin/linux.
	if runtime.GOOS == "windows" {
		t.Skip("pidListening uses lsof — not available on Windows (PR-2 wires WMI)")
	}
	// pidListening shells out to lsof; on a minimal CI host without it,
	// the helper returns -1, checkPort falls to Fail, and this test would
	// fail deterministically. Skip rather than flake.
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("pidListening requires lsof, which isn't on PATH here")
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	addr := lis.Addr().(*net.TCPAddr)

	pidFile := filepath.Join(t.TempDir(), "server.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	c := checkPort("port-test", addr.Port, pidFile)
	if c.Status != OK {
		t.Errorf("own-pid bind should be ok, got %v %s (hint: %s)", c.Status, c.Summary, c.Hint)
	}
}

func TestLibraryRootsCheck_MissingFails(t *testing.T) {
	c := checkLibraryRoots(Deps{LibraryRoots: []string{"/does/not/exist"}})
	if c.Status != Fail {
		t.Errorf("missing root should fail, got %v", c.Status)
	}
}

func TestLibraryRootsCheck_EmptyWarns(t *testing.T) {
	dir := t.TempDir() // freshly-empty
	c := checkLibraryRoots(Deps{LibraryRoots: []string{dir}})
	if c.Status != Warn {
		t.Errorf("empty root should warn, got %v", c.Status)
	}
	if !strings.Contains(c.Hint, dir) {
		t.Errorf("hint should name the empty dir, got %q", c.Hint)
	}
}

func TestLibraryRootsCheck_PopulatedOK(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "track.flac"), []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkLibraryRoots(Deps{LibraryRoots: []string{dir}})
	if c.Status != OK {
		t.Errorf("populated root should be ok, got %v: %s", c.Status, c.Hint)
	}
}

func TestLibraryRootsCheck_SkipsWhenEmpty(t *testing.T) {
	// No roots configured → ok (init will prompt). This matters for
	// first-run: `bridge doctor` is useful even before init.
	c := checkLibraryRoots(Deps{})
	if c.Status != OK {
		t.Errorf("no roots should be ok, got %v", c.Status)
	}
}

// TestRun_FullReportShape exercises the whole pipeline and asserts the
// report carries one entry per check with the right names.
func TestRun_FullReportShape(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "music")
	_ = os.MkdirAll(lib, 0o755)
	_ = os.WriteFile(filepath.Join(lib, "a.flac"), []byte{0}, 0o644)

	r := Run(Deps{
		ConfigDir:    filepath.Join(dir, "cfg"),
		DataDir:      filepath.Join(dir, "data"),
		LibraryRoots: []string{lib},
		APIPort:      mustFreePort(t),
		AdminPort:    mustFreePort(t),
	})

	wantNames := []string{
		"platform", "config-dir", "tls-cert",
		"port-api", "port-admin",
		"library-roots", "service-manager", "browser-opener",
		"inotify-watch-limit",
	}
	if len(r.Checks) != len(wantNames) {
		t.Fatalf("check count: got %d, want %d", len(r.Checks), len(wantNames))
	}
	for i, want := range wantNames {
		if r.Checks[i].Name != want {
			t.Errorf("check[%d].Name = %q, want %q", i, r.Checks[i].Name, want)
		}
	}
}

// mustFreePort grabs, then immediately releases, an ephemeral TCP
// port — racy with the follow-up bind but close enough for a test
// (we control all the binders here).
func mustFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}
