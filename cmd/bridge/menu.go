// The launcher menu shown when `bridge` is run with no arguments on
// a TTY. Reads platform install state via internal/packaging, paints
// a context-aware option list, and dispatches each choice to an
// existing flag-driven entry point (initCmd, runServe, pairCmd, etc.)
// in-process. Non-TTY callers fall through to the existing
// `usage + exit 2` path — automation and CI see zero behavior change.
package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// menuState bundles everything renderMenu and dispatch need to know.
// Resolved once per loop iteration via detectState; the loop calls
// it again after every dispatched action so the user sees an updated
// status line (e.g., after "Install as service" succeeds, the next
// repaint should show ● background service (SCM)).
type menuState struct {
	initialized bool
	cfgPath     string
	kind        packaging.ServiceKind
	isWindows   bool
	isAdmin     bool // Windows: UAC elevation; POSIX: always true
	isRoot      bool // POSIX: euid == 0; Windows: false
	// running reports whether something is answering on the admin
	// address RIGHT NOW, as opposed to `kind`, which only says a
	// service-manager artifact is installed. The two diverge exactly
	// when it matters: a stopped-but-installed bridge still reports its
	// launchd/SCM kind, so the menu used to claim it was up and "Open
	// admin console" opened a connection-refused tab.
	running bool
}

// detectState resolves the menu's view of the world from disk + env.
// Best-effort — InstalledKind / IsAdmin failures degrade to KindNone
// / false rather than killing the menu loop.
func detectState() menuState {
	cfgPath, ok := packaging.IsInitialized()
	kind, _ := packaging.InstalledKind()
	return menuState{
		initialized: ok,
		cfgPath:     cfgPath,
		kind:        kind,
		isWindows:   runtime.GOOS == "windows",
		isAdmin:     packaging.IsAdmin(),
		isRoot:      packaging.IsRoot(),
		// Only worth asking once there is a config to read the admin
		// address out of.
		running: ok && adminRunningProbe(cfgPath),
	}
}

// menuProbeTimeout bounds the run-state dial in detectState.
//
// The probe is SYNCHRONOUS, and deliberately so. A cached value updated
// by a background ticker would be stale exactly when this matters — in
// the moment right after the operator picks Start or Stop, which is the
// transition the run-state display exists to show. The menu is a
// prompt-driven loop rather than a render loop: detectState runs once per
// repaint, immediately after the user pressed Enter, so the worst case is
// a bounded pause before the menu paints, not a frozen UI.
//
// It is bounded tightly because it cannot be assumed to be a loopback
// dial. validateLoopbackAddress sits in the ELSE branch of the config
// validator, so a public-mode bridge may carry a routable admin address —
// and those hosts are firewalled (bridge.ars.md runs ufw), where a DROP
// rule gives a hang rather than an immediate refusal.
const menuProbeTimeout = 150 * time.Millisecond

// adminRunningProbe is the seam tests use to drive detectState's run-state
// without a network. Production code MUST NOT mutate it.
var adminRunningProbe = probeAdminRunning

