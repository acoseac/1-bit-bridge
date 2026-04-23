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
	"os"
)

const (
	ServerVersion   = "0.0.1"
	ProtocolVersion = 1
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serveCmd(os.Args[2:])
	case "pair":
		pairCmd(os.Args[2:])
	case "scan":
		scanCmd(os.Args[2:])
	case "version":
		fmt.Printf("1-bit-bridge %s (protocol v%d)\n", ServerVersion, ProtocolVersion)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `1-bit-bridge — companion server for the 1-bit iOS app.

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

func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	addr := fs.String("addr", ":7788", "listen address (host:port)")
	_ = fs.Parse(args)
	fmt.Fprintf(os.Stderr, "serve: not yet implemented (config=%s addr=%s)\n", *configPath, *addr)
	os.Exit(1)
}

func pairCmd(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	name := fs.String("name", "", "client name (e.g. \"iPhone 15 Pro\")")
	_ = fs.Parse(args)
	fmt.Fprintf(os.Stderr, "pair: not yet implemented (name=%s)\n", *name)
	os.Exit(1)
}

func scanCmd(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	_ = fs.Parse(args)
	fmt.Fprintf(os.Stderr, "scan: not yet implemented (config=%s)\n", *configPath)
	os.Exit(1)
}
