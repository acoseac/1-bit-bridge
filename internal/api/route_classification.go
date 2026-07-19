// Package api — per-route slow-loris defence via
// `http.ResponseController.SetWriteDeadline`.
//
// The LAN + tsnet `http.Server` instances run with `WriteTimeout: 0`
// (the documented `cmd/bridge/main.go` comment explains why — DSD
// downloads stream multi-GB files over slow links AND the SSE event
// stream is a long-lived connection). Without ANY write deadline, a
// slow-reading client on a bounded endpoint (POST /v1/upscale,
// GET /v1/health, etc.) can hold a server goroutine indefinitely
// while we wait for `Write` to drain.
//
// Per-route `SetWriteDeadline` closes this gap: bounded routes get
// a 60 s budget (covers the slowest legitimate non-streaming
// response even over a Tailscale relay); streaming routes (`/v1/read`,
// `/v1/download`, `/v1/manifest`) and SSE routes (`/v1/events`,
// `/v1/pairing/.../events`) opt out via the `streamingRoute` kind.
//
// The route registry below is the single source of truth. The
// `Handler()` body iterates it; a per-route classification test
// asserts every entry has an explicit kind (no zero-value ambiguity).
package api

import (
	"context"
	"net/http"
	"time"
)

// routeKind classifies a route's response-write semantics:
//
//   - boundedRoute: response is finite and small (<= a few MB).
//     A 60 s SetWriteDeadline applies — generous enough for a 1 MB
//     artwork JPEG over a slow Tailscale relay but tight enough to
//     surface a stuck connection before it accumulates.
//
//   - streamingRoute: response is unbounded (multi-GB downloads,
//     50k-track manifest streams) OR long-lived (SSE event streams).
//     NO write deadline applied. The client is expected to read
//     continuously; a slow reader is a legitimate operational mode,
//     not an attack surface.
//
// Don't pick by lexical guesswork — add new routes to the registry
// below with an explicit kind. The classification test fails on
// any unknown handler-pointer hitting `Handler()`.
type routeKind int

const (
	// boundedRoute is the default for any handler whose response
	// fits inside a 60 s write budget at the slowest supported
	// network (LAN: microseconds; Tailscale relay over mobile:
	// rare-case seconds, never minutes).
	boundedRoute routeKind = iota

	// streamingRoute opts a handler out of the per-route write
	// deadline. Used for /v1/download (multi-GB DSD files),
	// /v1/manifest (100+ MB JSON stream), /v1/read (byte-range
	// reads — typically small but conceptually streaming, and
	// some clients use it for tag-header windows much larger
	// than the bounded-route budget allows under a slow
	// network), and the two SSE routes (/v1/events,
	// /v1/pairing/.../events) whose write lifetime IS the
	// connection lifetime.
	streamingRoute
)

// boundedRouteWriteDeadline is the per-route write budget. 60 s
// covers the slowest legitimate non-streaming response on the
// project's supported network surface (Tailscale relay over
// mobile, fully-buffered admin responses). Set tighter and we
// false-positive on legitimate slow links; set looser and the
// slow-loris attacker holds the goroutine for too long. 60 s is
// the conservative middle.
const boundedRouteWriteDeadline = 60 * time.Second

// upscaleLongOpWriteDeadline is the per-route override for the two
// upscale endpoints whose synchronous server-side work scales with
// library size (POST /v1/upscale's folder walk; DELETE
// /v1/upscale/variants' per-variant fsync'd delete loop). 15 min is
// sized for a 50k-track library on a slow NAS; both routes are authed,
// so the relaxed slow-loris posture is acceptable.
const upscaleLongOpWriteDeadline = 15 * time.Minute

// boundedHandler wraps a handler with `SetWriteDeadline(now + d)`.
// Uses `http.ResponseController` (Go 1.20+) so the deadline travels
// through middleware that wraps the `ResponseWriter` (e.g. our
// `requestLogging` wrapper) — accessing the underlying TCP/TLS conn
// directly via type assertion would break under wrappers.
//
// Failure to set the deadline (older listener type, unsupported
// underlying conn) logs once at debug and proceeds; the route still
// runs unprotected, but no legitimate path on either of our two
// listeners (LAN crypto/tls.Conn, tsnet wrapped conn) returns a
// not-supported error in practice. The wrapper is fail-open by
// design: a bounded route running without a deadline is a
// pre-this-PR-state functional regression, NOT an outage.
func boundedHandler(h http.HandlerFunc) http.HandlerFunc {
	return boundedHandlerWithDeadline(h, boundedRouteWriteDeadline)
}