// probeAdminRunning reports whether the admin port accepts a connection.
//
// Deliberately a bare TCP connect, not an HTTP request: the question is
// "is a process holding this socket", and a bridge that is up but slow to
// answer (mid-scan, cold page cache) must not read as stopped.
// Note it probes cfg.AdminAddress — where the console actually LISTENS —
// which is not always the URL actOpenAdmin displays: in public mode
// operatorAdminURL renders the autocert domain or the reverse-proxy
// origin. That divergence is fine and deliberate. The question here is
// "is the bridge up", and the local bind answers it in every posture,
// whereas dialling the public URL would fold in DNS, TLS and the proxy.
func probeAdminRunning(cfgPath string) bool {
	// adminAddrFromCfg always yields a non-empty address (it falls back to
	// config.DefaultAdminAddress on every failure path), so there is no
	// empty case to guard.
	addr := adminAddrFromCfg(cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), menuProbeTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", probeLoopbackAddr(addr))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// menuOption is one row in the rendered menu. The action closure runs
// in-process when the user picks the row; it returns an exit code if
// the loop should terminate (-1 means "stay in the menu, repaint").
type menuOption struct {
	key    rune
	label  string
	action func(ctx context.Context, in *bufio.Reader, stdout, stderr io.Writer, s menuState) int
}

// optionsFor returns the per-state option list. Pure data — no I/O —
// so the table is unit-testable without a real config dir or service
// install. The order is the visible order; keys are user-typed.
func optionsFor(s menuState) []menuOption {
	switch {
	case !s.initialized:
		return []menuOption{
			{'1', "Setup wizard (init)", actSetup},
			{'2', "Run preflight (doctor)", actDoctor},
			{'Q', "Quit", actQuit},
		}
	case s.kind == packaging.KindNone:
		// Initialized but no service installed.
		opts := []menuOption{
			{'1', "Start the bridge now (this terminal)", actStartNow},
			{'2', installAsServiceLabel(s), actInstallService},
			{'3', "Pair a device", actPair},
			{'4', "Reset / re-run setup wizard", actSetup},
			{'5', "Uninstall (clean local state)", actUninstall},
			{'Q', "Quit", actQuit},
		}
		return opts
	default:
		// Initialized AND a service-manager artifact is installed.
		//
		// Start is offered UNCONDITIONALLY rather than only when the probe
		// says stopped. Two reasons: picking Stop previously left no menu
		// path back up (`bridge start` existed, but nothing in the menu
		// reached it), and gating rows on the probe would mean a probe that
		// is wrong — a bridge answering on an address we can't dial —
		// silently removes the operator's ability to act. The status line
		// carries the run state; the option list stays complete.
		return []menuOption{
			{'1', "Start service", actStart},
			{'2', "Stop service", actStop},
			{'3', "Restart service", actRestart},
			{'4', "Open admin console", actOpenAdmin},
			{'5', "Pair a device", actPair},
			{'6', "Uninstall service", actUninstall},
			{'Q', "Quit", actQuit},
		}
	}
}

// installAsServiceLabel adapts the install row's label to the
// platform's elevation context: on Windows-non-elevated the SCM path
// is gated so we hint that re-launching as Administrator is needed;
// on POSIX-running-as-root we warn that the install will resolve $HOME
// to /root and break the config dir resolution at next user-context
// launch.
func installAsServiceLabel(s menuState) string {
	base := "Install as a background service"
	if s.isWindows && !s.isAdmin {
		return base + "  (Requires Administrator)"
	}
	if !s.isWindows && s.isRoot {
		return base + "  (Warning: installing as root may break config paths)"
	}
	return base
}

// menuLoop is the top-level entry point. Called from main.go's no-args
// branch when stdin AND stdout are both TTYs. Returns when the user
// picks Quit (exit 0) or an action signals termination (e.g. "Start
// now" propagating runServe's exit code).
//
// CRITICAL: ctx here is plain context.Background() (NOT signal-wired).
// Per-invocation signal scoping happens INSIDE individual actions so
// Ctrl+C cancels just the action's serve session and returns to the
// menu — Go contexts can't be un-canceled, so a single shared
// signal ctx would lock out all subsequent invocations.
//
// Termination signals at the menu's input prompt (SIGINT / SIGTERM
// while the loop is blocked in `in.ReadBytes('\n')`) are handled by
// Go's default signal disposition — both default to "terminate the
// process," which is the right UX at a synchronous prompt. We don't
// wire ctx.Done() into the read loop because cooked-mode bufio reads
// don't observe ctx anyway; the reader can't unblock until the user
// presses Enter. PR #65 reviewer flagged the "ctx unused" pattern as
// a possible disconnect from main's signal-wired ctx — the actions
// that DO need cancellation (actStartNow) build their own scope, and
// idle-prompt cancellation is delegated to the runtime's default
// handler intentionally.
func menuLoop(ctx context.Context, in *bufio.Reader, stdout, stderr io.Writer) int {
	for {
		state := detectState()
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, logo(version.ServerVersion, version.ProtocolVersion))
		writeStatusLine(stdout, state)
		opts := optionsFor(state)
		fmt.Fprint(stdout, renderOptions(opts))
		fmt.Fprint(stdout, "\n  ", paint(cBoldMagenta, ">"), " ")
		choice := readMenuChoice(in, opts)
		var picked menuOption
		for _, o := range opts {
			if o.key == choice {
				picked = o
				break
			}
		}
		if picked.action == nil {
			// readMenuChoice returns 0 on invalid / EOF — repaint.
			continue
		}
		code := picked.action(ctx, in, stdout, stderr, state)
		if code >= 0 {
			return code
		}
		// Action wants the loop to continue. Pause briefly so the
		// user can read whatever the action printed before the
		// menu repaints over it.
		fmt.Fprint(stdout, "\n  ", paint(cDim, "Press Enter to return to the menu..."))
		_, _ = in.ReadString('\n')
	}
}

