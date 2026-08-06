// Package packaging renders service-manager unit files (launchd plist on
// darwin, systemd user unit on linux) and installs them where the user's
// service manager expects to find them. Called from `bridge init` so a
// single operator command leaves behind a running, auto-restarting
// service.
package packaging

import (
	"bytes"
	"embed"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

//go:embed *.tmpl
var tmplFS embed.FS

// ServiceLabel is the launchd Label / systemd unit name. Chosen to match
// the iOS bundle id ("com.acoseac.onebit") so `launchctl list` groups it
// sensibly alongside the app's user defaults.
const ServiceLabel = "com.acoseac.1-bit-bridge"

// Package-level literals extracted to satisfy SonarCloud go:S1192 across
// packaging.go, lifecycle_other.go, lifecycle_windows.go, and
// service_windows.go. Each value is shared across multiple call sites for
// systemd / per-OS state-dir / log-file path construction.
const (
	systemdUserFlag    = "--user"
	systemdUnitSuffix  = ".service"
	productName        = "1-bit-bridge"
	bridgeLogFileName  = "bridge.log"
	plistPathErrFormat = "resolve plist path: %w"
	scmConnectAdminErr = "connect SCM (need admin?): %w"
	scmConnectErr      = "connect SCM: %w"
	scmOpenSvcAdminErr = "open service (need admin?): %w"
	scmOpenSvcErr      = "open service: %w"
)

// Params bundles the values the templates expand. All paths must be
// absolute — launchd won't expand $HOME inside a plist, and systemd
// user units on most distros inherit a minimal PATH.
type Params struct {
	Label      string // e.g. com.acoseac.1-bit-bridge
	BinaryPath string // absolute path to the bridge binary
	ConfigPath string // absolute path to bridge.yaml
	WorkingDir string // absolute path; where stdout/stderr filenames resolve
	LogPath    string // absolute path to a log file
}

// Install writes the service unit for the current OS and asks the service
// manager to load it. Returns the path of the unit file that was written.
// Non-darwin / non-linux: returns an empty path and (nil) error — the
// caller should advise the user to run `bridge serve` manually.
//
// **Windows two-tier semantics**: tries the SCM service first; falls
// through to the Startup-folder launcher when SCM access is denied.
// Callers that need to *force* the Startup-folder path (because the
// operator explicitly chose option 1 in the wizard, even though they
// might be running elevated) MUST use `InstallStartup` instead —
// otherwise an Administrator-shell wizard run silently auto-elevates
// to SCM regardless of the operator's pick.
func Install(p Params) (unitPath string, err error) {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(p)
	case "linux":
		return installSystemd(p)
	case "windows":
		// Two-tier Windows install:
		//
		// 1. Elevated processes (UAC admin) get a proper SCM Windows
		//    Service. Survives logout, auto-starts on boot, integrates
		//    with the auto-installer's swap-binary path so updates
		//    don't strand the operator at the SCM "stop service"
		//    step.
		// 2. Non-elevated falls back to a Startup-folder shortcut.
		//    Survives reboot while the user is logged in; does NOT
		//    survive logout. The auto-installer's rename-trick still
		//    works under this layout (no SCM file lock to dodge).
		//
		// `tryInstallWindowsService` returns "" + nil when SCM access
		// is denied, signalling "fall through to startup folder".
		// Other errors are real failures and bubble up.
		if unitPath, err := tryInstallWindowsService(p); err != nil {
			return unitPath, err
		} else if unitPath != "" {
			return unitPath, nil
		}
		return installWindowsStartup(p)
	default:
		return "", nil
	}
}

// InstallStartup installs the Startup-folder launcher only on
// Windows; never touches SCM. Used by the wizard when the operator
// explicitly picks "Launch when I log in" (option 1) — without this
// strict path, `Install`'s SCM-first auto-elevation would silently
// install as a Windows Service when the wizard was run from an
// Administrator shell, and the post-install status / restart
// behaviour would mismatch the operator's stated choice.
//
// On non-Windows: returns ("", nil). The launchd / systemd paths
// have only one mode each, so the operator never sees this distinction
// outside Windows.
func InstallStartup(p Params) (unitPath string, err error) {
	if runtime.GOOS != "windows" {
		return "", nil
	}
	return installWindowsStartup(p)
}

