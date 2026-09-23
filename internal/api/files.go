package api

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// Entry is the JSON shape of a single directory entry returned by /v1/list.
// The path field is library-root-relative, not the server's absolute path.
//
// Reachable / Reason (v1.2 additive) are populated ONLY on the synthetic
// root-level entries returned in multi-root mode for an empty client
// path. Ordinary directories and files inside a root omit both fields
// (omitempty). iOS pre-1.2 ignores unknown fields; iOS 1.2+ can render
// a "library offline" hint instead of inferring it from a silent zero-
// size row.
//
// Reachable is a pointer-to-bool because Go's `omitempty` omits a bare
// `false` (zero-value rule) — using a pointer lets us emit `false` when
// we mean it and skip the field entirely when reachability is not
// relevant (any non-root entry).
//
// Reason is a stable machine-readable code, not free text — see the
// reachabilityStatus type's docblock for the value set.
type Entry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	IsDir     bool      `json:"isDir"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mtime"`
	Reachable *bool     `json:"reachable,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// StatResponse is the JSON shape returned by /v1/stat.
//
// Reachable / Reason (v1.2 additive) populate only when the requested
// path identifies a configured library root AND that root is currently
// unreachable. For descendants of a root, or for healthy roots, both
// fields are omitted. See Entry's docblock for the rationale.
type StatResponse struct {
	IsDir     bool      `json:"isDir"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"mtime"`
	Reachable *bool     `json:"reachable,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// list handles GET /v1/list?path=<rel>. Returns the entries of the resolved
// directory. Entries are sorted by name (case-insensitive) for stable client
// rendering. In multi-root mode an empty path returns synthetic top-level
// entries — one per configured root, keyed by basename — so iOS can
// enumerate roots the same way SMB enumerates shares.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	clientPath := safeQuery(r).Get("path")
	roots := s.resolver.Roots()
	if len(roots) > 1 && (clientPath == "" || clientPath == "/") {
		entries := make([]Entry, 0, len(roots))
		for _, root := range roots {
			base := filepath.Base(root)
			status := s.reachability.probe(r.Context(), root)
			if !status.Reachable {
				// An unreachable root stays visible as a directory entry
				// (legacy behaviour iOS already relies on) but now carries
				// Reachable: false + a stable Reason code so iOS 1.2+ can
				// surface "library offline" without descending and
				// inferring from a silent empty listing.
				reachable := false
				entries = append(entries, Entry{
					Name:      base,
					Path:      base,
					IsDir:     true,
					ModTime:   time.Time{},
					Reachable: &reachable,
					Reason:    status.Reason,
				})
				continue
			}
			// Healthy probe: reuse the ModTime captured by the probe's
			// os.Stat instead of re-stating the path. On network-mounted
			// libraries the second stat is the expensive one; pre-fix
			// each multi-root /v1/list paid two stats per healthy root.
			reachable := true
			entries = append(entries, Entry{
				Name:      base,
				Path:      base,
				IsDir:     true,
				Size:      0,
				ModTime:   status.ModTime,
				Reachable: &reachable,
			})
		}
		sortEntriesByName(entries)
		writeJSON(w, http.StatusOK, entries)
		return
	}
	abs, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, r, err); ok {
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a file, not a directory")
		return
	}

	dir, err := os.Open(abs)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't open this directory", err)
		return
	}
	defer dir.Close()
	raw, err := dir.Readdir(-1)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't read this directory", err)
		return
	}

	entries := make([]Entry, 0, len(raw))
	for _, ri := range raw {
		// Skip hidden files — macOS/Windows both drop noise into every
		// directory, none of it is music.
		if len(ri.Name()) > 0 && ri.Name()[0] == '.' {
			continue
		}
		// Readdir yields Lstat-shaped info, so a linked album directory
		// would report IsDir:false + the link's own byte length here
		// while /v1/stat (which goes through the resolver's os.Stat)
		// calls the same path a directory. Resolve it so one listing
		// can't disagree with the endpoints that act on its rows.
		fi := listEntryInfo(abs, ri)
		entries = append(entries, Entry{
			Name:    ri.Name(),
			Path:    childPath(clientPath, ri.Name()),
			IsDir:   fi.IsDir(),
			Size:    fi.Size(),
			ModTime: fi.ModTime().UTC(),
		})
	}
	sortEntriesByName(entries)
	writeJSON(w, http.StatusOK, entries)
}

