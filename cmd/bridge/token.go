package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// tokenCmd dispatches the `bridge token <subcommand>` family. Kept
// nested under one CLI verb so the surface stays organised — these
// are operator-facing lifecycle commands that pair naturally with
// each other (list / rotate / expire / revoke).
//
// `bridge pair` already exists at the top level for minting; rather
// than duplicating that logic under `bridge token mint`, the
// existing `pair` command stays where it is and `token` is the
// post-mint operations namespace.
func tokenCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		tokenUsage(stderr)
		return 2
	}
	switch args[0] {
	case "list":
		return tokenListCmd(args[1:], stdout, stderr)
	case "rotate":
		return tokenRotateCmd(args[1:], stdout, stderr)
	case "expire":
		return tokenExpireCmd(args[1:], stdout, stderr)
	case "revoke":
		return tokenRevokeCmd(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		tokenUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown token subcommand: %s\n\n", args[0])
		tokenUsage(stderr)
		return 2
	}
}

func tokenUsage(w io.Writer) {
	fmt.Fprint(w, `bridge token <subcommand>

Subcommands:
  list      Print all paired tokens with their lifecycle state.
  rotate    Replace the raw bytes of an existing token (preserves ID).
  expire    Set or clear the ExpiresAt cutoff for a token.
  revoke    Permanently delete a token.

Run "bridge token <subcommand> -h" for subcommand-specific flags.
`)
}

func tokenListCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
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
	store, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		fmt.Fprintf(stderr, "open token store: %v\n", err)
		return 1
	}
	tokens := store.List()
	if len(tokens) == 0 {
		fmt.Fprintln(stdout, "No paired tokens. Run `bridge pair --name <device>` to mint one.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tCREATED\tLAST USED\tROTATED\tEXPIRES")
	for _, t := range tokens {
		lastUsed := "never"
		if !t.LastUsedAt.IsZero() {
			lastUsed = t.LastUsedAt.UTC().Format(time.RFC3339)
		}
		rotated := "—"
		if !t.RotatedAt.IsZero() {
			rotated = t.RotatedAt.UTC().Format(time.RFC3339)
		}
		expires := "never"
		if t.ExpiresAt != nil && !t.ExpiresAt.IsZero() {
			expires = t.ExpiresAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID,
			t.Name,
			t.CreatedAt.UTC().Format(time.RFC3339),
			lastUsed,
			rotated,
			expires,
		)
	}
	_ = tw.Flush()
	return 0
}

func tokenRotateCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token rotate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "rotate: missing token ID argument")
		fmt.Fprintln(stderr, "Usage: bridge token rotate [--config X] <id>")
		return 2
	}
	id := fs.Arg(0)
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
	raw, tok, err := store.Rotate(id)
	if err != nil {
		fmt.Fprintf(stderr, "rotate: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Token rotated. The previous raw token is now invalid.")
	fmt.Fprintf(stdout, "  Device: %s\n", tok.Name)
	fmt.Fprintf(stdout, "  ID:     %s\n", tok.ID)
	fmt.Fprintf(stdout, "\nNew bearer token (paste into the 1-bit Bridge Editor; it won't be shown again):\n")
	fmt.Fprintf(stdout, "  %s\n", raw)
	fmt.Fprintln(stdout, "\nFor a re-pair QR, open the admin console and click 'Rotate' on this token.")
	return 0
}

func tokenExpireCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token expire", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	in := fs.Duration("in", 0, "invalidate the token after this duration from now (e.g. 24h, 7*24h). Required unless --clear is set.")
	clear := fs.Bool("clear", false, "remove an existing expiry from the token")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "expire: missing token ID argument")
		fmt.Fprintln(stderr, "Usage: bridge token expire [--config X] (--in <duration> | --clear) <id>")
		return 2
	}
	if *in == 0 && !*clear {
		fmt.Fprintln(stderr, "expire: must specify either --in <duration> or --clear")
		return 2
	}
	if *in != 0 && *clear {
		fmt.Fprintln(stderr, "expire: --in and --clear are mutually exclusive")
		return 2
	}
	id := fs.Arg(0)
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
	var exp *time.Time
	if !*clear {
		t := time.Now().Add(*in).UTC()
		exp = &t
	}
	tok, err := store.SetExpiry(id, exp)
	if err != nil {
		fmt.Fprintf(stderr, "expire: %v\n", err)
		return 1
	}
	if exp == nil {
		fmt.Fprintf(stdout, "Cleared expiry on token %s (%s).\n", tok.ID, tok.Name)
	} else {
		fmt.Fprintf(stdout, "Token %s (%s) will expire at %s (UTC).\n",
			tok.ID, tok.Name, tok.ExpiresAt.Format(time.RFC3339))
	}
	return 0
}

func tokenRevokeCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "revoke: missing token ID argument")
		fmt.Fprintln(stderr, "Usage: bridge token revoke [--config X] <id>")
		return 2
	}
	id := fs.Arg(0)
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
	if err := store.Revoke(id); err != nil {
		fmt.Fprintf(stderr, "revoke: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Token %s revoked. The device will need a fresh pair to reconnect.\n", id)
	return 0
}
