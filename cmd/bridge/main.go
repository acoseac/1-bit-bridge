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
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

const (
	ServerVersion   = "0.0.1"
	ProtocolVersion = 1
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run parses argv (without the program name) and dispatches to a subcommand.
// It is extracted from main so tests can drive it without spawning a process.
// Exit codes: 0 success, 1 subcommand failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return serveCmd(args[1:], stdout, stderr)
	case "pair":
		return pairCmd(args[1:], stdout, stderr)
	case "scan":
		return scanCmd(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "1-bit-bridge %s (protocol v%d)\n", ServerVersion, ProtocolVersion)
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

func serveCmd(args []string, stdout, stderr io.Writer) int {
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
	if *addrOverride != "" {
		cfg.ListenAddress = *addrOverride
	}
	fmt.Fprintf(stdout, "config loaded: libraryName=%q roots=%v listen=%s scanInterval=%s\n",
		cfg.LibraryName, cfg.LibraryRoots, cfg.ListenAddress, cfg.ScanInterval())
	fmt.Fprintln(stderr, "serve: not yet implemented (HTTPS server lands in a later PR)")
	return 1
}

func pairCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "client name (e.g. \"iPhone 15 Pro\")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmt.Fprintf(stderr, "pair: not yet implemented (name=%s)\n", *name)
	return 1
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
	fmt.Fprintf(stdout, "config loaded: libraryName=%q roots=%v\n", cfg.LibraryName, cfg.LibraryRoots)
	fmt.Fprintln(stderr, "scan: not yet implemented (manifest scanner lands in a later PR)")
	return 1
}
