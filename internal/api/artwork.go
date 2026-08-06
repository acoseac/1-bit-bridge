package api

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/enrich"
)

// artworkPendingRetryAfterSeconds is the value the handlers set on
// `Retry-After` when an MBID is known to the server but the cache
// file isn't on disk yet (202 response). 30s matches iOS's default
// base backoff; a longer value would starve the retry loop, a shorter
// one would hammer an enricher that's already rate-limited by MB /
// CAA / Deezer.
const artworkPendingRetryAfterSeconds = 30

// errMsgInternalError is the opaque user-facing error string returned
// from every 500 path in artwork. Real diagnostic context lands in the
// server-side log via writeErrorLog; the public response stays generic
// so a malicious caller can't probe filesystem layout.
const errMsgInternalError = "internal error"

// ArtworkDirProvider is the minimal interface api needs to serve cached
// artwork. Implemented by cmd/bridge's serveCmd (via the Enricher's
// CacheDir). Split out so api tests don't have to import internal/enrich.
type ArtworkDirProvider interface {
	ArtworkCacheDir() string
}

// localArtworkMBIDPrefix marks an `artworkMBID` whose bytes the SCANNER
// owns — embedded ID3 APIC art or a folder-level cover.jpg, hashed into
// `local-<sha256>` (see internal/manifest.extractLocalArtwork). The
// enricher never fetches these: `enrichOne` skips CAA / iTunes for a
// `local-` value while still STAMPING `enriched_at`, so "enrichment
// complete" says nothing about whether these bytes will come back.
// `Scanner.needsLocalArtworkRecovery` re-extracts them on the next scan
// when the cache file is missing — which is exactly what `bridge
// restore` needs, since `internal/backup` snapshots the DB, tokens,
// certs and bridge.yaml but NOT <dataDir>/artwork/.
const localArtworkMBIDPrefix = "local-"

// canonicalCoverSize is the only size the bridge ever WRITES a cover
// at. `enrich.SupportedCoverSizes` (and therefore `enrich.ParseSize`)
// admits 250 / 500 / 1200, but every writer — the enricher's
// `ensureArtworkCached`, the acoustic-match path, `bridge artwork`, and
// the scanner's `local-<sha>-500.jpg` — hardcodes 500. A request for a
// size no writer produces is a size-specific miss, NOT evidence that
// the release has no cover.
const canonicalCoverSize = 500