// boundedHandlerWithDeadline is the parameterized form backing both the
// 60 s default and the per-route `writeDeadline` overrides.
func boundedHandlerWithDeadline(h http.HandlerFunc, d time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		if err := rc.SetWriteDeadline(time.Now().Add(d)); err != nil {
			logger.Debug("boundedHandler: SetWriteDeadline failed; route runs unbounded",
				"err", err, "path", r.URL.Path)
		}
		h(w, r)
	}
}

// withCtxTimeout wraps the handler with `context.WithTimeout(r.Context(), d)`.
// Composes with `boundedHandler`: the write-deadline guards the
// socket; the ctx-timeout guards the DOWNSTREAM work (SQLite,
// resolver lookups, etc.) — both run against the same request.
//
// Used for fast-query routes (/v1/health, /v1/upscale/stats, etc.)
// where a wedged DB shouldn't be allowed to consume the full
// 60 s write-deadline budget before the goroutine releases. The
// 2 s / 10 s budgets here are deliberately well below the
// boundedRouteWriteDeadline so the ctx fires first and the
// downstream Store query returns `context.DeadlineExceeded`
// promptly (now possible because the Store methods are all
// ctx-aware — PR #216).
//
// On timeout the handler still emits its own response (the
// downstream Store error path will surface via writeErrorLog
// → 5xx). The wrapper doesn't itself write anything; it only
// adjusts the request's ctx.
func withCtxTimeout(d time.Duration, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		h(w, r.WithContext(ctx))
	}
}

// route is the per-route registry entry. `pattern` is the
// `http.ServeMux` pattern (method + path); `kind` is the
// classification; `handler` is the per-route handler closure
// the Server's `Handler()` builds.
//
// Kept as a method on Server so the handler closures can capture
// the Server pointer; the registry itself is computed inside
// `routeRegistry()` rather than as a package-level var so the
// methods are bound to the right instance for each call (no
// global state, tests can spin up multiple Servers in parallel).
type route struct {
	pattern string
	kind    routeKind
	handler http.HandlerFunc
	// writeDeadline overrides boundedRouteWriteDeadline for this route
	// (zero = the 60 s default). ONLY for bounded routes whose
	// server-side compute can legitimately exceed the default — the
	// deadline clock starts BEFORE the handler runs, so a synchronous
	// whole-library walk or per-variant delete loop would otherwise
	// finish its work server-side and then fail the response write,
	// which the client reads as a transport error and retries (re-running
	// the whole operation). Ignored on streaming routes.
	writeDeadline time.Duration
}

