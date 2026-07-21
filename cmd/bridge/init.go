package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"net"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
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
// baseConfig builds the minimal loopback-mode config shared by two
// writers: `bridge init` (as its base, before any --public mutations) and
// serve's --init-if-missing auto-init. Values are the loopback defaults;
// callers layer mode-specific fields on top. Kept as one helper so the two
// seed paths can't drift.
func baseConfig(roots []string, name, dataDir string) *config.Config {
	return &config.Config{
		LibraryRoots:    roots,
		ListenAddress:   config.DefaultListenAddress,
		AdminAddress:    config.DefaultAdminAddress,
		DataDir:         dataDir,
		ScanIntervalSec: config.DefaultScanIntervalSec,
		LibraryName:     name,
	}
}

func initCmd(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgDirFlag := fs.String("dir", "", "config directory (default per-OS standard)")
	nonInteractive := fs.Bool("yes", false, "accept all defaults without prompting")
	fs.BoolVar(nonInteractive, "y", *nonInteractive, "alias for --yes")
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
	// PR 5: public-VPS deployment posture flags.
	publicMode := fs.Bool("public", false, "configure as a public-VPS deployment (admin auth, no mDNS, no Tailscale by default)")
	publicDomain := fs.String("domain", "", "public hostname iOS clients dial (required with --public)")
	publicEmail := fs.String("email", "", "ACME contact email for Let's Encrypt (required with --public)")
	publicAdminAddress := fs.String("admin-address", "", "with --public: bind address for the admin console (e.g. 0.0.0.0:7789)")
	publicListenAddress := fs.String("listen-address", "", "with --public: bind for the iOS-facing API (default :443 for ACME)")
	publicProxy := fs.Bool("admin-tls-proxy", false, "with --public: a reverse proxy (Caddy/nginx) fronts admin TLS — disables native ACME wrapping of the admin listener")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *publicMode {
		if *publicDomain == "" {
			fmt.Fprintf(stderr, "--public requires --domain <fqdn>\n")
			return 2
		}
		if *publicEmail == "" && !*publicProxy {
			// Email is required ONLY when the bridge will run
			// autocert itself; reverse-proxy installs let the
			// proxy own ACME entirely.
			fmt.Fprintf(stderr, "--public requires --email <addr> (used for Let's Encrypt account registration)\n")
			return 2
		}
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
	//
	// Public mode (PR 5): library root is OPTIONAL. The
	// realistic VPS flow is `bridge init --public ...` first,
	// then the operator mounts their rclone B2 FUSE volume and
	// adds the root via the admin console once it's up. Forcing
	// the operator to pre-mount before init creates a chicken-
	// and-egg: rclone systemd units commonly use `After=
	// 1-bit-bridge.service` to read the bridge's config for
	// mount points. Either order should work without manual
	// gymnastics.
	// abs is the resolved library root, or empty for public-mode
	// installs that defer mount setup. Doctor preflight is
	// skipped when there's no root to preflight against —
	// public-mode operators run `bridge doctor` separately after
	// mounting their storage.
	var abs string
	switch {
	case *libraryRoot != "":
		// Value supplied via --library (interactive or --yes): validate
		// once and hard-error on a bad path (exit 1). Automation must pass
		// a real directory — re-prompting wouldn't help a non-interactive
		// caller.
		a, verr := resolveLibraryDir(*libraryRoot)
		if verr != nil {
			fmt.Fprintf(stderr, "%v\n", verr)
			return 1
		}
		abs = a
	case *publicMode:
		// Public-mode installs defer mount setup — no library root now.
	case *nonInteractive:
		fmt.Fprintf(stderr, "--yes requires --library <path>\n")
		return 2
	default:
		// Interactive: prompt with a bounded re-prompt loop so a single
		// paste typo doesn't abort the whole init (the operator retypes).
		a, code := promptLibraryDir(in, stdout, stderr)
		if code != 0 {
			return code
		}
		abs = a
	}

	// Preflight. Run after library-path resolution so doctor sees the
	// real path the user chose, not a default. --skip-doctor bypasses
	// for the rare case where the operator knows better than the check
	// (say: doctor fails on port 7789 bound by an existing bridge and
	// you're re-running init on purpose to rewrite config).
	if !*skipDoctor && abs != "" {
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
				return finishInit(in, *nonInteractive, stdout, stderr, cfgPath, dataDir, keepChoice)
			}
		} else {
			if !confirm(in, stdout, "Config file exists. Overwrite?", false) {
				fmt.Fprintf(stdout, "keeping existing config\n")
				keepChoice := resolveLaunchChoice(in, stdout, *nonInteractive, *skipService, *windowsService, *startNow)
				return finishInit(in, *nonInteractive, stdout, stderr, cfgPath, dataDir, keepChoice)
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
	//
	// MkdirAll preserves existing-dir mode, so re-runs over a 0o755
	// install would otherwise leave the broader perms in place. The
	// follow-up Chmod is what actually hardens upgrades. Chmod errors
	// are non-fatal: if perms can't be tightened (e.g. the dir is on
	// a filesystem that ignores POSIX modes, or Windows ACLs differ)
	// we still want init to succeed — surface a warning.
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "mkdir config dir: %v\n", err)
		return 1
	}
	if err := os.Chmod(cfgDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "warning: chmod config dir: %v\n", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "mkdir data dir: %v\n", err)
		return 1
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "warning: chmod data dir: %v\n", err)
	}

	var roots []string
	if abs != "" {
		roots = []string{abs}
	}
	cfg := baseConfig(roots, name, dataDir)
	if *publicMode {
		// Public-mode YAML shape (PR 5). Defaults:
		//   listenAddress: :443       (ACME prerequisite — TLS-ALPN-01)
		//   adminAddress:  :7789      (operator can override via --admin-address)
		//   tailscale.mode: disabled  (applyDefaults sets this; explicit
		//                              for readability of saved YAML)
		//   mdns.enabled:   false     (no LAN to advertise on)
		//   customEndpoints: [the autocert domain]
		//   autocert: { enabled, domain, email } OR
		//             { domain } + AdminTLSTerminatedByProxy when --admin-tls-proxy
		cfg.Deployment.Mode = "public"
		cfg.ListenAddress = ":443"
		if *publicListenAddress != "" {
			cfg.ListenAddress = *publicListenAddress
		}
		// Admin bind default depends on TLS posture
		// (CodeRabbit Major review post-PR-#295). Operator
		// override via --admin-address ALWAYS wins.
		//   - autocert-direct-TLS (no --admin-tls-proxy):
		//     defaults 0.0.0.0:7789 so the operator's iOS
		//     management surface can reach it from any
		//     interface. The TLS wrap via certManager is the
		//     trust boundary.
		//   - --admin-tls-proxy: defaults 127.0.0.1:7789. The
		//     reverse proxy talks to it on loopback; the
		//     bridge MUST NOT serve plain-HTTP admin on
		//     non-loopback interfaces (would leak session
		//     cookies / login creds if firewall is mis-wired
		//     or proxy is briefly down). The 0.0.0.0 default
		//     pre-fix was an unsafe shape.
		if *publicProxy {
			cfg.AdminAddress = "127.0.0.1:7789"
		} else {
			cfg.AdminAddress = "0.0.0.0:7789"
		}
		if *publicAdminAddress != "" {
			cfg.AdminAddress = *publicAdminAddress
		}
		cfg.Autocert.Domain = *publicDomain
		// customEndpoint: append the listen port unless it's
		// :443 (port 443 is the https default — iOS dials
		// "https://host" same as "https://host:443"). Keeps
		// the saved YAML compact for the standard ACME case.
		domainURL := "https://" + *publicDomain
		_, port, splitErr := net.SplitHostPort(cfg.ListenAddress)
		if splitErr == nil && port != "" && port != "443" {
			domainURL = "https://" + *publicDomain + ":" + port
		}
		cfg.CustomEndpoints = []string{domainURL}
		cfg.Tailscale.Mode = string(config.TailscaleModeDisabled)
		falseVal := false
		cfg.MDNS.Enabled = &falseVal
		if *publicProxy {
			cfg.Deployment.AdminTLSTerminatedByProxy = true
		} else {
			cfg.Autocert.Enabled = true
			cfg.Autocert.Email = *publicEmail
		}
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
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
	// First-mint at `bridge init` time picks up the broader SAN set
	// so the cert covers every URL the bridge will advertise from
	// the very first serve. Re-init against an existing cert leaves
	// the on-disk cert untouched (LoadOrGenerate path) and emits the
	// SAN-stale warning if the operator's CustomEndpoints changed.
	sanCfg := advertise.CertSANConfig{CustomEndpoints: cfg.CustomEndpoints}
	opts := servertls.GenerateOptions{
		Hostname:      host,
		ExtraDNSNames: advertise.GatherCertSANDNS(sanCfg),
		ExtraIPs:      advertise.GatherCertSANIPs(sanCfg),
	}
	if _, fp, err := servertls.LoadOrGenerateWithOptions(certPath, keyPath, opts); err != nil {
		fmt.Fprintf(stderr, "TLS cert: %v\n", err)
		return 1
	} else if !*publicMode {
		// Box the fingerprint so it stands out from the surrounding
		// init narration. Operators have to copy this exact string
		// to the iOS side at pairing time; framing it makes the
		// "this is the bit you need" beat unmissable.
		//
		// NEVER truncate the fingerprint — a SHA-256 colon-separated
		// hex is 95 chars and operators copy it byte-for-byte to the
		// iOS pin. splitFingerprint splits on a colon boundary so
		// concatenating the halves yields the original verbatim.
		//
		// Public mode skips this entirely: iOS clients on the public
		// path validate the publicly-trusted LE cert via standard
		// ATS rather than pinning, so the fingerprint isn't load-
		// bearing for the pairing flow.
		first, second := splitFingerprint(fp)
		fmt.Fprint(stdout, "\n")
		fmt.Fprint(stdout, box("TLS fingerprint", []string{
			"Pin this on the iOS side. Stable across restarts:",
			"",
			"  " + first,
			"  " + second,
		}))
	}

	// PR 5: public-mode admin credentials. Mint the initial
	// password + display it ONCE in a framed box. The
	// adminauth.Store persists only the bcrypt hash; the
	// plaintext is reachable nowhere else after this banner
	// scrolls off-screen.
	if *publicMode {
		storePath := filepath.Join(dataDir, "adminauth.json")
		store, err := adminauth.OpenStore(storePath)
		if err != nil {
			fmt.Fprintf(stderr, "adminauth: %v\n", err)
			return 1
		}
		plaintext, err := store.MintInitial("admin")
		if err != nil {
			// ErrAlreadyInitialised: the operator re-ran init
			// against an existing store. Tell them how to
			// recover rather than continuing with stale creds.
			fmt.Fprintf(stderr, "adminauth: %v\n", err)
			fmt.Fprintf(stderr, "  (existing admin credentials found at %s; run `bridge admin reset-password` to rotate)\n", storePath)
			return 1
		}
		fmt.Fprint(stdout, "\n")
		fmt.Fprint(stdout, box("Admin credentials — shown ONCE", []string{
			"Save these now. The plaintext is not stored anywhere.",
			"",
			"  Username:  admin",
			"  Password:  " + plaintext,
			"",
			"Rotate with:  bridge admin reset-password",
		}))
	}

	choice := resolveLaunchChoice(in, stdout, *nonInteractive, *skipService, *windowsService, *startNow)
	return finishInit(in, *nonInteractive, stdout, stderr, cfgPath, dataDir, choice)
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
		// `ask` itself appends `[def]:` to the prompt, so the prompt
		// must NOT already contain `[1]` — otherwise the rendered
		// line is `Choose [1] [1]:` (the original transcript bug).
		choice := ask(in, stdout, "Choose", "1")
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
func finishInit(in *bufio.Reader, nonInteractive bool, stdout, stderr io.Writer, cfgPath, dataDir string, choice launchChoice) int {
	// Load the config once up-front so the admin-address probe (Windows
	// auto-start path) and the browser-open at the end both use the
	// operator-configured bind address, not the hard-coded default.
	// `spawnNowOrWarn` previously probed `config.DefaultAdminAddress`
	// directly — on a non-default admin_address, the "already running"
	// check always missed, the second process tried to bind the
	// configured port, and the log filled with port-bind errors.
	adminAddr := config.DefaultAdminAddress
	var loadedCfg *config.Config
	if cfg, err := config.Load(cfgPath); err == nil {
		loadedCfg = cfg
		if cfg.AdminAddress != "" {
			adminAddr = cfg.AdminAddress
		}
	}

	// `printAdmin` is called once per mode with per-mode copy. On
	// Windows we also pass whether the server is expected to be live
	// right now — if it is, we poll briefly before opening the browser
	// so the user doesn't hit "site can't be reached" on a fast machine
	// that outran the cmd.exe handoff.
	//
	// Two URLs in play (kept distinct on purpose):
	//   - browseURL: what the operator types into a browser. Derived
	//     from `operatorAdminURL` so a public-mode install prints
	//     `https://<domain>[:port]/` (autocert direct-TLS) or
	//     `https://<domain>/` (reverse-proxy), NOT the literal bind
	//     target like `http://0.0.0.0:7789/` which is dial-broken
	//     from any other host. Loopback installs keep the historical
	//     `http://<adminAddress>/` shape.
	//   - probeURL semantics live on `adminAddr` directly: the
	//     listen-port probe before browser-open uses the bind
	//     target because that's where the local listener actually
	//     binds (proxy/autocert layer wraps it on top). Auto-
	//     opening a browser on the server's local desktop only
	//     fires when the bridge is starting in this process tree
	//     (Windows skip-service "open in cmd.exe" path); on a
	//     headless VPS this branch never runs.
	//
	// CodeRabbit/Gemini followup post-PR-#296.
	printAdmin := func(serverIsLive bool) {
		// Loopback installs default to http; the helper picks
		// https for public modes regardless of the override slot.
		browseURL := operatorAdminURL(loadedCfg, "http")
		if serverIsLive {
			if host, port, ok := splitHostPort(adminAddr); ok {
				_ = packaging.WaitForListen(host, port, 2*time.Second)
			}
		}
		fmt.Fprintf(stdout, "\nAdmin console: %s\n", browseURL)
		if serverIsLive {
			openInBrowser(browseURL)
		}
		fmt.Fprintf(stdout, "\nDone. Open the admin console to add library folders and pair iOS devices.\n")
	}

	// Resolve the running-binary path once up-front. Used by every
	// downstream branch (skipService handoff, service-install fallback,
	// future-launch hint). os.Executable can fail in unusual environments
	// (deleted binary mid-run, embedded test) — fall back to argv[0] so
	// later prints still surface a useful command. EvalSymlinks resolves
	// /usr/local/bin/bridge → /opt/homebrew/Cellar/... so the printed
	// command points at the real binary, not the launcher symlink.
	binary, err := os.Executable()
	if err != nil || binary == "" {
		binary = os.Args[0]
	}
	if resolved, lerr := filepath.EvalSymlinks(binary); lerr == nil {
		binary = resolved
	}

	if choice.skipService || (runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows") {
		// Interactive operators get a "Start it now in this terminal?"
		// prompt so they don't have to copy-paste a path-laden command
		// (the original PowerShell footgun: PS doesn't search CWD,
		// `bridge serve` returns CommandNotFound). Non-interactive
		// (--yes / piped stdin) gets the shell-aware handoff text
		// only — no prompt, preserves automation behavior.
		//
		// Stdin MUST be a real TTY before we prompt. confirm() falls
		// back to its default on a read error, so a piped/closed
		// stdin (e.g. `bridge init < /dev/null`) would otherwise
		// silently auto-start the server with no real consent —
		// flagged on PR review.
		fmt.Fprintln(stdout)
		if !nonInteractive && stdinIsTerminal(in) && confirm(in, stdout, "Start the bridge now in this terminal?", true) {
			// Per-invocation signal scope: Ctrl+C cancels just this
			// serve session, returns control to init's caller. We
			// derive from context.Background() because init wasn't
			// passed a parent ctx; future PRs that thread one in can
			// wire it here without breaking this contract.
			serveCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			fmt.Fprint(stdout, paint(cBrightCyan, "\nStarting the bridge — Ctrl+C to stop.\n\n"))
			return runServe(serveCtx, serveOpts{configPath: cfgPath}, stdout, stderr)
		}
		fmt.Fprint(stdout, shellHandoff(binary, cfgPath))
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
		// We're in the non-service branch — `choice.useService` is
		// false (option 2 is handled in the if-branch above), and
		// `choice.skipService` already returned at line 378. On
		// Windows that means "Startup-folder launcher" regardless of
		// `spawnNow`: both interactive option-1 (`spawnNow == true`)
		// AND non-interactive `bridge init --yes` (no `--service`
		// flag, `spawnNow == false`) are operator-explicit "no SCM"
		// requests. `Install`'s SCM-first auto-elevation would
		// silently install as a Windows Service when running elevated
		// — the resulting status line ("background service (SCM)")
		// wouldn't match the operator's choice. `InstallStartup`
		// bypasses the SCM tier. On macOS/Linux it returns ("", nil)
		// and we fall back to `Install` (launchd / systemd user unit)
		// — those platforms have only one install mode anyway.
		// (CodeRabbit / Gemini on PR #73 — first cut tied this to
		// `choice.spawnNow` and missed the non-interactive default.)
		if runtime.GOOS == "windows" {
			unitPath, err = packaging.InstallStartup(params)
		} else {
			unitPath, err = packaging.Install(params)
		}
		if err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			fmt.Fprintln(stderr, "You can still run the bridge manually:")
			fmt.Fprint(stderr, shellHandoff(binary, cfgPath))
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
		fmt.Fprintln(stderr, "Launcher is installed — the bridge will start at next logon. Or run:")
		fmt.Fprint(stderr, shellHandoff(binary, cfgPath))
		return false
	}
	return true
}

// stdinIsTerminal reports whether the bridge process's stdin is
// connected to a real terminal. Used to gate interactive prompts in
// init so a piped or closed stdin (e.g. `bridge init < /dev/null`,
// CI pipelines) doesn't accept the prompt's default by reading EOF
// out of confirm(). Takes a non-nil *bufio.Reader as a sanity check —
// when callers pass nil they explicitly mean "no interactivity".
func stdinIsTerminal(in *bufio.Reader) bool {
	if in == nil {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// splitFingerprint splits a TLS fingerprint string into two halves
// for two-line display inside a frameWidth-bounded box. Splits at a
// colon boundary near the midpoint so the line break visually
// anchors on a separator rather than inside a hex pair.
//
// Invariant: first + second == input. NEVER truncates — operators
// copy this byte-for-byte to the iOS pairing UI, and a missing
// character silently breaks pairing for every paired client.
//
// When the input contains no colons (a pathological format change
// upstream), splits at the midpoint instead. Still concatenable.
func splitFingerprint(fp string) (first, second string) {
	mid := len(fp) / 2
	for mid < len(fp) && fp[mid] != ':' {
		mid++
	}
	if mid < len(fp) {
		// mid points at a colon; include it on the first line so
		// the second line starts with a fresh hex byte.
		return fp[:mid+1], fp[mid+1:]
	}
	return fp[:len(fp)/2], fp[len(fp)/2:]
}

// futureLaunchHeader is the section header printed for each per-mode
// launch-hint branch in printFutureLaunchHint. Extracted so the literal
// lives in one place across the five `runtime.GOOS` switch arms.
const futureLaunchHeader = "How it'll start in the future:"

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
			fmt.Fprintln(stdout, futureLaunchHeader)
			fmt.Fprintln(stdout, "  • Automatically at boot (Windows Service, delayed-start)")
			fmt.Fprintln(stdout, "  • Survives logout — always on")
			fmt.Fprintf(stdout, "  • To stop: `sc stop %s` from an elevated shell\n", packaging.ServiceLabel)
		case choice.spawnNow:
			fmt.Fprintln(stdout, futureLaunchHeader)
			fmt.Fprintln(stdout, "  • Automatically when you log in (Startup-folder launcher)")
			fmt.Fprintf(stdout, "  • To stop now: close the minimized \"1-bit-bridge\" window, or End Task in Task Manager\n")
			fmt.Fprintln(stdout, "  • To start manually any time:")
			fmt.Fprint(stdout, shellHandoff(binary, cfgPath))
		default:
			fmt.Fprintln(stdout, futureLaunchHeader)
			fmt.Fprintln(stdout, "  • Automatically when you next log in (Startup-folder launcher)")
			fmt.Fprintln(stdout, "  • To start right now:")
			fmt.Fprint(stdout, shellHandoff(binary, cfgPath))
		}
	case "darwin":
		fmt.Fprintln(stdout, futureLaunchHeader)
		fmt.Fprintln(stdout, "  • Automatically at login (launchd user agent, already running)")
		fmt.Fprintf(stdout, "  • To stop: `launchctl bootout gui/$UID ~/Library/LaunchAgents/%s.plist`\n", packaging.ServiceLabel)
	case "linux":
		fmt.Fprintln(stdout, futureLaunchHeader)
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

// maxLibraryPrompts bounds the interactive library-path re-prompt loop so a
// non-TTY stdin that slipped past the menu's TTY gate can't spin forever.
const maxLibraryPrompts = 5

// resolveLibraryDir expands a leading ~, makes the path absolute, and
// verifies it's an existing directory. Returns the absolute path or an
// error describing why the path is unusable. Shared by the --library flag's
// single-shot validation and the interactive re-prompt loop.
func resolveLibraryDir(libRoot string) (string, error) {
	a, err := filepath.Abs(expandHome(libRoot))
	if err != nil {
		return "", fmt.Errorf("resolve library path: %v", err)
	}
	info, err := os.Stat(a)
	if err != nil {
		return "", fmt.Errorf("library path: %v", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", a)
	}
	return a, nil
}

// askLine is an EOF-aware single-line prompt. Unlike ask(), it surfaces
// io.EOF (alongside any data read so far) so the caller's re-prompt loop can
// tell "user closed stdin" (Ctrl+D) from "user pressed Enter on an empty
// line" — conflating them would let a closed pipe spin the retry loop. No
// default value: the loop decides what an empty line means.
func askLine(r *bufio.Reader, w io.Writer, prompt string) (string, error) {
	fmt.Fprintf(w, "%s: ", prompt)
	line, err := r.ReadString('\n')
	return strings.TrimSpace(line), err
}

// promptLibraryDir interactively asks for the library folder, re-prompting
// on an invalid path instead of aborting the whole init — a paste typo is
// the common failure and forcing a full re-run for it is a papercut. Returns
// (abs, 0) on success, or ("", code) to abort. Every abort uses exit 2 (the
// same code as the non-interactive "--yes requires --library" failure) so a
// piped-stdin run fails identically whether or not --yes is set:
//
//   - empty Enter: "library path is required".
//   - closed / unreadable stdin with no input (Ctrl+D, or any other read
//     error): "input closed; aborting." — returned immediately, never
//     spinning to the retry cap.
//   - a non-empty path that arrived together with a read error (typed then
//     Ctrl+D, no newline) is still validated; only if it's also invalid do
//     we abort (no more input to retry with).
//   - exhausted after maxLibraryPrompts invalid tries.
func promptLibraryDir(in *bufio.Reader, stdout, stderr io.Writer) (string, int) {
	for attempt := 0; attempt < maxLibraryPrompts; attempt++ {
		line, err := askLine(in, stdout, "Library folder to expose (absolute path)")
		// Any non-nil error (io.EOF on a closed pipe, or a genuine read
		// fault) means the stream won't yield more — treat it as terminal so
		// a broken reader can't spin the loop to its cap.
		noMoreInput := err != nil
		if line == "" {
			if noMoreInput {
				fmt.Fprintf(stderr, "\ninput closed; aborting.\n")
			} else {
				fmt.Fprintf(stderr, "library path is required\n")
			}
			return "", 2
		}
		abs, verr := resolveLibraryDir(line)
		if verr == nil {
			return abs, 0
		}
		fmt.Fprintln(stderr, paint(ansiRed, "✗ "+verr.Error()))
		if noMoreInput {
			// The bad path came with a closed/broken stream — no more input
			// to retry with, so abort rather than spin to the cap.
			return "", 2
		}
	}
	fmt.Fprintf(stderr, "Too many invalid attempts. Aborting.\n")
	return "", 2
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