// stat handles GET /v1/stat?path=<rel>. Returns a single-entry StatResponse.
// Works on both files and directories.
//
// Root-reachability special case: when clientPath identifies a configured
// library root AND that root is currently unreachable (timeout / EACCES /
// ENOENT on the absolute path), respond with a StatResponse carrying
// Reachable: false + Reason instead of falling through to the generic
// ErrNotFound 404. Without this, an iOS client probing a root via /stat
// gets an opaque 404 and can't distinguish "library is offline" from
// "user typed an unknown path" — both pre-PR-D outcomes for the same
// shape. Reachable: true on a healthy root is also emitted so iOS can
// trust the field without a parallel /v1/list call.
func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	clientPath := safeQuery(r).Get("path")
	if absRoot := s.matchesRoot(clientPath); absRoot != "" {
		status := s.reachability.probe(r.Context(), absRoot)
		if !status.Reachable {
			reachable := false
			writeJSON(w, http.StatusOK, StatResponse{
				IsDir:     true,
				Reachable: &reachable,
				Reason:    status.Reason,
			})
			return
		}
		// Healthy root: build the response from the probe's cached
		// stat to skip a second os.Stat. ResolveChecked would just
		// rerun the same syscall the probe already did. The size
		// field on a directory is conventionally 0 (matches what
		// info.Size() returns on most filesystems for directory
		// inodes) — keep it 0 here for consistency.
		reachable := true
		writeJSON(w, http.StatusOK, StatResponse{
			IsDir:     true,
			Size:      0,
			ModTime:   status.ModTime,
			Reachable: &reachable,
		})
		return
	}
	_, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, r, err); ok {
		return
	}
	writeJSON(w, http.StatusOK, StatResponse{
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime().UTC(),
	})
}

// read handles GET /v1/read?path=<rel>. Range header is REQUIRED per
// PROTOCOL.md — unranged reads are rejected with 400 (RFC 7233 reserves
// 416 for satisfiable-range errors, not missing-header ones). This
// endpoint is intended for tag-header windows (64–128 KB) and similar
// sub-file queries from the iOS scanner fast-path fallback. Whole-file
// reads should use /v1/download.
func (s *Server) read(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Range") == "" {
		writeError(w, http.StatusBadRequest,
			"range_required",
			"use /v1/download for unranged reads; /v1/read requires a Range header")
		return
	}
	s.serveFile(w, r)
}

// download handles GET /v1/download?path=<rel>. Supports Range; returns the
// whole file when unranged.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r)
}

