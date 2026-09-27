package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// adminCmd dispatches the `bridge admin <subcommand>` family: the
// console's credential and its sessions, from a shell on the bridge host.
func adminCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "bridge admin <subcommand>")
		fmt.Fprintln(stderr, "  reset-password        Rotate the admin console password and sign every console out")
		fmt.Fprintln(stderr, "                        (--keep-sessions leaves them signed in)")
		fmt.Fprintln(stderr, "  sign-out-everywhere   Sign every console out, keeping the password")
		fmt.Fprintln(stderr, "  login-link            Print a one-time URL that logs a browser into the console")
		fmt.Fprintln(stderr, "                        (--ttl extends its life for a cross-device hand-off)")
		return 2
	}
	switch args[0] {
	case "login-link":
		return adminLoginLink(args[1:], stdout, stderr)
	case "reset-password":
		return adminResetPasswordCmd(args[1:], stdin, stdout, stderr)
	case "sign-out-everywhere":
		return adminSignOutEverywhereCmd(args[1:], stdout, stderr)
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
// A running bridge re-reads the file at its next login attempt, so the
// rotation takes there with no restart. It also signs every console out,
// unless --keep-sessions: a running bridge refuses those sessions at their
// next request, and a restart does not bring them back
// (adminauth.SignOutEverywhere has the mechanism). Kept, they stay signed
// in through restarts too, since they persist in the same file.
//
// Until 2026-09-27 a rotation kept them and nothing else could end them,
// so a console signed in with a leaked password outlived the rotation for
// up to the 7-day hard cap. Before 2026-09-27 this said a restart revoked
// them, and the running bridge wrote the old password back at its next
// write of the file, the shutdown flush of that restart included.
func adminResetPasswordCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin reset-password", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to bridge.yaml (overrides the default lookup)")
	username := fs.String("username", "admin", "username to update (single-user system; \"admin\" by default)")
	fromStdin := fs.Bool("from-stdin", false, "read the new password from a single stdin line (no echo suppression — script-friendly)")
	keepSessions := fs.Bool("keep-sessions", false, "leave the consoles already signed in signed in (by default every one is signed out)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Refuse unexpected positional args so a fat-fingered
	// invocation like `bridge admin reset-password admin newpw`
	// (where the operator forgot --username) doesn't silently
	// proceed against the default admin user with the wrong
	// shape (CodeRabbit Minor review post-PR-#292).
	//
	// **Do NOT echo `fs.Args()` to stderr** (CodeRabbit Major
	// review post-PR-#296): the realistic fat-finger is exactly
	// the `bridge admin reset-password admin newpw` shape above,
	// which would leak the plaintext password into stderr +
	// every log / shell-transcript capturing it. Just say the
	// count.
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments after flags")
		fmt.Fprintln(stderr, "all options must be passed as flags; use --username / --from-stdin")
		return 2
	}

	cfg, _, err := loadConfigForAdminCmd(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	storePath := filepath.Join(cfg.DataDir, adminauth.FileName)
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

	action := adminauth.EndSessions
	if *keepSessions {
		action = adminauth.KeepSessions
	}
	if err := store.ResetPassword(*username, password, action); err != nil {
		fmt.Fprintf(stderr, "reset password: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Admin password updated for %q. A running bridge takes it at its next sign-in, with no restart.\n", *username)
	if *keepSessions {
		fmt.Fprintln(stdout, "Consoles already signed in stay signed in (--keep-sessions), and a restart does not sign them out.")
		fmt.Fprintln(stdout, "To sign them out: bridge admin sign-out-everywhere")
		return 0
	}
	fmt.Fprintln(stdout, msgSignedOutEverywhere)
	return 0
}

// msgSignedOutEverywhere is what both commands that sign every console out
// say about it, so the two cannot come to describe one mechanism
// differently.
const msgSignedOutEverywhere = "Every console that was signed in is signed out: a running bridge refuses their sessions at their next request, and a restart does not bring them back."

// adminSignOutEverywhereCmd ends every admin console session and leaves the
// password as it is: for a session that must end while the password need
// not change (a browser left signed in on a machine that is not the
// operator's), or after `reset-password --keep-sessions`. A console the
// operator wants kept signs in again.
//
// It writes a sign-out marker into `<dataDir>/adminauth.json`, beside the
// credential, because the running bridge holds the sessions in memory and
// would write them back over a file that only lost them;
// adminauth.SignOutEverywhere has the mechanism.
func adminSignOutEverywhereCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin sign-out-everywhere", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "all options must be passed as flags")
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	storePath := filepath.Join(cfg.DataDir, adminauth.FileName)
	store, err := adminauth.OpenStore(storePath)
	if err != nil {
		fmt.Fprintf(stderr, "open adminauth store: %v\n", err)
		return 1
	}
	if err := store.SignOutEverywhere(); err != nil {
		if errors.Is(err, adminauth.ErrNotInitialised) {
			fmt.Fprintf(stderr, "no admin credentials at %s, so no console to sign out of\n", storePath)
			return 1
		}
		fmt.Fprintf(stderr, "sign out everywhere: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, msgSignedOutEverywhere)
	fmt.Fprintln(stdout, "The password is unchanged; to change it too: bridge admin reset-password")
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

// loadConfigForAdminCmd resolves the bridge.yaml location and loads it,
// via the SHARED CLI resolver — override flag wins, then ./bridge.yaml,
// then the platform config dir.
//
// That last hop is what makes this command usable at all on the
// deployment it exists for. It used to fall back to the bare relative
// `defaultConfigPath`, so it could only ever find a config in the
// current directory — while `runServe`'s public-mode refuse-to-start
// prints "run `bridge admin reset-password`" to an operator whose config
// lives in the platform dir. Following that instruction from anywhere
// else died with `read config "bridge.yaml": no such file or directory`,
// dead-ending the documented recovery path.
func loadConfigForAdminCmd(override string) (*config.Config, string, error) {
	return loadCLIConfig(override)
}

// adminLoginLink prints a single-use URL that exchanges for a console session.
//
// It exists for two callers with very different budgets, which is what `--ttl`
// is for. An operator pastes this into a browser on the same machine, and the
// 60-second default is right for them. The hosted control plane mints it while
// the user is holding a PHONE; the URL then travels to another machine and waits
// for a human to notice and click, and at 60 seconds that was failing every
// time — see adminauth.LoginTicketTTL. Either way it cannot be redeemed twice.
//
// The URL is printed to stdout and nowhere else. It is a working credential for
// its lifetime, so it must not be logged or echoed into a shell transcript that
// outlives it.
func adminLoginLink(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin login-link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	username := fs.String("username", "admin", "account to log in as (single-user system; \"admin\" by default)")
	base := fs.String("base", "", "absolute base URL to prefix, e.g. https://host:7789 (default: print the path only)")
	ttl := fs.Duration("ttl", adminauth.LoginTicketTTL,
		fmt.Sprintf("how long the link stays valid (max %s) — raise it for a cross-device hand-off",
			adminauth.MaxLoginTicketTTL))
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "all options must be passed as flags")
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	store, err := adminauth.OpenStore(filepath.Join(cfg.DataDir, adminauth.FileName))
	if err != nil {
		fmt.Fprintf(stderr, "open adminauth store: %v\n", err)
		return 1
	}
	if !store.IsInitialised() {
		fmt.Fprintln(stderr, "no admin credentials yet — run `bridge admin reset-password` first")
		return 1
	}
	ticket, err := store.MintLoginTicketTTL(*username, *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "mint login ticket: %v\n", err)
		return 1
	}
	path := "/login/ticket?t=" + url.QueryEscape(ticket)
	if b := strings.TrimRight(strings.TrimSpace(*base), "/"); b != "" {
		path = b + path
	}
	fmt.Fprintln(stdout, path)
	return 0
}
