package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// initCmd walks a first-time operator through the minimum answers needed
// to get a running bridge: config dir, library root, then writes
// bridge.yaml, mints the TLS cert, installs a launchd/systemd user unit,
// and prints the admin console URL so they can open it and pair.
//
// Idempotent: re-running on a populated config dir offers to keep or
// rewrite the existing bridge.yaml. The TLS cert is always preserved —
// rotating it breaks every paired client's pin.
func initCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgDirFlag := fs.String("dir", "", "config directory (default per-OS standard)")
	nonInteractive := fs.Bool("yes", false, "accept all defaults without prompting")
	force := fs.Bool("force", false, "with --yes: overwrite an existing config (by default, --yes refuses to clobber)")
	libraryRoot := fs.String("library", "", "library root path (required with --yes)")
	libraryName := fs.String("name", "", "library display name (default: hostname)")
	skipService := fs.Bool("no-service", false, "skip launchd/systemd install; run `bridge serve` yourself")
	skipDoctor := fs.Bool("skip-doctor", false, "don't run `bridge doctor` preflight before init (not recommended)")
	windowsService := fs.Bool("service", false, "Windows only: install as a Windows Service (requires admin); default is a Startup-folder launcher")
	// Windows-only: when init installs the Startup-folder launcher, the
	// .cmd only fires on *next* logon. Default behaviour with an
	// interactive prompt is to also spawn the server right now so the
	// operator can open the admin console without a logout. For
	// non-interactive runs (--yes) we keep the old "install but don't
	// spawn" default unless --start-now is set, so CI scripts that
	// rebuild a tempdir with `bridge init --yes` don't leave an orphan
	// server behind.
	startNow := fs.Bool("start-now", false, "Windows only: after install, spawn `bridge serve` detached so the admin console is reachable without a logout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfgDir := *cfgDirFlag
	if cfgDir == "" {
		d, err := packaging.DefaultConfigDir()
		if err != nil {
			fmt.Fprintf(stderr, "resolve config dir: %v\n", err)
			return 1
		}
		cfgDir = d
	}
	// Always canonicalize to an absolute path — relative or ~-prefixed
	// inputs land verbatim in config.DataDir and the service templates,
	// where launchd / systemd have no cwd / no shell expansion, so the
	// service fails at start with a silent "no such file" from the
	// daemon's log. Resolving here keeps the rest of init trust-but-
	// verify-free.
	if absDir, err := filepath.Abs(expandHome(cfgDir)); err == nil {
		cfgDir = absDir
	} else {
		fmt.Fprintf(stderr, "resolve --dir path: %v\n", err)
		return 1
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	dataDir := filepath.Join(cfgDir, "data")

	fmt.Fprintf(stdout, "1-bit-bridge — first-time setup\n\n")
	fmt.Fprintf(stdout, "  Config dir:  %s\n", cfgDir)
	fmt.Fprintf(stdout, "  Data dir:    %s\n", dataDir)
	fmt.Fprintf(stdout, "  Config file: %s\n\n", cfgPath)

	in := bufio.NewReader(stdin)

	// Collect the library root.
	libRoot := *libraryRoot
	if libRoot == "" {
		if *nonInteractive {
			fmt.Fprintf(stderr, "--yes requires --library <path>\n")
			return 2
		}
		libRoot = ask(in, stdout, "Library folder to expose (absolute path)", "")
		if libRoot == "" {
			fmt.Fprintf(stderr, "library path is required\n")
			return 2
		}
	}
	abs, err := filepath.Abs(expandHome(libRoot))
	if err != nil {
		fmt.Fprintf(stderr, "resolve library path: %v\n", err)
		return 1
	}
	info, err := os.Stat(abs)
	if err != nil {
		fmt.Fprintf(stderr, "library path: %v\n", err)
		return 1
	}
	if !info.IsDir() {
		fmt.Fprintf(stderr, "%q is not a directory\n", abs)
		return 1
	}

	// Preflight. Run after library-path resolution so doctor sees the
	// real path the user chose, not a default. --skip-doctor bypasses
	// for the rare case where the operator knows better than the check
	// (say: doctor fails on port 7789 bound by an existing bridge and
	// you're re-running init on purpose to rewrite config).
	if !*skipDoctor {
		d := doctor.Deps{
			ConfigDir:    cfgDir,
			DataDir:      dataDir,
			LibraryRoots: []string{abs},
			APIPort:      7788,
			AdminPort:    7789,
		}
		if code := ensureDoctorClean(stdout, d); code != 0 {
			fmt.Fprintln(stdout)
			fmt.Fprintln(stdout, "fix the fail(s) above, or re-run with --skip-doctor to bypass.")
			return 1
		}
	}

	// Library name — prompt on interactive, hostname fallback otherwise.
	name := *libraryName
	if name == "" {
		if *nonInteractive {
			h, _ := os.Hostname()
			if h == "" {
				h = "1-bit Bridge"
			}
			name = h
		} else {
			h, _ := os.Hostname()
			name = ask(in, stdout, "Library display name", h)
			if name == "" {
				name = h
			}
		}
	}

	// Write or refresh the config file. Preserve the existing file if the
	// operator says no at the prompt — common when they're re-running
	// init to reinstall the service against an already-tuned config.
	//
	// Non-interactive (`--yes`) is the automation path, so we refuse to
	// silently clobber an existing config: the operator must pass `--force`
	// to acknowledge they mean to overwrite. Without the gate, a CI job or
	// packaging script that reruns `bridge init --yes` would silently wipe
	// an already-tuned installation.
	if _, err := os.Stat(cfgPath); err == nil {
		if *nonInteractive {
			if !*force {
				fmt.Fprintf(stdout, "config file already exists at %s; keeping it\n", cfgPath)
				fmt.Fprintf(stdout, "pass --force to overwrite non-interactively\n")
				keepChoice := resolveLaunchChoice(in, stdout, *nonInteractive, *skipService, *windowsService, *startNow)
				return finishInit(stdout, stderr, cfgPath, dataDir, keepChoice)
			}
		} else {
			if !confirm(in, stdout, "Config file exists. Overwrite?", false) {
				fmt.Fprintf(stdout, "keeping existing config\n")
				keepChoice := resolveLaunchChoice(in, stdout, *nonInteractive, *skipService, *windowsService, *startNow)
				return finishInit(stdout, stderr, cfgPath, dataDir, keepChoice)
			}
		}
	}

	// 0o700 because both dirs hold private material: bridge.yaml
	// (TLS fingerprint, library paths), data/cert.key (TLS private
	// key), data/tokens.json (bearer-token hashes), data/bridge.db.
	// On POSIX this prevents cross-user reads on shared hosts; on
	// Windows the Go file mode is advisory only — protection there
	// relies on per-user-profile NTFS ACLs at %LOCALAPPDATA%, which
	// already block other standard users.
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "mkdir config dir: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "mkdir data dir: %v\n", err)
		return 1
	}

	cfg := &config.Config{
		LibraryRoots:    []string{abs},
		ListenAddress:   config.DefaultListenAddress,
		AdminAddress:    config.DefaultAdminAddress,
		DataDir:         dataDir,
		ScanIntervalSec: config.DefaultScanIntervalSec,
		LibraryName:     name,
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "validate: %v\n", err)
		return 1
	}
	if err := cfg.Save(cfgPath); err != nil {
		fmt.Fprintf(stderr, "save config: %v\n", err)
		return 1
	}

	// Mint the TLS cert up-front so the fingerprint is stable from the
	// first serve onwards. If the cert already exists (re-init case),
	// LoadOrGenerate preserves it.
	certPath, keyPath := servertls.DefaultPaths(dataDir)
	host, _ := os.Hostname()
	if _, fp, err := servertls.LoadOrGenerate(certPath, keyPath, host); err != nil {
		fmt.Fprintf(stderr, "TLS cert: %v\n", err)
		return 1
	} else {
		fmt.Fprintf(stdout, "\nTLS fingerprint (stable across restarts):\n  %s\n", fp)
	}

	choice := resolveLaunchChoice(in, stdout, *nonInteractive, *skipService, *windowsService, *startNow)
	return finishInit(stdout, stderr, cfgPath, dataDir, choice)
}