// serveFile is the shared file-body path for /v1/read and /v1/download.
//
// Wraps the entire response in the SessionTracker's Begin/End so the
// updater's install path can refuse to swap-and-restart while a
// download is in flight (Hugo 2 / XMOS DAC DoP-lock loss is the
// invariant we're protecting — see internal/updater/sessions.go for
// the rationale). Tracker is nil-safe; pre-Phase-B bridges that
// don't wire one continue to work unchanged.
//
// **Variant routing** (v1.2): if the request carries `?variant=<id>`
// in addition to `?path=<rel>`, the resolver looks up the (path,
// variantID) pair in the variant store and serves the on-disk
// sidecar instead of the original. Path validation still runs on
// the source path (so `..`-escapes can't sneak through via the
// variant route). When the variant store is unwired (feature
// disabled) OR the row is missing, we 404 — iOS's typed
// BridgeError.http(404) maps to a clean fallback to the original
// on the next playback. A row that exists but whose sidecar is
// stale (source drift) yields 410 Gone, which iOS handles
// identically to 404 modulo the error message.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	if s.sessions != nil {
		s.sessions.Begin()
		defer s.sessions.End()
	}

	q := safeQuery(r)
	clientPath := q.Get("path")

	// UPnP upstream proxy fast-path: if the requested path matches a
	// row in upnp_track_routing, the bytes don't live on this
	// bridge's filesystem — they live on an upstream UPnP MediaServer
	// (e.g. a Chord 2Go's microSD card). Proxy the request via a
	// range-preserving GET to the upstream and stream bytes
	// bit-exact. NOT GATED on a variant query — variants are bridge-
	// minted sidecars by definition; an upstream-sourced track has
	// no variants today.
	if s.upnpProxyEnabled() && q.Get("variant") == "" {
		rt, lookupErr := s.upnpRouting.GetUPnPRouting(r.Context(), clientPath)
		if lookupErr != nil {
			// Surface DB faults loudly — silently falling through to
			// ResolveChecked would have a real database error masquerade
			// as "filesystem track" and produce a confusing 400/404.
			// Per Gemini on PR #352.
			LoggerFromContext(r.Context()).Error(
				"upnp routing lookup failed",
				"path", clientPath, "err", lookupErr)
		} else if rt != nil {
			s.proxyUPnP(w, r, rt)
			return
		}
		// (nil, nil) — no routing row → filesystem track → fall through.
	}

	abs, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, r, err); ok {
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a directory")
		return
	}

	// Variant branch: take over before the source-file open if the
	// caller asked for a variant. The source-path resolve above is
	// load-bearing — we run it first so a malformed `?path=` is
	// rejected with the standard 400 family BEFORE any variant
	// lookup happens. We pass the validated `info` through so the
	// freshness check uses the resolver's canonical stat instead
	// of any string-concatenation re-resolution downstream
	// (CodeQL alert: "uncontrolled data used in path expression"
	// + Gemini single-root regression — both go away when the
	// stat lives here, not in the manifest provider).
	if variantID := q.Get("variant"); variantID != "" {
		// Path normalization (collapse `//`, `.`, `..`) lives in the
		// manifest store's normalizePathForLookup so /v1/download?variant=,
		// /v1/upscale, and any other LookupTrack/LookupVariant caller
		// share one fix (Gemini on PR #147). The API layer hands over
		// the raw clientPath; the store applies path.Clean inside its
		// case-folded fallback.
		s.serveVariant(w, r, clientPath, info, variantID)
		return
	}

	f, err := os.Open(abs)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't open this file", err)
		return
	}
	defer f.Close()

	// Pre-set content-type: we treat every library file as opaque bytes —
	// the iOS side already knows what format it asked for (the manifest
	// told it). Standard binary content type keeps intermediaries from
	// transcoding anything.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")

	// http.ServeContent handles Range, If-Modified-Since, and the 206
	// partial-content bookkeeping for us. It also skips the body on HEAD
	// requests automatically.
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// serveVariant resolves (clientPath, variantID) → on-disk sidecar
// path and streams the bytes via http.ServeContent. The
// freshness check happens here (not in the variant store) so the
// canonical `os.FileInfo` from the resolver is the source of
// truth — no duplicate path resolution, no string-concatenation
// stat. The sidecar lives under the bridge's own data dir (not
// user-controlled).
//
// `sourceInfo` MUST come from `s.resolver.ResolveChecked` upstream
// in serveFile — that's how the path-traversal guard composes
// with the variant lookup. Don't call this with a user-supplied
// FileInfo from somewhere else.
func (s *Server) serveVariant(w http.ResponseWriter, r *http.Request, sourcePath string, sourceInfo os.FileInfo, variantID string) {
	if s.variantStore == nil {
		writeError(w, http.StatusNotFound, "variant_not_found", errMsgUpscalingNotEnabled)
		return
	}
	rec, err := s.variantStore.LookupVariant(r.Context(), sourcePath, variantID)
	if errors.Is(err, ErrVariantSidecarUnavailable) {
		// The row stays: the store has seen a file for it that is not
		// yet the one the row describes. Same wire answer as a sidecar
		// deleted under our feet, minus the reap — see the sentinel.
		writeError(w, http.StatusGone, "variant_missing_on_disk", "sidecar file missing")
		return
	}
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't look up this variant", err)
		return
	}
	if rec == nil {
		writeError(w, http.StatusNotFound, "variant_not_found", "no such variant")
		return
	}
	// Freshness gate. Mtime + size deltas indicate the source
	// has drifted since the sidecar was minted — operator's call
	// whether to re-convert (via `bridge upscale --force`) so we
	// never auto-delete a stale row here. iOS sees 410 Gone and
	// falls back to the original.
	//
	// Mtime comparison tolerates filesystem rounding granularities:
	// ext4 stores nanoseconds, NFS exports can truncate to
	// microseconds, and SMB / FAT32 mounts carry 2-second
	// granularity. A bare `!=` (or a tolerance narrower than the
	// FS's rounding step) would false-stale every variant on those
	// filesystems on the next bridge restart even when the source
	// was untouched. 2 s exactly covers FAT32 — the previous 1 ms
	// constant was three orders of magnitude too tight and produced
	// constant 410 Gone responses for libraries hosted on a NAS.
	// Real edits jump mtime by far more than 2 s (audacity save,
	// metadata rewrite, file replacement) so this tolerance still
	// reliably trips the gate when the source actually drifts.
	const mtimeToleranceNS int64 = 2_000_000_000
	mtimeDelta := rec.SourceMTimeNS - sourceInfo.ModTime().UnixNano()
	if mtimeDelta < 0 {
		mtimeDelta = -mtimeDelta
	}
	if mtimeDelta > mtimeToleranceNS || rec.SourceSize != sourceInfo.Size() {
		writeError(w, http.StatusGone, "variant_stale", "variant is out of date relative to source; falling back to original is recommended")
		return
	}
	f, err := os.Open(rec.SidecarPath)
	if err != nil {
		// Distinguish the "file genuinely gone" case (410 Gone,
		// iOS falls back to original, --gc reconciles) from
		// permission errors / I/O faults (5xx, operator must
		// see the real cause — silently mapping these to 410
		// would hide a permissions misconfig as if the variant
		// were permanently missing). CodeRabbit second-pass on
		// PR #108.
		if errors.Is(err, fs.ErrNotExist) {
			// Reactive cleanup (open-on-serve, no stat-then-open
			// — saves a syscall on the happy path AND avoids the
			// TOCTOU window where the file could disappear
			// between the stat and the open). The DB row is now
			// known-stale: drop it AND emit upscale.deleted so
			// iOS reconciles immediately (Ready chrome reverts,
			// currently-playing tracks fall back to source via
			// the existing PR-A2 SSE handler).
			//
			// Detached context — must NOT use r.Context() here.
			// A client that disconnected (the same event that
			// often surfaces the miss) would cancel our DB
			// write mid-transaction. Cleanup MUST reflect
			// reality regardless of who hung up first. 5 s
			// budget bounds the response goroutine if the DB
			// is wedged. Gemini HIGH on PR #218.
			// Is the FILE gone, or the VOLUME? LocateSidecar has
			// already stated both the recorded and the canonical
			// path — and on an unmounted variants volume BOTH are
			// under the dead mountpoint, so "missing at both" is
			// exactly what a healthy catalog looks like through a
			// hole in the filesystem.
			//
			// The other two reapers refuse the whole sweep in that
			// state (VariantsDirSweepBlock, the 2026-07-21 H4
			// hazard). This one had no such check, so an NFS drop at
			// 19:00 with the watcher's next tick at 19:30 meant
			// every track played in between lost its row — each with
			// an SSE telling the client the variant was gone — and
			// when the mount returned those files were unreferenced,
			// in the one shape `--gc` now refuses to reclaim.
			//
			// Deliberately INSIDE the ENOENT branch: this costs a
			// stat only once the open has already failed, so the
			// open-on-serve happy path above keeps the syscall it
			// was written to save.
			reapable := s.variantDeleter != nil && s.variantDeleter.SidecarStoreState().Available
			if s.variantDeleter != nil && !reapable {
				LoggerFromContext(r.Context()).Warn(
					"variant sidecar missing, but the variants directory is unavailable; keeping the row",
					slog.String("source_path", sourcePath),
					slog.String("variant_id", variantID),
				)
			}
			if reapable {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				// Use the canonical values from the looked-up
				// record, NOT the request input. The request
				// `sourcePath`/`variantID` may be case-folded
				// (iOS sends `share.normalize`d paths); the
				// LookupVariant result carries the canonical
				// row form that matches `Track.path` byte-
				// identical and that DeleteVariant's
				// `source_path = ?` predicate will hit.
				// Falling back to the request values when the
				// record didn't surface them (test stub
				// returning a sparse record) keeps the cleanup
				// path functional without a hard nil deref.
				canonSource := rec.SourcePath
				if canonSource == "" {
					canonSource = sourcePath
				}
				canonVariant := rec.VariantID
				if canonVariant == "" {
					canonVariant = variantID
				}
				if delErr := s.variantDeleter.DeleteVariant(cleanupCtx, canonSource, canonVariant); delErr != nil {
					LoggerFromContext(r.Context()).Warn(
						"variant DB cleanup failed after sidecar miss",
						slog.String("source_path", canonSource),
						slog.String("variant_id", canonVariant),
						slog.Any("err", delErr),
					)
				} else {
					publishUpscaleDeleted(s.EventPublisher(),
						[]string{canonSource}, []string{canonVariant})
				}
			}
			writeError(w, http.StatusGone, "variant_missing_on_disk", "sidecar file missing")
			return
		}
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't open the variant sidecar", err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't stat the variant sidecar", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// listEntryInfo is the FileInfo a /v1/list row reports for one entry of
// the directory at `dir`. It resolves an entry that merely POINTS at its
// content to the metadata of the TARGET, matching what `/v1/stat` and
// `/v1/download` see. `os.File.Readdir` documents its values as "as
// would be returned by Lstat", while both of those endpoints reach the
// file through `bridgefs.Resolver.ResolveChecked` → `os.Stat`, which
// follows. Without this, a linked album directory listed as
// `{"isDir": false, "size": 31}` (the link's own byte length) is called
// a directory by /v1/stat and rejected as one by /v1/download.
//
// Following is the right call rather than the reverse: `internal/fs`'s
// package doc states that linked content inside a configured root is
// trusted and served, so the divergence is an oversight, not a carve-out.
// The traversal guard is unaffected — it runs on the request path, and
// this only re-stats an entry the walk already reached.
//
// The RESOLVER-side endpoints need nothing: every one of them
// (`/v1/stat`, `/v1/read`, `/v1/download`, `/v1/lyrics`, `/v1/waveform`,
// the upscale path scope) goes through `ResolveChecked`'s `os.Stat`, so
// the listing is the one surface that ever saw the link itself. The
// SCANNER is a separate, deliberate story: `filepath.WalkDir` Lstats and
// descends into no link of any kind, on any platform, so an album behind
// one has never reached the manifest — a pre-existing property of the
// walk, not of this decision, and widening it would raise the cycle
// question.
//
// **CodeQL `go/path-injection` on the Join below is a false positive of
// the class dismissed for alerts #1-4** (see CLAUDE.md v0.1.4). Both
// components are safe, for different reasons: `dir` is the resolver's
// own output — `Resolve` rejects NUL bytes, rejects any `..` on the RAW
// segments BEFORE `path.Clean` canonicalizes, joins against the trusted
// root, and prefix-checks the absolute result — a barrier CodeQL's taint
// analysis cannot model; and `ri.Name()` is not client-supplied at all,
// it is a bare basename from `os.File.Readdir` on that already-validated
// directory. Don't contort this into a lexical re-check to appease the
// scanner — the caller already `os.Open`s the same `dir`.
func listEntryInfo(dir string, ri os.FileInfo) os.FileInfo {
	return resolveEntryInfo(ri, func() (os.FileInfo, error) {
		return os.Stat(filepath.Join(dir, ri.Name()))
	})
}

// resolveEntryInfo decides, from a listing entry's Lstat-shaped info and
// a stat of its target, which of the two the row reports.
//
// A REGULAR file and a DIRECTORY answer immediately and the stat is
// never taken — this runs once per entry on a directory that can hold
// thousands, often over a network mount where the second syscall is the
// expensive one, and Readdir's own Lstat has already said what they are.
// (`os.Stat` of a real directory returns the same directory; there is
// nothing to resolve.)
//
// Everything else is stat'd, and the test is "neither a regular file NOR
// a directory" rather than "is a symlink" because the shape that matters
// most is neither. A Windows directory JUNCTION (`mklink /J`,
// IO_REPARSE_TAG_MOUNT_POINT) is the live one: since Go 1.23's
// winsymlink change, `isReparseTagNameSurrogate` is true for a mount
// point, so Lstat gives it ModeIrregular and WITHHOLDS ModeDir — the
// junction reports IsDir() false with no ModeSymlink bit, failed the
// symlink test, and an album parked on another volume listed as a
// non-directory that iOS cannot open. A junction is the ORDINARY way to
// park one there, because a real symlink needs
// SeCreateSymbolicLinkPrivilege that a service account usually lacks.
// Read-only either way: nothing is deleted, the folder is just
// unbrowsable.
//
// The other non-regular POSIX kinds — a FIFO, a socket, a device node —
// stat to THEMSELVES, so widening the test costs them one syscall and
// changes no field of the row they were already getting.
//
// A stat failure falls back to the Readdir info, so a dangling link (or
// a target on a mount that just went away) still appears in the listing
// instead of vanishing from it.
//
// Taken as a function of (info, stat) rather than inline because the
// Windows shape cannot be constructed on any other platform, and a test
// that skips everywhere but one CI leg looks exactly like one that
// passed.
func resolveEntryInfo(ri os.FileInfo, stat func() (os.FileInfo, error)) os.FileInfo {
	if mode := ri.Mode(); mode.IsRegular() || mode.IsDir() {
		return ri
	}
	target, err := stat()
	if err != nil {
		return ri
	}
	return target
}

// childPath returns the library-relative path of a child given its parent's
// library-relative path and its own name. Uses forward slashes regardless
// of the server's OS.
//
// `path.Join` (not `filepath.Join`) normalises consecutive slashes, strips
// trailing slashes from `parent`, and otherwise canonicalises forward-
// slash paths uniformly across OSes. Pre-fix used manual concatenation
// (`filepath.ToSlash(filepath.Clean(parent)) + "/" + name`); the result
// is functionally identical on every input today (since `name` comes from
// `os.ReadDir` which strips path separators), but the manual path is a
// foot-gun for future refactors that might pass a less-disciplined `name`
// — `path.Join` collapses any double-slash that creeps in (gemini-style
// review bias).
func childPath(parent, name string) string {
	if parent == "" || parent == "/" {
		return name
	}
	return path.Join(filepath.ToSlash(parent), name)
}

// writeResolveError maps an fs resolver error to the right JSON error
// response. Returns true if an error was written (caller should bail).
//
// 4xx branches return the typed bridgefs sentinel's stable message
// verbatim — those messages are stable, short, user-actionable strings
// (e.g. "path not found", "unknown library root") that iOS surfaces
// directly and don't leak internal state. The wrapped err still flows
// through writeErrorLog so the per-request server log captures any
// wrapping detail (the failing path component, the syscall errno) for
// diagnostic correlation against the request_id, even though the wire
// response stays sanitized. writeErrorLog's level split keeps these
// 4xx records at Warn so they don't drown Error-level alerts (gemini
// bot review on PR #191).
//
// The default branch is 5xx — for an UNKNOWN resolver error we don't
// know the leak surface of err.Error(), so the wire body is the
// generic "internal error" string.
func writeResolveError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, bridgefs.ErrBadPath):
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request", bridgefs.ErrBadPath.Error(), err)
	case errors.Is(err, bridgefs.ErrUnknownRoot):
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request", bridgefs.ErrUnknownRoot.Error(), err)
	case errors.Is(err, bridgefs.ErrNotFound):
		writeErrorLog(w, r, http.StatusNotFound, "not_found", bridgefs.ErrNotFound.Error(), err)
	default:
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge encountered an internal error", err)
	}
	return true
}

