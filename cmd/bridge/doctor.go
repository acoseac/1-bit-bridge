package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// doctorCmd runs the preflight report. Exit codes:
//
//	0  — no fails (warnings allowed)
//	1  — at least one fail
//	2  — usage error
//
// The command reads whatever config it can find so the LibraryRoots /
// ports are accurate; a missing config isn't an error (users run
// `bridge doctor` before `bridge init`, and the platform / ports /
// service-manager checks work without a config).
func doctorCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to bridge.yaml (default: try the OS-standard location)")
	jsonOut := fs.Bool("json", false, "emit the report as JSON instead of the human-readable table")
	doFix := fs.Bool("fix", false, "best-effort remediation for warn/fail checks that have a known safe fix")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	d := buildDoctorDeps(*cfgPath)
	// The CLI is a short-lived foreground process with no signal-wired
	// scope to inherit, so Background is honest here — what actually
	// bounds this run is doctor's own per-subprocess deadline, which is
	// the fix that matters for the CLI: pre-deadline, `bridge doctor` on
	// a host with a wedged network mount hung in lsof with no output.
	ctx := context.Background()
	report := doctor.Run(ctx, d)

	if *doFix {
		// Best-effort remediation. The set of safely auto-fixable
		// checks is small (mkdir-class items where the only failure
		// mode is "directory missing"); destructive or
		// security-relevant fixes (port reassignment, service
		// re-install, cert rotation) are NEVER attempted from --fix.
		// Per-check fix outcomes go to STDERR when --json is set so
		// the human-readable progress lines don't contaminate the
		// JSON envelope on stdout (Qodo Bug on PR #78 — without this
		// `bridge doctor --json --fix` emits invalid JSON).
		fixOut := stdout
		if *jsonOut {
			fixOut = stderr
		}
		runFixes(fixOut, &report, d)
		// Re-run the checks so the displayed status matches reality.
		report = doctor.Run(ctx, d)
	}

	if *jsonOut {
		if code := writeJSONIndent(stdout, jsonReportEnvelope(report)); code != 0 {
			return code
		}
		// Honour the "exit 1 if any fail" rule even on the JSON
		// path so automation can rely on the exit code without
		// having to jq the report (Qodo Bug post-merge on PR #82).
		// Pre-fix the JSON branch returned 0 unconditionally,
		// silently treating failures as success.
		if report.HasFail() {
			return 1
		}
		return 0
	}
	printReport(stdout, report)
	if report.HasFail() {
		fmt.Fprintln(stdout, "\nfix the fail(s) above and re-run `bridge doctor`, or `bridge init --skip-doctor` to bypass (not recommended).")
		return 1
	}
	fmt.Fprintln(stdout, "\nall clear.")
	return 0
}

// doctorJSONSchemaVersion versions the `bridge doctor --json` envelope so
// orchestrators / CI consumers can branch on shape changes. Bump it only
// when a field is renamed/removed or its meaning changes (additive,
// omitempty-gated fields don't need a bump). Independent of the v1 wire
// protocol — this is a CLI report, not a paired-client surface.
const doctorJSONSchemaVersion = 1

// jsonDoctorReport is the wire shape of `bridge doctor --json`. The
// envelope is intentionally simple — self-describing metadata, a flat list
// of checks, plus summary counts — so jq-style scripting and container
// health-probes are one filter away. The per-check fields mirror
// doctor.Check exactly so a future admin-API surface can re-use the same
// shape without translation.
type jsonDoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Hint    string `json:"hint,omitempty"`
}

type jsonDoctorReport struct {
	SchemaVersion int               `json:"schemaVersion"`
	GeneratedAt   string            `json:"generatedAt"` // RFC3339, UTC
	Platform      string            `json:"platform"`    // runtime.GOOS
	Arch          string            `json:"arch"`        // runtime.GOARCH
	Checks        []jsonDoctorCheck `json:"checks"`
	OK            int               `json:"ok"`
	Warn          int               `json:"warn"`
	Fail          int               `json:"fail"`
}

func jsonReportEnvelope(r doctor.Report) jsonDoctorReport {
	out := jsonDoctorReport{
		SchemaVersion: doctorJSONSchemaVersion,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Platform:      runtime.GOOS,
		Arch:          runtime.GOARCH,
		Checks:        make([]jsonDoctorCheck, 0, len(r.Checks)),
		OK:            r.OKCount(),
		Warn:          r.WarnCount(),
		Fail:          r.FailCount(),
	}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, jsonDoctorCheck{
			Name:    c.Name,
			Status:  string(c.Status),
			Summary: c.Summary,
			Hint:    c.Hint,
		})
	}
	return out
}