// mbidPattern validates that a path segment looks like a MusicBrainz
// UUID. Prevents traversal and filesystem abuse through the {mbid}
// parameter. Used by the artist-image handler, which only accepts
// MusicBrainz-derived MBIDs.
var mbidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// artworkMBIDPattern validates the {mbid} segment for /v1/artwork/{mbid}.
// Accepts either a MusicBrainz UUID (set by the enricher after a
// successful Cover Art Archive fetch) OR a local-<sha256> sentinel set
// by the scanner when it found embedded ID3 APIC art or a folder-level
// cover.jpg / folder.jpg next to the audio file. The local- branch is
// lowercase-only to match `hex.EncodeToString` output deterministically.
// Same traversal protection as the strict UUID pattern: the alphabet
// is bounded to [a-z0-9-] so no path-segment escape is possible.
var artworkMBIDPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|local-[0-9a-f]{64})$`)

// mbidPredicate is the shape of one MBIDProbe method: an answerable
// yes/no about an MBID, plus the database fault that means "no answer".
type mbidPredicate func(ctx context.Context, mbid string) (bool, error)

// artworkMissOutcome is the three-way classification of a cache miss on
// /v1/artwork and /v1/artist-image. See MBIDProbe's docblock for the
// state machine; writeArtworkMiss turns each value into its wire form.
type artworkMissOutcome int

const (
	// missUnknown → 404 `not_found`: no track in the manifest
	// references this MBID, so nothing will ever cache it.
	missUnknown artworkMissOutcome = iota
	// missPending → 202 + Retry-After: bytes may still land.
	missPending
	// missNoImage → 404 `no_image`: terminal, the enricher took every
	// turn it will take and no image exists upstream.
	missNoImage
)

// classifyArtworkMiss decides which of the three miss states applies.
//
// Two branches answer `missPending` for reasons that are NOT "the
// enricher hasn't run yet", and both are load-bearing:
//
//   - **Probe faults.** Neither a DB-faulted `hasTrack` nor a DB-faulted
//     `pending` is an answer. Answering 202 costs the client one bounded
//     retry; answering a terminal 404 costs the image permanently. The
//     pending probe was already documented as fail-open — the gate in
//     front of it silently failed CLOSED, which is the bug this
//     centralisation fixes.
//   - **`local-` sentinels.** Those bytes are the scanner's, not the
//     enricher's (see localArtworkMBIDPrefix): the enricher stamps
//     `enriched_at` for them without ever fetching anything, so
//     "enrichment complete" is simply the wrong discriminator. The
//     scanner's `needsLocalArtworkRecovery` restores a missing cache
//     file on the next scan, so 202 is the truthful answer — this is
//     the ordinary post-`bridge restore` state, and on a well-tagged
//     library it is the overwhelming majority of covers.
//
// The `local-` branch is unreachable from /v1/artist-image, whose
// `mbidPattern` admits strict UUIDs only; it lives here rather than in
// the artwork handler so that a future relaxation of that pattern
// inherits the correct behaviour instead of silently regressing.
// logProbeFault records an unanswerable MBID probe. The `ctx.Err()` gate
// is the repo-wide convention for database / context-driven operations:
// a cancelled request (client hang-up, shutdown) would otherwise emit a
// burst of misleading Errors that describe normal teardown, not a fault.
//
// It gates the LOG ONLY. The caller has already decided to answer 202 by
// the time it gets here, and that decision must not depend on whether the
// line was written — suppressing a log is cosmetic, suppressing the
// fail-open would terminal-404 an image that exists.
func logProbeFault(ctx context.Context, msg, mbid string, err error) {
	if ctx.Err() != nil {
		return
	}
	LoggerFromContext(ctx).Error(msg, "mbid", mbid, "err", err)
}

func classifyArtworkMiss(ctx context.Context, mbid string, hasTrack, pending mbidPredicate) artworkMissOutcome {
	if hasTrack == nil || pending == nil {
		// No probe wired (tests, legacy) — pre-v1.1 behaviour: every
		// cache miss is a flat 404.
		return missUnknown
	}
	known, err := hasTrack(ctx, mbid)
	if err != nil {
		logProbeFault(ctx, "mbid known-probe failed", mbid, err)
		return missPending
	}
	if !known {
		return missUnknown
	}
	if strings.HasPrefix(mbid, localArtworkMBIDPrefix) {
		return missPending
	}
	stillPending, err := pending(ctx, mbid)
	if err != nil {
		logProbeFault(ctx, "mbid enrichment-pending probe failed", mbid, err)
		return missPending
	}
	if stillPending {
		return missPending
	}
	return missNoImage
}

// writeArtworkMiss renders a classified miss. `noun` is the subject of
// the user-facing message ("artwork" / "artist image").
//
// `Retry-After` is set on the 202 branch ONLY — a terminal 404 must
// never carry it, or clients read the response as retryable and the
// futile-ladder problem the three-way split exists to kill comes back.
func writeArtworkMiss(w http.ResponseWriter, outcome artworkMissOutcome, noun string) {
	switch outcome {
	case missPending:
		w.Header().Set("Retry-After", strconv.Itoa(artworkPendingRetryAfterSeconds))
		writeError(w, http.StatusAccepted, "pending",
			noun+" enrichment pending; retry after the Retry-After window")
	case missNoImage:
		writeError(w, http.StatusNotFound, "no_image",
			"enrichment complete; no "+noun+" available for this MBID")
	default:
		writeError(w, http.StatusNotFound, "not_found",
			noun+" not cached (unknown MBID)")
	}
}

// artistImage handles GET /v1/artist-image/{mbid}.
//
// Serves the Deezer-sourced artist portrait the enricher cached under
// <artworkCacheDir>/artist-<mbid>.jpg. Same MBID validation + 404 / 400
// semantics as /v1/artwork.
func (s *Server) artistImage(w http.ResponseWriter, r *http.Request) {
	if s.artworkDirs == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress",
			"artist-image service not ready")
		return
	}
	mbid := r.PathValue("mbid")
	if !mbidPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"mbid must be a MusicBrainz UUID")
		return
	}
	path := enrich.ArtistImagePath(s.artworkDirs.ArtworkCacheDir(), mbid)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Three-way miss split (see MBIDProbe): 202 only while
			// enrichment is genuinely pending — the cold-cache first
			// scan where the portrait may land any moment. Once every
			// track carrying the MBID is enriched and there's still no
			// file, no image exists upstream (Deezer has nothing for
			// this artist): answer a TERMINAL 404 so clients stop
			// asking. Pre-fix this state answered 202 forever and iOS
			// rode a multi-minute retry ladder per imageless artist on
			// every coverage sweep.
			var hasTrack, pending mbidPredicate
			if s.mbidProbe != nil {
				hasTrack = s.mbidProbe.HasTrackWithArtistMBID
				pending = s.mbidProbe.ArtistMBIDEnrichmentPending
			}
			writeArtworkMiss(w,
				classifyArtworkMiss(r.Context(), mbid, hasTrack, pending),
				"artist image")
			return
		}
		logger.Error("open artist image", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat artist image", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// artwork handles GET /v1/artwork/{mbid}?size=500.
//
// Serves the pre-cached JPEG the enricher fetched from Cover Art Archive.
// A miss returns 404 rather than falling through to CAA directly — we
// don't want to expose our server as a proxy and a miss simply means
// "enrichment hasn't caught up yet, try again later".
func (s *Server) artwork(w http.ResponseWriter, r *http.Request) {
	if s.artworkDirs == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress",
			"artwork service not ready")
		return
	}
	mbid := r.PathValue("mbid")
	if !artworkMBIDPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"mbid must be a MusicBrainz UUID or local-<sha256> hash")
		return
	}
	size, err := enrich.ParseSize(r.URL.Query().Get("size"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	cacheDir := s.artworkDirs.ArtworkCacheDir()
	path := enrich.ArtworkCachePath(cacheDir, mbid, size)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Size-specific miss. `ParseSize` accepts 250 / 500 / 1200
			// but nothing in the bridge WRITES anything but 500 (see
			// canonicalCoverSize), so an off-size request builds a path
			// that will never exist even for a release whose cover is
			// cached. Answering `no_image` there would tell the client
			// "no cover exists upstream — stop asking" about an image
			// the very next request could serve. `not_found` keeps the
			// answer inside the documented vocabulary and scoped to
			// what it really means: not cached under this key.
			if size != canonicalCoverSize {
				if _, statErr := os.Stat(enrich.ArtworkCachePath(cacheDir, mbid, canonicalCoverSize)); statErr == nil {
					writeError(w, http.StatusNotFound, "not_found",
						"artwork is not cached at this size; retry with size="+
							strconv.Itoa(canonicalCoverSize))
					return
				}
			}
			// See artistImage handler for the full rationale — the
			// same three-way split: 202 while enrichment is pending,
			// terminal 404 `no_image` once enrichment completed with
			// nothing cached (CAA/iTunes had no cover), plain 404 for
			// an MBID no track references.
			var hasTrack, pending mbidPredicate
			if s.mbidProbe != nil {
				hasTrack = s.mbidProbe.HasTrackWithArtworkMBID
				pending = s.mbidProbe.ArtworkMBIDEnrichmentPending
			}
			writeArtworkMiss(w,
				classifyArtworkMiss(r.Context(), mbid, hasTrack, pending),
				"artwork")
			return
		}
		logger.Error("open release artwork", "mbid", mbid, "size", size, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat release artwork", "mbid", mbid, "size", size, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
