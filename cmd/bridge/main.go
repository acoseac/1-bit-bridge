// Command bridge is the 1-bit-bridge server CLI.
//
// Subcommands:
//
//	bridge serve   run the HTTPS server (default port 7788)
//	bridge pair    mint a new bearer token for an iOS client
//	bridge scan    force a full library rescan
//	bridge version print version and protocol version
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// artworkDirBridge lets cmd/bridge expose the enricher's cache dir to
// internal/api without importing internal/enrich from there.
type artworkDirBridge string

func (a artworkDirBridge) ArtworkCacheDir() string { return string(a) }

// shutdownGrace is how long we wait for in-flight requests to drain before
// forcing the listener closed.
const shutdownGrace = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses argv (without the program name) and dispatches to a subcommand.
// It is extracted from main so tests can drive it without spawning a process.
// Exit codes: 0 success, 1 subcommand failure, 2 usage error.
//
// ctx is used by serveCmd to trigger graceful shutdown (signal from main or
// cancellation from a test).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCmd(ctx, args[1:], stdout, stderr)
	case "pair":
		return pairCmd(args[1:], stdout, stderr)
	case "scan":
		return scanCmd(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "1-bit-bridge %s (protocol v%d)\n", version.ServerVersion, version.ProtocolVersion)
		return 0
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand: %s\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `1-bit-bridge — companion server for the 1-bit iOS app.

Usage:
  bridge <subcommand> [flags]

Subcommands:
  serve    Run the HTTPS server.
  pair     Generate a new bearer token for an iOS client.
  scan     Force a full library rescan.
  version  Print version and protocol version.

Run "bridge <subcommand> -h" for subcommand-specific flags.
`)
}

func serveCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	addrOverride := fs.String("addr", "", "override listenAddress from config (host:port)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	if err := bridgefs.ValidateRoots(cfg.LibraryRoots); err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	if *addrOverride != "" {
		cfg.ListenAddress = *addrOverride
	}

	// Resolve TLS material (default: dataDir/server.{crt,key}; overridable
	// via cfg.TLSCertPath / cfg.TLSKeyPath).
	certPath, keyPath := cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath, keyPath = servertls.DefaultPaths(cfg.DataDir)
	}
	hostname, _ := os.Hostname()
	cert, fingerprint, err := servertls.LoadOrGenerate(certPath, keyPath, hostname)
	if err != nil {
		fmt.Fprintf(stderr, "TLS material: %v\n", err)
		return 1
	}

	store, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		fmt.Fprintf(stderr, "open token store: %v\n", err)
		return 1
	}

	manifestStore, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer manifestStore.Close()
	scanner := manifest.NewScanner(cfg.LibraryRoots, manifestStore)
	provider := manifest.NewProvider(cfg.LibraryRoots, manifestStore, scanner)

	// Fire up the periodic scanner in the background. It runs an initial
	// scan on startup, then rescans every cfg.ScanInterval().
	scanCtx, scanCancel := context.WithCancel(context.Background())
	defer scanCancel()
	go scanner.RunPeriodic(scanCtx, cfg.ScanInterval())

	// Fire up the MusicBrainz/CoverArt enricher in the background. It
	// pulls un-enriched tracks from the store and fills in
	// MusicBrainzAlbumID / ArtworkMBID, caching cover images under
	// <dataDir>/artwork/.
	userAgent := fmt.Sprintf(
		"%s/%s (+https://github.com/acoseac/1-bit-bridge)",
		"1-bit-bridge", version.ServerVersion,
	)
	mbClient := enrich.NewMusicBrainzClient("", userAgent, nil)
	caaClient := enrich.NewCoverArtClient("", userAgent, nil)
	deezerClient := enrich.NewDeezerClient("", userAgent, nil)
	artworkDir := filepath.Join(cfg.DataDir, "artwork")
	enricher := enrich.NewEnricher(manifestStore, mbClient, caaClient, deezerClient, artworkDir)
	go enricher.Run(scanCtx)

	apiSrv := api.New(cfg, store, provider, fingerprint).
		WithArtworkDirs(artworkDirBridge(artworkDir))
	httpSrv := &http.Server{
		Addr:      cfg.ListenAddress,
		Handler:   apiSrv.Handler(),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12},
	}

	// Listen first so we can report the actual bound address (useful when
	// cfg.ListenAddress is ":0" — which test code uses).
	lis, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		fmt.Fprintf(stderr, "listen %s: %v\n", cfg.ListenAddress, err)
		return 1
	}

	fmt.Fprintf(stdout, "1-bit-bridge v%s (protocol v%d) — listening on https://%s\n",
		version.ServerVersion, version.ProtocolVersion, lis.Addr())
	fmt.Fprintf(stdout, "Library: %q (roots: %v)\n", cfg.LibraryName, cfg.LibraryRoots)
	fmt.Fprintf(stdout, "TLS fingerprint (pin this on the iOS side):\n  %s\n", fingerprint)

	// Advertise on mDNS so iOS clients on the same LAN auto-discover
	// this server. Failures are non-fatal — mDNS is a nice-to-have,
	// and the server runs fine without it (users connect by IP).
	boundAddr, _ := lis.Addr().(*net.TCPAddr)
	var advertiser *bridgemdns.Advertiser
	if boundAddr != nil {
		a, err := bridgemdns.Advertise(bridgemdns.Config{
			InstanceName:    cfg.LibraryName,
			Port:            boundAddr.Port,
			ProtocolVersion: version.ProtocolVersion,
			LibraryName:     cfg.LibraryName,
		})
		if err != nil {
			fmt.Fprintf(stderr, "mDNS advertise failed (non-fatal): %v\n", err)
		} else {
			advertiser = a
			fmt.Fprintf(stdout, "mDNS: advertising as %q on %s\n", cfg.LibraryName, bridgemdns.Service)
		}
	}
	if advertiser != nil {
		defer advertiser.Close()
	}

	fmt.Fprintln(stdout, "Press Ctrl-C to shut down.")

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpSrv.ServeTLS(lis, "", "")
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "server error: %v\n", err)
			return 1
		}
	case <-ctx.Done():
		fmt.Fprintln(stdout, "\nShutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "shutdown: %v\n", err)
			return 1
		}
	}
	return 0
}

func pairCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	name := fs.String("name", "", "client name (e.g. \"iPhone 15 Pro\")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "pair: --name is required")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	store, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		fmt.Fprintf(stderr, "open token store: %v\n", err)
		return 1
	}
	raw, tok, err := store.Mint(*name)
	if err != nil {
		fmt.Fprintf(stderr, "mint token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Paired successfully.")
	fmt.Fprintf(stdout, "  Device: %s\n", tok.Name)
	fmt.Fprintf(stdout, "  ID:     %s\n", tok.ID)
	fmt.Fprintf(stdout, "\nBearer token (copy this into the 1-bit iOS app; it won't be shown again):\n")
	fmt.Fprintf(stdout, "  %s\n", raw)
	return 0
}

func scanCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
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
	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()
	scanner := manifest.NewScanner(cfg.LibraryRoots, store)

	fmt.Fprintf(stdout, "Scanning %v ...\n", cfg.LibraryRoots)
	start := time.Now()
	n, err := scanner.Scan(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "scan error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Scan complete: %d tracks indexed in %s\n", n, time.Since(start).Round(time.Millisecond))
	return 0
}
