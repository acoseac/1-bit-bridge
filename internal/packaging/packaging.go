// Package packaging renders service-manager unit files (launchd plist on
// darwin, systemd user unit on linux) and installs them where the user's
// service manager expects to find them. Called from `bridge init` so a
// single operator command leaves behind a running, auto-restarting
// service.
package packaging

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
func Install(p Params) (unitPath string, err error) {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(p)
	case "linux":
		return installSystemd(p)
	default:
		return "", nil
	}
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
	t, err := template.ParseFS(tmplFS, name)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// DefaultConfigDir returns the standard config location for the current
// OS. macOS uses ~/Library/Application Support/1-bit-bridge so it's
// backed up by Time Machine and survives reinstalls; Linux follows XDG.
func DefaultConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "1-bit-bridge"), nil
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
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Logs", "1-bit-bridge.log"), nil
	}
	// On Linux, keep logs alongside the config under XDG_STATE_HOME.
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "1-bit-bridge", "bridge.log"), nil
	}
	return filepath.Join(home, ".local", "state", "1-bit-bridge", "bridge.log"), nil
}
