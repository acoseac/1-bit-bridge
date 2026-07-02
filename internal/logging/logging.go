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
	"slices"
	"sync"
	"sync/atomic"
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
	// Seed the component attr directly into the handler's op chain rather than
	// slog.New(&dynamicHandler{}).With(...) — the latter allocates an
	// intermediate logger and clones the handler just to add one attr.
	return slog.New(&dynamicHandler{
		ops: []handlerOp{{attrs: []slog.Attr{slog.String("component", name)}}},
	})
}

// dynamicHandler resolves `slog.Default().Handler()` on every
// Handle() / Enabled() call. WithAttrs and WithGroup record the
// caller's intent locally (as an ordered op chain) so chained
// derivations survive the indirection — at Handle time we replay the
// saved ops, in order, onto the live default handler before forwarding.
//
// **Resolution caching**: the WithGroup/WithAttrs replay is the
// expensive part — each step deep-clones the handler tree. Pre-fix,
// every log line produced 1-2 throwaway handler clones for a
// `Component(name)` logger (1 attr from `.With("component", name)`).
// The `cache` field memoizes the fully-resolved chain keyed on the
// `*slog.Logger` pointer returned by `slog.Default()`; on the
// steady-state path (slog.SetDefault rare, log calls common) we hit
// the cache and bypass the clone. Cache invalidation is implicit:
// when slog.SetDefault swaps the default logger, the pointer
// changes and the next Handle() observes a miss + rebuilds.
//
// **Why the cache key is `*slog.Logger`, not `slog.Handler`**: Go
// `==` on interface values panics if the concrete type is not
// comparable (a struct containing a slice / map / func). Standard
// handlers (`*slog.TextHandler`, `*slog.JSONHandler`) are pointers
// so they're safe today, but `dynamicHandler` is a general-purpose
// shim and must accept any handler an operator installs via
// `slog.SetDefault`. `*slog.Logger` is always a pointer (always
// comparable), so the cache key is panic-proof regardless of what
// concrete handler the logger wraps. Caught by Gemini + CodeRabbit
// review on PR #99.
//
// Concurrency: cache is `atomic.Pointer[cachedResolution]` so reads
// are lock-free. Two goroutines racing on a cold cache may both
// rebuild and Store; both produce identical chains (pure function of
// h.ops + the resolved handler) so the loser's wasted work is one
// extra clone — same cost as the pre-fix per-call allocation, applied
// just once instead of every line.
//
// **Enabled() stays uncached** — slog.Handler.Enabled is short-
// circuit cheap on the level filter and doesn't deep-clone. Caching
// it would burn complexity for no gain.
type dynamicHandler struct {
	// ops is the WithGroup/WithAttrs derivation chain in CALL ORDER,
	// replayed against the live default handler at Handle time. Order
	// preserves slog's interleaving contract: an attr added BEFORE a
	// group stays outside it, one added AFTER nests inside. The prior
	// shape kept two flat slices (all groups, all attrs) and replayed
	// groups-then-attrs, which silently pushed a root attr — e.g.
	// Component's `component=…` — inside any later WithGroup.
	ops []handlerOp

	// cache stores the resolved handler chain keyed on the
	// `*slog.Logger` `slog.Default()` returned when we built it.
	// Lock-free reads; cache miss falls through to a rebuild + Store.
	cache atomic.Pointer[cachedResolution]
}

// handlerOp is one step of a dynamicHandler's derivation chain: a WithGroup
// when isGroup, otherwise a WithAttrs. Recording steps in a single ordered
// slice (rather than separate group/attr slices) is what preserves slog's
// group↔attr interleaving.
type handlerOp struct {
	isGroup bool
	name    string      // group name when isGroup
	attrs   []slog.Attr // attrs when !isGroup
}

// cachedResolution pairs a resolved handler chain with the
// `*slog.Logger` it was derived from. `base == slog.Default()` is
// the cache validity check; a slog.SetDefault landing between two
// Handle() calls swaps the pointer and forces a rebuild. Pointer-
// typed key is always comparable — see dynamicHandler doc.
type cachedResolution struct {
	base     *slog.Logger
	resolved slog.Handler
}

func (h *dynamicHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return slog.Default().Handler().Enabled(ctx, level)
}

// logHook is the decoupled observation seam for higher-level packages
// (e.g. internal/metrics) that want to count log events by level
// without forcing internal/logging — the absolute base of the bridge
// dependency tree — to import them.
//
// **Importer direction is one-way**: internal/metrics imports
// internal/logging and registers a callback via RegisterLogHook at
// init(). internal/logging never imports internal/metrics; this
// closes the package-cycle hazard the alternative would create.
//
// **Hot-path placement**: the hook is invoked at function entry,
// BEFORE the cache-hit early return at the top of Handle. Placing
// the invocation after the early return would mean the steady-state
// (cache-hit) path never reaches the counter — every increment
// would happen only on cold-path rebuilds.
//
// The hook stays nil until something registers — meaning the
// logging package compiles + tests in isolation with zero
// dependency on Prometheus or any sibling observability layer.
var logHook atomic.Pointer[func(level string)]

// RegisterLogHook installs a callback invoked on every log emission.
// Typically called from `internal/metrics.init()` to wire the log-
// level counter. Safe to call multiple times — last-writer-wins;
// pass nil to disable.
//
// `atomic.Pointer[func]` load is ~1 ns on amd64/arm64; the hot-path
// cost of the call site below is negligible relative to the
// downstream handler's I/O.
func RegisterLogHook(f func(level string)) {
	if f == nil {
		logHook.Store(nil)
		return
	}
	logHook.Store(&f)
}

func (h *dynamicHandler) Handle(ctx context.Context, r slog.Record) error {
	// Fire the metrics hook FIRST so steady-state cache hits still
	// produce counter increments — placing this below the early
	// return would zero out the hot path's contribution to the
	// counter, defeating the entire reason the hook exists.
	if hookPtr := logHook.Load(); hookPtr != nil {
		(*hookPtr)(r.Level.String())
	}
	logger := slog.Default()
	if c := h.cache.Load(); c != nil && c.base == logger {
		return c.resolved.Handle(ctx, r)
	}
	resolved := logger.Handler()
	for _, op := range h.ops {
		if op.isGroup {
			resolved = resolved.WithGroup(op.name)
		} else {
			resolved = resolved.WithAttrs(op.attrs)
		}
	}
	// Store a fresh resolution. Concurrent rebuilds race on Store;
	// last-writer-wins is fine because every winner produces the
	// same (h.ops + logger)-determined chain.
	h.cache.Store(&cachedResolution{base: logger, resolved: resolved})
	return resolved.Handle(ctx, r)
}

func (h *dynamicHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &dynamicHandler{ops: h.appendOp(handlerOp{attrs: slices.Clone(attrs)})}
}

func (h *dynamicHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &dynamicHandler{ops: h.appendOp(handlerOp{isGroup: true, name: name})}
}

// appendOp returns h.ops with op appended, in a single fresh allocation. It
// never mutates the parent's backing array (sized len+1, copied once), so
// derived handlers can share the parent's ops prefix safely.
func (h *dynamicHandler) appendOp(op handlerOp) []handlerOp {
	ops := make([]handlerOp, len(h.ops)+1)
	copy(ops, h.ops)
	ops[len(h.ops)] = op
	return ops
}
