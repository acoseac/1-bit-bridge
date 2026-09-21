package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// certCmd dispatches the `bridge cert <subcommand>` family. Cert
// rotation is annual (default cert lifetime is 397 days, capped under
// Apple ATS's 398-day enforcement) and the operator path also matters
// for forced rotations — key compromise, hostname change, or
// certificate-pinning hygiene. Living under one CLI verb keeps the
// surface compact and discoverable next to the existing `bridge token`
// namespace.
func certCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		certUsage(stderr)
		return 2
	}
	switch args[0] {
	case "info":
		return certInfoCmd(args[1:], stdout, stderr)
	case "rotate":
		return certRotateCmd(args[1:], stdin, stdout, stderr)
	case "-h", "--help", "help":
		certUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown cert subcommand: %s\n\n", args[0])
		certUsage(stderr)
		return 2
	}
}

func certUsage(w io.Writer) {
	fmt.Fprint(w, `bridge cert <subcommand>

Subcommands:
  info     Print the live cert's fingerprint and expiry.
  rotate   Regenerate the TLS cert + key.
           WARNING: rotating invalidates every paired device's
           pinned fingerprint — every device must re-pair.

Run "bridge cert <subcommand> -h" for subcommand-specific flags.
`)
}

func certInfoCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cert info", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	jsonOut := fs.Bool("json", false, "emit cert info as JSON instead of the human-readable layout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	certPath, _ := resolveCertPaths(cfg)
	info, err := servertls.Inspect(certPath)
	if err != nil {
		fmt.Fprintf(stderr, "inspect cert: %v\n", err)
		return 1
	}
	// ONE `now` for every verdict below — the JSON envelope's three
	// booleans and the human warning are answers about the same
	// validity window and must not straddle a tick, the rule
	// `bridge doctor`'s tls-cert line already states for its own two
	// comparisons.
	now := time.Now()
	// The window has a NEAR end, and a certificate that has not started
	// is rejected by clients exactly as an expired one is. `Inspect`
	// reports only DaysUntilExpiry, so this state reads as a comfortable
	// year of remaining life on every surface that grades that number
	// alone. Reachable on this product's hardware: the mint allows one
	// hour of clock skew, so a NUC or Pi that minted before NTP landed
	// leaves a future NotBefore behind once the clock is corrected.
	notYetValid := info.NotBefore.After(now)
	// Derived from NotAfter directly rather than from DaysUntilExpiry's
	// sign — `days = 0` covers both "expires in 23h" (still valid, the
	// near-expiry band) and "expired 23h ago" (already past it, the
	// hard one). Integer truncation makes the two indistinguishable on
	// that field alone (Gemini flagged on PR #46).
	//
	// The `!notYetValid` prefix here and on expiringSoon below is
	// PRECEDENCE, not redundancy, and it is what makes the three
	// booleans mutually exclusive — the envelope says exactly what the
	// switch below prints. Gemini read it as dead on PR #951, on the
	// premise that NotAfter is always after NotBefore. Nothing in this
	// path enforces that: `x509.CreateCertificate` and
	// `x509.ParseCertificate` both accept an inverted window (measured),
	// `LoadX509KeyPair` ignores dates entirely, and `tlsCertPath` takes
	// any operator-supplied pair — a hand-assembled or restored data dir
	// is a documented operator state. Dropped, an inverted window
	// serialises `notYetValid: true` AND `expired: true`.
	expired := !notYetValid && now.After(info.NotAfter)
	// Against the exact remaining duration and servertls's own
	// threshold, NOT `DaysUntilExpiry <= 30`: the day count truncates
	// toward zero, so a cert with 30 days 23 hours left reads as 30 and
	// `bridge cert info` would call it expiring while `bridge doctor`
	// and the next `bridge serve` — both of which compare the duration —
	// stay quiet. Same cert, same host, two answers.
	expiringSoon := !notYetValid && !expired && info.NotAfter.Sub(now) <= servertls.ExpiryWarningWindow
	if *jsonOut {
		envelope := map[string]any{
			"subject":         info.Subject,
			"fingerprint":     info.Fingerprint,
			"notBefore":       info.NotBefore.UTC().Format(time.RFC3339),
			"notAfter":        info.NotAfter.UTC().Format(time.RFC3339),
			"daysUntilExpiry": info.DaysUntilExpiry,
			"notYetValid":     notYetValid,
			"expired":         expired,
			"expiringSoon":    expiringSoon,
		}
		return writeJSONIndent(stdout, stderr, "cert info", envelope)
	}
	fmt.Fprintf(stdout, "Subject:     %s\n", info.Subject)
	fmt.Fprintf(stdout, "Fingerprint: %s\n", info.Fingerprint)
	fmt.Fprintf(stdout, "Not before:  %s\n", info.NotBefore.UTC().Format(time.RFC3339))
	fmt.Fprintf(stdout, "Not after:   %s\n", info.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintf(stdout, "Days until expiry: %d\n", info.DaysUntilExpiry)
	switch {
	case notYetValid:
		fmt.Fprintf(stdout, "WARNING: cert is NOT YET VALID — %s\n", servertls.NotYetValidRemediation)
	case expired:
		fmt.Fprintln(stdout, "WARNING: cert has expired. iOS clients will reject the connection.")
	case expiringSoon:
		fmt.Fprintln(stdout, "WARNING: cert is expiring soon. Plan a rotation; every paired device will need to re-pair.")
	}
	return 0
}

func certRotateCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cert rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	autoYes := fs.Bool("yes", false, "skip the interactive confirmation prompt")
	fs.BoolVar(autoYes, "y", *autoYes, "alias for --yes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	certPath, keyPath := resolveCertPaths(cfg)

	// Print the current fingerprint so the operator has a paper
	// trail of what they're replacing — useful for the eventual
	// "wait, what was the OLD fingerprint?" question after the
	// fact.
	if oldInfo, err := servertls.Inspect(certPath); err == nil {
		fmt.Fprintf(stdout, "Current fingerprint: %s\n", oldInfo.Fingerprint)
		fmt.Fprintf(stdout, "Current cert expires: %s (%d days)\n",
			oldInfo.NotAfter.UTC().Format(time.RFC3339),
			oldInfo.DaysUntilExpiry)
		// The one band where this command is NOT the fix, which is why
		// it is the only one the preamble carries: an expired or
		// expiring cert is why the operator is here and saying so would
		// be noise, but a cert that has not STARTED means the host
		// clock was ahead at mint time, and the mint about to run reads
		// that same clock. Rotating now produces a second certificate
		// with the same wrong dates — and burns every device's pin to
		// do it. Printed BEFORE the confirmation prompt so it is
		// something the operator can still act on.
		if oldInfo.NotBefore.After(time.Now()) {
			fmt.Fprintf(stdout, "\nWARNING: the current cert is NOT YET VALID (starts %s).\n  %s\n",
				oldInfo.NotBefore.UTC().Format(time.RFC3339), servertls.NotYetValidRemediation)
		}
	}

	if !*autoYes {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Rotating the TLS cert will:")
		fmt.Fprintln(stdout, "  • Generate a fresh ECDSA P-256 key + 397-day self-signed cert (Apple ATS cap).")
		fmt.Fprintln(stdout, "  • Invalidate every paired device's pinned fingerprint.")
		fmt.Fprintln(stdout, "  • Every iOS device must re-pair (admin console QR or bridge:// link).")
		fmt.Fprintln(stdout, "  • Restart the bridge to load the new cert.")
		// Prompt on stderr; the bullet list above stays on stdout. Same
		// convention update.go states and `restore` now also honours —
		// `bridge cert rotate > rotation.log` must not swallow the question.
		fmt.Fprint(stderr, "\nType 'yes' to continue: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		if strings.TrimSpace(resp) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}

	// Regenerate directly over the existing files. GenerateWithOptions is
	// a two-phase commit: it stages BOTH the new cert and key to temp
	// files (stagePEM) and only then atomically renames both into place,
	// so a failure writing EITHER file leaves the prior cert/key pair
	// fully intact and the bridge bootable. Do NOT pre-remove the old
	// files here — that would defeat the atomic overwrite and reintroduce
	// the unbootable-on-failed-rotation hazard the two-phase commit closes
	// (PR #487).
	// Rotate is the operator-driven path that picks up Tailscale +
	// custom-endpoint SAN changes since the last cert was minted. The
	// gather is shared with serve and with `bridge doctor`, so the
	// rotated cert covers every URL the bridge currently advertises in
	// /v1/health and the doctor's verdict was about this very set.
	opts := certSANOptions(cfg)
	if err := servertls.GenerateWithOptions(certPath, keyPath, opts); err != nil {
		fmt.Fprintf(stderr, "rotate: %v\n", err)
		return 1
	}
	info, err := servertls.Inspect(certPath)
	if err != nil {
		fmt.Fprintf(stderr, "rotate: cert generated but inspect failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "TLS cert rotated.")
	fmt.Fprintf(stdout, "  New fingerprint: %s\n", info.Fingerprint)
	fmt.Fprintf(stdout, "  Expires:         %s (%d days)\n",
		info.NotAfter.UTC().Format(time.RFC3339), info.DaysUntilExpiry)
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Next steps:")
	fmt.Fprintln(stdout, "  1. Restart the bridge so the new cert is served.")
	fmt.Fprintln(stdout, "  2. Open the admin console and re-pair every device — the existing")
	fmt.Fprintln(stdout, "     'Pair new device' / per-token 'Rotate' flows emit fresh QR codes")
	fmt.Fprintln(stdout, "     carrying the new fingerprint.")
	return 0
}

// certSANOptions builds the TLS SAN inputs every cert path in this
// binary mints or grades against: `bridge init`'s first mint, `bridge
// serve`'s load-or-mint, `bridge cert rotate`, and `bridge doctor`'s
// tls-cert-sans check.
//
// One helper because the four had the same three lines copied out four
// times, and the one that drifts is the one that decides an operator's
// cert is fine when a rotation would produce something different. The
// doctor's whole claim is "a rotation right now would cover these", so
// it has to be asking the same question `bridge cert rotate` answers.
//
// The Tailscale half of the gather shells out, bounded at 1.5 s and
// TTL-cached for 30 s process-wide (see internal/advertise) — the same
// probe `/v1/health` runs per request.
func certSANOptions(cfg *config.Config) servertls.GenerateOptions {
	hostname, _ := os.Hostname()
	sanCfg := advertise.CertSANConfig{CustomEndpoints: cfg.CustomEndpoints}
	return servertls.GenerateOptions{
		Hostname:      hostname,
		ExtraDNSNames: advertise.GatherCertSANDNS(sanCfg),
		ExtraIPs:      advertise.GatherCertSANIPs(sanCfg),
	}
}

// resolveCertPaths returns the cert + key paths the running bridge
// would use, applying the same `cfg.TLSCertPath` / `cfg.TLSKeyPath`
// → defaults fallback that `serveCmd` does. Centralising it here
// keeps the CLI commands consistent with the live serve invariants.
func resolveCertPaths(cfg *config.Config) (certPath, keyPath string) {
	certPath, keyPath = cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath, keyPath = servertls.DefaultPaths(cfg.DataDir)
	}
	return certPath, keyPath
}