// lessCaseFold compares two strings case-insensitively across the full
// Unicode range. Matches the alphabetical-but-CI ordering iOS users
// expect in a file browser — "Ébène" sorts near "e", not after "z" as
// the older ASCII-only byte fold produced.
//
// **Use sortEntriesByName for sorting**, not this directly inside
// `sort.Slice` — `strings.ToLower` allocates two strings per call,
// and `sort.Slice` calls the comparator O(N log N) times. The
// dedicated sort helper computes each lowercased key once. lessCaseFold
// stays as the canonical case-fold predicate (package-internal) for
// one-shot comparisons and to keep `TestLessCaseFoldUnicode` honest.
func lessCaseFold(a, b string) bool {
	return strings.ToLower(a) < strings.ToLower(b)
}

// sortEntriesByName sorts entries in-place by case-folded Name. Each
// element's lowercased key is computed once into a parallel slice and
// reused by the comparator — vs. the previous `sort.Slice` +
// `strings.ToLower` per comparison, which allocated 2 × O(N log N)
// strings per request. For a 2000-file directory the prior shape did
// ~44 000 string allocations per /v1/list call (review item).
func sortEntriesByName(entries []Entry) {
	keys := make([]string, len(entries))
	for i := range entries {
		keys[i] = strings.ToLower(entries[i].Name)
	}
	sort.Sort(entriesByCaseFold{entries: entries, keys: keys})
}

// entriesByCaseFold is the sort.Interface implementation paired with
// sortEntriesByName. Swap moves both the entry and its precomputed key
// so subsequent comparisons keep referencing the right key.
//
// Less uses the original Name as a tie-break when folded keys match
// — without it, fold-equal entries ("Apple"/"apple") permute
// arbitrarily under sort.Sort and any UI that depends on a stable
// listing across requests sees flicker (CodeRabbit on PR #71).
type entriesByCaseFold struct {
	entries []Entry
	keys    []string
}

func (s entriesByCaseFold) Len() int { return len(s.entries) }
func (s entriesByCaseFold) Less(i, j int) bool {
	if s.keys[i] != s.keys[j] {
		return s.keys[i] < s.keys[j]
	}
	return s.entries[i].Name < s.entries[j].Name
}
func (s entriesByCaseFold) Swap(i, j int) {
	s.entries[i], s.entries[j] = s.entries[j], s.entries[i]
	s.keys[i], s.keys[j] = s.keys[j], s.keys[i]
}
