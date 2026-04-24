package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	if err := fs.Parse(args); err != nil {
		return 2
	}

	d := buildDoctorDeps(*cfgPath)
	report := doctor.Run(d)

	printReport(stdout, report)

	if report.HasFail() {
		fmt.Fprintln(stdout, "\nfix the fail(s) above and re-run `bridge doctor`, or `bridge init --skip-doctor` to bypass (not recommended).")
		return 1
	}
	fmt.Fprintln(stdout, "\nall clear.")
	return 0
}

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
			if host, port, ok := splitHostPort(cfg.ListenAddress); ok {
				_ = host
				d.APIPort = port
			}
			if host, port, ok := splitHostPort(cfg.AdminAddress); ok {
				_ = host
				d.AdminPort = port
			}
			// Pidfile convention mirrors the default-install recipe in
			// CLAUDE.md: `<dataDir>/../server.pid` or `<dataDir>/bridge.pid`.
			// Neither is created by `bridge serve` today (issue for
			// PR-2), so the file is usually absent — doctor then skips
			// the "is it us?" branch and any port bind is a fail. Leave
			// the path set so a future pidfile lands it in the check.
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

// ensureNotInitialized is a helper consumed by `bridge init` before it
// starts touching the filesystem. Returns 0 when doctor is clean, 1
// otherwise; printing the report to the passed writer. Separate from
// doctorCmd so the init path can pre-seed Deps with the values its
// prompts produced, rather than re-reading a config that might not
// exist yet.
//
// Caller decides the exit action — we just return the code.
func ensureDoctorClean(w io.Writer, d doctor.Deps) int {
	report := doctor.Run(d)
	if report.HasFail() {
		printReport(w, report)
		return 1
	}
	return 0
}

// `bridge doctor` uses os.Stdin but never reads; keep the signature
// consistent with the other cmds for testability.
var _ = os.Stdin
