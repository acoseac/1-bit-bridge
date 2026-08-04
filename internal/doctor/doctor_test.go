package doctor

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
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
	// OwnPIDFile the binder is "someone else" and — when the owner probe
	// IS available (forced here so the verdict doesn't depend on whether
	// lsof happens to be installed on this host) — doctor fails.
	withPortProbe(t, true)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	addr := lis.Addr().(*net.TCPAddr)
	c := checkPort("port-test", addr.Port, "")
	if c.Status != Fail {
		t.Errorf("bound port %d without own-pidfile (probe available): got %v, want fail", addr.Port, c.Status)
	}
}

// withPortProbe forces portProbeAvailable for the duration of a test so
// checkPort's probe-available vs probe-unavailable branches can be
// asserted deterministically regardless of whether lsof is installed.
func withPortProbe(t *testing.T, available bool) {
	t.Helper()
	orig := portProbeAvailable
	t.Cleanup(func() { portProbeAvailable = orig })
	portProbeAvailable = func() bool { return available }
}

// TestPortCheck_BusyProbeUnavailableWarns pins F9: a bound port the owner
// probe can't attribute (no lsof / Windows) degrades to Warn rather than a
// hard Fail, so `bridge doctor` on a live install doesn't cry wolf about
// the bridge's own port.
func TestPortCheck_BusyProbeUnavailableWarns(t *testing.T) {
	withPortProbe(t, false)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	addr := lis.Addr().(*net.TCPAddr)
	c := checkPort("port-test", addr.Port, "")
	if c.Status != Warn {
		t.Errorf("bound port %d with probe unavailable: got %v, want warn", addr.Port, c.Status)
	}
}

// TestPortCheck_NonAddrInUseErrorWarns pins B51: a bind failure that is NOT
// EADDRINUSE — EACCES (a port <1024 without elevation), EADDRNOTAVAIL, a
// transient network error — must degrade to Warn, not the hard "another
// process owns this port" Fail that would wrongly block `bridge init`. The
// listenFunc seam injects the error deterministically (a real EACCES would
// need a privileged port, and root-vs-non-root would flip the outcome).
func TestPortCheck_NonAddrInUseErrorWarns(t *testing.T) {
	orig := listenFunc
	t.Cleanup(func() { listenFunc = orig })
	listenFunc = func(network, address string) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EACCES)}
	}
	c := checkPort("port-test", 443, "")
	if c.Status != Warn {
		t.Errorf("non-EADDRINUSE bind error: got %q (%s / %s), want warn", c.Status, c.Summary, c.Hint)
	}
}

// TestPortCheck_AddrInUseStillReachesOwnerProbe pins the other side of the
// TestIsAddrInUseMatchesRealBindConflict is the platform-anchored regression
// test for the dead-gate bug: it derives the error from the OS by actually
// double-binding a port, instead of asserting against a constant.
//
// That distinction is the whole point. The sibling test below injects
// `syscall.EADDRINUSE`, which on Windows is an INVENTED value
// (APPLICATION_ERROR + iota) that a real bind conflict never carries — so it
// passes there whether or not the classifier is correct. This one cannot:
// pre-fix, `errors.Is(err, syscall.EADDRINUSE)` was always false on Windows,
// so a genuine conflict fell through to the not-bindable Warn and the native
// owner-attribution probe became unreachable. Runs on every platform with no
// skip, so CI covers unix and `GOOS=windows go test` covers the rest.
func TestIsAddrInUseMatchesRealBindConflict(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	conflict, err := net.Listen("tcp", lis.Addr().String())
	if err == nil {
		conflict.Close()
		t.Fatal("second bind on an occupied port unexpectedly succeeded")
	}
	if !isAddrInUse(err) {
		t.Fatalf("isAddrInUse(%#v) = false for a REAL bind conflict on %s; "+
			"a genuine port conflict must classify as in-use", err, runtime.GOOS)
	}
	// And it must not over-match: an unrelated error is not a port conflict,
	// or every EACCES on a privileged port would become a hard Fail.
	if isAddrInUse(errors.New("some unrelated failure")) {
		t.Error("isAddrInUse matched an unrelated error")
	}
}

// B51 branch: an EADDRINUSE bind error is still classified as "port in use"
// and routed through the owner-probe path — with the probe forced available
// and no OwnPIDFile, that stays the pre-existing Fail (not the new
// not-bindable Warn).
func TestPortCheck_AddrInUseStillReachesOwnerProbe(t *testing.T) {
	withPortProbe(t, true)
	orig := listenFunc
	t.Cleanup(func() { listenFunc = orig })
	listenFunc = func(network, address string) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}
	c := checkPort("port-test", 7788, "")
	if c.Status != Fail {
		t.Errorf("EADDRINUSE (probe available, no ownPID): got %q (%s), want fail", c.Status, c.Summary)
	}
}