// writeStatusLine renders the two-line status block under the logo:
// initialization + service-kind badge, then the truncated config
// path. Uses paint() colors so a NO_COLOR run still gets readable
// plain text.
func writeStatusLine(w io.Writer, s menuState) {
	initLabel := paint(cDim, "○ not initialized")
	if s.initialized {
		initLabel = paint(cBrightYellow, "● initialized")
	}
	svcLabel := paint(cDim, "○ "+s.kind.Description())
	if s.kind != packaging.KindNone {
		svcLabel = paint(cBrightYellow, "● "+s.kind.Description())
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  status: %s   %s%s\n", initLabel, svcLabel, runStateSuffix(s))
	if s.cfgPath != "" {
		fmt.Fprintf(w, "  config: %s\n", truncateMid(s.cfgPath, frameWidth-12))
	}
}

// runStateSuffix renders the third status badge: whether the bridge is
// actually up, as distinct from installed.
//
// Empty before init, when there is no config to resolve an admin address
// from and "not running" would be noise on a first-run screen. Shown for
// every initialized state INCLUDING KindNone, because "Start the bridge
// now (this terminal)" in another window is a real and common way for it
// to be up without any service installed.
func runStateSuffix(s menuState) string {
	if !s.initialized {
		return ""
	}
	if s.running {
		return "   " + paint(cBrightYellow, "● running")
	}
	return "   " + paint(cDim, "○ not running")
}

// renderOptions paints the `[1] Start the bridge now ...` rows
// inside a single-line frame at frameWidth. Yellow + bold for the
// `[k]` keys; default fg for the labels.
func renderOptions(opts []menuOption) string {
	var lines []string
	for _, o := range opts {
		key := paint(cBrightYellow+cBold, fmt.Sprintf("[%c]", o.key))
		// Two-space gap between key and label so the labels
		// align visually, regardless of color escape lengths.
		lines = append(lines, key+"  "+o.label)
	}
	return frame("what would you like to do?", lines)
}

// readMenuChoice reads a line from the user. Returns the first byte
// of the trimmed input upper-cased, or 0 if the input contained any
// `\x1b` byte (arrow-key swallow — discard the line and re-prompt
// silently). Validates against the option set so an unrecognised
// key falls through to a re-prompt rather than dispatching nil.
//
// Documented limitation: bufio.ReadBytes blocks until newline, so
// a bare ↑ press shows nothing until the user presses Enter (then
// the line containing `\x1b[A` is discarded and the menu repaints).
// Trade-off for not importing golang.org/x/term raw mode — see
// PR #64 plan notes. DO NOT file as "menu hangs on arrow key".
func readMenuChoice(in *bufio.Reader, opts []menuOption) rune {
	line, err := in.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		// EOF or pipe-closed — exit the loop cleanly.
		return 'Q'
	}
	// Any arrow-key (or other CSI) escape sequence in the line ⇒
	// drop the whole line and re-prompt. Returning 0 signals the
	// loop's "no valid pick, repaint" path.
	if bytes.ContainsRune(line, '\x1b') {
		return 0
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return 0
	}
	pick := rune(trimmed[0])
	if pick >= 'a' && pick <= 'z' {
		pick = pick - 'a' + 'A'
	}
	for _, o := range opts {
		if o.key == pick {
			return pick
		}
	}
	return 0
}

// --- per-action handlers ---
//
// Each returns -1 to keep the menu loop running, or a non-negative
// exit code to terminate. Dispatch wraps the handler signature; each
// function delegates to the existing flag-driven entry points so the
// menu is purely a UI shell over the same code paths as the CLI.

// actQuit terminates the loop with exit 0.
func actQuit(_ context.Context, _ *bufio.Reader, stdout, _ io.Writer, _ menuState) int {
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, paint(cDim, "  goodbye."))
	return 0
}

