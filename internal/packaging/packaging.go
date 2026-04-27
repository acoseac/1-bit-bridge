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
		_ = tryUninstallWindowsService()
		return uninstallWindowsStartup()
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
	return filepath.Join(home, ".config", "systemd", "user", ServiceLabel+".service"), nil
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
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return path, fmt.Errorf("systemctl daemon-reload: %v: %s", err, string(out))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", ServiceLabel+".service").CombinedOutput(); err != nil {
		return path, fmt.Errorf("systemctl enable --now: %v: %s", err, string(out))
	}
	return path, nil
}

func uninstallSystemd() (string, error) {
	_ = exec.Command("systemctl", "--user", "disable", "--now", ServiceLabel+".service").Run()
	path, err := systemdUnitPath()
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return path, err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return path, nil
}

// --- template rendering ---

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
		"systemdEscape": func(s string) string {
			// systemd unit values are parsed as shell-like quoted strings
			// when wrapped in double quotes (which the template does).
			// Inside those quotes, `\\` and `\"` are the escape sequences
			// the parser expects, and CR/LF/NUL would terminate the value
			// early. Backslash goes first so it doesn't double-escape the
			// replacements for `"` and CR/LF that follow. A path like
			// `/Users/Bob's "Music"` or `C:\Users\bob` round-trips intact.
			r := strings.NewReplacer(
				`\`, `\\`,
				`"`, `\"`,
				"\n", `\n`,
				"\r", `\r`,
				"\x00", "",
			)
			return r.Replace(s)
		},
		"cmdEscape": func(s string) string { return CmdEscape(s) },
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

// CmdEscape prepares a string for use inside a double-quoted argument on
// a Windows `cmd.exe` command line. The only thing that can tear a
// double-quoted argument is a literal `"` — cmd.exe's escape for a quote
// inside quotes is `""`, NOT backslash-anything. Backslashes are fine
// inside quoted arguments; they're literal path separators. CR / LF
// would end the line, which would break the script. NUL can't appear in
// a Windows path, but stripping it is cheap insurance.
//
// Shared by the `cmdEscape` template func (startup.cmd.tmpl) and the
// runtime `SpawnDetached` helper so the init-time spawn and the
// logon-time Startup launcher produce byte-identical command lines.
func CmdEscape(s string) string {
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
		return filepath.Join(home, "Library", "Application Support", "1-bit-bridge"), nil
	case "windows":
		if x := os.Getenv("LOCALAPPDATA"); x != "" {
			return filepath.Join(x, "1-bit-bridge"), nil
		}
		return filepath.Join(home, "AppData", "Local", "1-bit-bridge"), nil
	}
	// XDG default when XDG_CONFIG_HOME is unset.
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "1-bit-bridge"), nil
	}
	return filepath.Join(home, ".config", "1-bit-bridge"), nil
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
			return filepath.Join(x, "1-bit-bridge", "bridge.log"), nil
		}
		return filepath.Join(home, "AppData", "Local", "1-bit-bridge", "bridge.log"), nil
	}
	// On Linux, keep logs alongside the config under XDG_STATE_HOME.
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "1-bit-bridge", "bridge.log"), nil
	}
	return filepath.Join(home, ".local", "state", "1-bit-bridge", "bridge.log"), nil
}