func TestPortCheck_OwnPIDMatches(t *testing.T) {
	// Bind from this process, write our own pid to a pidfile, and
	// assert doctor treats the bind as "us" → ok. lsof needs to see
	// our PID as the listener, which works on darwin/linux.
	if runtime.GOOS == "windows" {
		t.Skip("isPIDListeningOnPort uses lsof here — Windows has its own native probe + doctor_windows_test.go")
	}
	// isPIDListeningOnPort shells out to lsof; on a minimal CI host without
	// it, the predicate returns (false, nil), checkPort falls to Fail, and
	// this test would fail deterministically. Skip rather than flake.
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

// TestCheckAudioToolchain_NotEnabled pins the no-nag contract: with neither
// upscale nor analysis enabled (the default + every `bridge init` preflight),
// the check is a quiet OK and never probes for sox. This is the load-bearing
// guarantee that a minimal install isn't told to install an optional dep.
func TestCheckAudioToolchain_NotEnabled(t *testing.T) {
	c := checkAudioToolchain(Deps{})
	if c.Status != OK {
		t.Errorf("disabled features should be ok, got %v: %s", c.Status, c.Summary)
	}
	if c.Name != checkNameAudioToolchain {
		t.Errorf("name = %q, want %q", c.Name, checkNameAudioToolchain)
	}
	if !strings.Contains(c.Summary, "not enabled") {
		t.Errorf("summary should say not enabled, got %q", c.Summary)
	}
}

// TestCheckAudioToolchain_EnabledReflectsHostSox is tolerant of the host: it
// only asserts that enabling a feature flips the check away from the
// "not enabled" no-op into a real verdict against the host's sox. The
// parse/probe logic itself is pinned deterministically in
// internal/transcode (probe_sox_test.go).
func TestCheckAudioToolchain_EnabledReflectsHostSox(t *testing.T) {
	c := checkAudioToolchain(Deps{UpscaleEnabled: true})
	if strings.Contains(c.Summary, "not enabled") {
		t.Errorf("enabled feature must produce a real verdict, got no-op: %q", c.Summary)
	}
	if _, err := exec.LookPath("sox"); err != nil {
		if c.Status != Fail || !strings.Contains(c.Summary, "not found") {
			t.Errorf("no sox on PATH: want fail/'not found', got %v: %s", c.Status, c.Summary)
		}
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
		"inotify-watch-limit", "audio-toolchain", "fingerprint-toolchain",
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

// TestCheckFingerprintToolchain covers the three states an operator can be in.
//
// The middle one is the reason this check exists: fpcalc present but no
// AcoustID key is a SILENT failure. The feature degrades to off at startup
// with one stderr line that scrolls away, so without a doctor entry the only
// symptom is that fingerprinting never resolves anything — with nothing
// anywhere saying why.
func TestCheckFingerprintToolchain(t *testing.T) {
	t.Run("disabled is a no-op", func(t *testing.T) {
		c := checkFingerprintToolchain(Deps{FingerprintEnabled: false})
		if c.Status != OK {
			t.Fatalf("status = %v, want OK — a host that will never fingerprint must not be nagged", c.Status)
		}
		if !strings.Contains(c.Summary, "not enabled") {
			t.Errorf("summary = %q", c.Summary)
		}
	})

	t.Run("enabled without a key fails with a pointer to the fix", func(t *testing.T) {
		if _, err := acoustid.Probe(context.Background()); err != nil {
			t.Skip("fpcalc not installed; this case needs a working binary to reach the key check")
		}
		c := checkFingerprintToolchain(Deps{FingerprintEnabled: true, FingerprintHasAPIKey: false})
		if c.Status != Fail {
			t.Fatalf("status = %v, want Fail", c.Status)
		}
		if !strings.Contains(c.Hint, "acoustid.org/new-application") {
			t.Errorf("hint must say where to get a key, got %q", c.Hint)
		}
		// The report gets pasted into issues; it must never carry the key.
		if strings.Contains(c.Summary+c.Hint, "ACOUSTID_API_KEY=") {
			t.Error("the check must not echo a key value")
		}
	})

	t.Run("enabled with everything present passes", func(t *testing.T) {
		if _, err := acoustid.Probe(context.Background()); err != nil {
			t.Skip("fpcalc not installed")
		}
		c := checkFingerprintToolchain(Deps{FingerprintEnabled: true, FingerprintHasAPIKey: true})
		if c.Status != OK {
			t.Fatalf("status = %v, want OK: %s / %s", c.Status, c.Summary, c.Hint)
		}
	})
}

// A port held on IPv6 ONLY must not be reported free. Probing just
// 127.0.0.1 missed `[::]:port` under bindv6only and an explicit
// `[::1]:port` — and the bridge's own default listen address is a
// wildcard, so this is the ordinary case, not an exotic one. doctor said
// "free", init proceeded, and serve then failed to bind.
func TestCheckPortDetectsIPv6OnlyBinding(t *testing.T) {
	prev := listenFunc
	t.Cleanup(func() { listenFunc = prev })
	listenFunc = func(network, addr string) (net.Listener, error) {
		if strings.HasPrefix(addr, "[") { // the IPv6 probe
			return nil, &net.OpError{
				Op: "listen", Net: network,
				Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE},
			}
		}
		return prev(network, "127.0.0.1:0") // IPv4 is free
	}

	c := checkPort("api", 7788, "")
	if c.Status == OK {
		t.Fatalf("checkPort = %s (%q), want not-OK — the port is held on "+
			"IPv6 and binding only 127.0.0.1 cannot see it", c.Status, c.Summary)
	}
}

// The mirror case: a host with no IPv6 returns EADDRNOTAVAIL for ::1,
// which is an environment fact and must not read as a conflict.
func TestCheckPortIPv6UnavailableIsStillFree(t *testing.T) {
	prev := listenFunc
	t.Cleanup(func() { listenFunc = prev })
	listenFunc = func(network, addr string) (net.Listener, error) {
		if strings.HasPrefix(addr, "[") {
			return nil, &net.OpError{
				Op: "listen", Net: network,
				Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRNOTAVAIL},
			}
		}
		return prev(network, "127.0.0.1:0")
	}

	c := checkPort("api", 7788, "")
	if c.Status != OK {
		t.Errorf("checkPort = %s (%q), want OK — a v4-only host must not "+
			"report its free port as a problem", c.Status, c.Summary)
	}
}
