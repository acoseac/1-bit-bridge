// Package tailscale wraps the small surface area of the Tailscale CLI
// the bridge needs for HTTPS auto-pilot: detecting whether Tailscale is
// installed and what the local node's MagicDNS name is, and minting an
// HTTPS certificate via `tailscale cert`. Deliberately CLI-based (not
// LocalAPI) so the bridge keeps a small dependency footprint and runs
// fine on hosts without Tailscale installed at all.
//
// Test seam: `commandContext` defaults to `exec.CommandContext`. Tests
// inject a fake to drive specific stdout/stderr/exit-code shapes
// without spawning real processes. Production code MUST NOT mutate it.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("tailscale")

// commandContext is the test seam — production-default exec.CommandContext.
// Tests assign a fake that records args + returns canned output. Same
// convention as `renameFunc` in internal/manifest/extractors.go.
var commandContext = exec.CommandContext

// macAppStoreBinary is the canonical path for the Mac App Store install
// of Tailscale. The MAS sandbox doesn't put `tailscale` on $PATH, so a
// LookPath miss isn't the same as "not installed" on macOS.
const macAppStoreBinary = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// NodeInfo describes the local Tailscale node, populated by Detect.
//
// The zero value (CLIAvailable=false) is the "Tailscale not installed"
// state — every getter / consumer treats that as a soft "skip" condition,
// not an error. Tailscale not being installed is a perfectly valid state
// for hosts that pair iOS clients only over LAN.
type NodeInfo struct {
	// CLIAvailable is true when a runnable `tailscale` binary was found.
	CLIAvailable bool

	// BinaryPath is the resolved absolute path to the tailscale binary.
	// Empty when CLIAvailable is false. Stored so subsequent calls
	// (MintCert, status refreshes) can use the exact same binary the
	// detection picked, avoiding a TOCTOU race against $PATH changes
	// or the App Store install moving.
	BinaryPath string

	// NodeName is the local node's short name (e.g. "home-pc").
	// Sourced from `tailscale status --json`'s `Self.HostName`.
	NodeName string

	// MagicDNSName is the fully-qualified MagicDNS hostname
	// (e.g. "home-pc.sable-eagle.ts.net"). Empty when MagicDNS is
	// disabled in the tailnet.
	MagicDNSName string

	// TailnetSuffix is the bare tailnet suffix (e.g. "sable-eagle.ts.net"),
	// used by the SNI cert switcher to decide which connections route
	// to the LE cert vs the self-signed cert.
	TailnetSuffix string

	// LastError is the human-readable error message from the most-recent
	// failed CLI invocation, empty on success. Surfaced verbatim in the
	// admin tile.
	LastError string
}

