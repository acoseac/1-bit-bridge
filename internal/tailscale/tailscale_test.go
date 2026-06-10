package tailscale

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeCmd builds an exec.Cmd that runs `/bin/sh -c <script>` so tests
// can drive specific stdout/stderr/exit-code outputs without needing a
// real `tailscale` binary on the host. The script's exit status maps
// directly to the *exec.Cmd Run() return — non-zero exits produce
// *exec.ExitError, which is what classifyMintError pattern-matches.
//
// On Windows /bin/sh isn't available; tests using fakeCmd get skipped
// there. The bridge ships on Windows but the unit tests don't have to
// run identically on every platform.
func fakeCmd(t *testing.T, stdout, stderr string, exitCode int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	t.Helper()
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// `printf` for binary safety; a final `exit N` controls the
		// process status. Stderr is routed via 1>&2.
		script := ""
		if stdout != "" {
			script += "printf '%s' " + shellQuote(stdout) + ";"
		}
		if stderr != "" {
			script += "printf '%s' " + shellQuote(stderr) + " 1>&2;"
		}
		if exitCode != 0 {
			script += "exit " + itoa(exitCode)
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func skipOnNoSh(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("test uses /bin/sh which isn't available on this platform")
	}
}

// --- classifyMintError ---

func TestClassifyMintError_HTTPSCertsDisabled(t *testing.T) {
	// Real `tailscale cert` stderr when the tailnet hasn't enabled
	// HTTPS Certificates. Match must be case-insensitive +
	// substring-tolerant so a Tailscale CLI rewording doesn't
	// silently demote the error to "generic".
	stderr := "HTTPS is not enabled on this tailnet. Enable it at https://login.tailscale.com/admin/dns\n"
	got := classifyMintError(errors.New("exit status 1"), stderr)
	if !errors.Is(got, ErrHTTPSCertsDisabled) {
		t.Errorf("classifyMintError(...) = %v, want ErrHTTPSCertsDisabled", got)
	}
}

func TestClassifyMintError_PermissionDeniedOnDaemonSocket(t *testing.T) {
	// Real `tailscale cert` stderr when the running user can't talk
	// to the local tailscaled socket. The permission-denied keyword
	// alone is NOT enough — must co-occur with daemon-socket phrasing
	// so generic fs write errors don't get misclassified.
	stderr := "failed to dial tailscaled: dial unix /var/run/tailscale/tailscaled.sock: connect: permission denied\n"
	got := classifyMintError(errors.New("exit status 1"), stderr)
	if !errors.Is(got, ErrPermission) {
		t.Errorf("classifyMintError(daemon-socket permission denied) = %v, want ErrPermission", got)
	}
}

func TestClassifyMintError_FilesystemPermissionFallsThrough(t *testing.T) {
	// CodeRabbit caught this on PR #102 round 1: a blanket
	// "permission denied" match would map fs write errors (dataDir
	// not writable, --cert-file path not writable) to ErrPermission,
	// causing the admin tile to wrongly tell the operator to join
	// the tailscale group. Narrowed match → fs errors fall through
	// to the verbatim-stderr path so the operator sees the real
	// problem (and the actual broken path).
	stderr := "open /etc/bridge/tls/tailscale.crt: permission denied\n"
	got := classifyMintError(errors.New("exit status 1"), stderr)
	if errors.Is(got, ErrPermission) {
		t.Errorf("classifyMintError(fs-write permission denied) = %v, want unmatched generic (NOT ErrPermission)", got)
	}
	if !strings.Contains(got.Error(), "tailscale.crt") {
		t.Errorf("classifyMintError(...) error = %q, want verbatim stderr fragment to surface", got.Error())
	}
}

func TestClassifyMintError_GenericPassesThrough(t *testing.T) {
	// Unmatched stderr falls through to a wrapped generic error so
	// the admin tile still surfaces the verbatim message.
	stderr := "rate limited; retry in 1h\n"
	got := classifyMintError(errors.New("exit status 1"), stderr)
	if errors.Is(got, ErrHTTPSCertsDisabled) || errors.Is(got, ErrPermission) {
		t.Errorf("classifyMintError(...) = %v, want unmatched generic", got)
	}
	if !strings.Contains(got.Error(), "rate limited") {
		t.Errorf("classifyMintError(...) error = %q, want stderr fragment to surface", got.Error())
	}
}

// --- MintCert: context cancellation ---

// Pre-fix MintCert passed cmd.Run()'s error straight to classifyMintError
// which fell through to the `default:` "tailscale cert: <err>" branch
// when stderr was empty — surfacing a fake cert failure for a
// deliberate shutdown signal. The fix checks ctx.Err() first so callers
// can `errors.Is(err, context.Canceled)` to distinguish the two.
func TestMintCert_ContextCancellationReturnsTypedError(t *testing.T) {
	skipOnNoSh(t)

	// Stub binary that sleeps long enough for the context to cancel.
	// `tailscale cert` doesn't honour signals identically across
	// platforms, but Run() returns when the process exits regardless,
	// and our test only needs cmd.Run() to come back with a
	// context-aware error.
	dir := t.TempDir()
	stub := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 10\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Cancel the context before MintCert is called so cmd.Run() returns
	// promptly. exec.CommandContext kills the process when the context
	// cancels; the resulting error wraps signal.Killed (or similar).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	err := MintCert(ctx, stub, "magic.example.ts.net", certPath, keyPath)
	if err == nil {
		t.Fatal("MintCert: expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("MintCert: error = %v, want errors.Is(err, context.Canceled) — pre-fix the cancel masquerades as a generic cert failure", err)
	}
	// Belt-and-braces: the typed sentinels must NOT match — a cancel
	// is not a permission / HTTPS-disabled error.
	if errors.Is(err, ErrHTTPSCertsDisabled) || errors.Is(err, ErrPermission) {
		t.Errorf("MintCert: error = %v, must not match typed cert-failure sentinels", err)
	}
}

// --- Detect ---

func TestDetect_ParsesMagicDNSAndSuffix(t *testing.T) {
	skipOnNoSh(t)
	// Minimal `tailscale status --json` shape — only the fields Detect reads.
	stdout := `{
		"Self": {"HostName": "home-pc", "DNSName": "home-pc.sable-eagle.ts.net."},
		"MagicDNSSuffix": "sable-eagle.ts.net"
	}`
	orig := commandContext
	commandContext = fakeCmd(t, stdout, "", 0)
	t.Cleanup(func() { commandContext = orig })

	// Detect's binary-resolution path expects an actual binary on
	// $PATH or the macOS App Store fallback. To exercise the
	// JSON-parsing code path without needing a real `tailscale` we
	// shadow resolveBinary by writing a script and prepending its dir
	// to PATH. Cheap; isolates the test from system state.
	dir := t.TempDir()
	stub := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	info, err := Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !info.CLIAvailable {
		t.Errorf("CLIAvailable = false, want true")
	}
	if info.NodeName != "home-pc" {
		t.Errorf("NodeName = %q, want %q", info.NodeName, "home-pc")
	}
	if info.MagicDNSName != "home-pc.sable-eagle.ts.net" {
		t.Errorf("MagicDNSName = %q (note trailing-dot must be trimmed)", info.MagicDNSName)
	}
	if info.TailnetSuffix != "sable-eagle.ts.net" {
		t.Errorf("TailnetSuffix = %q, want %q", info.TailnetSuffix, "sable-eagle.ts.net")
	}
}

func TestDetect_MissingCLIReturnsZeroValueNoError(t *testing.T) {
	// Simulate "no tailscale on $PATH" by pointing PATH at an empty dir.
	// On macOS the App Store fallback at /Applications/Tailscale.app
	// might still exist on the runner; the test only asserts
	// CLIAvailable on the negative branch when BOTH lookups miss, so
	// we skip on macOS hosts that have the app installed.
	if _, err := os.Stat(macAppStoreBinary); err == nil {
		t.Skip("Mac App Store Tailscale install present — can't simulate clean miss without it being found")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	info, err := Detect(context.Background())
	if err != nil {
		t.Errorf("Detect on missing CLI returned err = %v, want nil (missing CLI is a soft state, not an error)", err)
	}
	if info.CLIAvailable {
		t.Errorf("CLIAvailable = true, want false")
	}
}

func TestDetect_MagicDNSDisabledReportsLastError(t *testing.T) {
	skipOnNoSh(t)
	// Tailnet without MagicDNS — Self.DNSName empty, MagicDNSSuffix empty.
	stdout := `{"Self": {"HostName": "home-pc", "DNSName": ""}, "MagicDNSSuffix": ""}`
	orig := commandContext
	commandContext = fakeCmd(t, stdout, "", 0)
	t.Cleanup(func() { commandContext = orig })

	dir := t.TempDir()
	stub := filepath.Join(dir, "tailscale")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	info, err := Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !info.CLIAvailable {
		t.Error("CLIAvailable = false, want true (CLI ran successfully)")
	}
	if info.MagicDNSName != "" {
		t.Errorf("MagicDNSName = %q, want empty (MagicDNS is disabled)", info.MagicDNSName)
	}
	if info.LastError == "" {
		t.Error("LastError empty, want a human-readable hint about MagicDNS being disabled")
	}
}

// --- LECertPaths / EnsureCertDir ---

func TestLECertPaths_FixedFilenameNotMagicDNSKeyed(t *testing.T) {
	// Plan-decision invariant: filenames are FIXED (`tailscale.crt` /
	// `tailscale.key`), NOT keyed on the MagicDNS hostname. A tailnet
	// or host rename would otherwise leave orphan files in dataDir
	// (e.g. `old-name.sable-eagle.ts.net.crt` lingering after rename).
	cert, key := LECertPaths("/data")
	if !strings.HasSuffix(cert, "tailscale.crt") {
		t.Errorf("cert = %q, want suffix tailscale.crt", cert)
	}
	if !strings.HasSuffix(key, "tailscale.key") {
		t.Errorf("key = %q, want suffix tailscale.key", key)
	}
	if !strings.Contains(cert, "tls") {
		t.Errorf("cert = %q, want path under /tls/", cert)
	}
}

func TestEnsureCertDir_CreatesIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureCertDir(dir); err != nil {
		t.Fatal(err)
	}
	// Idempotent — second call returns nil even though the dir exists.
	if err := EnsureCertDir(dir); err != nil {
		t.Errorf("second EnsureCertDir = %v, want nil (idempotent)", err)
	}
	info, err := os.Stat(filepath.Join(dir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Errorf("tls path = %v, want a directory", info.Mode())
	}
}

// TestTrimErr_TruncatesAtUTF8Boundary pins the rune-boundary trim on
// the 240-byte cap: a multi-byte rune straddling the cut must be
// dropped entirely, never surfaced to the admin tile's JSON body as a
// half-encoded sequence (same invariant auth.RecordClientVersion
// carries, PR #75).
func TestTrimErr_TruncatesAtUTF8Boundary(t *testing.T) {
	// 239 ASCII bytes + a 3-byte rune whose bytes occupy 239..241 —
	// the cut at 240 lands mid-rune.
	in := strings.Repeat("a", 239) + "世" + strings.Repeat("b", 50)
	got := trimErr(in, nil)
	if !utf8.ValidString(got) {
		t.Errorf("trimmed output is not valid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", 239) + "…"; got != want {
		t.Errorf("expected the straddling rune dropped to the boundary; got len %d: %q", len(got), got[230:])
	}
}

// TestTrimErr_ASCIIBoundaryUnchanged is the regression guard for the
// UTF-8-safe path: pure-ASCII input must truncate at exactly the cap,
// not over-trim.
func TestTrimErr_ASCIIBoundaryUnchanged(t *testing.T) {
	in := strings.Repeat("a", 300)
	got := trimErr(in, nil)
	if want := strings.Repeat("a", 240) + "…"; got != want {
		t.Errorf("ASCII truncation changed: len %d, want %d", len(got), len(want))
	}
}