// launchChoice bundles the three orthogonal knobs that control how
// init leaves the bridge behind: whether to skip the
// launchd/systemd/Startup install entirely (manual mode), whether to
// install the Windows Service via SCM instead of the Startup-folder
// .cmd, and whether to spawn `bridge serve` detached right now so the
// admin console is reachable without a logout. Only the last one is
// new on Windows — macOS launchd / Linux systemd already start the
// unit during install.
type launchChoice struct {
	skipService bool
	useService  bool
	spawnNow    bool
}

// resolveLaunchChoice merges flag-driven and prompt-driven input into a
// single choice. On Windows, when the user is interactive and didn't
// pre-pick via a flag, we show a 3-option picker so they don't end up
// with the old "init exits, server isn't running, admin URL 404s"
// experience. Non-interactive runs (--yes) keep today's flag semantics
// so scripts that call `bridge init --yes --no-service` don't get a
// surprise detached server.
func resolveLaunchChoice(in *bufio.Reader, stdout io.Writer, nonInteractive, skipService, useService, startNow bool) launchChoice {
	flagSet := skipService || useService
	if runtime.GOOS == "windows" && !nonInteractive && !flagSet {
		return promptLaunchMode(in, stdout)
	}
	return launchChoice{
		skipService: skipService,
		useService:  useService,
		spawnNow:    startNow,
	}
}