// actSetup runs the existing init wizard. Args is empty so the
// wizard prompts for everything; non-interactive flags (--yes etc.)
// are not exposed via the menu — automation should keep using the
// flag-driven path.
//
// CRITICAL: pass the menu's shared *bufio.Reader through to initCmd,
// not bare os.Stdin. Two bufio.Readers wrapping the same underlying
// fd each maintain their own buffer; if the menu read partially
// buffered the next chunk before dispatching here, the inner
// initCmd reader would miss those bytes and prompt-input would
// desync. initCmd's own `bufio.NewReader(stdin)` over our existing
// bufio.Reader works correctly — the inner reader satisfies its
// reads from the outer's buffer + downstream fd transparently.
func actSetup(_ context.Context, in *bufio.Reader, stdout, stderr io.Writer, _ menuState) int {
	_ = initCmd(nil, in, stdout, stderr)
	return -1
}

// actDoctor runs the preflight against the platform-default paths.
// Useful when the user wants to debug a "doctor failed" before
// committing to a setup wizard.
func actDoctor(_ context.Context, _ *bufio.Reader, stdout, stderr io.Writer, _ menuState) int {
	_ = doctorCmd(nil, stdout, stderr)
	return -1
}

// runServeForMenu is the test seam. Production points it at runServe;
// tests override to assert on the ctx without standing up a real
// HTTPS server. Required so TestServeContextNotShared can verify the
// per-invocation signal-scope contract that protects against the
// "Go contexts can't be un-canceled" footgun documented at the top
// of runServe.
var runServeForMenu = runServe

// menuNoConfigHint is the stderr line printed from every menu action
// that requires a loaded `bridge.yaml` (start now, install service,
// pair device). Three call sites; one literal so the wording stays
// consistent.
const menuNoConfigHint = "  no bridge.yaml found — run Setup wizard first."

// actStartNow drives a foreground serve session. CRITICAL: each
// invocation gets its own signal.NotifyContext scope so a Ctrl+C
// cancels just this serve, not the menu's outer ctx. A second
// invocation works because the previous serveCtx was scoped here.
func actStartNow(_ context.Context, _ *bufio.Reader, stdout, stderr io.Writer, s menuState) int {
	if s.cfgPath == "" {
		fmt.Fprintln(stderr, menuNoConfigHint)
		return -1
	}
	serveCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	fmt.Fprint(stdout, paint(cBrightCyan, "\n  Starting the bridge — Ctrl+C to stop and return to the menu.\n\n"))
	_ = runServeForMenu(serveCtx, serveOpts{configPath: s.cfgPath}, stdout, stderr)
	fmt.Fprint(stdout, paint(cDim, "\n  Bridge stopped. Returning to the menu.\n"))
	return -1
}

// actInstallService routes to the existing packaging.Install path.
// On Windows-non-elevated, the SCM-install option is gated by
// installAsServiceLabel; if the user picks it anyway, we surface the
// elevation error inline rather than letting it crash the menu.
func actInstallService(_ context.Context, in *bufio.Reader, stdout, stderr io.Writer, s menuState) int {
	if s.cfgPath == "" {
		fmt.Fprintln(stderr, menuNoConfigHint)
		return -1
	}
	// POSIX-running-as-root install is destructive (resolves $HOME
	// to /root). Confirm with a typed acknowledgment; bare yes/no
	// is too easy to mash.
	if !s.isWindows && s.isRoot {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, paint(cBrightYellow, "  Running as root will resolve $HOME to /root and"))
		fmt.Fprintln(stdout, paint(cBrightYellow, "  silently break the bridge config dir at next launch."))
		fmt.Fprintln(stdout, paint(cBrightYellow, "  Type INSTALL-AS-ROOT to proceed:"))
		fmt.Fprint(stdout, "  > ")
		line, _ := in.ReadString('\n')
		if strings.TrimSpace(line) != "INSTALL-AS-ROOT" {
			fmt.Fprintln(stdout, "  cancelled.")
			return -1
		}
	}
	if s.isWindows && !s.isAdmin {
		fmt.Fprintln(stdout, paint(cBrightYellow, "\n  SCM install requires Administrator."))
		fmt.Fprintln(stdout, paint(cDim, "  Re-launch this terminal with \"Run as Administrator\","))
		fmt.Fprintln(stdout, paint(cDim, "  then return here and pick this option again."))
		return -1
	}
	binary, err := os.Executable()
	if err != nil || binary == "" {
		binary = os.Args[0]
	}
	if resolved, lerr := filepath.EvalSymlinks(binary); lerr == nil {
		binary = resolved
	}
	// Match init.go's install pattern exactly: WorkingDir is the
	// resolved data dir (launchd / systemd templates embed it as
	// WorkingDirectory), DefaultLogPath errors are surfaced (not
	// swallowed), and the log dir is mkdir'd before Install.
	// Skipping any of these produces a service unit with empty
	// WorkingDirectory or an unwritable log path — symptoms include
	// "service installed" success followed by a daemon that never
	// listens (qodo PR #65 catch).
	logPath, err := packaging.DefaultLogPath()
	if err != nil {
		fmt.Fprintf(stderr, "  resolve log path: %v\n", err)
		return -1
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "  mkdir log dir: %v\n", err)
		return -1
	}
	// dataDir = sibling of cfgPath: <cfgDir>/data per init.go's
	// resolution. Re-deriving here rather than loading the full
	// config keeps the menu lightweight; the resolved path matches
	// what init.go set as cfg.DataDir at write time.
	dataDir := filepath.Join(filepath.Dir(s.cfgPath), "data")
	params := packaging.Params{
		BinaryPath: binary,
		ConfigPath: s.cfgPath,
		WorkingDir: dataDir,
		LogPath:    logPath,
	}
	unitPath, err := packaging.Install(params)
	if err != nil {
		fmt.Fprintf(stderr, "  install failed: %v\n", err)
		return -1
	}
	fmt.Fprintf(stdout, "  service installed: %s\n", unitPath)
	return -1
}

