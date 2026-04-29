// Tailscale MagicDNS / IP detection via the local `tailscale` CLI.
//
// Why the CLI and not the LocalAPI: the LocalAPI's Unix socket / named
// pipe path varies by platform and Tailscale version, and the
// tailscale.com/client/tailscale Go package would pull in a chunky
// dependency for two strings. Spawning the already-installed CLI is a
// best-effort short-circuit — if Tailscale isn't running, isn't
// installed, or the CLI returned within the timeout without a DNSName,
// the helpers return zero-values and the caller falls back to the
// interface-walk path that already classifies CGNAT IPs as
// `ClassTailscaleV4`.
package advertise

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// tailscaleCLITimeout caps a single CLI invocation. The iOS client's
// per-endpoint probe is 5s; the bridge-side detector must be well
// under that to keep `Endpoints()` responsive on hosts where Tailscale
// is installed but its sockets are slow (cold-boot, recent update).
const tailscaleCLITimeout = 1500 * time.Millisecond

// tailscaleStatus is the minimal subset of `tailscale status --json`
// we consume. Only `Self.DNSName` and `Self.TailscaleIPs` are read;
// every other field in the wire payload is silently ignored.
type tailscaleStatus struct {
	Self struct {
		DNSName      string   `json:"DNSName"`
		TailscaleIPs []string `json:"TailscaleIPs"`
	} `json:"Self"`
}

// tailscaleStatusJSONFunc is the test seam for the CLI invocation.
// Production callers use `runTailscaleStatusJSON`; tests swap in a
// fake that emits canned JSON or errors.
var tailscaleStatusJSONFunc = runTailscaleStatusJSON

// GetTailscaleDNSName returns the host's MagicDNS name (e.g.
// `home-pc.tailfoo.ts.net`) or "" if Tailscale isn't running, isn't
// installed, or the CLI returned within `tailscaleCLITimeout` without
// a DNSName field.
//
// Trailing `.` (FQDN form) is stripped — `tailscale status --json`
// historically emits both forms across versions; the SAN / URL-host
// uses are happier with the bare form.
//
// Silent on every error path. Tailscale not being installed is the
// common case on a non-tailnet host, not an operator-visible failure;
// errors land at `.debug` only.
func GetTailscaleDNSName() string {
	st, err := tailscaleStatusJSONFunc()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(st.Self.DNSName), ".")
}

// GetTailscaleIPs returns the host's Tailscale-assigned IPs from the
// same `tailscale status --json` payload that backs GetTailscaleDNSName.
// Used by the cert generator (PR 5) to seed SAN IPAddresses;
// complements (does not replace) the interface-walk path that already
// detects 100.x CGNAT addresses by IP-range membership.
//
// Returns nil on any error / when Tailscale isn't reachable.
func GetTailscaleIPs() []net.IP {
	st, err := tailscaleStatusJSONFunc()
	if err != nil {
		return nil
	}
	out := make([]net.IP, 0, len(st.Self.TailscaleIPs))
	for _, raw := range st.Self.TailscaleIPs {
		if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
			out = append(out, ip)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runTailscaleStatusJSON locates the `tailscale` binary and runs
// `status --json` against it. Return value is the parsed minimal
// payload or a non-nil error; callers treat any error as "Tailscale
// unreachable" and return zero-values.
//
// Search order:
//
//  1. PATH (covers the documented install on every platform)
//  2. macOS: `/Applications/Tailscale.app/Contents/MacOS/Tailscale`
//     — the Mac App Store / DMG install doesn't auto-link the CLI
//     symlink unless the operator runs `Tailscale > CLI` once.
//  3. Windows: `<ProgramFiles>\Tailscale\tailscale.exe` and
//     `<ProgramFiles(x86)>\Tailscale\tailscale.exe` — env-derived to
//     work on hosts that installed Windows on a non-default drive,
//     non-English locales (`C:\Programmes\…`), and 32-bit-on-64-bit
//     installs.
//
// Empty / missing env-vars in the Windows path are skipped (we never
// construct a path against `""`).
func runTailscaleStatusJSON() (tailscaleStatus, error) {
	bin, err := locateTailscaleBinary()
	if err != nil {
		return tailscaleStatus{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), tailscaleCLITimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil {
		return tailscaleStatus{}, err
	}

	var st tailscaleStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return tailscaleStatus{}, err
	}
	return st, nil
}

// locateTailscaleBinary returns the first executable `tailscale` path
// found on disk, or an error if none are reachable.
func locateTailscaleBinary() (string, error) {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p, nil
	}

	for _, candidate := range platformTailscaleFallbacks() {
		if candidate == "" {
			continue
		}
		// `os.Stat` is enough — exec.CommandContext will fail at
		// `Output()` if the file isn't actually executable, and we
		// surface that failure as "Tailscale unreachable" the same
		// as a missing binary. No need to mirror Unix mode bits here.
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("tailscale CLI not found")
}

// platformTailscaleFallbacks returns OS-specific candidate paths that
// don't appear on PATH by default. The macOS / Windows GUI installers
// land the binary outside PATH; on Linux the package manager always
// puts it on PATH so no fallback is needed.
func platformTailscaleFallbacks() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		}
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
			if dir := os.Getenv(env); dir != "" {
				out = append(out, filepath.Join(dir, "Tailscale", "tailscale.exe"))
			}
		}
		return out
	default:
		return nil
	}
}