// Uninstall reverses Install: stops the service and removes the unit
// file. Missing files are not an error so `bridge init` can rerun
// idempotently. Returns (unitPath, err).
func Uninstall() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	case "windows":
		// Windows install can land in either of two places (SCM or
		// startup folder) depending on elevation at install time.
		// Uninstall tries both — each is idempotent for the
		// not-installed case, so a fresh-init bridge or a SCM-only
		// install both clean up correctly. SCM uninstall first
		// because if the service is registered, removing it before
		// the Startup shortcut avoids a brief window where the
		// service tries to start a binary the operator is still
		// removing.
		//
		// The SCM error is surfaced (joined with the Startup result)
		// rather than swallowed: tryUninstallWindowsService already
		// returns nil for the benign cases (no admin, not registered),
		// so a non-nil here is a genuine failure (stuck stop, SCM down)
		// that would otherwise leave a zombie service reported as a
		// clean uninstall.
		scmErr := tryUninstallWindowsService()
		path, startupErr := uninstallWindowsStartup()
		return path, errors.Join(scmErr, startupErr)
	default:
		return "", nil
	}
}

// --- darwin / launchd ---

// launchdPlistPath returns $HOME/Library/LaunchAgents/<label>.plist.
func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", ServiceLabel+".plist"), nil
}

func installLaunchd(p Params) (string, error) {
	p.Label = ServiceLabel
	body, err := render("launchd.plist.tmpl", p)
	if err != nil {
		return "", err
	}
	path, err := launchdPlistPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// bootout (if already loaded) before writing, so we can bootstrap the
	// fresh copy. Ignore errors — launchctl returns non-zero when nothing
	// is currently loaded, which is a fine starting state for a first init.
	_ = exec.Command("launchctl", "bootout", "gui/"+uidString(), path).Run()
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return path, err
	}
	out, err := exec.Command("launchctl", "bootstrap", "gui/"+uidString(), path).CombinedOutput()
	if err != nil {
		return path, fmt.Errorf("launchctl bootstrap: %v: %s", err, string(out))
	}
	return path, nil
}

func uninstallLaunchd() (string, error) {
	path, err := launchdPlistPath()
	if err != nil {
		return "", err
	}
	_ = exec.Command("launchctl", "bootout", "gui/"+uidString(), path).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return path, err
	}
	return path, nil
}

func uidString() string {
	// os.Getuid returns -1 on Windows but we never call this on Windows.
	return fmt.Sprintf("%d", os.Getuid())
}

// --- linux / systemd user ---

func systemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", ServiceLabel+systemdUnitSuffix), nil
}

func installSystemd(p Params) (string, error) {
	body, err := render("systemd.service.tmpl", p)
	if err != nil {
		return "", err
	}
	path, err := systemdUnitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return path, err
	}
	if out, err := exec.Command("systemctl", systemdUserFlag, "daemon-reload").CombinedOutput(); err != nil {
		return path, fmt.Errorf("systemctl daemon-reload: %v: %s", err, string(out))
	}
	if out, err := exec.Command("systemctl", systemdUserFlag, "enable", "--now", ServiceLabel+systemdUnitSuffix).CombinedOutput(); err != nil {
		return path, fmt.Errorf("systemctl enable --now: %v: %s", err, string(out))
	}
	return path, nil
}

func uninstallSystemd() (string, error) {
	_ = exec.Command("systemctl", systemdUserFlag, "disable", "--now", ServiceLabel+systemdUnitSuffix).Run()
	path, err := systemdUnitPath()
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return path, err
	}
	_ = exec.Command("systemctl", systemdUserFlag, "daemon-reload").Run()
	return path, nil
}

// --- template rendering ---