// runFixes attempts safe auto-remediation for the subset of checks
// where we know the fix is mkdir-class (directory missing → create
// it). Anything destructive or security-relevant is OUT of scope;
// the operator stays in the loop for those. Each attempt is reported
// to the operator regardless of outcome.
func runFixes(w io.Writer, r *doctor.Report, d doctor.Deps) {
	fmt.Fprintln(w, "doctor --fix: attempting safe remediations…")
	for _, c := range r.Checks {
		if c.Status == doctor.OK {
			continue
		}
		switch c.Name {
		case "config-dir":
			if d.ConfigDir == "" {
				continue
			}
			// 0o700 to match init.go — the dir holds bridge.yaml
			// (TLS fingerprint, library paths) plus the data
			// subtree (TLS private key, tokens.json, manifest DB).
			// 0o755 would let other host users read the
			// fingerprint and library layout. Windows ignores
			// the mode and relies on per-user-profile NTFS ACLs
			// at %LOCALAPPDATA%.
			if err := os.MkdirAll(d.ConfigDir, 0o700); err != nil {
				fmt.Fprintf(w, "  ✗ %s: mkdir %s: %v\n", c.Name, d.ConfigDir, err)
				continue
			}
			// MkdirAll alone doesn't guarantee the effective mode
			// is 0o700: umask narrows fresh creates and existing-
			// directory mode is preserved. Follow up with an
			// explicit Chmod so the operator-facing "created
			// (0700)" line isn't a lie (CodeRabbit Major
			// post-merge on PR #82). Chmod failure is non-fatal
			// — surface it but keep the ✓ since the dir exists.
			if err := os.Chmod(d.ConfigDir, 0o700); err != nil {
				fmt.Fprintf(w, "  ⚠ %s: created %s but chmod 0700 failed: %v\n", c.Name, d.ConfigDir, err)
				continue
			}
			fmt.Fprintf(w, "  ✓ %s: created %s (0700)\n", c.Name, d.ConfigDir)
		default:
			// No safe auto-fix declared for this check. Skip silently
			// to keep the operator's eye on the fixes we DID attempt.
		}
	}
}

// We need the JSON helper from status.go's writeJSONIndent — declared
// there.
var _ = json.Marshal // keep encoding/json import used (helper lives in status.go)

// buildDoctorDeps resolves the doctor.Deps from the environment:
// config (if readable) plus per-OS default dirs. A best-effort
// resolution is fine — doctor's checks handle missing inputs with
// warn-level messages rather than outright failures.
func buildDoctorDeps(cfgPath string) doctor.Deps {
	d := doctor.Deps{
		APIPort:   7788,
		AdminPort: 7789,
	}
	// Config dir: either derived from --config, or the OS default.
	if cfgPath != "" {
		d.ConfigDir = filepath.Dir(cfgPath)
	} else if dir, err := packaging.DefaultConfigDir(); err == nil {
		d.ConfigDir = dir
	}
	// If a config file exists at the derived path, pull LibraryRoots /
	// ports / dataDir from it so doctor's checks are accurate.
	candidatePath := cfgPath
	if candidatePath == "" && d.ConfigDir != "" {
		candidatePath = filepath.Join(d.ConfigDir, "bridge.yaml")
	}
	if candidatePath != "" {
		if cfg, err := config.Load(candidatePath); err == nil {
			d.DataDir = cfg.DataDir
			d.LibraryRoots = cfg.LibraryRoots
			d.LibraryWatchEnabled = cfg.LibraryWatch.Enabled
			d.UpscaleEnabled = cfg.Upscale.Enabled
			d.AnalysisEnabled = cfg.Analysis.Enabled
			d.FingerprintEnabled = cfg.Fingerprint.Enabled
			// Presence only — the doctor report must never carry the key.
			d.FingerprintHasAPIKey = cfg.Fingerprint.ResolvedAPIKey() != ""
			if host, port, ok := splitHostPort(cfg.ListenAddress); ok {
				_ = host
				d.APIPort = port
			}
			if host, port, ok := splitHostPort(cfg.AdminAddress); ok {
				_ = host
				d.AdminPort = port
			}
			// `bridge serve` writes this file while it runs
			// (writeServerPIDFile, keyed off the same DataDir), so on a
			// live install the "is it us?" branch resolves. It is absent
			// when the bridge is stopped, which is the correct time for a
			// bound port to read as someone else's.
			//
			// NOTE: attribution can still fail on a binary granted
			// cap_net_bind_service — dumpable=0 denies port→pid lookup to
			// an unprivileged observer — which is why checkPort degrades
			// to Warn on a live pid and consults /proc/net/tcp on Linux.
			// The in-process caller sidesteps all of that via
			// Deps.OwnedPorts.
			d.OwnPIDFile = filepath.Join(cfg.DataDir, "server.pid")
		}
	}
	return d
}

// splitHostPort is a tiny wrapper around net.SplitHostPort that returns
// ok=false on error so callers can branch cleanly without a separate
// err dance.
func splitHostPort(addr string) (host string, port int, ok bool) {
	if addr == "" {
		return "", 0, false
	}
	h, p, err := splitHostPortRaw(addr)
	if err != nil || p == 0 {
		return "", 0, false
	}
	return h, p, true
}

