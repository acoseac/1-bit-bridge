// Package logging owns the bridge's structured-logging entry point.
// All `internal/` packages route their telemetry through a
// `Component(name)` logger so user-submitted logs carry a stable,
// grep-friendly `component=<name>` attribute and key/value pairs
// for paths, IDs, error text — diagnosing field issues no longer
// requires regexing concatenated `log.Printf` strings.
//
// Convention:
//   - Every package gets `var logger = logging.Component("scanner")`
//     (or `enricher`, `admin`, `auth`, `manifest`, `api`, `tls`,
//     `updater`).
//   - Levels: `Error` for caught failures (caller logs once and
//     continues or returns), `Warn` for degraded-but-functional
//     state, `Info` for state transitions worth a line in the
//     log, `Debug` for development noise (default handler hides
//     these).
//   - Attribute keys are short and stable: `path`, `mbid`, `addr`,
//     `rows`, `err`. Don't invent per-callsite key names.
//   - CLI commands in `cmd/bridge/` keep `fmt.Fprintf(stdout/stderr)`
//     — those are user output, not telemetry.
//
// The text handler is deliberately the default. JSON is one
// `slog.NewJSONHandler` swap away if/when log shipping needs it,
// but the operator-facing default is still a tail of a service
// log file, where the text handler is more readable.
package logging

import (
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	once sync.Once
	root *slog.Logger
)

// Init configures the root slog logger to write text-format records
// to w. Called once at process startup from `cmd/bridge/main.go`.
// Repeat calls are no-ops — the logger is locked in after the first
// configuration so a misuse from a test can't flip the destination
// underneath an in-flight log line.
//
// nil w falls back to os.Stderr (matches the stdlib `log` default
// behaviour so tests that import a package without calling Init
// still see output).
func Init(w io.Writer) *slog.Logger {
	once.Do(func() {
		if w == nil {
			w = os.Stderr
		}
		handler := slog.NewTextHandler(w, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
		root = slog.New(handler)
		slog.SetDefault(root)
	})
	return root
}

// Component returns a child logger with `component=<name>` attached
// to every record. Each package declares one at package scope:
//
//	var logger = logging.Component("scanner")
//
// then writes `logger.Info("starting scan", "roots", len(roots))`.
//
// Until `Init` is called, Component falls back to a stderr handler
// so package-init-time logs (rare) and tests still see output.
func Component(name string) *slog.Logger {
	if root == nil {
		// Test-time fallback. Don't trigger `once.Do` here — leaving
		// the once unfired lets `Init` still configure the
		// production destination later if main.go runs after a
		// package-init log line (it does, by Go ordering, but
		// tests can import in any order).
		fallback := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
		return slog.New(fallback).With("component", name)
	}
	return root.With("component", name)
}
