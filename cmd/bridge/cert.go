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
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	jsonOut := fs.Bool("json", false, "emit cert info as JSON instead of the human-readable layout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
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
	if *jsonOut {
		expired := time.Now().After(info.NotAfter)
		expiringSoon := !expired && info.DaysUntilExpiry <= 30
		envelope := map[string]any{
			"subject":         info.Subject,
			"fingerprint":     info.Fingerprint,
			"notBefore":       info.NotBefore.UTC().Format(time.RFC3339),
			"notAfter":        info.NotAfter.UTC().Format(time.RFC3339),
			"daysUntilExpiry": info.DaysUntilExpiry,
			"expired":         expired,
			"expiringSoon":    expiringSoon,
		}
		return writeJSONIndent(stdout, envelope)
	}
	fmt.Fprintf(stdout, "Subject:     %s\n", info.Subject)
	fmt.Fprintf(stdout, "Fingerprint: %s\n", info.Fingerprint)
	fmt.Fprintf(stdout, "Not before:  %s\n", info.NotBefore.UTC().Format(time.RFC3339))
	fmt.Fprintf(stdout, "Not after:   %s\n", info.NotAfter.UTC().Format(time.RFC3339))
	fmt.Fprintf(stdout, "Days until expiry: %d\n", info.DaysUntilExpiry)
	// Use NotAfter directly for the expired-vs-still-valid split
	// rather than relying on DaysUntilExpiry's sign — `days = 0`
	// covers both "expires in 23h" (still valid, near-expiry
	// warning applies) and "expired 23h ago" (already expired,
	// hard warning applies). Integer truncation makes the two
	// indistinguishable on that field alone (Gemini flagged on
	// PR #46).
	now := time.Now()
	switch {
	case now.After(info.NotAfter):
		fmt.Fprintln(stdout, "WARNING: cert has expired. iOS clients will reject the connection.")
	case info.DaysUntilExpiry <= 30:
		fmt.Fprintln(stdout, "WARNING: cert is expiring soon. Plan a rotation; every paired device will need to re-pair.")
	}
	return 0
}

func certRotateCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cert rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	autoYes := fs.Bool("yes", false, "skip the interactive confirmation prompt")
	fs.BoolVar(autoYes, "y", *autoYes, "alias for --yes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
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
	}

	if !*autoYes {
		fmt.Fprintln(stdout, "")
		fmt.Fprintln(stdout, "Rotating the TLS cert will:")
		fmt.Fprintln(stdout, "  • Generate a fresh ECDSA P-256 key + 397-day self-signed cert (Apple ATS cap).")
		fmt.Fprintln(stdout, "  • Invalidate every paired device's pinned fingerprint.")
		fmt.Fprintln(stdout, "  • Every iOS device must re-pair (admin console QR or bridge:// link).")
		fmt.Fprintln(stdout, "  • Restart the bridge to load the new cert.")
		fmt.Fprint(stdout, "\nType 'yes' to continue: ")
		var resp string
		_, _ = fmt.Fscanln(stdin, &resp)
		if strings.TrimSpace(resp) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return 1
		}
	}

	// Regenerate directly over the existing files. writePEM commits each
	// PEM via a temp-file + atomic rename, so a failed or interrupted write
	// (disk full, process kill) leaves the prior cert/key intact and the
	// bridge bootable. Do NOT pre-remove the old files here — that would
	// defeat the atomic overwrite and reintroduce the
	// unbootable-on-failed-rotation hazard the atomic write closes.
	hostname, _ := os.Hostname()
	// Rotate is the operator-driven path that picks up Tailscale +
	// custom-endpoint SAN changes since the last cert was minted. We
	// gather the broader SAN set here so the rotated cert covers every
	// URL the bridge currently advertises in /v1/health.
	sanCfg := advertise.CertSANConfig{CustomEndpoints: cfg.CustomEndpoints}
	opts := servertls.GenerateOptions{
		Hostname:      hostname,
		ExtraDNSNames: advertise.GatherCertSANDNS(sanCfg),
		ExtraIPs:      advertise.GatherCertSANIPs(sanCfg),
	}
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