// actPair mints a new bearer token via the existing pair codepath.
// Prompts for the device name first; the underlying pairCmd refuses
// without --name so we have to gather it interactively.
func actPair(_ context.Context, in *bufio.Reader, stdout, stderr io.Writer, s menuState) int {
	if s.cfgPath == "" {
		fmt.Fprintln(stderr, menuNoConfigHint)
		return -1
	}
	fmt.Fprintln(stdout)
	fmt.Fprint(stdout, "  Device name (e.g. \"iPhone 15 Pro\"): ")
	line, _ := in.ReadString('\n')
	name := strings.TrimSpace(line)
	if name == "" {
		fmt.Fprintln(stdout, "  cancelled.")
		return -1
	}
	_ = pairCmd([]string{"--config", s.cfgPath, "--name", name}, stdout, stderr)
	return -1
}

// actUninstall removes the service-manager artifact AND offers to
// wipe the config dir. Two confirms: one for the service uninstall
// (no-op when no service installed), one for the destructive wipe.
//
// The wipe prompt requires the operator to type the literal string
// "WIPE" before any deletion happens — a plain `[y/N]` prefix-match
// on "y" was a fat-finger hazard (PR #N). Mirrors the
// `INSTALL-AS-ROOT` typed-phrase pattern from `actInstallService`.
//
// **Library files are never touched BY THIS COMMAND** — the wipe
// is `os.RemoveAll(cfgDir)` where `cfgDir` is the platform config
// dir (`~/Library/Application Support/1-bit-bridge` on macOS, etc.)
// containing only `bridge.yaml` + the `data/` subdirectory. The
// user-supplied `--library` path lives outside that tree and is
// referenced by yaml text, not joined into any bridge-owned
// filesystem location. The reassurance is printed to the operator
// here because at least one user reached out asking exactly this.
//
// The wording used to make the stronger claim: that the bridge itself
// could not delete library files at all, being read-only by design.
// That stopped being true when the web upload / delete-as-trash
// surface landed — `trash.Manager` unlinks inside a library root,
// gated live on `library.allowDelete`, and runServe calls it "the
// only thing in the bridge that removes library content". Scoping the
// promise to THIS COMMAND keeps the reassurance the operator actually
// asked for while leaving it true.
//
// TestUninstallPromptDoesNotClaimDeletionIsImpossible scans the
// Fprint call sites in this file — not the whole source — because the
// paragraph you are reading has to be able to describe the old claim
// without tripping its own guard.
func actUninstall(_ context.Context, in *bufio.Reader, stdout, stderr io.Writer, s menuState) int {
	fmt.Fprintln(stdout)
	if s.kind != packaging.KindNone {
		fmt.Fprintf(stdout, "  Uninstall the %s? [y/N] ", s.kind.Description())
		line, _ := in.ReadString('\n')
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
			if _, err := packaging.Uninstall(); err != nil {
				fmt.Fprintf(stderr, "  service uninstall failed: %v\n", err)
			} else {
				fmt.Fprintln(stdout, "  service uninstalled.")
			}
		}
	}
	if s.cfgPath != "" {
		// Derive the config dir from `s.cfgPath`'s parent rather
		// than `packaging.DefaultConfigDir()`. The two would diverge
		// when the bridge was initialized via a non-default
		// `--config` path (operator with `bridge serve --config
		// /custom/path/bridge.yaml`); `DefaultConfigDir()` would
		// then point at an unused platform default and the wipe
		// would either no-op or hit the wrong directory. gemini bot
		// review on PR #75 caught the inconsistency — `actSetup`
		// already uses `filepath.Dir(s.cfgPath)` for the data dir
		// (see line ~372).
		cfgDir := filepath.Dir(s.cfgPath)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "  Wipe local config + data dirs?")
		fmt.Fprintln(stdout, "    Will delete:")
		fmt.Fprintf(stdout, "      • %s/ (config, data, certs, tokens)\n", cfgDir)
		fmt.Fprintln(stdout, "    Will NOT touch:")
		fmt.Fprintln(stdout, "      • your music library — this wipe removes only the config dir")
		fmt.Fprintln(stdout, "        above; --library paths live outside it and are not touched")
		fmt.Fprint(stdout, "  Type WIPE to confirm: ")
		line, _ := in.ReadString('\n')
		if strings.TrimSpace(line) == "WIPE" {
			if err := os.RemoveAll(cfgDir); err != nil {
				fmt.Fprintf(stderr, "  wipe failed: %v\n", err)
			} else {
				fmt.Fprintln(stdout, "  config + data dirs removed.")
			}
		} else {
			fmt.Fprintln(stdout, "  cancelled (need exact typed phrase WIPE).")
		}
	}
	return -1
}