// promptLaunchMode is the Windows-only 3-option picker. Mode 1 (the
// recommended default) installs the Startup-folder launcher AND spawns
// the server right now so init doesn't leave the admin console dark.
// Mode 2 installs the SCM service (which starts itself). Mode 3 is
// "I'll run `bridge serve` myself" — used by operators who want zero
// residue.
func promptLaunchMode(in *bufio.Reader, stdout io.Writer) launchChoice {
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "How should the bridge start up on this machine?")
	fmt.Fprintln(stdout, "  [1] Launch when I log in  (recommended — Startup-folder launcher)")
	fmt.Fprintln(stdout, "  [2] Always-on Windows Service  (requires admin; survives logout)")
	fmt.Fprintln(stdout, "  [3] Only when I start it manually  (I'll run `bridge serve` myself)")
	for {
		choice := ask(in, stdout, "Choose [1]", "1")
		switch strings.TrimSpace(choice) {
		case "1", "":
			return launchChoice{spawnNow: true}
		case "2":
			return launchChoice{useService: true}
		case "3":
			return launchChoice{skipService: true}
		}
		fmt.Fprintln(stdout, "  (enter 1, 2, or 3)")
	}
}

// finishInit installs the service (or tells the user how to run manually)
// and prints the admin console URL. Separated from the main init path so
// the "keep existing config" branch can reach it without rebuilding the
// Config struct.
//
// The admin URL + browser open always runs at the end, even on the
// "skip service" and "unsupported OS" paths — a successful init is
// useless to the operator if they don't know where to point their
// browser. The browser-open is best-effort (no stderr on headless
// machines), so the cost of always attempting it is zero.
func finishInit(stdout, stderr io.Writer, cfgPath, dataDir string, choice launchChoice) int {
	// Load the config once up-front so the admin-address probe (Windows
	// auto-start path) and the browser-open at the end both use the
	// operator-configured bind address, not the hard-coded default.
	// `spawnNowOrWarn` previously probed `config.DefaultAdminAddress`
	// directly — on a non-default admin_address, the "already running"
	// check always missed, the second process tried to bind the
	// configured port, and the log filled with port-bind errors.
	adminAddr := config.DefaultAdminAddress
	if cfg, err := config.Load(cfgPath); err == nil && cfg.AdminAddress != "" {
		adminAddr = cfg.AdminAddress
	}

	// `printAdmin` is called once per mode with per-mode copy. On
	// Windows we also pass whether the server is expected to be live
	// right now — if it is, we poll briefly before opening the browser
	// so the user doesn't hit "site can't be reached" on a fast machine
	// that outran the cmd.exe handoff.
	printAdmin := func(serverIsLive bool) {
		url := "http://" + adminAddr + "/"
		if serverIsLive {
			if host, port, ok := splitHostPort(adminAddr); ok {
				_ = packaging.WaitForListen(host, port, 2*time.Second)
			}
		}
		fmt.Fprintf(stdout, "\nAdmin console: %s\n", url)
		if serverIsLive {
			openInBrowser(url)
		}
		fmt.Fprintf(stdout, "\nDone. Open the admin console to add library folders and pair iOS devices.\n")
	}

	if choice.skipService || (runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows") {
		fmt.Fprintf(stdout, "\nSkipping service install. Start the bridge with:\n")
		fmt.Fprintf(stdout, "  bridge serve --config %s\n", cfgPath)
		printAdmin(false)
		return 0
	}

	// --service on non-Windows is a usage error; the flag only makes
	// sense with SCM. Call it out loudly so the user doesn't think
	// they got a Windows-Service-equivalent on their Mac.
	if choice.useService && runtime.GOOS != "windows" {
		fmt.Fprintf(stderr, "--service is a Windows-only flag; ignored on %s\n", runtime.GOOS)
		choice.useService = false
	}

	binary, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "locate current binary: %v\n", err)
		return 1
	}
	binary, _ = filepath.EvalSymlinks(binary)
	logPath, err := packaging.DefaultLogPath()
	if err != nil {
		fmt.Fprintf(stderr, "log path: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "mkdir log dir: %v\n", err)
		return 1
	}
	params := packaging.Params{
		BinaryPath: binary,
		ConfigPath: cfgPath,
		WorkingDir: dataDir,
		LogPath:    logPath,
	}
	// serverIsLive tracks whether the final printAdmin should open a
	// browser: true if SCM started the service or we just spawned it
	// detached; false if only the Startup-folder launcher landed and it
	// won't fire until next logon (which today can't happen — spawnNow
	// is the default — but is the compatibility path for
	// `--yes` callers).
	var serverIsLive bool
	var unitPath string
	if choice.useService {
		// SCM install. Requires admin; fails otherwise with a clear
		// "access denied" from the SCM layer.
		unitPath, err = packaging.InstallWindowsService(params)
		if err != nil {
			fmt.Fprintf(stderr, "Windows Service install: %v\n", err)
			fmt.Fprintf(stderr, "If the error mentions \"access\" or \"denied\", re-run init from an elevated PowerShell.\n")
			fmt.Fprintf(stderr, "Otherwise fall back to the Startup-folder install: bridge init (no --service flag).\n")
			return 1
		}
		fmt.Fprintf(stdout, "Windows Service installed: %s\n", unitPath)
		serverIsLive = true // SCM's Start() fired during Install.
	} else {
		unitPath, err = packaging.Install(params)
		if err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			fmt.Fprintf(stderr, "You can still run the bridge manually:\n  %s serve --config %s\n", binary, cfgPath)
			return 1
		}
		fmt.Fprintf(stdout, "Service installed at:\n  %s\n", unitPath)
		// macOS launchd `bootstrap` / Linux systemctl `enable --now`
		// started the daemon as part of install. On Windows the Startup
		// .cmd only runs at next logon, so we spawn a detached child
		// here if the user asked for Mode 1.
		switch runtime.GOOS {
		case "darwin", "linux":
			serverIsLive = true
		case "windows":
			if choice.spawnNow {
				serverIsLive = spawnNowOrWarn(stdout, stderr, binary, cfgPath, logPath, adminAddr)
			}
		}
	}
	fmt.Fprintf(stdout, "Logs:\n  %s\n", logPath)

	printFutureLaunchHint(stdout, choice, binary, cfgPath)
	printAdmin(serverIsLive)
	return 0
}