// Detect runs `which tailscale` (with the macOS App Store fallback)
// followed by `tailscale status --json` and returns a populated NodeInfo.
//
// CLI-not-found is NOT an error — it returns NodeInfo{CLIAvailable: false}
// with nil err. A genuine error (CLI present but `status` fails, JSON
// can't be parsed, MagicDNS suffix can't be derived) returns a non-nil
// err so the caller can log at .error level.
func Detect(ctx context.Context) (NodeInfo, error) {
	binary, ok := resolveBinary()
	if !ok {
		return NodeInfo{}, nil
	}
	info := NodeInfo{CLIAvailable: true, BinaryPath: binary}

	cmd := commandContext(ctx, binary, "status", "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		info.LastError = trimErr(stderr.String(), err)
		return info, fmt.Errorf("tailscale status: %w", err)
	}

	var raw struct {
		Self struct {
			HostName string `json:"HostName"`
			DNSName  string `json:"DNSName"`
		} `json:"Self"`
		MagicDNSSuffix string `json:"MagicDNSSuffix"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		info.LastError = "tailscale status returned malformed JSON"
		return info, fmt.Errorf("decode tailscale status JSON: %w", err)
	}

	// MagicDNSSuffix arrives without leading dot ("sable-eagle.ts.net").
	// DNSName arrives WITH trailing dot ("home-pc.sable-eagle.ts.net.").
	// Normalize both.
	info.NodeName = strings.TrimSpace(raw.Self.HostName)
	info.TailnetSuffix = strings.TrimSpace(raw.MagicDNSSuffix)
	info.MagicDNSName = strings.TrimSuffix(strings.TrimSpace(raw.Self.DNSName), ".")

	if info.TailnetSuffix == "" {
		info.LastError = "MagicDNS not enabled in tailnet"
		return info, nil
	}
	if info.MagicDNSName == "" {
		// Defensive — Self.DNSName should always be set when MagicDNS
		// is on, but if it's empty we can synthesise it from
		// HostName + suffix so the rest of the pipeline still works.
		if info.NodeName != "" {
			info.MagicDNSName = info.NodeName + "." + info.TailnetSuffix
		}
	}
	return info, nil
}

// resolveBinary locates the tailscale CLI binary, preferring $PATH but
// falling back to the macOS App Store install location which doesn't
// register itself with $PATH (gotcha #2 from the plan review).
func resolveBinary() (string, bool) {
	if path, err := exec.LookPath("tailscale"); err == nil {
		return path, true
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat(macAppStoreBinary); err == nil {
			return macAppStoreBinary, true
		}
	}
	return "", false
}

// MintCert exec's `tailscale cert --cert-file=<certPath> --key-file=<keyPath> <magicDNS>`.
// Returns nil on success, a typed error on the well-known failure modes:
//
//   - ErrHTTPSCertsDisabled — the tailnet hasn't enabled the HTTPS
//     Certificates feature in the admin console. Operator must visit
//     login.tailscale.com/admin/dns and toggle it on.
//   - ErrPermission — the local tailscaled socket refuses connections
//     from the running user (Linux-specific; user needs to be in the
//     `tailscale` group or run with sudo).
//   - generic error — any other failure (network, rate-limit, etc.).
//     Log the message and surface in the admin tile.
//
// Idempotent: running it on a host that already has a fresh cert just
// re-mints (Tailscale serves a cached LE response if one is current).
// Caller is responsible for deciding whether to re-mint based on the
// existing cert's notAfter.
func MintCert(ctx context.Context, binary, magicDNS, certPath, keyPath string) error {
	if binary == "" {
		return ErrCLINotFound
	}
	if magicDNS == "" {
		return errors.New("tailscale: MintCert requires a MagicDNS name")
	}
	cmd := commandContext(ctx, binary,
		"cert",
		"--cert-file="+certPath,
		"--key-file="+keyPath,
		magicDNS,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return classifyMintError(err, stderr.String())
	}
	return nil
}

// ErrCLINotFound surfaces "tailscale binary not on $PATH and not at the
// macOS App Store fallback path". Caller-distinguishable via errors.Is.
var ErrCLINotFound = errors.New("tailscale: CLI binary not found")

// ErrHTTPSCertsDisabled is the typed error returned when MintCert
// detects the tailnet hasn't enabled HTTPS Certificates. The admin
// console renders distinct guidance for this case.
var ErrHTTPSCertsDisabled = errors.New("tailscale: HTTPS Certificates not enabled in tailnet")

// ErrPermission is the typed error returned when the local tailscaled
// socket refuses connections from the running user (Linux gotcha).
var ErrPermission = errors.New("tailscale: permission denied talking to local daemon (Linux: add the running user to the tailscale group, or run `sudo tailscale cert <magicdns>` once manually)")

// classifyMintError pattern-matches stderr to surface known failure modes
// as typed errors. Strings come from observed `tailscale cert` output;
// any unmatched failure falls through to a wrapped generic error so the
// admin tile still surfaces the verbatim message.
func classifyMintError(runErr error, stderr string) error {
	low := strings.ToLower(stderr)
	switch {
	case strings.Contains(low, "https is not enabled"),
		strings.Contains(low, "https certificates are not enabled"),
		strings.Contains(low, "enable https in the dns page"):
		return ErrHTTPSCertsDisabled
	case strings.Contains(low, "permission denied"),
		strings.Contains(low, "operation not permitted"):
		return ErrPermission
	default:
		return fmt.Errorf("tailscale cert: %w (%s)", runErr, trimErr(stderr, runErr))
	}
}

// trimErr produces a short single-line message from stderr (or the run
// error) suitable for surfacing in the admin tile's LastError field.
// Bounded length so a runaway `tailscale` log doesn't explode the JSON
// body.
func trimErr(stderr string, runErr error) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		if runErr != nil {
			s = runErr.Error()
		}
	}
	// Single line — admin tile renders inline.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxLen = 240
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// LECertPaths returns the canonical on-disk paths for the LE cert + key.
// Fixed filenames (NOT magicDNS-keyed) so a tailnet/host rename or
// reinstallation doesn't leave orphan files in dataDir; matches the
// `server.crt` / `server.key` convention. Lives under <dataDir>/tls/
// (created 0o700 by the caller).
func LECertPaths(dataDir string) (certPath, keyPath string) {
	dir := filepath.Join(dataDir, "tls")
	return filepath.Join(dir, "tailscale.crt"), filepath.Join(dir, "tailscale.key")
}

// EnsureCertDir creates the <dataDir>/tls/ directory at 0o700. Idempotent.
// Caller invokes this once before MintCert so the cert/key write target
// directory exists. Mirrors the convention internal/backup uses for
// ensuring its <dataDir>/backups/ root.
func EnsureCertDir(dataDir string) error {
	return os.MkdirAll(filepath.Join(dataDir, "tls"), 0o700)
}

// silence unused-import warning if `logger` ends up being reorg'd later.
var _ = logger

// FreshnessThreshold is how long before notAfter the renewer treats the
// cert as "due for re-mint". Tailscale's LE certs are 90-day; 14 days
// of headroom matches Let's Encrypt's typical renewal window with safety
// margin so a single failed renew tick still leaves another 13 days to
// recover.
const FreshnessThreshold = 14 * 24 * time.Hour
