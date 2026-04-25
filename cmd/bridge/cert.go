package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// certCmd dispatches the `bridge cert <subcommand>` family. Cert
// rotation is rare (default cert lifetime is 10 years) but the
// operator path matters when one is forced — key compromise,
// hostname change, or certificate-pinning hygiene. Living under one
// CLI verb keeps the surface compact and discoverable next to the
// existing `bridge token` namespace.
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
		fmt.Fprintln(stdout, "  • Generate a fresh ECDSA P-256 key + 10-year self-signed cert.")
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

	// Remove existing files before regenerating — `Generate` writes
	// fresh ones; the existing files won't conflict with the rename
	// inside writePEM (it uses O_TRUNC) but explicit removal makes
	// the failure mode clearer if perms get in the way.
	_ = os.Remove(certPath)
	_ = os.Remove(keyPath)

	hostname, _ := os.Hostname()
	if err := servertls.Generate(certPath, keyPath, hostname); err != nil {
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