// spawnNowOrWarn tries to fire up a detached `bridge serve` right now.
// Returns true on success. On failure, warns the operator and falls
// back to the "next logon" path — init still exits cleanly because the
// launcher .cmd is already on disk.
//
// Skips the spawn if something is already listening on the admin port
// — re-running init while the SCM service (or a previous detached
// launcher) is up shouldn't produce a port-bind error buried in the
// log. `adminAddr` comes from the loaded config (caller passes
// `cfg.AdminAddress`); falls back to 127.0.0.1:7789 only if the addr
// isn't host:port parseable.
func spawnNowOrWarn(stdout, stderr io.Writer, binary, cfgPath, logPath, adminAddr string) bool {
	host, port, ok := splitHostPort(adminAddr)
	if !ok {
		host, port = "127.0.0.1", 7789
	}
	if packaging.IsListening(host, port) {
		fmt.Fprintf(stdout, "A bridge is already running on %s:%d; skipping auto-start.\n", host, port)
		return true
	}
	if err := packaging.SpawnDetached(binary, cfgPath, logPath); err != nil {
		fmt.Fprintf(stderr, "auto-start failed: %v\n", err)
		fmt.Fprintf(stderr, "Launcher is installed — the bridge will start at next logon. Or run:\n  %s serve --config %s\n", binary, cfgPath)
		return false
	}
	return true
}

