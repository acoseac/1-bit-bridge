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
				return finishInit(stdout, stderr, cfgPath, dataDir, *skipService, *windowsService)
			}
		} else {
			if !confirm(in, stdout, "Config file exists. Overwrite?", false) {
				fmt.Fprintf(stdout, "keeping existing config\n")
				return finishInit(stdout, stderr, cfgPath, dataDir, *skipService, *windowsService)
			}
		}
	}

	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "mkdir config dir: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
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

	return finishInit(stdout, stderr, cfgPath, dataDir, *skipService, *windowsService)
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
func finishInit(stdout, stderr io.Writer, cfgPath, dataDir string, skipService, windowsService bool) int {
	printAdmin := func() {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(stdout, "\nDone. Start the bridge with: bridge serve --config %s\n", cfgPath)
			return
		}
		url := "http://" + cfg.AdminAddress + "/"
		fmt.Fprintf(stdout, "\nAdmin console: %s\n", url)
		openInBrowser(url)
		fmt.Fprintf(stdout, "\nDone. Open the admin console to add library folders and pair iOS devices.\n")
	}

	if skipService || (runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows") {
		fmt.Fprintf(stdout, "\nSkipping service install. Start the bridge with:\n")
		fmt.Fprintf(stdout, "  bridge serve --config %s\n", cfgPath)
		printAdmin()
		return 0
	}

	// --service on non-Windows is a usage error; the flag only makes
	// sense with SCM. Call it out loudly so the user doesn't think
	// they got a Windows-Service-equivalent on their Mac.
	if windowsService && runtime.GOOS != "windows" {
		fmt.Fprintf(stderr, "--service is a Windows-only flag; ignored on %s\n", runtime.GOOS)
		windowsService = false
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
	var unitPath string
	if windowsService {
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
	} else {
		unitPath, err = packaging.Install(params)
		if err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			fmt.Fprintf(stderr, "You can still run the bridge manually:\n  %s serve --config %s\n", binary, cfgPath)
			return 1
		}
		fmt.Fprintf(stdout, "Service installed at:\n  %s\n", unitPath)
	}
	fmt.Fprintf(stdout, "Logs:\n  %s\n", logPath)

	printAdmin()
	return 0
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
func openInBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return
	}
	_ = cmd.Start()
}