// systemd unit-value escapers. systemd applies THREE distinct layers of
// processing to unit-file values, and their scopes differ — which is why
// there are two escapers and why only ONE of the two settings families is
// written between double quotes:
//
//   - Specifier expansion (%h, %u, …) applies to nearly every setting,
//     including WorkingDirectory / StandardOutput / ExecStart. A literal
//     `%` must always be doubled to `%%` or systemd fails specifier
//     expansion at unit parse. BOTH escapers double it.
//   - Shell-style quote removal + C-escape unescaping applies ONLY to the
//     settings documented as taking a quoted command line — systemd.syntax(5)
//     is explicit that "this style of quoting is not used for all settings,
//     but only for those documented as such", and systemd.exec(5) documents
//     it for Exec*=, not for WorkingDirectory=/StandardOutput=/StandardError=.
//     Those three parsers (config_parse_working_directory,
//     config_parse_exec_output's `append:` branch) hand the RAW rvalue to
//     path_simplify_and_warn(…, PATH_CHECK_ABSOLUTE|PATH_CHECK_FATAL), so a
//     leading `"` makes the value non-absolute and the unit FAILS TO LOAD.
//     They are therefore written UNQUOTED, and their escaper must not emit
//     in-quote escapes that would land verbatim in the path.
//   - Environment-variable substitution ($FOO / ${FOO}) applies ONLY to
//     the arguments of the Exec* command lines (ExecStart=, ExecStop=, …)
//     and is NOT suppressed by the surrounding double quotes. A literal
//     `$` in an Exec path must be doubled to `$$`, or systemd expands it
//     (e.g. `/opt/My$Music/bridge.yaml` → `/opt/My/bridge.yaml`) and the
//     service fails to start. A `$` in a NON-Exec path setting must be
//     left alone — doubling it there would corrupt the path to `$$`.
//
// Unquoted is correct for the path settings whether or not a given systemd
// build would have stripped the quotes: a path setting takes the rest of
// the line VERBATIM, including spaces, so there is nothing the quotes were
// buying. `systemd-analyze verify` on a real Linux host is the confirmation
// step for the quote-removal half of the reasoning above.
var (
	// systemdEscapePathReplacer is for path-valued, NON-Exec settings
	// (WorkingDirectory, StandardOutput=append:, StandardError=append:),
	// which the template writes UNQUOTED.
	//
	// It doubles `%` (specifier expansion reaches these settings) and
	// strips CR/LF/NUL. The strip is the load-bearing guard: the unit file
	// is line-based, so a raw newline in a path would end the directive and
	// let the remainder of the value be parsed as a SECOND unit directive.
	// Emitting `\n` instead would be worse than useless here — with no
	// unescaping layer on these settings it would land as a literal
	// backslash-n inside the path.
	//
	// It deliberately does NOT touch `$` (no env-var substitution on these
	// settings — doubling would corrupt the path to `$$`), nor `\` and `"`
	// (with no quote removal / C-unescaping, both are ordinary path bytes
	// and must survive verbatim). A path that ENDS in `\` is handled
	// separately, by rejection — see systemdEscapePath.
	systemdEscapePathReplacer = strings.NewReplacer(
		`%`, `%%`,
		"\n", "",
		"\r", "",
		"\x00", "",
	)
	// systemdEscapeExecReplacer is for the Exec* command lines, which the
	// template writes BETWEEN double quotes. Those DO get quote removal and
	// C-escape unescaping, so `\` and `"` need the in-quote escapes systemd's
	// parser expects, and `$` is doubled because Exec arguments undergo
	// env-var substitution after quote removal. Backslash is listed first so
	// the escape it introduces isn't itself reconsidered (strings.NewReplacer
	// is single-pass, so this is belt-and-braces). A path like
	// `/Users/Bob's "Music"` or `C:\Users\bob` round-trips intact.
	systemdEscapeExecReplacer = strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`%`, `%%`,
		`$`, `$$`,
		"\n", `\n`,
		"\r", `\r`,
		"\x00", "",
	)
)