// routeRegistry returns the complete route table for this Server.
// Single source of truth — `Handler()` iterates this; the
// classification test reads it.
//
// **Ordering**: any order is functionally fine (ServeMux indexes
// by pattern), but the registry is grouped by feature area
// (browse / manifest / artwork / upscale / events / pairing) so a
// new route lands next to its siblings.
//
// Adding a new route: append a `route` entry with an EXPLICIT
// `kind`. The boundedRoute / streamingRoute distinction is the
// single decision an author must make per route; the test catches
// missing-classification because every entry needs a kind by
// construction (zero-value `boundedRoute` is the safe-by-default
// path — a streaming author who forgets to set `streamingRoute`
// gets a too-aggressive write deadline AND a test failure if
// they bypass `routeRegistry` entirely).
func (s *Server) routeRegistry() []route {
	return []route{
		// /v1/health is unauthed; everything below is authed.
		// 2 s ctx-timeout: health probe must NEVER block more than
		// ~the iOS UI's "still alive?" poll interval. A wedged
		// CountTracks should surface as 5xx within 2 s rather
		// than backing up the goroutine queue.
		{pattern: "GET /v1/health", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.health)},

		// Browse — short JSON responses except `/v1/read` and
		// `/v1/download`, which stream file bytes.
		// 10 s ctx-timeout: directory enumerations on a 50k-track
		// SMB-mounted library can legitimately take seconds on
		// the cold path; 10 s leaves comfortable headroom while
		// still bounding a hung mount.
		{pattern: "GET /v1/list", kind: boundedRoute, handler: withCtxTimeout(10*time.Second, s.authed(s.list))},
		{pattern: "GET /v1/stat", kind: boundedRoute, handler: s.authed(s.stat)},
		{pattern: "GET /v1/read", kind: streamingRoute, handler: s.authed(s.read)},
		{pattern: "GET /v1/download", kind: streamingRoute, handler: s.authed(s.download)},

		// Manifest — 50k-track libraries produce 100+ MB streams.
		// Rate-limit middleware wraps authed which wraps the
		// streaming handler.
		{pattern: "GET /v1/manifest", kind: streamingRoute, handler: s.authed(s.rateLimitManifest(s.manifestHandler))},

		// Artwork — bounded JPEG payloads (~600 px = ~50-200 KB).
		{pattern: "GET /v1/artwork/{mbid}", kind: boundedRoute, handler: s.authed(s.artwork)},
		{pattern: "GET /v1/artist-image/{mbid}", kind: boundedRoute, handler: s.authed(s.artistImage)},

		// PDF album booklet — streamingRoute, NOT bounded: booklets run
		// 10-64 MB, and over a slow Tailscale DERP relay (~1 MB/s) the
		// bounded 60s write deadline would tear the transfer mid-file.
		// Same classification rationale as /v1/download.
		{pattern: "GET /v1/booklet/{mbid}", kind: streamingRoute, handler: s.authed(s.booklet)},

		// Custom playlist / smart-mix cover art — operator-uploaded JPEGs
		// (~600 px). 404 when none (iOS falls back to the auto-mosaic).
		{pattern: "GET /v1/smart-playlist-image/{slug}", kind: boundedRoute, handler: s.authed(s.smartMixCover)},
		{pattern: "GET /v1/playlist-image/{id}", kind: boundedRoute, handler: s.authed(s.playlistCover)},

		// Waveform — tiny binary peak-envelope sidecar (~1-25 KB).
		// boundedRoute: it's a small file, no streaming exemption
		// needed. /v1/analysis/stats is the management-section poller —
		// 2 s ctx-timeout like /v1/upscale/stats so a wedged
		// CountAnalysis query surfaces as 5xx fast.
		{pattern: "GET /v1/waveform", kind: boundedRoute, handler: s.authed(s.waveform)},
		{pattern: "GET /v1/analysis/stats", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.authed(s.analysisStats))},

		// Upscale — small JSON responses on every endpoint
		// (stats / batches / variants — never streams).
		// POST /v1/upscale runs a SYNCHRONOUS filepath.WalkDir over the
		// requested folder before responding — a whole-library enqueue
		// on a big SMB/FUSE-mounted root takes minutes, so it gets the
		// long-op write deadline instead of the 60 s default.
		{pattern: "POST /v1/upscale", kind: boundedRoute, handler: s.authed(s.upscaleRequest), writeDeadline: upscaleLongOpWriteDeadline},
		// 2 s ctx-timeout: /v1/upscale/stats is iOS's
		// management-section poller — a wedged CountVariants
		// query backs up at scary rates under high-frequency
		// polling. Surface as 5xx within 2 s instead.
		{pattern: "GET /v1/upscale/stats", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.authed(s.upscaleStats))},
		// Diagnostics summary — atomic-counter + sliding-window
		// reads only; no SQLite queries, no subprocess spawns, no
		// disk-blocking work. 2 s ctx-timeout matches the other
		// fast-query routes; the handler returns in <1 ms in
		// steady state.
		{pattern: "GET /v1/diagnostics", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.authed(s.diagnostics))},
		{pattern: "POST /v1/upscale/batch", kind: boundedRoute, handler: s.authed(s.upscaleBatchSubmit)},
		{pattern: "GET /v1/upscale/batches", kind: boundedRoute, handler: s.authed(s.upscaleBatchList)},
		{pattern: "DELETE /v1/upscale/batches/{id}", kind: boundedRoute, handler: s.authed(s.upscaleBatchCancel)},
		// The ?confirm=true all-variants shape deletes one fsync'd
		// row + file per variant — tens of thousands of cached
		// variants exceed the 60 s default, so long-op deadline.
		{pattern: "DELETE /v1/upscale/variants", kind: boundedRoute, handler: s.authed(s.upscaleDelete), writeDeadline: upscaleLongOpWriteDeadline},

		// DLNA renderer discovery — bounded JSON snapshot of the
		// SSDP-discovered MediaRenderer cache. 2 s ctx-timeout
		// matches the other fast-query routes; the handler is a
		// single cache-snapshot RLock + JSON marshal, returns in
		// <1 ms in steady state.
		{pattern: "GET /v1/renderers", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.authed(s.renderers))},

		// Playlists — user-wide backup safe (any paired device can
		// list / restore / update / delete). Small JSON on list /
		// delete; PUT carries a (capped) playlist body. All bounded.
		{pattern: "GET /v1/playlists", kind: boundedRoute, handler: s.authed(s.listPlaylists)},
		{pattern: "GET /v1/playlists/{id}", kind: boundedRoute, handler: s.authed(s.getPlaylist)},
		{pattern: "PUT /v1/playlists/{id}", kind: boundedRoute, handler: s.authed(s.putPlaylist)},
		{pattern: "DELETE /v1/playlists/{id}", kind: boundedRoute, handler: s.authed(s.deletePlaylist)},

		// Playback telemetry — bulk insert from the iOS offline queue,
		// plus the cursor-paged all-devices read feed (user-wide
		// listening history; 2 s ctx-timeout matches the other
		// fast-query routes).
		{pattern: "POST /v1/history/batch", kind: boundedRoute, handler: withCtxTimeout(10*time.Second, s.authed(s.historyBatch))},
		{pattern: "GET /v1/history", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.authed(s.historyList))},

		// Atlas rich-tier metadata (Phase 2) — the iOS app (holding the Atlas
		// credential) ferries bios / descriptions / genres in via POST; the
		// bridge caches them + serves them back to all the user's devices.
		// Small JSON, single-row DB ops. 2 s ctx-timeout on the reads matches
		// the other fast-query routes; 10 s on the ingest's couple of UPSERTs.
		{pattern: "GET /v1/atlas-meta/release/{mbid}", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.authed(s.atlasMetaRelease))},
		{pattern: "GET /v1/atlas-meta/artist/{mbid}", kind: boundedRoute, handler: withCtxTimeout(2*time.Second, s.authed(s.atlasMetaArtist))},
		{pattern: "POST /v1/atlas-ingest", kind: boundedRoute, handler: withCtxTimeout(10*time.Second, s.authed(s.atlasIngest))},
		// Phase-H bulk-harvest credential handoff from the iOS app — a single
		// small atomic file write.
		{pattern: "POST /v1/atlas-harvest/credential", kind: boundedRoute, handler: withCtxTimeout(5*time.Second, s.authed(s.atlasHarvestCredential))},

		// Server-generated smart playlists — a single fast cache read
		// (bounded; generation is background/admin, never in-request).
		{pattern: "GET /v1/smart-playlists", kind: boundedRoute, handler: withCtxTimeout(10*time.Second, s.authed(s.smartPlaylists))},

		// SSE — long-lived per-connection write stream; MUST opt
		// out of the per-route write deadline.
		{pattern: "GET /v1/events", kind: streamingRoute, handler: s.authed(s.events)},
		{pattern: "GET /v1/pairing/{requestID}/events", kind: streamingRoute, handler: s.pairingEvents},

		// Pairing — small JSON / 204 responses (unauthed by
		// design; pollSecret + cert pin are the trust anchors).
		{pattern: "POST /v1/pairing/requests", kind: boundedRoute, handler: s.pairingRequest},
		{pattern: "GET /v1/pairing/{requestID}", kind: boundedRoute, handler: s.pairingPoll},
		{pattern: "DELETE /v1/pairing/{requestID}", kind: boundedRoute, handler: s.pairingDelete},
	}
}