// actStart routes to packaging.Start and then confirms, rather than
// asserting, that the service came up.
//
// Mirrors actRestart's shape for the same reason it has one: on Windows
// Startup-folder installs the lifecycle call is "spawn detached", so the
// new process has not bound its admin socket by the time the call
// returns, and an unconditional "service started." is a claim the next
// screen immediately contradicts.
//
// packaging.Start is idempotent ("service should be up"), so picking this
// on an already-running bridge is harmless — which is what lets the menu
// offer it unconditionally instead of gating on the run-state probe.
func actStart(_ context.Context, _ *bufio.Reader, stdout, stderr io.Writer, s menuState) int {
	if err := packaging.Start(); err != nil {
		fmt.Fprintf(stderr, "  start failed: %v\n", err)
		return -1
	}
	// Non-empty by construction — adminAddrFromCfg falls back to
	// config.DefaultAdminAddress on every failure path.
	addr := adminAddrFromCfg(s.cfgPath)
	if waitForListen(addr, 5*time.Second) {
		fmt.Fprintln(stdout, "  service started.")
	} else {
		fmt.Fprintf(stdout, "  start issued, but admin port %s didn't respond within 5s — check the bridge log.\n", addr)
	}
	return -1
}

// actStop / actRestart route to packaging.Stop / Restart.
func actStop(_ context.Context, _ *bufio.Reader, stdout, stderr io.Writer, _ menuState) int {
	if err := packaging.Stop(); err != nil {
		fmt.Fprintf(stderr, "  stop failed: %v\n", err)
		return -1
	}
	fmt.Fprintln(stdout, "  service stopped.")
	return -1
}

func actRestart(_ context.Context, _ *bufio.Reader, stdout, stderr io.Writer, s menuState) int {
	if err := packaging.Restart(); err != nil {
		fmt.Fprintf(stderr, "  restart failed: %v\n", err)
		return -1
	}
	// Verify the restarted process actually came back up. The previous
	// shape printed "service restarted." unconditionally; on Windows
	// Startup-folder installs `Restart` is "kill + spawn detached", so
	// the new process hasn't bound its admin socket yet at this point —
	// users hit "127.0.0.1:7789 refused to connect" immediately after
	// the success message. Probe the admin port and surface the truth.
	addr := adminAddrFromCfg(s.cfgPath)
	if addr != "" && waitForListen(addr, 5*time.Second) {
		fmt.Fprintln(stdout, "  service restarted.")
	} else if addr != "" {
		fmt.Fprintf(stdout, "  service restart issued, but admin port %s didn't respond within 5s — check the bridge log.\n", addr)
	} else {
		// Couldn't resolve admin address (no config). Fall back to
		// the optimistic message since we have nothing to probe.
		fmt.Fprintln(stdout, "  service restarted.")
	}
	return -1
}