// printReport emits a fixed-width formatted table. Each line is
// [status] name: summary, with hints indented on the next line when
// present. The goal is for the user to paste the output into an issue
// and have enough context to act.
func printReport(w io.Writer, r doctor.Report) {
	fmt.Fprintln(w, "1-bit-bridge preflight:")
	fmt.Fprintln(w)
	for _, c := range r.Checks {
		badge := badgeForStatus(c.Status)
		fmt.Fprintf(w, "  %s %-18s  %s\n", badge, c.Name, c.Summary)
		if c.Hint != "" && c.Status != doctor.OK {
			fmt.Fprintf(w, "    ↳ %s\n", c.Hint)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %d ok, %d warn, %d fail\n",
		r.OKCount(), r.WarnCount(), r.FailCount())
}

func badgeForStatus(s doctor.Status) string {
	// On a colour-capable TTY use a glyph + ANSI colour; everywhere
	// else (NO_COLOR, piped, dumb terminal) keep the original
	// fixed-width plaintext badges so existing CI / log scrapers
	// keep parsing. The width matters for column alignment in the
	// printReport %-18s formatter — `colorEnabled() == true` keeps
	// the same visual width by emitting the ANSI escape around a
	// fixed-width plaintext payload.
	if !colorEnabled() {
		switch s {
		case doctor.OK:
			return "[ok]  "
		case doctor.Warn:
			return "[warn]"
		case doctor.Fail:
			return "[FAIL]"
		default:
			return "[?]   "
		}
	}
	switch s {
	case doctor.OK:
		return paint(ansiGreen, "[ok]  ")
	case doctor.Warn:
		return paint(ansiYellow, "[warn]")
	case doctor.Fail:
		return paint(ansiRed, "[FAIL]")
	default:
		return "[?]   "
	}
}

// ensureNotInitialized is a helper consumed by `bridge init` before it
// starts touching the filesystem. Returns 0 when doctor is clean, 1
// otherwise; printing the report to the passed writer. Separate from
// doctorCmd so the init path can pre-seed Deps with the values its
// prompts produced, rather than re-reading a config that might not
// exist yet.
//
// Caller decides the exit action — we just return the code.
func ensureDoctorClean(w io.Writer, d doctor.Deps) int {
	// Background for the same reason as doctorCmd: `bridge init` is a
	// short-lived foreground process, and the per-subprocess deadline
	// inside doctor is what keeps a wedged mount from hanging preflight.
	report := doctor.Run(context.Background(), d)
	if report.HasFail() {
		printReport(w, report)
		return 1
	}
	return 0
}

// `bridge doctor` uses os.Stdin but never reads; keep the signature
// consistent with the other cmds for testability.
var _ = os.Stdin

// adminDoctorRunner returns the closure admin.Deps.DoctorRun is wired
// with, so the console runs the SAME checks as `bridge doctor` from the
// SAME Deps assembly. Duplicating buildDoctorDeps in internal/admin would
// give the two surfaces separate notions of what to check, and the one
// that drifts is the one nobody runs from a shell.
//
// ownedPorts are the ports THIS process bound. Passing them lets the
// port checks be answered from knowledge instead of deduced from a bind
// probe — which is not merely faster: in-process the probe can only
// fail, because the port really is in use, by us.
func adminDoctorRunner(cfgPath string, ownedPorts []int) func(context.Context) *admin.DoctorReport {
	return func(ctx context.Context) *admin.DoctorReport {
		d := buildDoctorDeps(cfgPath)
		d.OwnedPorts = ownedPorts
		rep := doctor.Run(ctx, d)
		out := &admin.DoctorReport{
			Checks: make([]admin.DoctorCheck, 0, len(rep.Checks)),
			OK:     rep.OKCount(),
			Warn:   rep.WarnCount(),
			Fail:   rep.FailCount(),
		}
		for _, c := range rep.Checks {
			out.Checks = append(out.Checks, admin.DoctorCheck{
				Name:    c.Name,
				Status:  string(c.Status),
				Summary: c.Summary,
				Hint:    c.Hint,
			})
		}
		return out
	}
}

// ownedListenPorts returns the TCP ports this serve process binds, for
// doctor.Deps.OwnedPorts.
//
// Derived from the config the running server was started with rather than
// from the listeners themselves: the two agree by construction (runServe
// binds these very addresses and fails to start otherwise), and reaching
// into the live listeners would mean threading them through the admin
// wiring for a diagnostic.
//
// A port that failed to parse is simply omitted — the check then falls
// back to the normal probe path, which is the correct degradation.
func ownedListenPorts(cfg *config.Config) []int {
	if cfg == nil {
		return nil
	}
	var out []int
	for _, addr := range []string{cfg.ListenAddress, cfg.AdminAddress} {
		if _, port, ok := splitHostPort(addr); ok {
			out = append(out, port)
		}
	}
	return out
}
