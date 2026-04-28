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
// **Why a dynamic handler under the hood**: Go evaluates package-
// level `var logger = logging.Component(...)` declarations during
// package initialization, BEFORE main()'s `logging.Init(...)` runs.
// A naive Component returning `slog.New(handler)` would capture
// whatever handler exists at init time and stick with it forever —
// post-Init Windows-service-log redirects or operator-supplied
// destinations would never reach the package-level loggers (high-
// priority bot review on PR #77).
//
// We work around that with `dynamicHandler`, a thin shim that
// resolves `slog.Default().Handler()` on every Handle() call. The
// shim remembers attrs and groups locally so chained
// `.With("component", name)` survives the indirection. After
// `logging.Init(w)` calls `slog.SetDefault(...)`, every previously-
// constructed logger naturally picks up the new handler — no
// re-instantiation required.
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
)

var (
	once sync.Once
)

// Init configures slog's default Logger to write text-format records
// to w. Called once at process startup from `cmd/bridge/main.go`.
// Repeat calls are no-ops — the destination is locked in after the
// first configuration so a misuse from a test can't flip the
// destination underneath an in-flight log line.
//
// nil w falls back to os.Stderr.
//
// `Init` mutates `slog.Default()` rather than holding a private
// root: every Component-returned logger reads through `dynamicHandler`,
// which calls `slog.Default().Handler()` at log time. Updating the
// default is what propagates the new handler to every existing
// component logger.
func Init(w io.Writer) *slog.Logger {
	once.Do(func() {
		if w == nil {
			w = os.Stderr
		}
		handler := slog.NewTextHandler(w, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		})
		slog.SetDefault(slog.New(handler))
	})
	return slog.Default()
}

// Component returns a logger that tags every record with
// `component=<name>` and resolves the underlying handler at
// log-time via `slog.Default()`. Safe to call before `Init` —
// calls before Init route to slog's default-default (stderr text
// handler) and pick up the operator-supplied destination on the
// first record after Init runs.
//
//	var logger = logging.Component("scanner")
//	logger.Info("starting scan", "roots", len(roots))
func Component(name string) *slog.Logger {
	return slog.New(&dynamicHandler{}).With("component", name)
}

// dynamicHandler resolves `slog.Default().Handler()` on every
// Handle() / Enabled() call. WithAttrs and WithGroup remember the
// caller's intent locally so chained derivations survive the
// indirection — at Handle time we re-apply the saved
// groups+attrs to the live default handler before forwarding.
//
// Cost: one extra map-deref per log call (slog.Default() reads an
// atomic) plus a linear loop over saved groups/attrs. Both are
// dwarfed by the actual log formatting and write, so this is fine
// for the bridge's log volume (a few records per second under
// load).
type dynamicHandler struct {
	groups []string    // ordered list of WithGroup names
	attrs  []slog.Attr // accumulated WithAttrs additions
}

func (h *dynamicHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return slog.Default().Handler().Enabled(ctx, level)
}

func (h *dynamicHandler) Handle(ctx context.Context, r slog.Record) error {
	handler := slog.Default().Handler()
	for _, g := range h.groups {
		handler = handler.WithGroup(g)
	}
	if len(h.attrs) > 0 {
		handler = handler.WithAttrs(h.attrs)
	}
	return handler.Handle(ctx, r)
}

func (h *dynamicHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	groups := append([]string(nil), h.groups...)
	return &dynamicHandler{groups: groups, attrs: merged}
}

func (h *dynamicHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := append(append([]string(nil), h.groups...), name)
	attrs := append([]slog.Attr(nil), h.attrs...)
	return &dynamicHandler{groups: groups, attrs: attrs}
}