// systemdEscapePath escapes a value for an UNQUOTED systemd path setting,
// and REFUSES one that would change the meaning of the unit file.
//
// systemd joins a line ending in `\` with the line that follows it,
// replacing the backslash with a space — line continuation, and it applies
// to every setting, path settings included. So `WorkingDirectory=/srv/x\`
// silently absorbs the `Restart=always` beneath it: the unit loads, the
// working directory is wrong, and auto-restart is gone with no diagnostic.
//
// There is no escape available. The path settings get no unquoting layer
// (that is the whole reason they are written unquoted), so a doubled `\\`
// still ends the line with a backslash and continues just the same.
// Silently trimming it would hand systemd a path the operator didn't
// configure. Refusing is the only option that neither corrupts the unit nor
// lies about the path — and it surfaces at `bridge init --service` time,
// where the operator can fix the path, rather than as a mystery at boot.
//
// Returning an error from a template func aborts template.Execute, so
// `render` surfaces it and `installSystemd` never writes the file.
// Trailing CR/LF/NUL are stripped by the replacer first, so the check sees
// the bytes that would actually land on the line.
func systemdEscapePath(s string) (string, error) {
	escaped := systemdEscapePathReplacer.Replace(s)
	if strings.HasSuffix(escaped, `\`) {
		return "", fmt.Errorf(
			"path %q ends in a backslash: systemd would treat it as a line continuation and swallow the next unit directive; rename the directory", s)
	}
	return escaped, nil
}

func render(name string, p Params) ([]byte, error) {
	// The launchd plist is XML; the systemd unit is INI-like. Each template
	// picks the right escape func for its own fields — both escapes are
	// registered here so the templates can name them without per-file
	// plumbing. Using text/template (not html/template) because neither
	// target is HTML and we want precise, mechanical escaping of literal
	// path characters, not context-aware HTML sanitization.
	funcs := template.FuncMap{
		"xmlEscape": func(s string) string {
			// xml.EscapeText is the canonical path for Go's XML stdlib.
			// It handles <, >, &, ' and " in a way that survives inside
			// any plist <string> element, including paths with ampersands
			// or apostrophes ("Bob's Music", "A & B Records/album.flac").
			var b bytes.Buffer
			if err := xml.EscapeText(&b, []byte(s)); err != nil {
				return s
			}
			return b.String()
		},
		// systemd processes Exec* command lines and plain path settings
		// differently, so the template uses two escapers AND quotes only
		// the Exec* form — see systemdEscape{Exec,Path}Replacer for the
		// full rationale.
		"systemdEscapeExec": systemdEscapeExecReplacer.Replace,
		"systemdEscapePath": systemdEscapePath,
		"cmdEscape":         func(s string) string { return CmdEscape(s) },
	}
	t, err := template.New(name).Funcs(funcs).ParseFS(tmplFS, name)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// CmdEscape prepares a string for use as a double-quoted argument inside a
// Windows BATCH FILE (startup.cmd.tmpl). Two cmd.exe hazards are handled:
//
//   - A literal `"` ends the quoted argument; cmd.exe's in-quote escape for
//     a quote is `""`, NOT backslash-anything (backslashes are literal path
//     separators and round-trip fine inside quotes).
//   - A literal `%` triggers variable expansion — cmd.exe expands `%VAR%`
//     (and `%1`..`%9`) EVEN INSIDE double quotes, so a path like
//     `C:\Music 50% Off\bridge.exe` would be mangled (cmd hunts for a
//     closing `%`, and a `%NAME%` matching a real env var is substituted).
//     In a batch file the literal-percent escape is `%%` (cmd collapses
//     `%%`→`%` at parse time), so we double it.
//
// CR / LF would end the line and break the script; NUL can't appear in a
// Windows path but stripping it is cheap insurance.
//
// This is the BATCH-FILE escaper, used by the `cmdEscape` template func for
// startup.cmd.tmpl. The `%%` doubling is correct ONLY for a `.cmd`/`.bat`
// file — cmd.exe does NOT collapse `%%` when a command is passed via
// `cmd /c` on the command line, so the runtime `SpawnDetached` helper uses
// cmdArgEscape instead. For a `%`-free path (the common case) both produce
// byte-identical output; for a `%`-path they agree at RUNTIME (a batch `%%`
// collapses to the same single `%` the /c form carries).
func CmdEscape(s string) string {
	return strings.NewReplacer(
		`"`, `""`,
		`%`, `%%`,
		"\n", " ",
		"\r", "",
		"\x00", "",
	).Replace(s)
}

// cmdArgEscape is the command-line counterpart to CmdEscape for a command
// passed to `cmd /c <line>` (the SpawnDetached path) rather than written to
// a batch file. It is deliberately CmdEscape MINUS the `%` doubling: cmd.exe
// does NOT collapse `%%`→`%` for a `/c` command line (only when reading a
// .bat/.cmd file), so doubling here would leave a literal `%%` in the path.
//
// There is no robust way to escape a literal `%` on the cmd command line —
// caret does not escape it — so a `%`-containing path is passed through
// as-is (the pre-existing behaviour). This affects only the transient
// init-time spawn; the durable Startup-folder launcher is a .cmd file that
// escapes `%` correctly via CmdEscape.
func cmdArgEscape(s string) string {
	return strings.NewReplacer(
		`"`, `""`,
		"\n", " ",
		"\r", "",
		"\x00", "",
	).Replace(s)
}

// DefaultConfigDir returns the standard config location for the current
// OS. macOS uses ~/Library/Application Support/1-bit-bridge so it's
// backed up by Time Machine and survives reinstalls; Linux follows XDG;
// Windows uses %LOCALAPPDATA%\1-bit-bridge (roaming is overkill for a
// per-machine SQLite DB + TLS cert).
func DefaultConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", productName), nil
	case "windows":
		if x := os.Getenv("LOCALAPPDATA"); x != "" {
			return filepath.Join(x, productName), nil
		}
		return filepath.Join(home, "AppData", "Local", productName), nil
	}
	// XDG default when XDG_CONFIG_HOME is unset.
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, productName), nil
	}
	return filepath.Join(home, ".config", productName), nil
}

// DefaultLogPath returns a per-OS log file location.
func DefaultLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "1-bit-bridge.log"), nil
	case "windows":
		// Co-locate the log with the data/config dir — keeps the whole
		// install self-contained under one folder and avoids sprinkling
		// writes under %APPDATA% (which roams) vs %LOCALAPPDATA% (which
		// doesn't).
		if x := os.Getenv("LOCALAPPDATA"); x != "" {
			return filepath.Join(x, productName, bridgeLogFileName), nil
		}
		return filepath.Join(home, "AppData", "Local", productName, bridgeLogFileName), nil
	}
	// On Linux, keep logs alongside the config under XDG_STATE_HOME.
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, productName, bridgeLogFileName), nil
	}
	return filepath.Join(home, ".local", "state", productName, bridgeLogFileName), nil
}