// printFutureLaunchHint tells the operator how the bridge is going to
// come up next time — the asymmetry we're fixing is that Windows init
// used to leave them with a dead port and no explanation. Per-mode
// copy; macOS / Linux get a shorter note since their service managers
// make this self-evident.
func printFutureLaunchHint(stdout io.Writer, choice launchChoice, binary, cfgPath string) {
	fmt.Fprintln(stdout)
	switch runtime.GOOS {
	case "windows":
		switch {
		case choice.useService:
			fmt.Fprintln(stdout, "How it'll start in the future:")
			fmt.Fprintln(stdout, "  • Automatically at boot (Windows Service, delayed-start)")
			fmt.Fprintln(stdout, "  • Survives logout — always on")
			fmt.Fprintf(stdout, "  • To stop: `sc stop %s` from an elevated shell\n", packaging.ServiceLabel)
		case choice.spawnNow:
			fmt.Fprintln(stdout, "How it'll start in the future:")
			fmt.Fprintln(stdout, "  • Automatically when you log in (Startup-folder launcher)")
			fmt.Fprintf(stdout, "  • To stop now: close the minimized \"1-bit-bridge\" window, or End Task in Task Manager\n")
			fmt.Fprintf(stdout, "  • To start manually any time: %s serve --config %s\n", binary, cfgPath)
		default:
			fmt.Fprintln(stdout, "How it'll start in the future:")
			fmt.Fprintln(stdout, "  • Automatically when you next log in (Startup-folder launcher)")
			fmt.Fprintf(stdout, "  • To start right now: %s serve --config %s\n", binary, cfgPath)
		}
	case "darwin":
		fmt.Fprintln(stdout, "How it'll start in the future:")
		fmt.Fprintln(stdout, "  • Automatically at login (launchd user agent, already running)")
		fmt.Fprintf(stdout, "  • To stop: `launchctl bootout gui/$UID ~/Library/LaunchAgents/%s.plist`\n", packaging.ServiceLabel)
	case "linux":
		fmt.Fprintln(stdout, "How it'll start in the future:")
		fmt.Fprintln(stdout, "  • Automatically at login (systemd user unit, already running)")
		fmt.Fprintf(stdout, "  • To stop: `systemctl --user stop %s.service`\n", packaging.ServiceLabel)
	}
}

// --- helpers ---

func ask(r *bufio.Reader, w io.Writer, prompt, def string) string {
	if def != "" {
		fmt.Fprintf(w, "%s [%s]: ", prompt, def)
	} else {
		fmt.Fprintf(w, "%s: ", prompt)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func confirm(r *bufio.Reader, w io.Writer, prompt string, defYes bool) bool {
	hint := "[y/N]"
	if defYes {
		hint = "[Y/n]"
	}
	fmt.Fprintf(w, "%s %s: ", prompt, hint)
	line, err := r.ReadString('\n')
	if err != nil {
		return defYes
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return defYes
	}
	return line == "y" || line == "yes"
}

// expandHome expands a leading "~" to $HOME, because operators paste
// paths copied from terminals and shells usually handle that themselves.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// openInBrowser tries to pop the admin URL in the operator's browser.
// Best-effort — ignore errors so headless machines don't surface a
// confusing failure on a successful init.
//
// Windows uses `cmd /c start "" <url>`: the empty first quoted
// argument is the window title that `start` expects when its first
// positional is itself a quoted string (a URL counts, on paths with
// spaces). Without the empty title, `start` treats the URL as the
// title and silently does nothing.
func openInBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd.exe", "/c", "start", "", url)
	default:
		return
	}
	_ = cmd.Start()
}
