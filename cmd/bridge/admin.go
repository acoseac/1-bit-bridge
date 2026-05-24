package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// adminCmd dispatches the `bridge admin <subcommand>` family.
// Today the only subcommand is `reset-password`; future additions
// (rotate-session-secret, list-sessions, etc.) plug in here.
func adminCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "bridge admin <subcommand>")
		fmt.Fprintln(stderr, "  reset-password   Rotate the admin console password")
		return 2
	}
	switch args[0] {
	case "reset-password":
		return adminResetPasswordCmd(args[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown subcommand: bridge admin %s\n", args[0])
		return 2
	}
}

// adminResetPasswordCmd rewrites the admin credentials file at
// `<dataDir>/adminauth.json`. Prompts for the new password twice on
// a TTY (no echo) so a typo doesn't lock the operator out; supports
// --from-stdin for scripts (single read, no echo suppression).
//
// Active sessions are NOT invalidated — operator who wants existing
// sessions revoked too should restart the bridge afterwards.
func adminResetPasswordCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to bridge.yaml (overrides the default lookup)")
	username := fs.String("username", "admin", "username to update (single-user system; \"admin\" by default)")
	fromStdin := fs.Bool("from-stdin", false, "read the new password from a single stdin line (no echo suppression — script-friendly)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Refuse unexpected positional args so a fat-fingered
	// invocation like `bridge admin reset-password admin newpw`
	// (where the operator forgot --username) doesn't silently
	// proceed against the default admin user with the wrong
	// shape (CodeRabbit Minor review post-PR-#292).
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments after flags: %v\n", fs.Args())
		fmt.Fprintln(stderr, "all options must be passed as flags; use --username / --from-stdin")
		return 2
	}

	cfg, _, err := loadConfigForAdminCmd(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	storePath := filepath.Join(cfg.DataDir, "adminauth.json")
	store, err := adminauth.OpenStore(storePath)
	if err != nil {
		fmt.Fprintf(stderr, "open adminauth store: %v\n", err)
		return 1
	}

	var password string
	if *fromStdin {
		br := bufio.NewReader(stdin)
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "read stdin: %v\n", err)
			return 1
		}
		password = strings.TrimRight(line, "\r\n")
		if password == "" {
			fmt.Fprintln(stderr, "new password from stdin is empty — aborting")
			return 1
		}
	} else {
		pw, err := readPasswordTwice(stdin, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		password = pw
	}

	if err := store.ResetPassword(*username, password); err != nil {
		fmt.Fprintf(stderr, "reset password: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Admin password updated for %q. Restart the bridge to invalidate existing sessions.\n", *username)
	return 0
}

// readPasswordTwice prompts the operator for the new password
// twice and confirms they match. Uses the terminal's raw-mode echo
// suppression so the password doesn't print to scrollback. Non-TTY
// stdin returns an error directing the operator to --from-stdin.
func readPasswordTwice(stdin io.Reader, stdout, stderr io.Writer) (string, error) {
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("stdin is not a terminal — pass --from-stdin to read the password from a single line")
	}
	fmt.Fprint(stdout, "New password: ")
	a, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stdout)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(a) == 0 {
		return "", errors.New("password must not be empty")
	}
	fmt.Fprint(stdout, "Confirm: ")
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(stdout)
	if err != nil {
		return "", fmt.Errorf("read confirmation: %w", err)
	}
	if string(a) != string(b) {
		return "", errors.New("passwords did not match")
	}
	return string(a), nil
}

// loadConfigForAdminCmd resolves the bridge.yaml location and
// loads it. Mirrors the discovery logic the other CLI subcommands
// use (override flag wins, else the default config path constant).
func loadConfigForAdminCmd(override string) (*config.Config, string, error) {
	path := override
	if path == "" {
		path = defaultConfigPath
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, "", fmt.Errorf("load %s: %w", path, err)
	}
	return cfg, path, nil
}