// adminAddrFromCfg returns the host:port the admin server listens on.
// Falls back to config.DefaultAdminAddress in two cases (CodeRabbit on
// PR #72): (a) cfgPath unset, (b) config.Load returns ANY error —
// because a typo in an unrelated YAML field (e.g. a missing
// libraryRoots dir) would otherwise silently disable the restart
// probe exactly when the restarted process is most likely to fail.
// The currently-running bridge already loaded the config successfully
// (otherwise we couldn't be here restarting it), so the default port
// is the safe fallback.
func adminAddrFromCfg(cfgPath string) string {
	if cfgPath == "" {
		return config.DefaultAdminAddress
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return config.DefaultAdminAddress
	}
	if cfg.AdminAddress != "" {
		return cfg.AdminAddress
	}
	return config.DefaultAdminAddress
}

// waitForListen polls a TCP address until it accepts a connection or
// the deadline expires. Used by actRestart to confirm the restarted
// bridge process bound its admin socket. 200ms per-attempt timeout
// keeps the user-visible delay tight when the service is healthy;
// 5s overall deadline covers the slow-cold-start case (Windows SCM
// service, bg-service restart).
//
// Uses Dialer.DialContext with a derived deadline so the dial honours
// cancellation cleanly (golangci-lint `noctx` compliant; CodeRabbit on
// PR #72).
func waitForListen(addr string, timeout time.Duration) bool {
	// Loopback-map so the restart health-check isn't fooled into "service
	// didn't respond" by a wildcard admin bind Windows can't dial.
	addr = probeLoopbackAddr(addr)
	overall, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: 200 * time.Millisecond}
	for {
		conn, err := dialer.DialContext(overall, "tcp", addr)
		if err == nil {
			conn.Close()
			return true
		}
		if overall.Err() != nil {
			return false
		}
		// Sleep between attempts but bail out if the overall deadline
		// fires mid-sleep. Without this the function could overshoot
		// the deadline by up to a full poll interval.
		select {
		case <-overall.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// actOpenAdmin opens the local admin console URL in the user's
// browser. Loads cfg.AdminAddress from bridge.yaml so a customised
// admin port reaches the right URL — runServe binds and advertises
// from the same field, and a hardcoded :7789 would 404 whenever the
// operator changed it (PR #65 reviewer catch). Falls back to the
// default if cfg load fails. Best-effort — on a headless host the
// browser-open command fails silently and we just print the URL.
func actOpenAdmin(_ context.Context, _ *bufio.Reader, stdout, _ io.Writer, s menuState) int {
	// Route through operatorAdminURL (the single source of truth for
	// browser routing) so a public-posture bridge prints its real
	// https URL (autocert domain / reverse-proxy) instead of a plain
	// http://<bindAddr> that would break. "http" is the loopback
	// default; the helper forces https for public mode.
	url := operatorAdminURL(nil, "http")
	if s.cfgPath != "" {
		if cfg, err := config.Load(s.cfgPath); err == nil {
			url = operatorAdminURL(cfg, "http")
		} else {
			// Surface the load failure rather than silently opening the
			// default-address URL, which may not match the operator's
			// configured admin address (CodeRabbit on PR #478).
			fmt.Fprintf(stdout, "  (warning: could not load %s: %v; using default admin address)\n", s.cfgPath, err)
		}
	}
	fmt.Fprintf(stdout, "  Admin console: %s\n", url)
	// Don't open a tab that can only say "connection refused". The run
	// state comes from the same repaint that drew the status line, so the
	// message the operator sees here agrees with the badge above it.
	//
	// This prints the URL first and then declines, so an operator whose
	// bridge IS up on an address we couldn't dial still has the link.
	if !s.running {
		fmt.Fprintln(stdout, "  not opening a browser: nothing is answering there yet — pick \"Start service\" first.")
		return -1
	}
	// Delegate rather than re-implementing the per-OS switch. This copy had
	// drifted: it invoked `cmd /c start <url>` WITHOUT the empty
	// window-title argument, so on Windows `start` treated the quoted URL
	// as the title and opened nothing. openInBrowser has always passed it.
	// One implementation means the next platform quirk gets fixed once.
	openInBrowser(url)
	return -1
}
