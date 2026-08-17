// Command bridge is the 1-bit-bridge server CLI.
//
// Subcommands:
//
//	bridge init    first-time setup: config, TLS cert, launchd/systemd unit
//	bridge serve   run the HTTPS server (default port 7788)
//	bridge pair    mint a new bearer token for an iOS client
//	bridge scan    force a full library rescan
//	bridge version print version and protocol version
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/term"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/atlasharvest"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/tlsacme"

	// Imported for its init() side effects (log-hook + collector
	// registration) AND for `metrics.RegisterTsnetProvider` invoked
	// from the tsnet startup goroutine below. Without this import,
	// /metrics would expose an empty registry and the log-event
	// counter would stay at zero forever.
	"github.com/acoseac/1-bit-bridge/internal/metrics"
	"github.com/acoseac/1-bit-bridge/internal/pairing"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
	"github.com/acoseac/1-bit-bridge/internal/tsnet"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Package-scoped logger for the cmd/bridge wiring layer. Most
// startup output is `fmt.Fprintf(stdout/stderr)` because it's user-
// facing; this is for backend-style telemetry from helpers like
// upscaleStatsAdapter that fire on every request and shouldn't spam
// the operator's terminal but should be visible via `bridge logs`.
var logger = logging.Component("bridge")

// variantStoreAdapter implements api.VariantStore on top of a
// manifest.Provider. Just translates between the two packages'
// equivalent record shapes — the api package can't import the
// manifest package directly (would create an upward cycle), so
// this thin adapter lives at the wiring point. Same pattern as
// MBIDProbe / ManifestProvider.
// tsnetH3Listener pairs one HTTP/3 (QUIC) server with the UDP
// PacketConn it serves on. Dual-stack tailnet nodes carry both an
// IPv4 (`100.x.y.z`) and IPv6 (`fd7a:...`) tailnet IP, and
// `tsnet.Server.ListenPacket` requires an explicit IP per call —
// so we bind one listener per assigned IP and run each on its own
// goroutine. The HTTP/2 path via `ListenTLS(cfg.ListenAddress)`
// accepts on every tailnet IP for free (it gets a port-only
// unspecified-IP form); HTTP/3 doesn't have that shortcut.
type tsnetH3Listener struct {
	srv  *http3.Server
	conn net.PacketConn
}

// tsnetH3State holds the per-IP HTTP/3 listeners stored from inside
// the tsnet startup goroutine (after `tsnetServer.Start` succeeds)
// and read on the shutdown path. The whole slice is published as a
// single `atomic.Pointer[tsnetH3State]` value so shutdown sees a
// coherent snapshot — appending to a shared slice from the goroutine
// while shutdown was iterating would race.
type tsnetH3State struct {
	listeners []tsnetH3Listener
}

type variantStoreAdapter struct {
	provider *manifest.Provider
}

func (a *variantStoreAdapter) LookupVariant(ctx context.Context, sourcePath, variantID string) (*api.VariantRecord, error) {
	v, err := a.provider.LookupVariant(ctx, sourcePath, variantID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return &api.VariantRecord{
		// Canonical values from the row — NOT the request input.
		// Case-insensitive lookup may have resolved a folded
		// request against the canonical-case row; the api
		// consumer needs the canonical form for any subsequent
		// DeleteVariant + SSE publish so the wire shape matches
		// `Track.path` byte-for-byte. CodeRabbit Major on PR #209.
		SourcePath:    v.SourcePath,
		VariantID:     v.VariantID,
		SidecarPath:   v.SidecarPath,
		SourceMTimeNS: v.SourceMTimeNS,
		SourceSize:    v.SourceSize,
	}, nil
}

// atlasHarvestSink adapts the manifest store to the Phase-H harvest client's
// MetaSink. The harvest result's Source/SourceURL (the winning bio's provenance
// — wiki / lastfm / tadb + its URL) is persisted into artist_atlas so iOS can
// render "Read more on <source>" (Phase A4).
type atlasHarvestSink struct{ store *manifest.Store }

func (a atlasHarvestSink) UpsertArtistMeta(ctx context.Context, m atlasharvest.ArtistMeta) error {
	return a.store.UpsertArtistAtlasMeta(ctx, manifest.ArtistAtlasMeta{
		ArtistMBID: m.MBID,
		Found:      m.Found,
		Bio:        m.Bio,
		BioSummary: m.BioSummary,
		Genres:     m.Genres,
		Source:     m.Source,
		SourceURL:  m.SourceURL,
	})
}

// UpsertReleaseMeta persists harvested album text (description / record label /
// genre) into release_atlas — the same overlay the iOS ferry writes via
// /v1/atlas-ingest — so the bulk harvest fills "About this album" the way it
// fills artist bios (Phase D).
func (a atlasHarvestSink) UpsertReleaseMeta(ctx context.Context, m atlasharvest.ReleaseMeta) error {
	return a.store.UpsertReleaseAtlasMeta(ctx, manifest.ReleaseAtlasMeta{
		ReleaseMBID: m.MBID,
		Found:       m.Found,
		Description: m.Description,
		RecordLabel: m.RecordLabel,
		Genres:      m.Genres,
		Source:      m.Source,
		SourceURL:   m.SourceURL,
	})
}

// bookletSinkAdapter adapts *manifest.Store to the harvest client's
// BookletSink — a pass-through except BookletsToFetch, whose row type is
// narrowed to the (mbid, etag) pair the fetch sweep needs (keeps the
// atlasharvest package from importing internal/manifest).
type bookletSinkAdapter struct{ store *manifest.Store }

func (b bookletSinkAdapter) DistinctAlbumReleaseMBIDs(ctx context.Context) ([]string, error) {
	return b.store.DistinctAlbumReleaseMBIDs(ctx)
}

func (b bookletSinkAdapter) BookletsToCheck(ctx context.Context, candidates []string, maxAttempts int) ([]string, error) {
	return b.store.BookletsToCheck(ctx, candidates, maxAttempts)
}

func (b bookletSinkAdapter) UpsertBookletAvailability(ctx context.Context, mbid string, available bool, etag string, size int64) error {
	return b.store.UpsertBookletAvailability(ctx, mbid, available, etag, size)
}

func (b bookletSinkAdapter) SetBookletTagAndBumpIndex(ctx context.Context, releaseMBID, tag string) (int64, error) {
	return b.store.SetBookletTagAndBumpIndex(ctx, releaseMBID, tag)
}

func (b bookletSinkAdapter) BookletsToFetch(ctx context.Context, limit, maxAttempts int) ([]atlasharvest.BookletFetchItem, error) {
	rows, err := b.store.BookletsToFetch(ctx, limit, maxAttempts)
	if err != nil {
		return nil, err
	}
	out := make([]atlasharvest.BookletFetchItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, atlasharvest.BookletFetchItem{ReleaseMBID: r.ReleaseMBID, Etag: r.Etag})
	}
	return out, nil
}

func (b bookletSinkAdapter) MarkBookletFetched(ctx context.Context, mbid string) error {
	return b.store.MarkBookletFetched(ctx, mbid)
}

func (b bookletSinkAdapter) MarkBookletFetchFailed(ctx context.Context, mbid string) error {
	return b.store.MarkBookletFetchFailed(ctx, mbid)
}

func (b bookletSinkAdapter) MarkBookletUnavailable(ctx context.Context, mbid string) error {
	return b.store.MarkBookletUnavailable(ctx, mbid)
}

func (b bookletSinkAdapter) DeleteBookletsNotIn(ctx context.Context, universe []string) ([]string, error) {
	return b.store.DeleteBookletsNotIn(ctx, universe)
}

// bookletDiskStore persists fetched booklet PDFs under <dataDir>/booklets/
// via the atomicwrite helpers. Paths come from api.BookletPath so the
// writer and the /v1/booklet server can never disagree about layout.
//
// Both methods validate the MBID themselves. The previous docblock claimed
// the values were "strict UUIDs by the time they reach here (validated at
// the check-response boundary and the API handler)" — that was FALSE for
// this path: the GET handler validates, but nothing in the harvest chain
// did (grepping mbidPattern/isValidMBID across internal/atlasharvest and
// internal/manifest/booklets.go returned nothing), and
// DistinctAlbumReleaseMBIDs filtered only on NULL/empty/`local-%`. Since
// `mbid` is the LEADING component of the join and atomicwrite.WriteBytes
// runs os.MkdirAll on the parent, a traversing tag value would create its
// own parent directories rather than failing (2026-07-20 review, F29).
type bookletDiskStore struct{ dir string }

// errInvalidBookletMBID is returned rather than silently skipping so a
// malformed value surfaces in the harvest logs instead of looking like a
// booklet that simply never arrived.
var errInvalidBookletMBID = errors.New("booklet: mbid is not a MusicBrainz UUID")

func (b bookletDiskStore) WriteBooklet(mbid string, r io.Reader) error {
	if !api.IsValidBookletMBID(mbid) {
		return errInvalidBookletMBID
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return atomicwrite.WriteBytes(api.BookletPath(b.dir, mbid), data, ".booklet-*.pdf.tmp")
}

func (b bookletDiskStore) RemoveBooklet(mbid string) error {
	if !api.IsValidBookletMBID(mbid) {
		return errInvalidBookletMBID
	}
	err := os.Remove(api.BookletPath(b.dir, mbid))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// atlasCoverRefetcher adapts the enricher's authenticated premium-cover fetcher
// to the harvest client's CoverRefetcher: it upgrades a release's cached cover
// (<artworkDir>/<mbid>-500.jpg) to premium once Atlas has reverse-resolved one.
// Size 500 matches what the enricher caches + what iOS requests.
type atlasCoverRefetcher struct {
	premium    enrich.PremiumCoverFetcher
	artworkDir string
	store      *manifest.Store
}

func (a atlasCoverRefetcher) RefetchPremium(ctx context.Context, releaseMBID string) (bool, error) {
	if a.premium == nil {
		return false, atlasharvest.ErrNoCredential
	}
	path := enrich.ArtworkCachePath(a.artworkDir, releaseMBID, 500)
	got, err := a.premium.RefetchPremium(ctx, path, releaseMBID, 500)
	if errors.Is(err, enrich.ErrNoCredential) {
		// Translate the enrich-layer sentinel to the harvest client's contract
		// sentinel (this adapter is the bridge between the two packages).
		return got, atlasharvest.ErrNoCredential
	}
	if got && a.store != nil {
		// The cover file was just overwritten with premium bytes. Record its
		// content version + bump indexed_at so the iOS delta-sync re-receives
		// these tracks and invalidates its (albumID-keyed, not URL-keyed) cover
		// cache — the /v1/artwork/{mbid} URL is stable while the bytes changed.
		// Idempotent (the store guards on a version change), so a re-fetch of the
		// same premium bytes is a no-op. Best-effort: a hash/DB hiccup leaves the
		// cover served correctly (just not auto-refreshed on iOS until the manual
		// Clear-caches or a periodic full sync) — never fail the refetch over it,
		// but log at warn so a persistent permission / DB-lock issue is visible.
		if ver, herr := hashFileShort(path); herr != nil {
			logging.Component("atlasharvest").Warn("artwork version: hash cover", "mbid", releaseMBID, "err", herr)
		} else if ver != "" {
			if _, serr := a.store.SetArtworkVersionAndBumpIndex(ctx, releaseMBID, ver); serr != nil {
				logging.Component("atlasharvest").Warn("artwork version: record", "mbid", releaseMBID, "err", serr)
			}
		}
	}
	return got, err
}

// hashFileShort returns a short hex SHA-256 (16 chars / 64 bits — ample for a
// cover-change marker) of the file at path. Used as the artwork content version
// so the manifest signals a cover upgrade to iOS without ferrying the bytes.
func hashFileShort(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// analysisStoreAdapter implements api.AnalysisStore on top of a
// manifest.Provider — the /v1/waveform handler's lookup. Same
// upward-cycle-avoidance pattern as variantStoreAdapter.
type analysisStoreAdapter struct {
	provider *manifest.Provider
}

func (a *analysisStoreAdapter) LookupAnalysis(ctx context.Context, sourcePath string) (*api.AnalysisRecord, error) {
	al, err := a.provider.LookupAnalysis(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	if al == nil {
		return nil, nil
	}
	return &api.AnalysisRecord{
		// Canonical row values — the case-insensitive lookup may have
		// resolved a folded request against the canonical-case row.
		SourcePath:    al.SourcePath,
		WaveformPath:  al.WaveformPath,
		WaveformTag:   al.WaveformTag,
		SourceMTimeNS: al.SourceMTimeNS,
		SourceSize:    al.SourceSize,
		Spectrum:      al.Spectrum,
	}, nil
}

// variantDeleterAdapter implements api.VariantDeleter on top of a
// manifest.Store. Translates between manifest.VariantRow and
// api.VariantSummary (the api-package-local projection). Same
// upward-cycle-avoidance pattern variantStoreAdapter uses.
type variantDeleterAdapter struct {
	store *manifest.Store
}

func (a *variantDeleterAdapter) AllVariants(ctx context.Context) ([]api.VariantSummary, error) {
	rows, err := a.store.AllVariants(ctx)
	if err != nil {
		return nil, err
	}
	return variantSummariesFromRows(rows), nil
}

func (a *variantDeleterAdapter) ListVariantsByPathPrefix(ctx context.Context, prefix string) ([]api.VariantSummary, error) {
	rows, err := a.store.ListVariantsByPathPrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return variantSummariesFromRows(rows), nil
}

func (a *variantDeleterAdapter) ListVariantsForPath(ctx context.Context, sourcePath string) ([]api.VariantSummary, error) {
	rows, err := a.store.ListVariantsForPath(ctx, sourcePath)
	if err != nil {
		return nil, err
	}
	return variantSummariesFromRows(rows), nil
}

func (a *variantDeleterAdapter) DeleteVariant(ctx context.Context, sourcePath, variantID string) error {
	return a.store.DeleteVariant(ctx, sourcePath, variantID)
}

func variantSummariesFromRows(rows []manifest.VariantRow) []api.VariantSummary {
	out := make([]api.VariantSummary, len(rows))
	for i, r := range rows {
		out[i] = api.VariantSummary{
			SourcePath:  r.SourcePath,
			VariantID:   r.VariantID,
			SidecarPath: r.SidecarPath,
			SizeBytes:   r.SizeBytes,
		}
	}
	return out
}

// inflightDropperAdapter implements api.InflightDropper on top of a
// transcode.Pool. One-line passthrough; lives here so the api
// package keeps its narrow handler-facing interface without an
// inverted dependency on internal/transcode.
type inflightDropperAdapter struct {
	pool *transcode.Pool
}

func (a *inflightDropperAdapter) DropInflight(matches func(sourcePath string) bool) int {
	return a.pool.DropInflight(matches)
}

// integrityVariantListerAdapter implements integrity.VariantLister on
// top of a manifest.Store. Mirrors variantDeleterAdapter's narrow
// projection but for the watcher's use — no SidecarPath/SizeBytes
// in the api.VariantSummary mapping is needed here since the
// watcher reads them off its own VariantSnapshot type.
type integrityVariantListerAdapter struct {
	store *manifest.Store
}

func (a *integrityVariantListerAdapter) AllVariants() ([]integrity.VariantSnapshot, error) {
	rows, err := a.store.AllVariants(context.Background())
	if err != nil {
		return nil, err
	}
	out := make([]integrity.VariantSnapshot, len(rows))
	for i, r := range rows {
		out[i] = integrity.VariantSnapshot{
			SourcePath:  r.SourcePath,
			VariantID:   r.VariantID,
			SidecarPath: r.SidecarPath,
		}
	}
	return out, nil
}

// integrityVariantDeleterAdapter implements integrity.VariantDeleter
// on top of a manifest.Store. Same one-line passthrough as
// variantDeleterAdapter's DeleteVariant; lives separately so the
// integrity package stays decoupled from internal/api's
// VariantSummary type.
type integrityVariantDeleterAdapter struct {
	store *manifest.Store
}

func (a *integrityVariantDeleterAdapter) DeleteVariant(sourcePath, variantID string) error {
	return a.store.DeleteVariant(context.Background(), sourcePath, variantID)
}

// integritySidecarListerAdapter implements integrity.SidecarLister on
// top of a manifest.Store. Mirrors integrityVariantListerAdapter but
// projects only `track_variants.sidecar_path` (no per-row metadata
// reads — the forward-sweep sweeper only needs to know "what paths
// exist in the DB" to diff against the filesystem walk).
type integritySidecarListerAdapter struct {
	store *manifest.Store
}

func (a *integritySidecarListerAdapter) AllSidecarPaths(ctx context.Context) (map[string]struct{}, error) {
	return a.store.AllSidecarPaths(ctx)
}

// upscaleEnqueuerAdapter implements api.UpscaleEnqueuer on top
// of a transcode.Pool plus a manifest.Store + bridgefs.Resolver.
// Per-call work:
//  1. Resolve the library-relative path to abs via the canonical
//     resolver (handles single-root + multi-root layouts).
//  2. Fetch the Track row from the store to read source rate /
//     isDSD for eligibility.
//  3. Pick target rate via transcode.ResolveTargetRate; skip
//     when the source is already at/above it (returns
//     ErrUpscaleIneligible).
//  4. Construct a JobSpec, capture freshness from disk, hand to
//     the Pool. Translate transcode.ErrQueueFull → the api
//     package's ErrUpscaleQueueFull sentinel so the handler can
//     `errors.Is` cleanly without importing transcode.
type upscaleEnqueuerAdapter struct {
	pool     *transcode.Pool
	store    *manifest.Store
	resolver *bridgefs.Resolver
	cfg      *config.Config
	// outputDir resolves the CURRENT effective variants dir per call
	// (closure over the live config holder). A construction-time
	// string snapshot silently ignored hot variants-dir changes
	// (POST /api/upscale/variants-dir) until restart — new sidecars
	// kept landing on, and disk checks kept grading, the old path.
	outputDir func() string
	// soxInfo returns the cached ProbeSox snapshot, so the eligibility
	// gate can refuse a source THIS sox build cannot decode instead of
	// advertising it and failing the job later. Read through the same
	// 30 s-TTL cache the admin tile uses, so the two surfaces agree and
	// an enqueue costs no extra fork-exec. Nil-safe: an unwired closure
	// (direct-construction tests) skips the check, matching its
	// documented fail-open posture.
	soxInfo func() (transcode.SoxInfo, error)
}

// soxCanDecode reports whether the installed sox can read absPath. Fails
// OPEN on a nil closure or a probe error — the probe is an optimisation
// for honest refusals, never a new way for enqueue to break.
func (a *upscaleEnqueuerAdapter) soxCanDecode(absPath string) bool {
	if a.soxInfo == nil {
		return true
	}
	info, err := a.soxInfo()
	if err != nil {
		return true
	}
	return info.CanDecode(absPath)
}

// resolveAndLookupTrack is the shared scaffolding for `EnqueueOne`
// and `EnqueueOptimize`: resolve abs path + look up the manifest
// Track row + filter DSD. Returns the typed error sentinels both
// callers map directly into their wire response. Kind-specific
// checks (rate/bits validation, eligibility predicate, target
// resolution) stay in each caller.
//
// Use Lookup (not Get): iOS hands in a path through
// `share.normalize(path:)` — lowercase + leading slash — while
// the manifest stores the FS-canonical case. LookupTrack does an
// exact-match fast path then falls back to LOWER() via the v3
// functional index. Per CLAUDE.md PR #126.
func (a *upscaleEnqueuerAdapter) resolveAndLookupTrack(libraryRelativePath string) (string, *manifest.Track, error) {
	abs, err := a.resolver.Resolve(libraryRelativePath)
	if err != nil {
		return "", nil, api.ErrUpscaleSourceMissing
	}
	track, err := a.store.LookupTrack(context.Background(), libraryRelativePath)
	if err != nil {
		// DB errors propagate so the handler logs the real cause
		// instead of silently enqueuing — the resumability check
		// below depends on the same store.
		return "", nil, fmt.Errorf("get track row: %w", err)
	}
	if track == nil {
		// File exists on disk but scanner hasn't reached it yet.
		// Silent reject (no remediation path beyond rescan).
		return "", nil, api.ErrUpscaleIneligible
	}
	if track.IsDSD != nil && *track.IsDSD {
		return "", nil, api.ErrUpscaleIneligible
	}
	// Refuse what this sox build cannot decode. Sits with the DSD filter
	// because it answers the same question — "can the pipeline actually
	// process this?" — and both callers must get the same answer.
	//
	// The case that motivated it is ALAC: lossless, so IsLossyCodec
	// doesn't exclude it; OptimizeEligible names "ALAC" outright; and
	// since PR #440 populated PCM geometry for M4A it has non-nil
	// SampleRate/BitsPerSample too. So it cleared every gate and reached
	// a sox with no MP4 demuxer — the client had already been told the
	// track was eligible (wand enabled on iOS) before the job failed.
	if !a.soxCanDecode(abs) {
		return "", nil, api.ErrUpscaleIneligible
	}
	return abs, track, nil
}

// finalizeAndEnqueue is the shared trailing scaffolding for both
// `EnqueueOne` and `EnqueueOptimize`: capture freshness, run the
// resumability check, hand to the pool, map pool errors. The kind-
// specific spec construction (target rate, bits, Kind field) stays
// in each caller; this function takes the prepared spec.
//
// `errPoolClosedAsSourceMissing` chooses between the two documented
// `ErrPoolClosed` mappings:
//   - true → `api.ErrUpscaleSourceMissing` (upscale path; matches
//     CLAUDE.md PR #109's CodeRabbit nit — iOS UX wants the
//     "feature unavailable" framing).
//   - false → `fmt.Errorf("pool closed: %w", ...)` (optimize path;
//     Gemini bot review on PR #270 — wrap original sentinel so the
//     handler logs the real cause; optimize is invisible runtime
//     infrastructure with no user-facing toast).
func (a *upscaleEnqueuerAdapter) finalizeAndEnqueue(spec transcode.JobSpec, trackPath string, errPoolClosedAsSourceMissing bool) error {
	if err := spec.FreshnessFromFile(); err != nil {
		return api.ErrUpscaleSourceMissing
	}
	existing, getVErr := a.store.LookupVariant(context.Background(), trackPath, spec.VariantID())
	if getVErr != nil {
		return fmt.Errorf("get variant row: %w", getVErr)
	}
	if existing != nil && existing.SourceMTimeNS == spec.SourceMTimeNS && existing.SourceSize == spec.SourceSize {
		return api.ErrUpscaleIneligible
	}
	enqueueErr := a.pool.Enqueue(spec)
	switch {
	case errors.Is(enqueueErr, transcode.ErrQueueFull):
		return api.ErrUpscaleQueueFull
	case errors.Is(enqueueErr, transcode.ErrPoolClosed):
		if errPoolClosedAsSourceMissing {
			return api.ErrUpscaleSourceMissing
		}
		return fmt.Errorf("pool closed: %w", enqueueErr)
	case errors.Is(enqueueErr, transcode.ErrDuplicateInflight):
		// Already queued/running from a prior request — treat as accepted.
		return nil
	case enqueueErr != nil:
		return fmt.Errorf("enqueue: %w", enqueueErr)
	}
	return nil
}

// buildOptimizeSpec runs the optimize-kind eligibility gate against
// `track` and returns the JobSpec on accept, or a typed error on
// reject. Split out of `EnqueueOptimize` to keep that function's
// cognitive complexity below the repo gate. Pure path / DB-free.
func buildOptimizeSpec(track *manifest.Track, absPath, outputDir string) (transcode.JobSpec, error) {
	if track.SampleRate == nil || track.BitsPerSample == nil {
		return transcode.JobSpec{}, api.ErrUpscaleIneligible
	}
	sourceHz := int(*track.SampleRate)
	sourceBits := *track.BitsPerSample
	if !transcode.OptimizeEligible(track.Path, track.Codec, sourceHz, sourceBits) {
		return transcode.JobSpec{}, api.ErrUpscaleIneligible
	}
	target, err := transcode.ResolveTargetRateForOptimize(sourceHz)
	if err != nil {
		return transcode.JobSpec{}, fmt.Errorf("resolve optimize target rate: %w", err)
	}
	// `OptimizeEligible` above is the authoritative gate; the
	// resolver always returns a real target now (does NOT re-evaluate
	// "is the source at the floor" — a 44.1/24 candidate flows
	// through with target=44.1k). Don't reintroduce a `target == 0`
	// skip (Gemini bot review on PR #270).
	return transcode.JobSpec{
		SourceAbsPath:    absPath,
		SourceLibraryRel: track.Path,
		SourceSampleRate: sourceHz,
		SourceBits:       sourceBits,
		TargetSampleRate: target,
		TargetBits:       16,
		Quality:          transcode.QualityVeryHigh,
		OutputDir:        outputDir,
		Kind:             transcode.JobKindOptimize,
	}, nil
}

// EnqueueOptimize is the CarPlay-targeted parallel of EnqueueOne.
// Downsamples hi-res PCM sources to 16-bit / 44.1k or 48k
// (family-preserving — see transcode.TargetRateForOptimize). Reuses
// the same Pool, same `track_variants` table, same resumability
// gate via spec.VariantID() (kind-aware). The only shape
// differences vs. upscale: eligibility predicate
// (transcode.OptimizeEligible — PCM-hi-res only), target-rate
// resolver (ResolveTargetRateForOptimize), TargetBits fixed to 16,
// and Kind: transcode.JobKindOptimize on the JobSpec.
//
// Same error taxonomy as EnqueueOne so the handler's switch arm
// doesn't have to discriminate.
func (a *upscaleEnqueuerAdapter) EnqueueOptimize(libraryRelativePath string) error {
	abs, track, err := a.resolveAndLookupTrack(libraryRelativePath)
	if err != nil {
		return err
	}
	spec, err := buildOptimizeSpec(track, abs, a.outputDir())
	if err != nil {
		return err
	}
	return a.finalizeAndEnqueue(spec, track.Path, false)
}

func (a *upscaleEnqueuerAdapter) EnqueueOne(libraryRelativePath string) error {
	abs, track, err := a.resolveAndLookupTrack(libraryRelativePath)
	if err != nil {
		return err
	}
	// Lossy sources are never upscaled (PROTOCOL.md documents the
	// gate as "PCM"; upscaling decoded lossy audio adds no fidelity).
	// Mirrors Coordinator.Submit's batch walk — single source of
	// truth in manifest.IsLossyCodec.
	if manifest.IsLossyCodec(track.Codec) {
		return api.ErrUpscaleIneligible
	}
	if track.SampleRate == nil {
		return api.ErrUpscaleIneligible
	}
	sourceHz := int(*track.SampleRate)
	// SourceBits feeds the live worker grid's signal chain only; nil → 0
	// (unknown). Upscale targets a fixed bit depth, so a missing source
	// bit depth is not an eligibility concern.
	srcBits := 0
	if track.BitsPerSample != nil {
		srcBits = *track.BitsPerSample
	}
	// Resolve the target from the LIVE operator-controlled setting in
	// scan_state (GetUpscaleTarget) — the same source the batch path
	// (upscaleBatchCoordinatorAdapter.Submit) and the admin Inspector
	// PATCH use. Reading a.cfg here froze the boot-time YAML target:
	// admin target edits go to the DB via SetUpscaleTarget and NEVER
	// touch the copy-on-write config, so the per-track POST /v1/upscale
	// path kept converting to the stale bootstrap rate/bits after any
	// runtime change (the sibling outputDir field was made a live closure
	// for this same staleness class). Fall back to the bridge.yaml
	// bootstrap value when the DB target is unset — startup seeds it, so
	// this is a belt-and-braces path. Passing the resolved integer rate
	// as a string keeps ResolveTargetRate's "never downsample /
	// already-at-target → skip" semantics intact.
	rateSetting := a.cfg.Upscale.EffectiveTargetRate()
	targetBits := a.cfg.Upscale.EffectiveTargetBits()
	if liveRate, liveBits, gErr := a.store.GetUpscaleTarget(context.Background()); gErr == nil {
		rateSetting = strconv.Itoa(liveRate)
		targetBits = liveBits
	} else if !errors.Is(gErr, manifest.ErrUpscaleTargetUnset) {
		// ErrUpscaleTargetUnset is the fresh-DB "never seeded" case → fall
		// through to the bridge.yaml bootstrap default (startup seeds
		// scan_state). Any OTHER error is a real store fault — and the
		// imminent LookupVariant in finalizeAndEnqueue reads the same
		// store and would hit it too — so propagate it (matching the
		// adapter's LookupTrack/LookupVariant DB-error propagation) rather
		// than silently converting at the stale bootstrap target. Gemini
		// medium on PR #524.
		return fmt.Errorf("get live upscale target: %w", gErr)
	}
	target, err := transcode.ResolveTargetRate(rateSetting, sourceHz)
	if err != nil {
		return fmt.Errorf("resolve target rate: %w", err)
	}
	if target == 0 {
		return api.ErrUpscaleIneligible
	}
	// Use the manifest's canonical-case path — NOT the iOS-shaped
	// input — for `SourceLibraryRel`. The variant insert hits a
	// `FOREIGN KEY (source_path) REFERENCES tracks(path)` constraint
	// on the case-sensitive PRIMARY KEY; passing the lowercase iOS
	// shape through to UpsertVariant makes the FK fail at write
	// time. Per CLAUDE.md PR #126.
	spec := transcode.JobSpec{
		SourceAbsPath:    abs,
		SourceLibraryRel: track.Path,
		SourceSampleRate: sourceHz,
		SourceBits:       srcBits,
		TargetSampleRate: target,
		TargetBits:       targetBits,
		Quality:          transcode.QualityVeryHigh,
		OutputDir:        a.outputDir(),
	}
	return a.finalizeAndEnqueue(spec, track.Path, true)
}

// upscaleBatchCoordinatorAdapter implements api.BatchCoordinator
// over a *transcode.Coordinator. Translates between the two
// packages' equivalent value types (SubmitResult, ThroughputSnapshot,
// UpscaleBatchRow → BatchSubmitResult, BatchThroughput, BatchRow)
// and injects the bridge-wired `outputDir` so the api package
// stays free of that concern. Mirrors the upscaleEnqueuerAdapter
// shape.
type upscaleBatchCoordinatorAdapter struct {
	coord *transcode.Coordinator
	store *manifest.Store
	// outputDir resolves the CURRENT effective variants dir per call
	// — see upscaleEnqueuerAdapter.outputDir for the hot-reload
	// rationale.
	outputDir func() string
}

// translateApiSubmitResult is the shared result/error translator
// used by both Submit and SubmitOptimize on the api-side adapter.
// Translates transcode's typed disk-space error into the api
// package's mirror, and copies the success-result fields field-
// for-field (the two value types intentionally don't share a
// definition so the api package stays free of internal/transcode).
// Extracted to dedup the result-copy boilerplate after the
// optimize-batch surface landed on PR #276.
func translateApiSubmitResult(res *transcode.SubmitResult, err error) (api.BatchSubmitResult, error) {
	if err != nil {
		var dskErr *transcode.InsufficientDiskSpaceError
		if errors.As(err, &dskErr) {
			return api.BatchSubmitResult{}, &api.BatchInsufficientDiskSpace{
				ProjectedBytes: dskErr.ProjectedBytes,
				RequiredBytes:  dskErr.RequiredBytes,
				AvailableBytes: dskErr.AvailableBytes,
			}
		}
		return api.BatchSubmitResult{}, err
	}
	return api.BatchSubmitResult{
		BatchID:            res.BatchID.String(),
		Path:               res.Path,
		TargetRate:         res.TargetRate,
		TargetBits:         res.TargetBits,
		TotalFiles:         res.TotalFiles,
		AlreadyCovered:     res.AlreadyCovered,
		ProjectedSizeBytes: res.ProjectedSizeBytes,
		AvailableBytes:     res.AvailableBytes,
		EnqueuedCount:      res.EnqueuedCount,
	}, nil
}

func (a *upscaleBatchCoordinatorAdapter) Submit(ctx context.Context, libraryRelPath string, targetRate, targetBits int) (api.BatchSubmitResult, error) {
	// Resolve the active target from scan_state when the caller
	// didn't override. Coordinator.Submit validates the resolved
	// values and returns an error on out-of-range.
	if targetRate == 0 || targetBits == 0 {
		rate, bits, err := a.store.GetUpscaleTarget(ctx)
		if err == nil {
			if targetRate == 0 {
				targetRate = rate
			}
			if targetBits == 0 {
				targetBits = bits
			}
		}
	}
	res, err := a.coord.Submit(ctx, libraryRelPath, targetRate, targetBits, a.outputDir())
	return translateApiSubmitResult(res, err)
}

func (a *upscaleBatchCoordinatorAdapter) SubmitOptimize(ctx context.Context, libraryRelPath string) (api.BatchSubmitResult, error) {
	res, err := a.coord.SubmitOptimize(ctx, libraryRelPath, a.outputDir())
	return translateApiSubmitResult(res, err)
}

func (a *upscaleBatchCoordinatorAdapter) Cancel(id uuid.UUID) error {
	return a.coord.Cancel(id)
}

func (a *upscaleBatchCoordinatorAdapter) ListBatches(limit int) ([]api.BatchRow, error) {
	rows, err := a.store.ListUpscaleBatches(context.Background(), limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.BatchRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, api.BatchRow{
			ID:             r.ID.String(),
			Path:           r.Path,
			TargetRate:     r.TargetRate,
			TargetBits:     r.TargetBits,
			Status:         r.Status,
			TotalFiles:     r.TotalFiles,
			ProcessedFiles: r.ProcessedFiles,
			FailedFiles:    r.FailedFiles,
			SkippedFiles:   r.SkippedFiles,
			Error:          r.Error,
			CreatedAt:      r.CreatedAt,
			UpdatedAt:      r.UpdatedAt,
		})
	}
	return out, nil
}

func (a *upscaleBatchCoordinatorAdapter) Throughput() api.BatchThroughput {
	t := a.coord.Throughput()
	return api.BatchThroughput{
		JobsPerHour: t.JobsPerHour,
		EtaSeconds:  t.EtaSeconds,
		Samples:     t.Samples,
	}
}

// adminBatchCoordinatorAdapter implements admin.AdminBatchCoordinator
// over a *transcode.Coordinator. Translates between the two
// packages' equivalent value types so the admin package stays free
// of internal/transcode (mirrors UpscaleEnqueuer / UpscaleStats).
type adminBatchCoordinatorAdapter struct {
	coord *transcode.Coordinator
	store *manifest.Store
	// outputDir resolves the CURRENT effective variants dir per call
	// — see upscaleEnqueuerAdapter.outputDir for the hot-reload
	// rationale.
	outputDir func() string
}

// translateAdminSubmitResult mirrors translateApiSubmitResult for
// the admin-side adapter; same dedup pattern, different target
// value types (admin.AdminBatchSubmitResult / AdminBatchInsufficient-
// DiskSpace can't share with api's because the admin package
// intentionally doesn't import internal/api).
func translateAdminSubmitResult(res *transcode.SubmitResult, err error) (admin.AdminBatchSubmitResult, error) {
	if err != nil {
		var dskErr *transcode.InsufficientDiskSpaceError
		if errors.As(err, &dskErr) {
			return admin.AdminBatchSubmitResult{}, &admin.AdminBatchInsufficientDiskSpace{
				ProjectedBytes: dskErr.ProjectedBytes,
				RequiredBytes:  dskErr.RequiredBytes,
				AvailableBytes: dskErr.AvailableBytes,
			}
		}
		return admin.AdminBatchSubmitResult{}, err
	}
	return admin.AdminBatchSubmitResult{
		BatchID:            res.BatchID.String(),
		Path:               res.Path,
		TargetRate:         res.TargetRate,
		TargetBits:         res.TargetBits,
		TotalFiles:         res.TotalFiles,
		AlreadyCovered:     res.AlreadyCovered,
		ProjectedSizeBytes: res.ProjectedSizeBytes,
		AvailableBytes:     res.AvailableBytes,
		EnqueuedCount:      res.EnqueuedCount,
	}, nil
}

func (a *adminBatchCoordinatorAdapter) Submit(ctx context.Context, libraryRelPath string, targetRate, targetBits int) (admin.AdminBatchSubmitResult, error) {
	if targetRate == 0 || targetBits == 0 {
		if rate, bits, err := a.store.GetUpscaleTarget(ctx); err == nil {
			if targetRate == 0 {
				targetRate = rate
			}
			if targetBits == 0 {
				targetBits = bits
			}
		}
	}
	res, err := a.coord.Submit(ctx, libraryRelPath, targetRate, targetBits, a.outputDir())
	return translateAdminSubmitResult(res, err)
}

func (a *adminBatchCoordinatorAdapter) SubmitOptimize(ctx context.Context, libraryRelPath string) (admin.AdminBatchSubmitResult, error) {
	res, err := a.coord.SubmitOptimize(ctx, libraryRelPath, a.outputDir())
	return translateAdminSubmitResult(res, err)
}

func (a *adminBatchCoordinatorAdapter) Cancel(idHex string) error {
	id, err := uuid.Parse(idHex)
	if err != nil {
		return fmt.Errorf("parse batch id %q: %w", idHex, err)
	}
	return a.coord.Cancel(id)
}

func (a *adminBatchCoordinatorAdapter) ListBatches(limit int) ([]admin.AdminBatchRow, error) {
	rows, err := a.store.ListUpscaleBatches(context.Background(), limit)
	if err != nil {
		return nil, err
	}
	out := make([]admin.AdminBatchRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, admin.AdminBatchRow{
			ID:             r.ID.String(),
			Path:           r.Path,
			TargetRate:     r.TargetRate,
			TargetBits:     r.TargetBits,
			Status:         r.Status,
			TotalFiles:     r.TotalFiles,
			ProcessedFiles: r.ProcessedFiles,
			FailedFiles:    r.FailedFiles,
			SkippedFiles:   r.SkippedFiles,
			Error:          r.Error,
			// time.Time encoded as RFC 3339 — avoids JS Number
			// precision loss on int64 ns values > 2^53. The
			// iOS-facing api.BatchRow keeps int64 ns because Swift
			// handles Int64 natively. Per Gemini high on PR #202.
			CreatedAt: time.Unix(0, r.CreatedAt).UTC(),
			UpdatedAt: time.Unix(0, r.UpdatedAt).UTC(),
		})
	}
	return out, nil
}

// adminVariantDeleterAdapter implements admin.AdminVariantDeleter
// over the api.Server's `RunVariantDelete` method. Translates
// between the two packages' equivalent request / response types so
// the admin package stays free of internal/api (mirrors
// `adminBatchCoordinatorAdapter` / UpscaleStats decoupling), AND so
// the destructive list/unlink/DB-delete/SSE loop has exactly one
// implementation regardless of caller (admin console vs. iOS app
// vs. direct curl). The shared loop is what guarantees the
// `upscale.deleted` SSE event fans out identically across all
// three surfaces — paired clients reconcile their local state
// regardless of origin.
//
// Nil-safe at construction: when `apiSrv` is nil (test harness,
// pre-feature build) the admin Deps field is set to nil, and the
// admin handler's `s.deps.VariantDeleter == nil` short-circuit
// surfaces 503. The adapter itself never panics on a nil server.
type adminVariantDeleterAdapter struct {
	apiSrv *api.Server
}

func (a *adminVariantDeleterAdapter) Delete(ctx context.Context, req admin.AdminVariantDeleteRequest) (admin.AdminVariantDeleteResponse, error) {
	if a == nil || a.apiSrv == nil {
		return admin.AdminVariantDeleteResponse{}, admin.ErrAdminVariantDeleterUnavailable
	}
	apiReq := api.VariantDeleteRequest{
		All:    req.All,
		Prefix: req.Prefix,
		Path:   req.Path,
		Kind:   req.Kind,
	}
	resp, err := a.apiSrv.RunVariantDelete(ctx, apiReq)
	if err != nil {
		if errors.Is(err, api.ErrVariantDeleteUnavailable) {
			// Translate the api package's sentinel to the admin
			// package's sentinel so the admin handler can match
			// without importing internal/api.
			return admin.AdminVariantDeleteResponse{}, admin.ErrAdminVariantDeleterUnavailable
		}
		return admin.AdminVariantDeleteResponse{}, err
	}
	return admin.AdminVariantDeleteResponse{
		DeletedCount: resp.DeletedCount,
		FreedBytes:   resp.FreedBytes,
		DeletedPaths: resp.DeletedPaths,
	}, nil
}

func (a *adminBatchCoordinatorAdapter) Throughput() admin.AdminBatchThroughput {
	t := a.coord.Throughput()
	return admin.AdminBatchThroughput{
		JobsPerHour: t.JobsPerHour,
		EtaSeconds:  t.EtaSeconds,
		Samples:     t.Samples,
	}
}

// upscaleStatsAdapter implements api.UpscaleStatsProvider. Mirrors
// the admin /api/upscale/stats handler's data sources field-for-
// field so the operator's Settings tile and a paired iOS client see
// the same numbers.
//
// Two pool-related closures (rather than a captured `*transcode.Pool`):
// the pool reference itself can be nil (operator never enabled the
// feature) AND the operator can flip `cfg.Upscale.Enabled = false`
// mid-flight without restart, leaving a live but logically-disabled
// Pool. The closures evaluate both conditions at snapshot time so
// `enabled` and the `pool` payload move together — same gating the
// admin `UpscaleStats` closure already uses.
//
// **Known limitation**: `cfg.Upscale.Enabled` is read here without
// synchronization while the admin PATCH handler writes the same
// field under `admin.Server.mu`. This data race already existed in
// the admin tile's closure (cmd/bridge/main.go:909) and is out-of-
// scope for this endpoint addition; the proper fix is an `atomic.Bool`
// on `*config.Config` (touching admin's writer too). Worst case
// today: a single 5 s poll snapshot reads a racing flag value and
// reports `enabled` inconsistently with the freshly-PATCHed state;
// the next poll converges.
//
// Sox precheck is TTL-cached (mirrors `admin.Server.cachedSoxAvailability`,
// also 30 s) so the per-5-s poll doesn't shell out 12×/min — the
// precheck forks `sox --version`, which is cheap but not free, and
// gemini-code-assist reasonably flagged the per-call cost on PR #111.
type upscaleStatsAdapter struct {
	pool    func() *transcode.Pool
	enabled func() bool
	store   *manifest.Store

	soxMu sync.Mutex
	soxAt time.Time
	soxOK bool
}

const upscaleStatsSoxTTL = 30 * time.Second

func (a *upscaleStatsAdapter) UpscaleStatsSnapshot(ctx context.Context) (api.UpscaleStats, error) {
	var snap api.UpscaleStats
	if a.enabled() {
		if p := a.pool(); p != nil {
			s := p.Stats()
			snap.Pool = &api.UpscalePoolStats{
				Workers:  s.Workers,
				QueueCap: s.QueueCap,
				QueueLen: s.QueueLen,
				Inflight: s.Inflight,
				Enqueued: s.Enqueued,
				Done:     s.Done,
				Failed:   s.Failed,
			}
		}
	}
	snap.Enabled = (snap.Pool != nil)
	soxOK := a.cachedSoxOK()
	snap.SoxAvailable = &soxOK
	if a.store != nil {
		count, bytes, err := a.store.CountVariants(ctx)
		if err != nil {
			// Two failure shapes get different treatment
			// (Gemini HIGH on PR #218):
			//
			//   - ctx-cancellation (handler 2s timeout fired,
			//     SSE publisher 2s timeout fired, or the caller
			//     disconnected) — return the error so the
			//     handler can emit a 5xx promptly. iOS treats
			//     5xx as "feature status unknown" (same UX
			//     the pre-PR-218 silent-zero produced).
			//
			//   - Genuine SQL faults (disk full, corruption,
			//     migration mid-flight) — keep the legacy
			//     degrade-and-log policy: log + return live
			//     counters with zero cachedVariants. Operators
			//     check logs; iOS shows "feature off". This
			//     mirrors the admin tile's PR #110 contract.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return snap, err
			}
			logger.Warn("upscale stats: count variants", "err", err)
		} else {
			snap.CachedVariants = count
			snap.CachedBytes = bytes
		}
	}
	return snap, nil
}

// cachedSoxOK returns the most recent `transcode.PrecheckSox` result
// or runs a fresh probe when the cache is older than
// `upscaleStatsSoxTTL`. Mirrors `admin.Server.cachedSoxAvailability`'s
// 30 s TTL so the operator's Settings tile and the iOS-facing
// endpoint stay aligned on what the host reports.
func (a *upscaleStatsAdapter) cachedSoxOK() bool {
	a.soxMu.Lock()
	defer a.soxMu.Unlock()
	if !a.soxAt.IsZero() && time.Since(a.soxAt) < upscaleStatsSoxTTL {
		return a.soxOK
	}
	a.soxOK = (transcode.PrecheckSox() == nil)
	a.soxAt = time.Now()
	return a.soxOK
}

// analysisStatsAdapter implements api.AnalysisStatsProvider. Mirrors
// upscaleStatsAdapter, minus the live pool: serve-side analysis
// generation is CLI-driven (`bridge analyze`), so there's no long-lived
// serve pool to snapshot — `Enabled` reflects the config+sox gate
// directly (not pool presence), and Pool stays nil. Counts come from
// CountAnalysis; sox precheck shares the same 30 s TTL cache shape.
type analysisStatsAdapter struct {
	enabled func() bool
	store   *manifest.Store

	soxMu sync.Mutex
	soxAt time.Time
	soxOK bool
}

func (a *analysisStatsAdapter) AnalysisStatsSnapshot(ctx context.Context) (api.AnalysisStats, error) {
	var snap api.AnalysisStats
	snap.Enabled = a.enabled()
	soxOK := a.cachedSoxOK()
	snap.SoxAvailable = &soxOK
	if a.store != nil {
		count, bytes, err := a.store.CountAnalysis(ctx)
		if err != nil {
			// Same two-shape treatment as upscaleStatsAdapter: ctx
			// cancellation/timeout surfaces as a 5xx; genuine SQL
			// faults degrade to logged + zero counts.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return snap, err
			}
			logger.Warn("analysis stats: count analysis", "err", err)
		} else {
			snap.CachedWaveforms = count
			snap.CachedBytes = bytes
		}
	}
	return snap, nil
}

func (a *analysisStatsAdapter) cachedSoxOK() bool {
	a.soxMu.Lock()
	defer a.soxMu.Unlock()
	if !a.soxAt.IsZero() && time.Since(a.soxAt) < upscaleStatsSoxTTL {
		return a.soxOK
	}
	a.soxOK = (transcode.PrecheckSox() == nil)
	a.soxAt = time.Now()
	return a.soxOK
}

// soxToolchainCache memoizes one transcode.ProbeSox result for adminSoxTTL
// so the admin Settings page (which probes sox on every load) doesn't
// fork-exec sox per request. Both the availability closure (precheck, the
// admin.Deps.UpscalePrecheck contract) and the FLAC closure
// (admin.Deps.UpscaleSoxFLAC) read the SAME snapshot, so wiring both costs
// at most one probe per TTL window.
type soxToolchainCache struct {
	mu   sync.Mutex
	at   time.Time
	info transcode.SoxInfo
	err  error
}

const adminSoxTTL = 30 * time.Second

func (c *soxToolchainCache) snapshot() (transcode.SoxInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < adminSoxTTL {
		return c.info, c.err
	}
	c.info, c.err = transcode.ProbeSox(context.Background())
	c.at = time.Now()
	return c.info, c.err
}

// precheck satisfies admin.Deps.UpscalePrecheck (nil == sox runnable).
func (c *soxToolchainCache) precheck() error {
	_, err := c.snapshot()
	return err
}

// flac satisfies admin.Deps.UpscaleSoxFLAC. known is false when the probe
// errored or `sox --help` couldn't be parsed — the admin then omits the
// FLAC field rather than asserting a guess.
func (c *soxToolchainCache) flac() (hasFLAC, known bool) {
	info, err := c.snapshot()
	if err != nil {
		return false, false
	}
	return info.HasFLAC, info.FormatsKnown
}

// updateInfoAdapter bridges *updater.Updater to api.UpdaterStatus +
// admin's read-side without coupling those packages to the updater
// type. Trivial; lives here at the wiring point so the api / admin
// packages stay agnostic of where their update info comes from.
//
// dataDir + binaryPath are needed for the install path so the
// adapter can construct InstallOptions on each call without making
// the admin package aware of either. canInstall is captured at
// construction time from runtime.GOOS so the dashboard can gate the
// Install button on platform support.
type updateInfoAdapter struct {
	u          *updater.Updater
	sessions   *updater.Tracker
	dataDir    string
	binaryPath string
	canInstall bool
}

func (a updateInfoAdapter) UpdateInfo() api.UpdateInfo {
	s := a.u.Status()
	return api.UpdateInfo{
		LatestVersion:    s.LatestVersion,
		UpdateAvailable:  s.UpdateAvailable,
		ReleaseNotesURL:  s.ReleaseNotesURL,
		MinClientVersion: version.MinClientVersion,
	}
}

func (a updateInfoAdapter) Status() admin.UpdateStatus {
	s := a.u.Status()
	return admin.UpdateStatus{
		CurrentVersion:   s.CurrentVersion,
		LatestVersion:    s.LatestVersion,
		UpdateAvailable:  s.UpdateAvailable,
		ReleaseNotesURL:  s.ReleaseNotesURL,
		Channel:          s.Channel,
		LastCheck:        s.LastCheck,
		LastError:        s.LastError,
		MinClientVersion: version.MinClientVersion,
		CanInstall:       a.canInstall,
		CanRollback:      a.canRollback(),
		DeferredReason:   s.DeferredReason,
		RejectedVersion:  a.rejectedVersion(),
	}
}

// rejectedVersion reads the release the operator rolled back on this
// host, so the dashboard can explain a bridge that reports an available
// update and never installs it. Read from the same on-disk marker the
// auto-install gate consults — deriving it any other way would let the
// two disagree. Same per-call-read shape as canRollback's stat, and on
// the same 30 s SSE cadence.
func (a updateInfoAdapter) rejectedVersion() string {
	if a.dataDir == "" {
		return ""
	}
	st, err := updater.LoadState(a.dataDir)
	if err != nil {
		return ""
	}
	return st.RejectedVersion
}

// canRollback reports whether the installer's backup of the previous
// binary is present. RollbackBinary renames "<binary>.bak" over the live
// path, so the presence of that file IS the precondition — deriving it
// any other way (a flag in update-state.json, say) would let the two
// disagree after a manual cleanup.
//
// Conservative on a stat error: an unreadable path means we cannot
// promise a rollback would work, and offering one we can't honour is the
// failure this flag exists to prevent.
func (a updateInfoAdapter) canRollback() bool {
	if a.binaryPath == "" {
		return false
	}
	fi, err := os.Stat(a.binaryPath + ".bak")
	return err == nil && !fi.IsDir()
}

func (a updateInfoAdapter) CheckNow(ctx context.Context) admin.UpdateStatus {
	a.u.CheckNow(ctx)
	return a.Status()
}

func (a updateInfoAdapter) Install(ctx context.Context, force bool) (admin.UpdateStatus, error) {
	st, err := a.u.Install(ctx, updater.InstallOptions{
		DataDir:    a.dataDir,
		BinaryPath: a.binaryPath,
		Force:      force,
		Sessions:   a.sessions,
	})
	return admin.UpdateStatus{
		CurrentVersion:   st.CurrentVersion,
		LatestVersion:    st.LatestVersion,
		UpdateAvailable:  st.UpdateAvailable,
		ReleaseNotesURL:  st.ReleaseNotesURL,
		Channel:          st.Channel,
		LastCheck:        st.LastCheck,
		LastError:        st.LastError,
		MinClientVersion: version.MinClientVersion,
		CanInstall:       a.canInstall,
	}, mapUpdaterError(err)
}

func (a updateInfoAdapter) Rollback(force bool) error {
	return mapUpdaterError(a.u.Rollback(updater.InstallOptions{
		DataDir:    a.dataDir,
		BinaryPath: a.binaryPath,
		Force:      force,
		Sessions:   a.sessions,
	}))
}

// mapUpdaterError translates internal/updater's typed sentinel
// errors to the admin-package equivalents so handlers_api.go's
// classifyUpdateError can switch on errors.Is without importing
// internal/updater. The original error message is preserved as the
// %w child so the operator-facing detail still threads through.
//
// New sentinel pairings land here as the Phase C / future work
// expands the install error surface.
func mapUpdaterError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, updater.ErrNoUpdate):
		return fmt.Errorf(errWrapDetailFormat, admin.ErrUpdateNoUpdate, err.Error())
	case errors.Is(err, updater.ErrActiveSessions):
		return fmt.Errorf(errWrapDetailFormat, admin.ErrUpdateActiveSessions, err.Error())
	case errors.Is(err, updater.ErrInstallInFlight):
		return fmt.Errorf(errWrapDetailFormat, admin.ErrUpdateInstallInFlight, err.Error())
	case errors.Is(err, updater.ErrInstallNotSupported):
		return fmt.Errorf(errWrapDetailFormat, admin.ErrUpdateNotSupported, err.Error())
	case errors.Is(err, updater.ErrPathNotWritable):
		return fmt.Errorf(errWrapDetailFormat, admin.ErrUpdatePathNotWritable, err.Error())
	default:
		return err
	}
}

// artworkDirBridge lets cmd/bridge expose the enricher's cache dir to
// internal/api without importing internal/enrich from there.
type artworkDirBridge string

func (a artworkDirBridge) ArtworkCacheDir() string { return string(a) }

// shutdownGrace is how long we wait for in-flight requests to drain before
// forcing the listener closed.
const shutdownGrace = 5 * time.Second

// maybeRollbackOnBoot consults <dataDir>/update-state.json and acts
// on whatever the previous install attempt's outcome was:
//
//   - first boot after a successful install: stamp installedAt and
//     retain bridge.bak for one more boot.
//   - boot after that: delete bridge.bak.
//   - first boot after a FAILED install (we're not at TargetVersion):
//     restore bridge.bak and clear the marker. Service manager will
//     then respawn into the restored old binary on next exit.
//   - everything else: no-op.
//
// Failures here are logged but non-fatal — a botched rollback still
// lets the server start up; the operator can recover manually.
func maybeRollbackOnBoot(stderr io.Writer, dataDir, binaryPath string) {
	st, err := updater.LoadState(dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "updater: load state: %v\n", err)
		return
	}
	action := updater.DecideBootAction(st, version.ServerVersion, time.Now().UTC())
	switch action {
	case updater.BootNoop:
		return
	case updater.BootInstallSucceeded:
		// New binary booted; record installedAt so the NEXT boot
		// knows it can clean up the .bak.
		st.Status = "installed"
		st.InstalledAt = time.Now().UTC()
		if err := updater.SaveState(dataDir, st); err != nil {
			fmt.Fprintf(stderr, "updater: mark installed: %v\n", err)
		}
	case updater.BootCleanupBak:
		// Second boot after a successful install: clean up.
		if err := updater.RemoveBackup(binaryPath, ".bak"); err != nil {
			fmt.Fprintf(stderr, "updater: remove .bak: %v\n", err)
		}
		if err := updater.ClearState(dataDir); err != nil {
			fmt.Fprintf(stderr, "updater: clear state: %v\n", err)
		}
	case updater.BootInstallFailed:
		// New binary didn't come up at the expected version.
		// Restore the previous binary and clear the marker so the
		// next exit (planned or via service-manager respawn) lands
		// us back on the old version.
		fmt.Fprintf(stderr, "updater: install of %s did not reach the expected version (running %s); rolling back to .bak\n",
			st.TargetVersion, version.ServerVersion)
		if err := updater.RollbackBinary(binaryPath, ".bak"); err != nil {
			fmt.Fprintf(stderr, "updater: rollback failed: %v (manual recovery needed at %s)\n",
				err, binaryPath)
		}
		if err := updater.ClearState(dataDir); err != nil {
			fmt.Fprintf(stderr, "updater: clear state: %v\n", err)
		}
	case updater.BootClearNotSwapped:
		// The marker was armed but the install never reached its first
		// destructive step (killed during the Windows SCM stop, say), so
		// .bak predates the binary we're running — restoring it would
		// downgrade the host rather than roll it back. Clear only.
		fmt.Fprintf(stderr, "updater: install of %s never reached the binary swap; leaving %s.bak alone\n",
			st.TargetVersion, binaryPath)
		if err := updater.ClearState(dataDir); err != nil {
			fmt.Fprintf(stderr, "updater: clear state: %v\n", err)
		}
	case updater.BootClearAbandoned:
		// Marker is older than the recency window — nothing
		// actionable, just clean up.
		if err := updater.ClearState(dataDir); err != nil {
			fmt.Fprintf(stderr, "updater: clear abandoned state: %v\n", err)
		}
	}
}

func main() {
	// Windows-service dispatch. When SCM launches bridge.exe, os.Args
	// is the service binary's configured ImagePath (`bridge.exe serve
	// --config <path>`), and stdout/stderr aren't attached to a console.
	// runAsWindowsService translates SCM Stop into ctx cancel, so the
	// existing graceful-shutdown path in serveCmd runs unchanged.
	//
	// The stub in service_other.go always returns false on non-Windows,
	// so this branch is a no-op off Windows.
	if isWindowsService() {
		redirectServiceIO() // stdout/stderr → %PROGRAMDATA%\1-bit-bridge\bridge.log
		// Init slog AFTER the service IO redirect — the redirected
		// os.Stderr is what we want telemetry to land in (the
		// service log file).
		logging.Init(os.Stderr)
		// Surface service-dispatch errors to whatever stdio we have so
		// operators see them in the log. Previously this was
		// `_ = runAsWindowsService(...)`, which meant a service that
		// failed on boot (e.g. svc.Run couldn't register with the SCM,
		// or the subcommand exited non-zero) became a clean exit 0 —
		// SCM would just retry silently per its restart policy, leaving
		// no trace of what actually broke.
		if err := runAsWindowsService(
			context.Background(),
			"1-bit-bridge",
			func(ctx context.Context) error {
				code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
				if code != 0 {
					return fmt.Errorf("subcommand exited with code %d", code)
				}
				return nil
			},
			os.Stderr,
		); err != nil {
			fmt.Fprintf(os.Stderr, "service dispatch: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Init slog telemetry. CLI commands keep their `fmt.Fprintf`
	// stdout/stderr surfaces — slog is for backend telemetry only
	// (scanner / enricher / admin / etc.) and lands on stderr by
	// default, matching the service log destination on Windows
	// (post-redirect) and stderr on macOS / Linux.
	logging.Init(os.Stderr)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses argv (without the program name) and dispatches to a subcommand.
// It is extracted from main so tests can drive it without spawning a process.
// Exit codes: 0 success, 1 subcommand failure, 2 usage error.
//
// ctx is used by serveCmd to trigger graceful shutdown (signal from main or
// cancellation from a test).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		// Bare `bridge` on a real TTY drops into the launcher menu.
		// Pipes / non-TTY (CI scripts, `bridge | cat`, automation)
		// fall through to the existing usage + exit 2 — automation
		// MUST keep seeing the original behavior.
		//
		// menuLoop owns its own ctx (context.Background) and creates
		// per-action signal scopes inside dispatch — see the comment
		// on menuLoop. The serve subcommand path keeps using ctx as
		// before.
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			return menuLoop(context.Background(), bufio.NewReader(os.Stdin), stdout, stderr)
		}
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "init":
		return initCmd(args[1:], os.Stdin, stdout, stderr)
	case "serve":
		return serveCmd(ctx, args[1:], stdout, stderr)
	case "pair":
		return pairCmd(args[1:], stdout, stderr)
	case "scan":
		return scanCmd(ctx, args[1:], stdout, stderr)
	case "upscale":
		return upscaleCmd(ctx, args[1:], stdout, stderr)
	case "analyze":
		return analyzeCmd(ctx, args[1:], stdout, stderr)
	case "optimize":
		return optimizeCmd(ctx, args[1:], stdout, stderr)
	case "variants":
		return variantsCmd(ctx, args[1:], stdout, stderr)
	case "artwork":
		return artworkCmd(ctx, args[1:], stdout, stderr)
	case "enrichment":
		return enrichmentCmd(ctx, args[1:], stdout, stderr)
	case "duplicates":
		return duplicatesCmd(ctx, args[1:], stdout, stderr)
	case "fingerprint":
		return fingerprintCmd(ctx, args[1:], stdout, stderr)
	case "doctor":
		return doctorCmd(args[1:], stdout, stderr)
	case "update":
		return updateCmd(ctx, args[1:], os.Stdin, stdout, stderr)
	case "backup":
		return backupCmd(args[1:], stdout, stderr)
	case "restore":
		return restoreCmd(args[1:], os.Stdin, stdout, stderr)
	case "token":
		return tokenCmd(args[1:], stdout, stderr)
	case "cert":
		return certCmd(args[1:], os.Stdin, stdout, stderr)
	case "status":
		return statusCmd(ctx, args[1:], stdout, stderr)
	case "health":
		return healthCmd(ctx, args[1:], stdout, stderr)
	case "logs":
		return logsCmd(ctx, args[1:], stdout, stderr)
	case "library":
		return libraryCmd(ctx, args[1:], stdout, stderr)
	case "admin":
		return adminCmd(args[1:], os.Stdin, stdout, stderr)
	case "manifest":
		return manifestCmd(args[1:], os.Stdin, stdout, stderr)
	case "tsnet":
		return tsnetCmd(ctx, args[1:], os.Stdin, stdout, stderr)
	case "start":
		return startCmd(args[1:], stdout, stderr)
	case "stop":
		return stopCmd(args[1:], stdout, stderr)
	case "restart":
		return restartCmd(args[1:], stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "1-bit-bridge %s (protocol v%d)\n", version.ServerVersion, version.ProtocolVersion)
		return 0
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand: %s\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `1-bit-bridge — companion server for the 1-bit iOS app.

Usage:
  bridge <subcommand> [flags]

Subcommands:
  init     First-time setup: writes config, mints TLS cert, installs service.
  serve    Run the HTTPS server.
  start    Boot the installed service (launchd / systemd / SCM).
  stop     Stop the installed service.
  restart  Bounce the installed service.
  status   Probe the running bridge — track count, endpoints, uptime.
  health   Container liveness probe: exit 0 iff the API listener accepts TCP on the
           configured listen address (the Docker HEALTHCHECK; no auth, no TLS probe).
  logs     Tail the per-OS bridge log file. -f to follow.
  library  Manage library roots: bridge library add|remove <path>.
  manifest Library-index maintenance: bridge manifest clear-missing purges tracks +
           folders rows marked missing across recent scans (typed-WIPE confirmation;
           --yes for scripts).
  admin    Manage admin console credentials (public-mode deployments).
  pair     Generate a new bearer token for an iOS client.
  scan     Force a full library rescan.
  analyze  Generate peak-envelope waveform sidecars for the iOS scrubber (requires sox +
           opt-in flag). bridge analyze --gc removes orphan sidecars.
  upscale  Generate high-rate FLAC sidecars from PCM sources (requires sox + opt-in flag).
  optimize Generate CarPlay-targeted 16-bit / 44.1k or 48k FLAC sidecars from hi-res PCM
           sources (requires sox + opt-in flag). Family-preserving downsample: 88.2/176.4k
           → 44.1k; 96/192k → 48k. Shrinks 100 MB hi-res FLAC to ~15-20 MB for fast
           CarPlay / cellular streaming with zero fidelity loss vs. what the head unit
           accepts.
  variants Manage the transcoded-variant cache: bridge variants move --to <path>
           relocates every cached sidecar (and its DB row) onto another disk.
  artwork  Maintain on-disk artwork cache: bridge artwork --gc removes orphans.
  enrichment
           Inspect and re-queue metadata gaps: bridge enrichment misses lists tracks
           short of a cover / artist MBID / release MBID and which of the three each
           lacks; bridge enrichment retry re-queues them (the "Retry missing" button,
           scripted).
  fingerprint
           Acoustically fingerprint audio files and report what AcoustID knows about
           them (requires fpcalc). A diagnostic for the tracks whose tags are too poor
           to match on text — it prints coverage, cost and the acceptance verdict, and
           writes nothing.
  duplicates
           Report duplicate track groups the iOS client would collapse, tiered by
           evidence (different-format / same-format / inconclusive, plus self-nested
           upload accidents). Read-only: deletes and moves are structurally absent.
  doctor   Preflight: check ports, directories, service manager before init.
  update   Check for / install a new bridge release from GitHub.
  backup   Snapshot bridge state into <dataDir>/backups/<timestamp>/.
  restore  Restore bridge state from a snapshot directory.
  token    Manage paired tokens (list / rotate / expire / revoke).
  cert     Inspect or rotate the TLS cert (info / rotate).
  tsnet    Embedded tailnet node management (auth | status | logout).
  version  Print version and protocol version.

Run "bridge <subcommand> -h" for subcommand-specific flags.

First-time install:
  bridge init                    # writes config + installs launchd/systemd unit
                                 # prints admin console URL at http://127.0.0.1:7789/
`)
}

// serveOpts bundles the inputs runServe needs. Lifted out of serveCmd's
// flag parsing so PR 2's interactive "Start now" picker (and PR 3's
// menu launcher) can drive a serve session in-process without
// re-parsing flags. Each call gets a fresh, signal-wired ctx from the
// caller — never share a parent ctx across multiple runServe
// invocations or the second call sees a canceled parent and shuts
// down instantly (Go contexts can't be un-canceled).
type serveOpts struct {
	configPath    string
	addrOverride  string
	initIfMissing bool
}

func serveCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	addrOverride := fs.String("addr", "", "override listenAddress from config (host:port)")
	initIfMissing := fs.Bool("init-if-missing", false, "on first run, write a default config if --config is missing, then serve (container convenience; env overrides still apply)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runServe(ctx, serveOpts{configPath: *configPath, addrOverride: *addrOverride, initIfMissing: *initIfMissing}, stdout, stderr)
}

// autoInitDefaultRoot is the library mount path serve's --init-if-missing
// seeds into the config when BRIDGE_LIBRARY_ROOTS is unset — the convention
// docs/docker.md uses (`-v ~/music:/library:ro`).
const autoInitDefaultRoot = "/library"

// writeAutoInitConfig writes a minimal loopback seed config for serve's
// --init-if-missing container path. It is DELIBERATELY sparse and does NOT
// read BRIDGE_* env: the config.Load that immediately follows applies
// applyEnvOverrides, so BRIDGE_LIBRARY_ROOTS / BRIDGE_LIBRARY_NAME /
// BRIDGE_DATA_DIR inject dynamically at runtime every boot — env stays the
// live source of truth and the YAML is only a fallback (baking day-one env
// values onto disk would mask a later docker-compose change). libraryRoots
// defaults to /library; dataDir "data" resolves under the config dir
// (matching init). No cert mint here — serve's LoadOrGenerate first-mints on
// the persistent volume. MkdirAll(0o700) because config.Save writes its temp
// file in the config dir, which fails if the parent doesn't exist yet.
func writeAutoInitConfig(cfgPath string) error {
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	cfg := baseConfig([]string{autoInitDefaultRoot}, config.DefaultLibraryName, "data")
	if err := cfg.NormalizeAndValidate(); err != nil {
		return fmt.Errorf("validate seed config: %w", err)
	}
	if err := cfg.Save(cfgPath); err != nil {
		return fmt.Errorf("save seed config: %w", err)
	}
	return nil
}

// runServe is the library-callable serve loop. Identical behavior to
// the flag-driven serveCmd path — same TLS material, same admin
// listener, same SIGINT graceful-shutdown — just with the inputs
// pre-parsed. Returns the exit code the CLI would.
func runServe(ctx context.Context, opts serveOpts, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	configPath := &opts.configPath
	addrOverride := &opts.addrOverride
	// Resolve the config location ONCE, before anything reads it — and
	// keep the resolved value in *configPath, because every later
	// consumer of the path (the auto-init stat just below, the admin
	// console's Save target via filepath.Abs, the backup source set, the
	// doctor runner) reads that same variable.
	//
	// The flag's default is "" (an explicit value still wins), so the raw
	// value is NOT a usable path: filepath.Abs("") returns the process
	// CWD — a DIRECTORY — which silently became admin.Deps.CfgPath and
	// made every admin config mutation fail its temp-file rename, put
	// BridgeYAML: "" in the backup set (bridge.yaml silently absent from
	// every snapshot), and had the admin Diagnostics page load a
	// directory as YAML. os.Stat("") likewise reports IsNotExist, which
	// sent every flag-less `serve --init-if-missing` down the auto-init
	// branch even with a perfectly good ./bridge.yaml sitting there.
	//
	// resolveConfigPath returns the location a config SHOULD live at even
	// when none exists yet (ok=false), which is exactly what the
	// auto-init writer needs.
	//
	// ABSOLUTE, because resolving is only half the job: two of
	// resolveConfigPath's four branches hand back the bare relative
	// "bridge.yaml" (the local-first hit and the no-config-anywhere
	// fallback), and an explicit relative --config is echoed verbatim.
	// The path is then STORED and read much later by consumers that do
	// their own file I/O — the backup ticker's first snapshot can be 24h
	// after boot — so a relative value silently means "wherever the
	// process CWD happens to be by then". That is not hypothetical here:
	// the installed service units set WorkingDirectory to the DATA dir,
	// so a relative "bridge.yaml" resolves under <cfgDir>/data, where
	// backup.Snapshot's os.Stat misses and it SKIPS the config with no
	// error — the same silently-configless snapshot this commit is
	// fixing, reintroduced through a different door.
	//
	// filepath.Abs only fails when os.Getwd does; keep the relative value
	// in that case rather than losing the path entirely.
	resolved, _ := resolveConfigPath(*configPath)
	if abs, absErr := filepath.Abs(resolved); absErr == nil {
		resolved = abs
	}
	*configPath = resolved
	// Container convenience: if --init-if-missing is set (the Docker CMD
	// sets it) and no config exists yet, write a sparse default so the
	// image needs only a single `docker run` / `docker compose up`. A bare
	// `bridge serve` without the flag is unchanged — a missing config still
	// errors below, directing the operator to `bridge init`.
	if opts.initIfMissing {
		if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
			if werr := writeAutoInitConfig(*configPath); werr != nil {
				fmt.Fprintf(stderr, "auto-init: %v\n", werr)
				return 2
			}
			logger.Info("no config found — wrote a default; env (BRIDGE_LIBRARY_ROOTS/NAME/…) still overrides it at runtime",
				"path", *configPath, "libraryRoots", autoInitDefaultRoot)
		}
	}
	cfg, cfgFile, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, configLoadFailedFormat, err)
		return 2
	}
	// Identical to `resolved` by construction (resolveConfigPath echoes a
	// non-empty explicit path back), but taking the loader's own answer
	// keeps the two from drifting if that ever stops being true — this is
	// the file whose contents `cfg` came from, and the file the admin
	// console must write back to.
	*configPath = cfgFile
	if err := bridgefs.ValidateRoots(cfg.LibraryRoots); err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	// Runtime library-root accessibility check.
	//
	// The shape check in config.Validate() no longer stats individual
	// roots (see CLAUDE.md "Things that have bitten before" — public-
	// mode VPS deployments with FUSE-mounted libraries can't be
	// stat'd by root, which would otherwise take down `sudo bridge
	// update` and friends). Strictness is mode-dependent here:
	//
	//   - loopback: any unreachable root is a refuse-to-start, matches
	//     the historical contract (typo'd YAML protection).
	//
	//   - public: log a warning per failing root and continue. The
	//     scanner's PR-#74 error-subtree machinery prevents the
	//     deletion pass from wiping the manifest of a momentarily-
	//     unreadable root, so the bridge can come up serving cached
	//     state while a slow FUSE mount catches up.
	if rootErrs := cfg.CheckLibraryRootsAccessible(); len(rootErrs) > 0 {
		if cfg.IsPublic() {
			for _, e := range rootErrs {
				logger.Warn("library root not accessible at startup — scan will retry, manifest will reflect what's currently visible",
					"path", e.Path, "err", e.Err)
			}
		} else {
			for _, e := range rootErrs {
				fmt.Fprintf(stderr, "config: %v\n", e)
			}
			return 2
		}
	}
	if *addrOverride != "" {
		// Validate the override the same way config.Validate validates
		// ListenAddress — otherwise an invalid value like "notaport"
		// slips through and only surfaces much later as a net.Listen
		// failure with an opaque error.
		if _, _, err := net.SplitHostPort(*addrOverride); err != nil {
			fmt.Fprintf(stderr, "invalid --addr %q: %v\n", *addrOverride, err)
			return 2
		}
		cfg.ListenAddress = *addrOverride
	}

	// Resolve TLS material (default: dataDir/server.{crt,key}; overridable
	// via cfg.TLSCertPath / cfg.TLSKeyPath).
	certPath, keyPath := cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath, keyPath = servertls.DefaultPaths(cfg.DataDir)
	}
	hostname, _ := os.Hostname()
	// Serve loads the existing cert if any; the SAN-stale check
	// inside LoadOrGenerateWithOptions warns at startup when the
	// on-disk cert's SANs don't cover the currently-advertised
	// endpoints (Tailscale IPs/DNS, custom URLs). We pass the broader
	// option set so that warning fires correctly — first-mint inside
	// `bridge serve` also picks up the broader set on a fresh data
	// dir without a prior `bridge init`.
	sanCfg := advertise.CertSANConfig{CustomEndpoints: cfg.CustomEndpoints}
	tlsOpts := servertls.GenerateOptions{
		Hostname:      hostname,
		ExtraDNSNames: advertise.GatherCertSANDNS(sanCfg),
		ExtraIPs:      advertise.GatherCertSANIPs(sanCfg),
	}
	cert, fingerprint, err := servertls.LoadOrGenerateWithOptions(certPath, keyPath, tlsOpts)
	if err != nil {
		fmt.Fprintf(stderr, "TLS material: %v\n", err)
		return 1
	}
	// Read the cert's NotAfter so /v1/health can surface it to iOS,
	// which uses it to warn the operator before the cert actually
	// expires (Apple's 397-day cap means re-pair roughly annually).
	// Use the in-memory `CertNotAfter(cert)` helper rather than
	// re-reading the PEM from disk via `Inspect(certPath)` — the cert
	// object was already loaded a few lines up and parsing it twice
	// is wasted I/O. Gemini bot review on PR #134.
	//
	// On failure we log so operators can diagnose why the iOS expiry
	// warning isn't firing — silent omission was the prior behaviour
	// and matched CodeRabbit's concern. The wire field still drops
	// out cleanly via omitempty when certNotAfter stays zero.
	var certNotAfter time.Time
	if when, infoErr := servertls.CertNotAfter(cert); infoErr == nil {
		certNotAfter = when
	} else {
		logger.Warn("could not read cert NotAfter; /v1/health certNotAfter omitted",
			"path", certPath, "err", infoErr)
	}

	// SNI cert switcher. Routes incoming TLS handshakes to the
	// self-signed cert (LAN/mDNS/IP-literal SNI — iOS pins this
	// fingerprint at first contact) or to a Tailscale-issued LE cert
	// when the SNI ends in the local node's MagicDNS suffix. The LE
	// cert is loaded asynchronously by the auto-mint goroutine below;
	// until that completes the manager falls through to self-signed
	// for every connection (= today's behaviour).
	//
	// PR 3 extension: when `autocert.enabled` is set, a third
	// route fires for the operator-configured public domain
	// (see `acmeManager` wiring below). Routes are mutually
	// exclusive by SNI: autocert serves only its configured
	// domain, Tailscale serves only `*.<magicdns>.ts.net`, every
	// other SNI falls through to self-signed.
	certManager := servertls.NewManager(cert)

	// Autocert (ACME / Let's Encrypt) wiring. Only fires when
	// the operator opts in via `autocert.enabled: true` in
	// public mode — the config Validate already enforced the
	// prerequisites (domain + email + port-443 reachability).
	// Loopback installs and public installs running behind a
	// reverse proxy stay autocert-free; the SNI switcher keeps
	// the self-signed cert as the only routing target.
	var acmeManager *tlsacme.Manager
	if cfg.Autocert.Enabled {
		am, amErr := tlsacme.New(tlsacme.Config{
			Domain:     cfg.Autocert.Domain,
			Email:      cfg.Autocert.Email,
			CacheDir:   cfg.EffectiveAutocertCacheDir(),
			UseStaging: cfg.Autocert.UseStaging,
		})
		if amErr != nil {
			fmt.Fprintf(stderr, "autocert: %v\n", amErr)
			return 1
		}
		acmeManager = am
		certManager.SetAutocertProvider(am.Domain(), am.GetCertificate, am.NextProtos())
		// Pairing-QR fingerprint reads the SERVED leaf from the autocert
		// cache (not a synthetic-hello GetCertificate, which returns a
		// different leaf) so the QR advertises the cert iOS captures.
		certManager.SetAutocertCachedCertFn(am.CachedCert)
		fmt.Fprintf(stdout, "autocert: ACME provisioning enabled for %q (cache: %s)\n",
			am.Domain(), cfg.EffectiveAutocertCacheDir())
		if cfg.Autocert.UseStaging {
			fmt.Fprintf(stdout, "autocert: using LE STAGING directory — certs will not be browser-trusted\n")
		}
	}

	store, err := auth.OpenStore(filepath.Join(cfg.DataDir, tokensFileName))
	if err != nil {
		fmt.Fprintf(stderr, "open token store: %v\n", err)
		return 1
	}
	defer func() {
		// Flush any LastUsedAt updates debounced by Validate so a
		// just-before-exit hit doesn't lose its timestamp.
		if err := store.FlushLastUsed(); err != nil {
			fmt.Fprintf(stderr, "auth: flush LastUsedAt on shutdown: %v\n", err)
		}
	}()

	// Record our PID so `bridge doctor` can attribute the bound port to
	// this process rather than reporting a conflict against the
	// operator's own running bridge. doctor has read this file since it
	// was written; nothing ever wrote it, so the attribution branch (and
	// PR #432's native Windows port->PID probe, which only runs once
	// there is a PID to attribute to) was unreachable.
	//
	// Non-fatal by design: a diagnostic aid failing to write must not
	// stop the bridge serving.
	pidPath, pidErr := writeServerPIDFile(cfg.DataDir)
	if pidErr != nil {
		fmt.Fprintf(stderr, "pidfile: %v (doctor will not be able to "+
			"attribute the bound port to this process)\n", pidErr)
	} else {
		defer removeServerPIDFile(pidPath)
	}

	manifestStore, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer manifestStore.Close()
	// bgWriters tracks the background goroutines that WRITE the manifest store
	// (periodic scanner, enricher, fs watcher, atlas harvester, and the admin
	// server whose bgScans drain is the SQLite guard). On shutdown they MUST
	// fully drain before manifestStore.Close() runs — an in-flight
	// UpsertTrackBatch / MarkEnriched / booklet / admin-scan write racing the
	// close is the SQLite-corruption class (B8, via the watcher's fired
	// AfterFunc dispatches) or "database is closed" shutdown noise + a
	// truncated final batch (B27). The deferred scanCancel()/adminCancel()
	// below run FIRST in LIFO order, so by the time this fires the writers are
	// already exiting; it just waits for them, grace-bounded so a wedged
	// writer can't block process exit.
	//
	// The wait is INLINE here, NOT routed through a function variable assigned
	// later in runServe: any early return between the first tracked goroutine
	// and that assignment would leave the variable nil, make this defer a
	// no-op, and let a live writer race Store.Close() — reintroducing the exact
	// corruption class this guards (Gemini HIGH, post-merge review of #534).
	// `WaitGroup.Wait` on a zero counter returns immediately, so exiting before
	// any writer starts costs nothing.
	var bgWriters sync.WaitGroup
	defer func() {
		done := make(chan struct{})
		go func() { bgWriters.Wait(); close(done) }()
		// NewTimer + defer Stop, NOT time.After: time.After's Timer survives
		// until it fires even when `done` wins the select, and runServe is
		// re-entered every time the launcher menu starts the bridge again, so
		// the abandoned timers would accumulate (the PR #290 convention).
		graceTimer := time.NewTimer(shutdownGrace)
		defer graceTimer.Stop()
		select {
		case <-done:
		case <-graceTimer.C:
			fmt.Fprintln(stderr, "shutdown: background manifest writers did not drain within grace")
		}
	}()
	// Single source of truth for the artwork cache directory. The
	// scanner writes scanner-side `local-<sha256>-500.jpg` here when
	// it finds embedded ID3 APIC art or a folder-level cover.jpg /
	// folder.jpg; the enricher writes MusicBrainz `<mbid>-500.jpg`
	// here for its CAA / iTunes path; the API handler reads from the
	// same directory and serves both transparently via the relaxed
	// /v1/artwork mbid regex.
	artworkDir := filepath.Join(cfg.DataDir, "artwork")
	scanner := manifest.NewScanner(cfg.LibraryRoots, manifestStore, artworkDir)
	// missing_count grace period — defaults applied in config.applyDefaults
	// so operators who never touch the `scanner:` YAML block still get the
	// standard 3-scan threshold.
	scanner.SetDeleteThreshold(cfg.Scanner.DeleteAfterMissingScans)
	// Duplicates policy MUST be wired BEFORE RunPeriodic starts below:
	// the startup scan runs the stamping pass, and an unwired scanner
	// stamps with FilterOff — which would CLEAR every suppression on
	// each boot (mass indexed_at bumps → iOS delta churn) only for the
	// next pass to re-suppress. The live config holder doesn't exist yet
	// at this point (it belongs to apiSrv), so the closure late-binds it:
	// boot snapshot until the holder lands, live holder afterwards — the
	// two can't differ in between, because config mutations only arrive
	// via the admin server, which starts later.
	var dupeCfgLive atomic.Pointer[config.RuntimeConfig]
	scanner.SetDupePolicy(func() dupes.Policy {
		live := cfg
		if h := dupeCfgLive.Load(); h != nil {
			live = h.Load()
		}
		return dupePolicyFromConfig(live)
	})
	provider := manifest.NewProvider(manifestStore, scanner)

	// Fire up the periodic scanner in the background. It runs an initial
	// scan on startup, then rescans every cfg.ScanInterval().
	//
	// scanCtx derives from serveCmd's parent ctx so a SIGINT (or
	// any other parent cancel) propagates straight to the scanner,
	// enricher, and updater goroutines that share this context. The
	// previous version derived from context.Background() and relied
	// on the deferred scanCancel() to fire — which works in steady
	// state but trips the contextcheck linter and means the
	// background workers can't observe cancellation until serveCmd's
	// shutdown path runs.
	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()
	bgWriters.Add(1)
	go func() {
		defer bgWriters.Done()
		scanner.RunPeriodic(scanCtx, cfg.ScanInterval())
	}()

	// Tailscale integration. Branched on cfg.Tailscale.EffectiveMode():
	//
	//   - `cli` (default): existing flow — tailscaleAutoPilot shells
	//     out to `tailscale status --json` + `tailscale cert <magic>`
	//     and feeds the resulting LE cert into certManager's SNI
	//     switcher.
	//   - `tsnet` (opt-in): bridge becomes its own embedded tailnet
	//     node via internal/tsnet. The tsnet listener (added below)
	//     terminates LE in-process — no on-disk cert material, no
	//     SNI switcher needed for *.ts.net connections. CLI auto-
	//     pilot is skipped entirely.
	//   - `disabled`: skip both. LAN listener only; *.ts.net
	//     connections aren't served.
	//
	// All three modes converge on the same handler/admin/scanner.
	// In all modes, errors from the tailscale path are logged but
	// non-fatal — the bridge keeps serving on the LAN listener.
	// Fail closed on an invalid mode — silently defaulting to `cli`
	// would let a typo (`mode: tnset`) re-enable the CLI shell-out
	// when the operator intended `tsnet`. EffectiveMode's whole
	// purpose is to reject typos; respecting that here is the
	// honest shape (CodeRabbit Major on PR #139).
	//
	// Exit code 2 = config/usage error (CodeRabbit round-2 on PR
	// #139). Matches the rest of runServe's config-validation
	// branches; a config typo shouldn't look like a runtime
	// failure (1) to whatever supervisor is watching.
	tsMode, modeErr := cfg.Tailscale.EffectiveMode()
	if modeErr != nil {
		fmt.Fprintf(stderr, "tailscale.mode: %v\n", modeErr)
		return 2
	}

	var tailscaleAuto *tailscaleAutoPilot
	var tsnetServer *tsnet.Server
	switch tsMode {
	case config.TailscaleModeCLI:
		tailscaleAuto = newTailscaleAutoPilot(cfg.DataDir, cfg.ListenAddress, certManager, stderr)
		tailscaleAuto.Start(scanCtx)
	case config.TailscaleModeTsnet:
		// Build the tsnet.Server but DO NOT block the listen step
		// on Up() — interactive auth can take minutes on first
		// run, and we want the LAN listener up immediately.
		// Up() runs in a goroutine; the second http.Server gated
		// on its success is started later in this function.
		ts, err := newTsnetServer(cfg, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "tsnet: %v (LAN listener still active)\n", err)
		} else {
			tsnetServer = ts
		}
	case config.TailscaleModeDisabled:
		// No-op. Operator explicitly opted out.
	}

	// Optional fsnotify-based instant-update watcher. Off by default
	// (cfg.LibraryWatch.Enabled). When on, the periodic full scan
	// remains the safety net — the watcher just shortens the
	// time-to-visibility for newly-dropped files in the common case.
	// Failure to construct the watcher (e.g. older kernel without
	// inotify support) is non-fatal — log a Warn and continue
	// periodic-only.
	if cfg.LibraryWatch.Enabled {
		debounce := time.Duration(cfg.LibraryWatch.EffectiveDebounceSeconds()) * time.Second
		watcher, werr := manifest.NewWatcher(scanner, debounce)
		if werr != nil {
			fmt.Fprintf(stderr, "library watcher: %v (periodic scan still active)\n", werr)
		} else {
			bgWriters.Add(1)
			go func() {
				defer bgWriters.Done()
				if err := watcher.Run(scanCtx); err != nil {
					fmt.Fprintf(stderr, "library watcher exited: %v\n", err)
				}
			}()
		}
	}

	// Fire up the MusicBrainz/CoverArt enricher in the background. It
	// pulls un-enriched tracks from the store and fills in
	// MusicBrainzAlbumID / ArtworkMBID, caching cover images under
	// <dataDir>/artwork/.
	userAgent := fmt.Sprintf(
		"%s/%s (+https://github.com/acoseac/1-bit-bridge)",
		"1-bit-bridge", version.ServerVersion,
	)
	// Upstream metadata/cover-art roots are configurable (default public
	// MusicBrainz / Cover Art Archive). Point cfg.Enrich.* at a self-hosted
	// 1-bit-atlas mirror to keep enrichment on your own network.
	mbClient := enrich.NewMusicBrainzClient(cfg.Enrich.MusicBrainzBaseURL, userAgent, nil)
	caaClient := enrich.NewCoverArtClient(cfg.Enrich.CoverArtBaseURL, userAgent, nil)
	deezerClient := enrich.NewDeezerClient("", userAgent, nil)
	// Phase-H harvest credential store, opened early so the enricher can wire
	// the Phase-B authenticated premium-cover fetcher BEFORE its worker
	// goroutine starts (avoids a field-write/read race). The same store is
	// reused by the bulk-harvest client below. Gated identically to that
	// client: harvest requires atlas.enabled (the bios it pulls are only
	// served when WithAtlasMeta is wired). nil = feature off, or the state
	// file failed to open.
	var harvestState *atlasharvest.StateStore
	if cfg.Atlas.HarvestEnabled && cfg.Atlas.Enabled {
		hs, herr := atlasharvest.OpenStateStore(filepath.Join(cfg.DataDir, "atlas-harvest.json"))
		if herr != nil {
			fmt.Fprintf(stderr, "atlas harvest: open state: %v (feature disabled)\n", herr)
		} else {
			harvestState = hs
		}
	}

	// artworkDir is defined above (single source of truth) and shared
	// with the scanner so scanner-side `local-*` files and enricher-
	// side `<mbid>-*` files cohabit one directory.
	enricher := enrich.NewEnricher(manifestStore, mbClient, caaClient, deezerClient, artworkDir)
	// Phase B: when the harvest credential store is available, fetch the
	// cross-source premium canonical (Qobuz/Tidal) from Atlas ahead of CAA,
	// caching it under the MBID path /v1/artwork already serves — premium
	// covers with zero iOS change. The bulk_harvest bearer is read from the
	// store at request time (never baked into the open-source binary).
	var premiumCovers enrich.PremiumCoverFetcher
	if harvestState != nil {
		premiumCovers = enrich.NewAtlasPremiumFetcher(harvestState, userAgent, nil)
		enricher.WithPremiumCovers(premiumCovers)
	}
	// Acoustic fingerprinting: the cache is constructed here and attached
	// BEFORE the enricher goroutine starts, so there is no field-write/read
	// race — the same reasoning as the premium-cover wiring above. It is
	// populated later by the sweeper, which needs apiSrv's hot-reloading
	// resolver and therefore cannot be built until further down.
	//
	// Gated on the config flag AND both prerequisites: a `true` config with
	// fpcalc or the API key missing degrades to feature-off here, so the
	// bridge still boots. fingerprintCache stays nil in that case, and a nil
	// AcousticLookup means the fallback is simply never consulted.
	var fingerprintCache *acoustid.Cache
	var fingerprintDegraded string // bounded key for the admin Jobs card ("" = not demoted)
	acoustIDKey := cfg.Fingerprint.ResolvedAPIKey()
	if cfg.Fingerprint.Enabled {
		if ok, reason := fingerprintFeatureReady(ctx, acoustIDKey != "", stderr); ok {
			fingerprintCache = acoustid.NewCache(fingerprintCacheCap)
			enricher.WithAcousticFallback(acousticLookupAdapter{cache: fingerprintCache})
		} else {
			fingerprintDegraded = reason
		}
	}
	bgWriters.Add(1)
	go func() {
		defer bgWriters.Done()
		enricher.Run(scanCtx)
	}()

	// Artwork-cache LRU eviction. Bounds the on-disk size of the shared
	// <dataDir>/artwork/ cache (scanner local-* + enricher <mbid>-* + Atlas
	// premium covers) at cfg.Artwork.CacheMaxBytes. Off by default
	// (cap <= 0 = unbounded, the historical behaviour) — only spawned when a
	// cap is configured, where it becomes the prerequisite for whole-library
	// hi-res covers not filling a small host disk.
	if cfg.Artwork.CacheMaxBytes > 0 {
		go runArtworkCacheSweeper(scanCtx, artworkDir, cfg.Artwork.CacheMaxBytes, artworkCacheSweepInterval)
	}

	// Periodic state-snapshot ticker. Captures bridge.db / tokens.json /
	// cert / key / config into <dataDir>/backups/<timestamp>/ at the
	// configured cadence (default 24h). Uses the same scanCtx as the
	// other periodic workers so a SIGINT cancels it cleanly. Snapshots
	// are best-effort — failures are logged but never crash serve.
	//
	// `EffectiveIntervalHours` returns 0 when the operator has explicitly
	// disabled the ticker (`intervalHours: 0`); we skip the goroutine in
	// that case. The on-demand CLI path stays available regardless.
	backupSources := buildBackupSources(cfg, *configPath)
	var backupRunState *sweepStatus[struct{}]
	if hrs := cfg.Backup.EffectiveIntervalHours(); hrs > 0 {
		backupRunState = &sweepStatus[struct{}]{}
		backupInterval := time.Duration(hrs) * time.Hour
		// Joined on bgWriters like every other background worker. It is not
		// a manifest-store writer — `vacuumInto` opens its own mode=ro
		// connection, so it never races Store.Close() — but leaving it
		// fire-and-forget still let Snapshot/Prune/ReapOrphans run past
		// runServe's return, which is what the documented shutdown contract
		// forbids and what produced an intermittent
		// `TempDir RemoveAll: directory not empty` under data/backups/ in
		// TestServeStartsAndServesHealth.
		bgWriters.Add(1)
		go func() {
			defer bgWriters.Done()
			runBackupTicker(scanCtx, backupSources, cfg.Backup.EffectiveKeep(), backupInterval, stdout, stderr, backupRunState)
		}()
	}

	// Sessions tracker counts inflight /v1/read + /v1/download
	// requests. The Install path consults Inflight() before
	// swapping the binary so Hugo 2 / XMOS DAC DoP-lock loss can't
	// happen via a mid-stream restart.
	//
	// Constructed BEFORE the updater so the Phase C auto-installer's
	// InstallOptions can reference it.
	sessions := updater.NewTracker()

	// Resolve the running binary path once at startup. Install
	// swaps the file at this exact path. os.Executable() may
	// return an error in unusual environments (deleted binary
	// running, embedded test); fall back to argv[0] so the
	// failure surfaces later in Install's preflight rather than
	// blocking the whole server boot.
	binaryPath, exeErr := os.Executable()
	if exeErr != nil {
		fmt.Fprintf(stderr, "updater: os.Executable failed (install path may not work): %v\n", exeErr)
		binaryPath = os.Args[0]
	}
	// Resolve symlinks so a symlinked install swaps the REAL binary, not
	// the link — matching what update.go, init.go and menu.go already do
	// at their own os.Executable() sites (update.go's comment: "Mirrors
	// the binary resolution in init.go's service install"). runServe was
	// the one that didn't, and this value feeds maybeRollbackOnBoot,
	// updateInfoAdapter.binaryPath and AutoInstallOpts.BinaryPath.
	//
	// On darwin os.Executable returns the INVOCATION path unresolved, so
	// an admin-console install via /usr/local/bin/bridge -> /opt/.../bridge
	// replaced the symlink with a regular file: the real binary stayed at
	// the old version, launchd kept running it, and `bridge version` on
	// PATH reported the new one. Best-effort — a resolve failure keeps the
	// unresolved path rather than blocking boot (F0c).
	if resolved, lerr := filepath.EvalSymlinks(binaryPath); lerr == nil {
		binaryPath = resolved
	}

	// Boot-time rollback housekeeping: read update-state.json and
	// either confirm the install succeeded (mark installed, retain
	// .bak for one boot) or restore .bak when the new version
	// failed to come up cleanly. Failures are logged but
	// non-fatal — operator can still recover by hand.
	maybeRollbackOnBoot(stderr, cfg.DataDir, binaryPath)

	// Background updater: polls GitHub Releases on a configurable
	// cadence (Phase A), exposes operator-triggered Install via the
	// admin console + CLI (Phase B), and optionally auto-installs
	// inside a quiet-hours window with the same safeties as Phase
	// B's manual path (Phase C).
	//
	// Lives off scanCtx so a SIGINT cancels it cleanly alongside
	// the scanner. Poll failures are non-fatal — the bridge serves
	// fine without update awareness; the admin UI shows "couldn't
	// reach GitHub" in the LastError field.
	updOpts := updater.Options{
		// AutoInstall is on every platform now that Phase B-Windows
		// (PR #48) wired the rename-trick swap with SCM-stop
		// coordination. The auto-installer still gates on the
		// session tracker, quiet-hours, and the Phase C compat
		// gate identically across platforms.
		AutoInstall: cfg.Update.AutoInstall,
		// Compat-gate token snapshot. The updater calls this on each
		// install attempt to decide whether the candidate's
		// MinClientVersion would orphan a still-paired older client.
		TokenSnapshot: store.List,
	}
	if cfg.Update.CheckIntervalHours > 0 {
		updOpts.CheckInterval = time.Duration(cfg.Update.CheckIntervalHours) * time.Hour
	}
	if cfg.Update.QuietHours != "" {
		// Validate already passed; this can't fail.
		start, end, _ := config.ParseQuietHours(cfg.Update.QuietHours)
		updOpts.QuietHoursStart = start
		updOpts.QuietHoursEnd = end
	}
	if cfg.Update.AutoInstall {
		// Auto-install wires the install opts when the operator
		// opted in via config. Phase B-Windows (PR #48) added the
		// SCM-coordinated rename-trick swap, so Windows is now a
		// supported auto-install platform — same gate sequence as
		// darwin/linux.
		updOpts.AutoInstallOpts = &updater.InstallOptions{
			DataDir:    cfg.DataDir,
			BinaryPath: binaryPath,
			Sessions:   sessions,
			Force:      false,
		}
		// On successful auto-install we exit; service-manager
		// (launchd / systemd / SCM) respawns into the new binary.
		// The Phase B `maybeRollbackOnBoot` housekeeping then
		// verifies version-match and either confirms or rolls back.
		updOpts.AutoInstallRestart = func() {
			fmt.Fprintln(stdout, "Restarting after auto-install (service manager will respawn).")
			// Route through the graceful cancellation — the same closure SIGINT
			// and the admin restart use (admin.Deps.Restart == cancel) — so the
			// transcode/analysis pools drain, the auth debounce flushes, and the
			// manifest DB checkpoints before exit. os.Exit(0) here would skip
			// every runServe defer (the "restart MUST NOT os.Exit(0)" contract).
			cancel()
		}
	}
	upd := updater.New(updOpts)
	// Start the background poll loop: it refreshes the GitHub-release status on
	// the configured cadence AND drives the opt-in auto-installer (maybeAutoInstall
	// runs ONLY from Run). Lives off scanCtx so a SIGINT cancels it alongside the
	// scanner. Without this call, background update checks never fire and
	// update.autoInstall is dead — only the manual "Check now" path works.
	go upd.Run(scanCtx)

	updAdapter := updateInfoAdapter{
		u:          upd,
		sessions:   sessions,
		dataDir:    cfg.DataDir,
		binaryPath: binaryPath,
		// Phase B-Windows (PR #48) wired the swap path on Windows
		// alongside darwin/linux. CanInstall is true everywhere
		// the binary builds.
		canInstall: true,
	}

	// pairing.Store backs the admin-approval pairing flow (POST/GET/DELETE
	// /v1/pairing/*). In-memory: pending requests are ephemeral by design,
	// and a bridge restart is detected by iOS via the bridgeStartedAt echo.
	// Approve mints a real bearer token via auth.Store.Mint; an undelivered
	// approval (TTL+grace without iOS DELETE ack) revokes the minted token
	// to prevent orphans after a network blip mid-handoff.
	pairingStore := pairing.NewStore(pairing.Options{
		RevokeToken: store.Revoke,
	})
	defer pairingStore.Close()

	// Upscale feature gate: config flag + sox-on-PATH startup probe.
	// `cfg.Upscale.Enabled == true` AND a working sox in PATH are
	// the joint precondition for the feature. A missing sox with
	// the flag on logs an error and degrades to "feature off"
	// in-memory — the rest of the server keeps running unaffected.
	// iOS sees `upscaleEnabled: false` on /v1/health in either
	// disabled case.
	upscaleActive := cfg.Upscale.Enabled
	if upscaleActive && !soxFeatureReady(ctx, "upscale", stderr) {
		upscaleActive = false
	}
	provider.SetUpscaleEnabled(upscaleActive)

	// Analysis feature gate: same shape as upscale — config flag AND a
	// working sox in PATH (analysis decodes through sox). A missing sox
	// with the flag on degrades to "feature off" in-memory. Serve-side
	// generation is CLI-driven (`bridge analyze`); serve only advertises
	// the `waveform` flag + serves /v1/waveform from cached sidecars.
	analysisActive := cfg.Analysis.Enabled
	if analysisActive && !soxFeatureReady(ctx, "analysis", stderr) {
		analysisActive = false
	}

	// Smart playlists read precomputed analysis + history (no decode), so
	// there's no sox precheck. The harmonic Auto Mix + Daily Mix discovery
	// self-omit when analysis isn't active; the listening families work from
	// history alone.
	smartPlaylistsActive := cfg.SmartPlaylists.Enabled

	// LE-cert expiry provider for /v1/health (public mode). Live
	// closure so background autocert renewals surface on the next
	// health probe without a restart; same backing source as the
	// admin's autocert tile so the two surfaces never drift.
	// Returns the zero time when autocert isn't wired (loopback /
	// pre-mint window), and api.Server omits the wire field in
	// that case.
	var leCertExpiry func() time.Time
	if acmeManager != nil {
		am := acmeManager
		leCertExpiry = func() time.Time { return am.Status().NotAfter }
	}

	apiSrv := api.New(cfg, store, provider, fingerprint).
		WithArtworkDirs(artworkDirBridge(artworkDir)).
		WithMBIDProbe(provider).
		WithUpdater(updAdapter).
		WithSessionTracker(sessions).
		WithPairing(pairingStore).
		WithCertExpiry(certNotAfter).
		WithLECertExpiry(leCertExpiry).
		WithUpscale(upscaleActive, &variantStoreAdapter{provider: provider}).
		WithCarPlayOptimize(upscaleActive && cfg.Upscale.EffectiveOptimizeEnabled()).
		WithAnalysis(analysisActive, &analysisStoreAdapter{provider: provider}).
		WithAnalysisStats(&analysisStatsAdapter{
			enabled: func() bool { return analysisActive },
			store:   manifestStore,
		}).
		WithDeviceRegistrar(manifestStore).
		WithPlaylistStore(manifestStore).
		WithFavoritesStore(manifestStore).
		WithHistoryStore(manifestStore).
		WithAtlasMeta(cfg.Atlas.Enabled, cfg.Atlas.EffectiveMetaTTL(), manifestStore).
		WithPlaylistCoverStore(manifestStore)
	// Conditionally wire the smart-playlist feed so the health flag + the
	// 404-when-off shape stay honest when cfg.SmartPlaylists.Enabled is false.
	if smartPlaylistsActive {
		apiSrv.WithSmartPlaylistStore(manifestStore)
	}

	// Phase-H bulk harvest (opt-in via cfg.Atlas.HarvestEnabled). The iOS app
	// provisions a bulk_harvest credential to POST /v1/atlas-harvest/credential;
	// the client below submits the library's artist MBIDs to Atlas + delta-syncs
	// the harvested bios into the artist_atlas overlay served by /v1/atlas-meta.
	// Dormant (cheap idle ticks) until a credential lands.
	var harvestClient *atlasharvest.Client
	// Harvest requires Atlas enrichment: the harvested bios land in artist_atlas,
	// which is only SERVED (GET /v1/atlas-meta) when cfg.Atlas.Enabled wires
	// WithAtlasMeta above. Harvesting without it would write bios nothing serves.
	if cfg.Atlas.HarvestEnabled && !cfg.Atlas.Enabled {
		fmt.Fprintln(stderr, "atlas harvest: harvestEnabled requires atlas.enabled (bios are served via /v1/atlas-meta) — harvest disabled")
	}
	// bookletsDir is the PDF booklet cache path. Declared outside the
	// harvest block: the admin inspector's loopback booklet route
	// serves from it too (Deps.BookletPath below), and the path is
	// derivable whether or not the harvest client is running.
	bookletsDir := filepath.Join(cfg.DataDir, "booklets")
	// harvestState was opened earlier (so the enricher could wire the premium-
	// cover fetcher before its worker started). Reuse it for the bulk-harvest
	// client; nil = feature off or the state file failed to open.
	if harvestState != nil {
		apiSrv.WithAtlasHarvest(harvestState)
		harvestClient = &atlasharvest.Client{
			State: harvestState,
			MBIDs: manifestStore,
			Sink:  atlasHarvestSink{store: manifestStore},
			// Cover bulk-harvest: submit the library's release MBIDs + upgrade
			// their cached covers to premium once Atlas resolves them. Reuses
			// the enricher's authenticated premium fetcher (premiumCovers is
			// non-nil whenever harvestState is, both gated on atlas.enabled).
			Refetcher: atlasCoverRefetcher{premium: premiumCovers, artworkDir: artworkDir, store: manifestStore},
			Log:       logging.Component("atlasharvest"),
			// Skip the booklet orphan-GC while a library (re)scan is in flight
			// (B46): mid-rescan the album-release-MBID universe is transiently
			// partial (e.g. a hybrid root add/remove runs WipeFilesystemTracks +
			// rescan), so GCing against it would delete + re-fetch live booklets.
			ScanInProgress: scanner.IsScanning,
		}
		// PDF booklet check + fetch loops (v1.8) ride the same harvest
		// credential. The cache dir failing to create degrades to
		// availability-checks-only (no downloads, /v1/booklet answers 202)
		// rather than disabling the feature.
		harvestClient.Booklets = bookletSinkAdapter{store: manifestStore}
		if err := os.MkdirAll(bookletsDir, 0o700); err != nil {
			fmt.Fprintf(stderr, "booklets: create cache dir: %v (downloads disabled)\n", err)
		} else {
			harvestClient.BookletFiles = bookletDiskStore{dir: bookletsDir}
		}
		apiSrv.WithBooklets(manifestStore, bookletsDir, harvestClient.NudgeBookletFetch)
	}
	if harvestClient != nil {
		bgWriters.Add(1)
		go func() {
			defer bgWriters.Done()
			harvestClient.Run(scanCtx)
		}()
	}

	cfgHolder := apiSrv.ConfigHolder()
	// Flip the duplicates-policy closure from the boot snapshot to the
	// live holder (see SetDupePolicy above) so settings PATCHes reach
	// the next stamping pass.
	dupeCfgLive.Store(cfgHolder)

	// DLNA MediaServer (opt-in, LAN-only). Starts a parallel
	// http.Server on its own port + an SSDP advertiser so any DLNA
	// renderer (Chord 2go, Lumin, Bluesound, etc.) on the LAN can
	// browse the library and stream files. Refused in public
	// deployment mode by `dlna.ShouldEnableDLNA` regardless of the
	// operator's `cfg.DLNA.Enabled` value — the gate is non-
	// overridable. nil-safe lifecycle: when disabled or setup fails,
	// the returned wrapper's Stop is a no-op and `dlnaEnabled` is
	// false. The `dlnaEnabled` flag flows into `apiSrv.WithDLNA(...)`
	// below so /v1/health.features advertises `dlnaServer` in
	// lockstep with the actual running listener.
	// UPnP UPSTREAM ingestion + file-serving proxy. Opt-in via
	// `upnpUpstream.enabled` in bridge.yaml. Walks each configured
	// MediaServer's "Browse Folders" tree into the manifest at the
	// scan-interval cadence + proxies /v1/download for any path whose
	// row carries a upnp_track_routing entry. When disabled the
	// lifecycle is a no-op + the proxy hooks stay nil + the filesystem
	// path serves every track.
	//
	// **Started BEFORE the DLNA MediaServer** so the DLNA file
	// handler can pick up `upnpLC.HostResolver()` and pass the proxy
	// + routing lookup into the dlna ServerConfig. Pre-this-reorder
	// the DLNA `/dlna/file/{trackID}` handler returned 404 for any
	// UPnP-routed track (silent decline on cast), since it only knew
	// the local filesystem resolver.
	upnpLC := startUPnPUpstreamIfEnabled(ctx, cfg, manifestStore, apiSrv, logger)
	defer upnpLC.Stop()

	dlnaLC, dlnaEnabled := startDLNAIfEnabled(ctx, cfg, manifestStore, apiSrv.Resolver(), upnpLC, logger)
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		dlnaLC.Stop(stopCtx)
	}()
	apiSrv.WithDLNA(dlnaEnabled)

	// SSDP MediaRenderer discovery — opt-in via
	// `dlna.discovery.enabled` in bridge.yaml. Gated AND-wise on
	// the DLNA MediaServer being up (`dlnaEnabled` above) so the
	// `rendererDiscovery` feature flag in /v1/health stays
	// coherent. When disabled, the lifecycle is a no-op + the
	// api.Server's `rendererDiscovery` slot stays nil + the
	// `/v1/renderers` handler returns 404.
	discLC := startDLNADiscoveryIfEnabled(ctx, cfg, dlnaEnabled, logger)
	defer discLC.Stop()
	if discLC.snapshotter != nil {
		apiSrv.WithRendererDiscovery(discLC.snapshotter)
	}

	// (UPnP upstream wiring moved above the DLNA wiring so the DLNA
	// file handler can pick up the proxy + routing lookup — see the
	// new location.)

	// Public advertisement of upstream MediaServers on `/v1/health` —
	// nil-safe (returns nil when the feature is disabled, so the
	// `upnpUpstreamServers` field stays off the wire on pre-feature
	// deploys). iOS uses the published rows for sub-source filtering
	// inside the bridge's Library view (Track.path starts_with
	// pathPrefix).
	apiSrv.WithUPnPUpstreamPublicProvider(upnpLC.installPublicProvider(cfgHolder, manifestStore))

	// Background sweep for the pairing rate-limiter's per-IP map.
	// Hourly cadence drops limiters untouched for ≥ 6 h, keeping the
	// map bounded under high churn (operator deep-links + diverse
	// client-IP set on the LAN). Stop fn is deferred so the goroutine
	// exits cleanly on shutdown.
	stopRLGC := apiSrv.StartPairingRateLimitGC()
	defer stopRLGC()

	// /v1/manifest per-token rate limiter idle-entry reaper. 10 min
	// cadence + 1 h idle timeout keeps the limiter map bounded across
	// long-running bridges with high client churn. Defaults in
	// internal/config.DefaultManifest* — operators tune via
	// `limits: manifest:` in bridge.yaml.
	stopManifestRL := apiSrv.StartManifestRateLimitReaper()
	defer stopManifestRL()

	// deviceSeen debounce-map reaper. The map is keyed on the
	// client-supplied X-Device-Token, so a token-rotating authed
	// client would grow it unboundedly without a sweep. Entries
	// at/past deviceRegistrarTTL are dead weight (touchDevice
	// re-upserts past the TTL anyway), so reaping changes only
	// memory, never the registration cadence.
	stopDeviceSeen := apiSrv.StartDeviceSeenReaper()
	defer stopDeviceSeen()

	// Start the event broker that backs GET /v1/events. iOS uses
	// it to receive push notifications for upscale completions and
	// pairing approvals (in lieu of polling). When publishers
	// aren't yet wired (this PR ships the endpoint + broker; the
	// transcode + pairing wiring follows in a separate PR), the
	// endpoint stays functional but emits only heartbeats. iOS
	// clients fall back to polling when /v1/events returns 404 (no
	// broker) or when no real events arrive within the management
	// section's lifetime.
	stopBroker := apiSrv.StartEventBroker()
	defer stopBroker()

	// Phase 2.5: long-lived transcode worker pool inside `bridge
	// serve`. Only instantiated when the feature is fully active
	// — saves goroutines + a manifest store reference when the
	// operator hasn't opted in, and matches the "off means
	// completely off" guarantee the iOS gating relies on.
	//
	// Constructed AFTER apiSrv so the adapter can borrow the
	// api server's Resolver instance via `apiSrv.Resolver()`
	// instead of building a snapshot from cfg.LibraryRoots.
	// Critical: the api Resolver hot-reloads via SetRoots when
	// the admin removes/adds a library root at runtime; a
	// snapshot resolver would silently keep routing to the old
	// root set and the upscale endpoint would either resolve
	// stale paths or 404 on freshly-added ones (Qodo bug 2 on
	// PR #109).
	//
	// Pool lives for the rest of serveCmd's lifetime; deferred
	// Stop() drains in-flight sox processes during graceful
	// shutdown (SIGTERM from the service manager → cancellable
	// via `transcode.Pool.stopCtx`). The defer fires AFTER
	// httpSrv.Shutdown completes, so accepting POST /v1/upscale
	// can't race the pool teardown.
	// Auto-analysis: when the feature is active, run a long-lived
	// analyze pool + a background sweeper that enqueues tracks missing a
	// fresh waveform on a settle-delay-then-scan-interval cadence.
	// Generation also stays available via `bridge analyze`. The pool's
	// deferred Stop drains in-flight decodes during graceful shutdown
	// (shares scanCtx with the other periodic workers). apiSrv.Resolver()
	// is the hot-reloading resolver so a runtime root add/remove is
	// reflected without restart (same rationale as the upscale enqueuer).
	// analysisPool + the sweeper's nudge/status live in runServe scope so
	// the admin Deps closures (wired further down) can read them; all stay
	// nil when the feature is off, and the closures are only installed
	// when analysisActive.
	// postScanNudges collects every buffered-1 nudge channel that wants a
	// non-blocking poke after each successful scan. ONE SetPostScanHook
	// registration (below, once every sweeper has appended) fans out to
	// all of them.
	//
	// **The single registration is load-bearing**: SetPostScanHook
	// REPLACES the stored callback, so a second call silently unhooks the
	// first sweeper. When the auto-optimize sweeper was added, registering
	// its own hook here would have left freshly scanned tracks unanalysed
	// with nothing failing anywhere. Append to the slice; never call
	// SetPostScanHook a second time.
	var postScanNudges []chan struct{}

	var analysisPool *analyze.Pool
	var analysisNudge chan struct{}
	var analysisSweepState *sweepStatus[admin.AnalysisSweepCounts]
	if analysisActive {
		analysisPool = analyze.NewPool(manifestStore, cfg.Analysis.EffectiveWorkers(), cfg.Analysis.EffectiveQueueCap())
		defer analysisPool.Stop()
		// Buffered-1 nudge: the scanner's post-scan hook and the admin
		// "Analyze now" button both non-blocking-send; a pending nudge
		// coalesces (the sweep about to run covers it).
		analysisNudge = make(chan struct{}, 1)
		analysisSweepState = &sweepStatus[admin.AnalysisSweepCounts]{}
		// Post-scan nudge: analyse freshly indexed music right after every
		// successful scan (periodic, startup, and admin-triggered — all
		// route through Scanner.Scan) instead of waiting out the next
		// sweep tick. Registered via postScanNudges rather than its own
		// SetPostScanHook call — see that slice's docstring.
		postScanNudges = append(postScanNudges, analysisNudge)
		// Joined via bgWriters: the sweeper queries the store and enqueues
		// analysis jobs whose completions write it back, so it must drain
		// before Store.Close() like the other manifest writers.
		bgWriters.Add(1)
		go func() {
			defer bgWriters.Done()
			runAnalysisSweeper(scanCtx, manifestStore, apiSrv.Resolver(),
				analyze.WaveformDirFor(cfg.DataDir), analysisPool, cfg.ScanInterval(),
				analysisNudge, analysisSweepState)
		}()
	}

	// Fingerprint sweeper. Wired HERE rather than beside the enricher because
	// it needs apiSrv's hot-reloading resolver: a snapshot taken at enricher
	// construction would keep routing against the old roots after an admin
	// add/remove. Joined to bgWriters so a live fpcalc child cannot outlive
	// runServe.
	var fingerprintNudge chan struct{}
	var fingerprintSweepState *sweepStatus[admin.FingerprintSweepCounts]
	if fingerprintCache != nil {
		sweeper := &fingerprintSweeper{
			store:     manifestStore,
			resolver:  apiSrv.Resolver(),
			client:    acoustid.NewClient("", acoustIDKey, userAgent, nil),
			cache:     fingerprintCache,
			workers:   cfg.Fingerprint.EffectiveWorkers(),
			maxPerRun: cfg.Fingerprint.EffectiveMaxPerRun(),
			length:    cfg.Fingerprint.EffectiveLength(),
		}
		// Buffered-1 nudge for the admin "Sweep now" button — same
		// coalescing contract as the analysis sweeper's.
		fingerprintNudge = make(chan struct{}, 1)
		fingerprintSweepState = &sweepStatus[admin.FingerprintSweepCounts]{}
		bgWriters.Add(1)
		go func() {
			defer bgWriters.Done()
			runFingerprintSweeper(scanCtx, sweeper, cfg.Fingerprint.EffectiveSweepInterval(),
				fingerprintNudge, fingerprintSweepState)
		}()
	}

	// Duplicates stamping sweeper — ALWAYS wired (not feature-gated):
	// stamping runs regardless of policy (stats work with the filter
	// off), and the nudge is what makes an off→on settings flip
	// hot-apply. Nudge-only (no tick, no startup run — the scan tail
	// owns the periodic cadence); bgWriters-joined because the pass
	// writes the store.
	duplicatesNudge := make(chan struct{}, 1)
	duplicatesSweepState := &sweepStatus[duplicatesSweepCounts]{}
	bgWriters.Add(1)
	go func() {
		defer bgWriters.Done()
		runDuplicatesSweeper(scanCtx, scanner, duplicatesNudge, duplicatesSweepState, duplicatesDeferRetry)
	}()

	// Smart-playlist regenerator (shares scanCtx). analysisActive (the
	// sox-resolved flag) gates the harmonic/discovery families.
	var smartMixRunState *sweepStatus[struct{}]
	if smartPlaylistsActive {
		smartMixRunState = &sweepStatus[struct{}]{}
		// Joined via bgWriters — the regenerator persists generated playlists
		// to the store, so it must drain before Store.Close().
		bgWriters.Add(1)
		go func() {
			defer bgWriters.Done()
			runSmartPlaylistRegenerator(scanCtx, manifestStore, analysisActive,
				cfg.SmartPlaylists.EffectiveRegenerateInterval(), smartMixRunState)
		}()
	}

	// One TTL-cached sox probe shared by every consumer: the admin's
	// availability + FLAC closures (so the Settings page does at most one
	// fork-exec per 30 s window regardless of tile count) AND the upscale
	// enqueuer's per-source decodability check. Declared here rather than
	// beside the admin wiring below because the enqueuer is constructed
	// first; one instance keeps all three reading the same snapshot.
	soxCache := &soxToolchainCache{}

	var upscalePool *transcode.Pool
	var upscaleCoordinator *transcode.Coordinator
	// Auto-optimize sweeper handles, in runServe scope so the admin Deps
	// closures wired further down can read them. Both stay nil when the
	// feature can't run (no upscale pool, or the optimize kind opted out).
	var autoOptimizeNudge chan struct{}
	var autoOptimizeSweepState *sweepStatus[admin.AutoOptimizeSweepCounts]
	// autoOptimizeEnabledFn is the SHARED live predicate: the sweeper asks
	// it whether to do work, and the admin card asks it what to report.
	// One closure, deliberately — duplicating the three gates would let the
	// card claim "active" while every sweep short-circuits, which is the
	// same live-runtime-vs-persisted-config divergence /v1/upscale/stats
	// exists to avoid.
	var autoOptimizeEnabledFn func() bool
	if upscaleActive {
		upscalePool = transcode.NewPool(manifestStore, cfg.Upscale.EffectiveWorkers(), cfg.Upscale.EffectiveQueueCap())
		defer upscalePool.Stop()
		// v1.3 Coordinator wraps the Pool with per-batch tracking.
		// `RecoverInterruptedBatches` runs synchronously inside
		// NewCoordinator — any rows left in pending/running from a
		// crash mid-batch transition to `interrupted` BEFORE Submit
		// is reachable. The publish closure is wired below after
		// the SSE broker handle is in scope; Pool callbacks are
		// wired alongside.
		var err error
		// Resolver closure: library-relative → absolute via the live
		// api.Server resolver. Without this the Coordinator enqueues
		// JobSpecs with empty SourceAbsPath and every sox run fails
		// (CodeRabbit critical on PR #201).
		batchResolver := func(libraryRel string) (string, error) {
			abs, _, err := apiSrv.Resolver().ResolveChecked(libraryRel)
			return abs, err
		}

		upscaleCoordinator, err = transcode.NewCoordinator(upscalePool, manifestStore, cfg.DataDir, nil, batchResolver)
		if err != nil {
			fmt.Fprintf(stderr, "upscale coordinator: %v\n", err)
			return 1
		}
		// Same cached probe the per-track enqueuer and the admin tile read,
		// so the batch walk refuses sources this sox build cannot decode
		// instead of enqueuing jobs that are certain to fail.
		upscaleCoordinator.WithSoxInfo(soxCache.snapshot)
		// Seed the DB-backed target settings from the YAML bootstrap
		// on first run. Once seeded, admin Settings edits become
		// authoritative; YAML stays the bootstrap-only path.
		if _, _, err := manifestStore.GetUpscaleTarget(ctx); err != nil {
			if errors.Is(err, manifest.ErrUpscaleTargetUnset) {
				rate := cfg.Upscale.EffectiveBootstrapTargetRate()
				bits := cfg.Upscale.EffectiveBootstrapTargetBits()
				if seedErr := manifestStore.SetUpscaleTarget(ctx, rate, bits); seedErr != nil {
					fmt.Fprintf(stderr, "seed upscale target: %v\n", seedErr)
					return 1
				}
			}
		}
		// liveVariantsDir resolves the effective variants dir from the
		// LIVE config holder so hot changes via POST
		// /api/upscale/variants-dir take effect without a restart —
		// both for where new sidecars land and for the pre-flight
		// disk checks that grade that volume.
		liveVariantsDir := func() string {
			live := cfgHolder.Load()
			if live == nil {
				// Defensive: the holder is seeded before serving, but a
				// nil snapshot must not panic a Submit — "" makes the
				// coordinator's disk check fall back to its dataDir.
				return ""
			}
			return live.Upscale.EffectiveVariantsDir(live.DataDir)
		}
		apiSrv.WithUpscaleEnqueuer(&upscaleEnqueuerAdapter{
			pool:      upscalePool,
			store:     manifestStore,
			resolver:  apiSrv.Resolver(),
			cfg:       cfg,
			outputDir: liveVariantsDir,
			soxInfo:   soxCache.snapshot,
		})
		apiSrv.WithBatchCoordinator(&upscaleBatchCoordinatorAdapter{
			coord:     upscaleCoordinator,
			store:     manifestStore,
			outputDir: liveVariantsDir,
		})
		// Variant lifecycle: DELETE /v1/upscale/variants + reactive
		// serve-side cleanup + integrity ticker all share the same
		// store + pool refs. Wired together so the feature flag
		// `deleteVariants` in /v1/health reliably reflects whether
		// the underlying paths are reachable — see api.go's health
		// handler for the gate.
		apiSrv.WithVariantDeleter(&variantDeleterAdapter{store: manifestStore})
		apiSrv.WithInflightDropper(&inflightDropperAdapter{pool: upscalePool})

		// Auto-optimize sweeper: pre-generates CarPlay `optimized-*`
		// variants so iOS never has to play the hi-res source while it
		// waits for one (see cmd/bridge/auto_optimize.go for why the lazy
		// path structurally misses on first play). Wired only when the
		// pool exists AND the optimize kind is enabled; the `enabled`
		// flag itself is read LIVE per sweep so an admin Settings flip
		// hot-applies on the next nudge.
		//
		// bgWriters-joined: completions call UpsertVariant, so the
		// sweeper's work must drain before Store.Close() like every other
		// manifest writer.
		if cfg.Upscale.EffectiveOptimizeEnabled() {
			autoOptimizeNudge = make(chan struct{}, 1)
			autoOptimizeSweepState = &sweepStatus[admin.AutoOptimizeSweepCounts]{}
			postScanNudges = append(postScanNudges, autoOptimizeNudge)
			autoOptimizeEnabledFn = func() bool {
				live := cfgHolder.Load()
				if live == nil {
					return false // defensive: never sweep on a nil snapshot
				}
				// All three gates: the master toggle, the optimize kind, and
				// the pre-generation flag. `upscale.enabled` is included even
				// though the pool is wired at boot — an operator can PATCH it
				// off mid-flight, and the sweeper must stop with it.
				return live.Upscale.Enabled &&
					live.Upscale.EffectiveOptimizeEnabled() &&
					live.Upscale.AutoOptimize.Enabled
			}
			sweeper := &autoOptimizeSweeper{
				store: manifestStore,
				// apiSrv.Resolver() is the hot-reloading resolver, so a
				// runtime root add/remove is honoured without a restart
				// (same rationale as the upscale enqueuer's).
				resolver:  apiSrv.Resolver(),
				enqueue:   upscalePool.Enqueue,
				outputDir: liveVariantsDir,
				enabled:   autoOptimizeEnabledFn,
				// Same cached probe every other consumer reads, so a source
				// this sox build can't decode is skipped instead of being
				// re-enqueued and re-failed on every sweep.
				soxInfo: soxCache.snapshot,
				maxPerSweep: func() int {
					if live := cfgHolder.Load(); live != nil {
						return live.Upscale.AutoOptimize.EffectiveMaxPerSweep()
					}
					return config.DefaultAutoOptimizeMaxPerSweep
				},
				minFreeBytes: func() int64 {
					if live := cfgHolder.Load(); live != nil {
						return live.Upscale.AutoOptimize.EffectiveMinFreeBytes()
					}
					return config.DefaultAutoOptimizeMinFreeBytes
				},
				diskFree: transcode.AvailableDiskSpaceNearest,
			}
			bgWriters.Add(1)
			go func() {
				defer bgWriters.Done()
				runAutoOptimizeSweeper(scanCtx, sweeper, cfg.AutoOptimizeInterval(),
					autoOptimizeNudge, autoOptimizeSweepState)
			}()
		}

		// Periodic integrity sweep: walks `track_variants` on the
		// configured cadence (default 1 h, opt-out via
		// `integrity.variantSweepIntervalSec: 0`) and reconciles
		// rows whose sidecar files no longer exist on disk. Pairs
		// with the reactive open-on-serve cleanup in
		// internal/api/files.go::serveVariant — that path closes
		// the active-playback case immediately; this one catches
		// the not-currently-playing case. The publish closure
		// builds the typed api.UpscaleDeletedEvent so the SSE wire
		// shape stays in lockstep with the operator-driven delete
		// handler. Skipped silently when the interval is ≤ 0.
		// The effective variants dir is passed so the watcher can
		// refuse a sweep when the whole directory reads missing /
		// empty with rows in the catalog (cleanly-unmounted
		// variants volume) instead of mass-deleting every row.
		sweepInterval := cfg.VariantSweepInterval()
		if sweepInterval > 0 {
			variantWatcher := integrity.NewVariantWatcher(
				&integrityVariantListerAdapter{store: manifestStore},
				&integrityVariantDeleterAdapter{store: manifestStore},
				func(paths, variantIDs []string) {
					apiSrv.EventPublisher().Publish("upscale.deleted", api.UpscaleDeletedEvent{
						Paths:      paths,
						VariantIDs: variantIDs,
						DeletedAt:  time.Now(),
					})
				},
				cfg.Upscale.EffectiveVariantsDir(cfg.DataDir),
				sweepInterval,
			)
			stopVariantWatcher := variantWatcher.Start(scanCtx)
			defer stopVariantWatcher()
		}

		// Background forward-sweep GC: walks the variants directory
		// for `.flac` files NOT present in `track_variants.sidecar_path`
		// and unlinks them. Pairs with the operator-triggered
		// `bridge upscale --gc` (cmd/bridge/upscale.go) — that path
		// keeps its unbounded-sweep semantics for one-shot operator
		// cleanups; this background variant is chunked + opt-in via
		// `cfg.Integrity.OrphanSidecarSweepIntervalSec`. Default zero
		// (disabled) — operators on minimal deploys see zero
		// behavioural change. Skipped silently when the interval is
		// ≤ 0. See CLAUDE.md "Bridge background GC" for the snapshot
		// + chunking + cursor invariants.
		gcInterval := cfg.OrphanSidecarSweepInterval()
		if gcInterval > 0 {
			orphanSweeper := integrity.NewOrphanSidecarSweeper(
				&integritySidecarListerAdapter{store: manifestStore},
				cfg.Upscale.EffectiveVariantsDir(cfg.DataDir),
				gcInterval,
			)
			stopOrphanSweeper := orphanSweeper.Start(scanCtx)
			defer stopOrphanSweeper()
		}
	}

	// THE post-scan hook — one registration fanning out to every sweeper
	// that asked for a nudge (see postScanNudges). Registered here, after
	// both the analysis and auto-optimize blocks have appended, because
	// SetPostScanHook REPLACES rather than appends.
	//
	// Landing after RunPeriodic already started is the pre-existing,
	// intended pattern: Scanner.postScanHook is an atomic.Pointer
	// specifically so boot-time wiring can't race an in-flight startup
	// scan, and each sweeper's settle-delay-then-sweep covers a startup
	// scan that finished before this line ran.
	//
	// The sends are non-blocking on buffered-1 channels, so the hook
	// stays cheap enough for the scanner goroutine as its contract
	// requires — a pending nudge coalesces with the sweep it would have
	// triggered.
	if len(postScanNudges) > 0 {
		nudges := postScanNudges // capture; the slice isn't appended to after this
		scanner.SetPostScanHook(func() {
			for _, ch := range nudges {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		})
	}

	// /v1/upscale/stats wiring. Always registered so paired iOS
	// clients can render a clean "feature off" state on bridges where
	// the operator hasn't enabled upscaling — same nil-safe contract
	// the admin tile uses. The closure mirrors the admin
	// /api/upscale/stats handler exactly so the operator's Settings
	// page and the iOS management section show the same numbers.
	//
	// Three sources combined:
	//   1. Live pool counters — only when upscalePool != nil AND
	//      cfg.Upscale.Enabled (operator can disable mid-flight via a
	//      PATCH; the long-lived Pool stays alive until restart, but
	//      we honour the live flag and report no pool to keep the wire
	//      semantics in lockstep with /v1/health.upscaleEnabled).
	//   2. Cached-variants count + total bytes from `track_variants`
	//      — survives across restarts and reflects historical work,
	//      so it stays non-zero when the feature was disabled without
	//      `--gc`. SQL failure degrades to "0 cached" with a logged
	//      warning rather than turning the whole response into a 5xx.
	//   3. Sox-availability probe — same `transcode.PrecheckSox` the
	//      admin tile consumes, gated by the same 30 s TTL cache the
	//      admin handler uses (the admin cache holds it; we re-probe
	//      directly here, accepting one extra fork-exec per 5 s poll
	//      since iOS only polls when the management page is fore-
	//      grounded — typically zero polls per minute on average).
	upscaleStats := &upscaleStatsAdapter{
		pool: func() *transcode.Pool { return upscalePool },
		enabled: func() bool {
			live := cfgHolder.Load()
			return upscalePool != nil && live != nil && live.Upscale.Enabled
		},
		store: manifestStore,
	}
	apiSrv.WithUpscaleStats(upscaleStats)

	// SSE upstream-publisher wiring. The broker started at line 992
	// (StartEventBroker) is now ready; hook the two upstream services
	// (transcode pool, pairing store) to publish state-change events
	// to it. Both use a setter pattern post-construction because
	// pairingStore is built before apiSrv (chicken-and-egg with the
	// broker reference), and the upscalePool snapshot needs the
	// upscaleStatsAdapter built above.
	//
	// Pool fires onStateChange after each job transition (enqueue,
	// completion, sox-fail, store-fail). The closure captures the
	// `upscaleStats` adapter so the published payload is identical
	// to what `/v1/upscale/stats` returns — iOS reuses the same
	// decoder across SSE and polling transports.
	if upscalePool != nil {
		broker := apiSrv.EventPublisher()
		upscalePool.SetOnStateChange(func() {
			// SSE publisher fires from a single long-lived
			// goroutine. A wedged CountVariants query inside
			// UpscaleStatsSnapshot would otherwise block all
			// subsequent SSE deliveries (job completions, batch
			// updates, etc.). 2 s is the same budget the
			// /v1/upscale/stats HTTP route uses for the same
			// query. Gemini Medium on PR #218.
			//
			// `defer cancel()` (vs. explicit `cancel()` after the
			// Snapshot call) keeps the cleanup robust against a
			// future panic or early-return between the WithTimeout
			// and the Snapshot. Matches the project-wide convention
			// for `context.WithTimeout` (mirror of the four
			// `defer cancel()` sites at lines 1900-1977 below).
			// Gemini Medium on PR #219.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			snap, err := upscaleStats.UpscaleStatsSnapshot(ctx)
			if err != nil {
				// Drop the publish — pushing a partial snapshot
				// (zero cachedVariants on timeout) would cause
				// iOS clients to flash "feature off" briefly
				// before the next successful poll. The
				// degrade-and-log policy lives inside the
				// adapter; here we just suppress the event.
				logger.Debug("upscale.stats SSE: snapshot timed out", "err", err)
				return
			}
			broker.Publish("upscale.stats", snap)
		})
		// Wire the Coordinator's SSE publish closure now that the
		// broker is in scope. Coordinator was constructed earlier
		// (with publish=nil) to keep `RecoverInterruptedBatches`
		// running BEFORE we accept any pool callbacks.
		if upscaleCoordinator != nil {
			upscaleCoordinator.SetPublish(func(evt transcode.BatchProgressEvent) {
				broker.Publish("upscale.batch", evt)
			})
		}
		// Per-job completion fires after UpsertVariant commits. The
		// pool passes primitives so it never imports the api package;
		// the closure builds the typed wire shape here. Topic name is
		// flat alphanumeric+dot per the broker convention (sister to
		// "upscale.stats" / "pairing.<id>"). iOS gates ladder rungs on
		// the "upscaleCompleteEvents" capability flag advertised in
		// /v1/health, so pre-feature iOS clients won't observe this
		// event yet.
		upscalePool.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
			broker.Publish("upscale.complete", api.UpscaleCompleteEvent{
				Path:          path,
				VariantID:     variantID,
				SampleRate:    sampleRate,
				BitsPerSample: bitsPerSample,
				CompletedAt:   completedAt,
			})
			// v1.3 batch attribution: forward to the Coordinator's
			// callback so the owning `upscale_batches` row's
			// `processed_files` counter advances. The Coordinator
			// no-ops on zero batchID (legacy per-track jobs from the
			// pre-v1.3 `POST /v1/upscale` path).
			if upscaleCoordinator != nil {
				upscaleCoordinator.OnJobComplete(path, variantID, sampleRate, bitsPerSample, durationSeconds, batchID, completedAt)
			}
		})
		// v1.3 per-job failure callback. Used by the Coordinator to
		// bump `failed_files` on the owning batch's row AND surface
		// the redacted error string to the admin Jobs page via the
		// `upscale.batch` SSE event.
		upscalePool.SetOnJobFailed(func(path, variantID, errMsg string, durationSeconds float64, batchID uuid.UUID, failedAt time.Time) {
			if upscaleCoordinator != nil {
				upscaleCoordinator.OnJobFailed(path, variantID, errMsg, durationSeconds, batchID, failedAt)
			}
		})
	}

	// Pairing store fires onStateChange after Approve / Decline /
	// timer-driven Pending→Expired. The closure builds an
	// `api.PairingStateEvent` from the Request snapshot — same wire
	// shape `/v1/pairing/{id}` returns. TTLSecondsRemaining is
	// computed exactly the way Store.Poll computes it (deadline =
	// CreatedAt + ttl, floor at zero). Token / TokenID land only
	// for the Approved state per the existing wire contract.
	bridgeStartedAtUnix := apiSrv.StartedAt().UnixMilli()
	// Use the live store's TTL rather than the package default — if
	// pairing.NewStore was constructed with a custom TTL via Options
	// (or a future config field), this closure must match. Gemini bot
	// review on PR #136 caught the fragility of the package-constant
	// reference.
	pairingTTL := pairingStore.TTL()
	pairingStore.SetOnStateChange(func(snap pairing.Request) {
		ev := api.PairingStateEvent{
			Status:           snap.State.String(),
			BridgeStartedAt:  bridgeStartedAtUnix,
			VerificationCode: snap.VerificationCode,
		}
		if snap.State == pairing.StatePending {
			deadline := snap.CreatedAt.Add(pairingTTL)
			rem := time.Until(deadline)
			if rem < 0 {
				rem = 0
			}
			ev.TTLSecondsRemaining = int(rem / time.Second)
		}
		if snap.State == pairing.StateApproved {
			ev.Token = snap.RawToken
			ev.TokenID = snap.TokenID
		}
		apiSrv.EventPublisher().Publish("pairing."+snap.ID, ev)
	})

	tlsConfig := &tls.Config{
		GetCertificate: certManager.Get,
		MinVersion:     tls.VersionTLS12,
	}
	// Merge autocert's required ALPN proto-id ("acme-tls/1") into
	// the public API listener's NextProtos so LE's TLS-ALPN-01
	// challenge handshake finds the right path.
	//
	// **MUST include "h2" + "http/1.1" explicitly** when NextProtos
	// is non-empty (Gemini-high security review on PR #293).
	// `http.Server.serveTLS` auto-adds these only when
	// `TLSConfig.NextProtos == nil`; once we set it to anything
	// (e.g. just `acme-tls/1`), the auto-add is suppressed and
	// modern browsers fail to negotiate HTTP/2 against the public
	// API. Loopback installs (extra==nil) skip this branch entirely
	// and see byte-identical handshake behaviour.
	if extra := certManager.NextProtos(); len(extra) > 0 {
		tlsConfig.NextProtos = append([]string{"h2", "http/1.1"}, extra...)
	}

	httpSrv := &http.Server{
		Addr:      cfg.ListenAddress,
		Handler:   apiSrv.Handler(),
		TLSConfig: tlsConfig,
		// Defence-in-depth against slow-loris / half-open sockets.
		// WriteTimeout is deliberately left UNSET (zero) because
		// `/v1/download` streams multi-GB DSD files to iOS (and needs
		// many minutes under slow Wi-Fi / Tailscale relays); setting
		// WriteTimeout would cut the response mid-flight and crash
		// Hugo 2's DoP lock. ReadHeaderTimeout + ReadTimeout guard the
		// request side only; IdleTimeout drains kept-alive connections.
		//
		// ReadHeaderTimeout tightened from 10s to 5s (PR-C audit
		// follow-up). A legitimate request completes its header
		// transmission in well under a second on every supported
		// network (LAN, Tailscale, Wi-Fi over mobile); 5s leaves
		// ample headroom for the slowest legitimate clients while
		// halving the slot-occupation window a slowloris attacker
		// can hold per connection. ReadTimeout (60s) covers the
		// body-read window for `POST /v1/pairing/requests` (4 KiB
		// max body, easily comfortable inside 60s under any sane
		// network).
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Admin console: plain HTTP on a loopback address (default
	// 127.0.0.1:7789). Shares the api server's Resolver so hot-add/remove
	// of library roots lands on both sides in lockstep.
	//
	// *configPath was already resolved AND absolutised at the top of
	// runServe, so this is a no-op Clean rather than a second resolution.
	// It stays as the belt-and-braces path for the one case that skips
	// the earlier absolutise — filepath.Abs failing on a broken Getwd.
	absCfgPath, _ := filepath.Abs(*configPath)
	// Shared instance across the admin tile AND the api server: the
	// api layer's `/v1/health.endpoints` advertising in tsnet mode
	// queries this same source for the embedded node's MagicDNSName +
	// tailnet IPs (5s TTL cached at the api boundary). Constructed
	// once so the per-source state (rare LastError stickiness, etc.)
	// stays consistent across both consumers.
	tailscaleAdminSrc := newTailscaleAdminSource(tailscaleAuto, tsnetServer, absCfgPath, cfgHolder)
	apiSrv.SetTailscaleStatus(tailscaleAdminSrc)

	// Admin auth (public mode only). adminauth.Store holds the
	// bcrypt-hashed credentials + in-memory session state; the
	// rate limiter caps failed-login attempts per (clientIP,
	// username). In loopback mode both stay nil and the admin
	// middleware is a passthrough — preserves the historical
	// no-auth contract.
	//
	// Public-mode refuse-to-start: if `deployment.mode: public`
	// is set but no admin credentials have been minted yet (no
	// `bridge init --public` run, no `bridge admin reset-password`),
	// surface a clear error rather than serve unauthenticated
	// traffic.
	var adminAuthStore *adminauth.Store
	var loginLimiter *adminauth.RateLimiter
	if cfg.IsPublic() {
		adminAuthPath := filepath.Join(cfg.DataDir, "adminauth.json")
		adminAuthStore, err = adminauth.OpenStore(adminAuthPath)
		if err != nil {
			fmt.Fprintf(stderr, "adminauth: open %s: %v\n", adminAuthPath, err)
			return 1
		}
		if !adminAuthStore.IsInitialised() {
			fmt.Fprintf(stderr, "adminauth: no admin credentials at %s — run `bridge admin reset-password` (or `bridge init --public` on a fresh install)\n", adminAuthPath)
			return 1
		}
		loginLimiter = adminauth.NewRateLimiter()
		defer loginLimiter.Stop()
	}

	// Admin TLS wiring (public mode + direct-TLS path only). When
	// IsPublic() && !AdminTLSTerminatedByProxy, wrap the admin
	// listener via certManager's AdminTLSConfig — the same SNI
	// switcher fronts both the public API and the admin
	// console. Loopback installs and reverse-proxy deployments
	// stay on plain HTTP (the proxy fronts TLS; the bridge
	// serves on a private interface).
	var adminTLSConfig *tls.Config
	adminScheme := "http"
	if cfg.IsPublic() && !cfg.Deployment.AdminTLSTerminatedByProxy {
		adminTLSConfig = certManager.AdminTLSConfig()
		adminScheme = "https"
	}
	// Autocert status closure for the admin tile / API endpoint.
	// Nil-safe: when autocert isn't wired, returns the
	// "disabled" snapshot (admin tile hidden, /api returns
	// disabled shape).
	autocertStatusClosure := func() admin.AutocertStatusSnapshot {
		if acmeManager == nil {
			return admin.AutocertStatusSnapshot{}
		}
		st := acmeManager.Status()
		return admin.AutocertStatusSnapshot{
			Domain:      st.Domain,
			CertPresent: st.CertPresent,
			NotAfter:    st.NotAfter,
			LastError:   st.LastError,
			LastCheck:   st.LastCheck,
		}
	}

	// PR 4 hot-reload callbacks. The Settings PATCH path fires
	// these after persisting the new config so the runtime
	// flips into the new posture without a restart.
	//
	// **atomic.Pointer indirection** is load-bearing on two
	// fronts (Gemini High + CodeRabbit Major on PR #294):
	//   1. Race-safe across the admin-Serve goroutine (which
	//      may receive a PATCH before main reaches the
	//      net.Listen step below) and the main goroutine
	//      that populates the pointer post-listener.
	//   2. **Catch-up apply**: if a PATCH arrives during the
	//      startup window, the callback records the desired
	//      state in `mdnsDesired` (atomic.Bool) — the
	//      post-listener init reads this and applies the
	//      latest desired state to the freshly-created
	//      lifecycle, so config-on-disk and runtime stay in
	//      lockstep.
	var mdnsLife atomic.Pointer[mdnsLifecycle]
	var mdnsDesired atomic.Pointer[bool] // nil = no override pending
	mdnsToggleCallback := func(b bool) {
		// Capture desired state for the startup-window catch-up.
		v := b
		mdnsDesired.Store(&v)
		if ml := mdnsLife.Load(); ml != nil {
			ml.Set(b)
		}
	}
	tailscaleDisableCallback := func() {
		if tailscaleAuto != nil {
			tailscaleAuto.Disable()
		}
	}

	adminSrv, err := admin.New(admin.Deps{
		CfgHolder:   cfgHolder,
		CfgPath:     absCfgPath,
		Auth:        store,
		Manifest:    manifestStore,
		Scanner:     scanner,
		Resolver:    apiSrv.Resolver(),
		Fingerprint: fingerprint,
		// Pairing-QR baker asks the SNI cert switcher what fingerprint a
		// device will capture when it dials a given host, so a public-mode
		// QR advertises the autocert LE fingerprint (what the device sees)
		// instead of the self-signed LAN pin (which it never would).
		FingerprintForHost: certManager.FingerprintForServerName,
		AdminAuth:          adminAuthStore,
		LoginLimiter:       loginLimiter,
		TLSConfig:          adminTLSConfig,
		AutocertStatus:     autocertStatusClosure,
		MDNSToggle:         mdnsToggleCallback,
		TailscaleDisable:   tailscaleDisableCallback,
		StartedAt:          time.Now().UTC(),
		ScanCtx:            scanCtx,
		Restart:            cancel,
		// UPnP upstream admin surface (Bridge PR E). Nil when the
		// feature is disabled — admin handlers return a clean 404 +
		// the Devices page hides the card. ctx is passed so async
		// rescans inherit the bridge's run-scope (a request-context
		// would cancel the walk when the operator's browser
		// disconnects).
		UPnPUpstream:    upnpLC.installAdminAdapter(ctx, cfgHolder, absCfgPath, manifestStore, upnpLC.ingester),
		Updater:         updAdapter,
		BackupSources:   backupSources,
		LogPath:         adminLogPath(),
		Tailscale:       tailscaleAdminSrc,
		Pairing:         pairingStore,
		IsSupervised:    supervision.IsSupervised(),
		UpscalePrecheck: soxCache.precheck,
		UpscaleSoxFLAC:  soxCache.flac,
		// Backs the Inspector's no_decoder badge from the SAME probe the
		// enqueue gates use, so a tile can't say "eligible" about a source
		// the batch walk would refuse.
		SoxCanDecode: func(p string) bool {
			info, err := soxCache.snapshot()
			if err != nil {
				return true // fail open, as every other consumer does
			}
			return info.CanDecode(p)
		},
		// Live runtime state of audio analysis (startup-computed gate),
		// so the admin tile's `enabled` matches /v1/health's `waveform`
		// flag rather than the persisted config flag.
		AnalysisActive: func() bool { return analysisActive },
		// Analysis pool + sweeper surfaces (nil when the feature is off —
		// the admin then omits the fields, mirroring the upscale tile).
		AnalysisPoolStats: analysisPoolStatsClosure(analysisPool),
		// Ports this process bound, so the console's preflight answers
		// the port checks from knowledge rather than a bind probe that
		// can only fail against our own listeners.
		DoctorRun:             adminDoctorRunner(absCfgPath, ownedListenPorts(cfg)),
		AnalysisSweep:         analysisSweepClosure(analysisSweepState),
		TriggerAnalysisSweep:  nudgeTriggerClosure(analysisNudge),
		AnalysisSchemaVersion: analyze.WaveformSchemaVersion,
		// Fingerprint job card: always wired so a feature-off bridge still
		// explains WHY (config flag + degraded reason); the trigger stays
		// nil unless the sweeper is actually running.
		FingerprintState: fingerprintStateClosure(cfg.Fingerprint.Enabled,
			fingerprintCache != nil, fingerprintDegraded, fingerprintSweepState),
		TriggerFingerprintSweep: nudgeTriggerClosure(fingerprintNudge),
		// Auto-optimize card + trigger. Both nil unless the sweeper is
		// wired (upscale pool present AND the optimize kind enabled), so a
		// bridge that can't pre-generate renders no card rather than a
		// permanently-inactive one. The `enabled` reader is live because
		// the flag hot-applies.
		AutoOptimizeState:        autoOptimizeStateClosure(autoOptimizeEnabledFn, "", autoOptimizeSweepState),
		TriggerAutoOptimizeSweep: nudgeTriggerClosure(autoOptimizeNudge),
		TriggerDuplicatesPass:    nudgeTriggerClosure(duplicatesNudge),
		DuplicatesSweepRun:       jobRunClosure(duplicatesSweepState),
		// Last/next-run recorders for the smart-mix + backup cards (nil
		// when the respective loop isn't running).
		SmartMixRun: jobRunClosure(smartMixRunState),
		BackupRun:   jobRunClosure(backupRunState),
		// Artist-image coverage source for the dashboard enrichment card —
		// one ReadDir over the shared artwork cache dir, called behind the
		// admin's 60s enrichment-meta TTL (never per-tick).
		ArtistImageMBIDs: func() (map[string]struct{}, error) {
			return enrich.CachedArtistImageMBIDs(artworkDir)
		},
		// Why the enricher stopped short, by bounded reason. In-memory and
		// process-lifetime — it answers "is this library unmatchable, or is
		// the matcher broken?", which the aggregate miss count cannot.
		EnrichSkipReasons: enricher.SkipReasons,
		// "Retry missing" harvest nudge: zeroing the last-submit stamp makes
		// the harvest client's next tick re-submit the full library (Atlas
		// re-attempts unresolved bios/descriptions; submit is idempotent).
		// nil-equivalent when harvest isn't wired: returns false.
		HarvestForceSubmit: func() bool {
			if harvestState == nil {
				return false
			}
			if err := harvestState.SetLastSubmit(time.Time{}); err != nil {
				logger.Warn("enrichment retry: reset harvest submit stamp", "err", err)
				return false
			}
			return true
		},
		// The in-process half of "Retry missing" for fingerprinting. The
		// persisted no-match verdict and this cache suppress the same
		// candidates, and the sweeper reads the cache first, so a retry that
		// cleared only the database would not re-open a file answered this
		// session. nil cache (fingerprinting disabled) reports nothing
		// dropped, which is accurate — there is no sweeper to re-open it for.
		FingerprintForget: func(prefix string) int {
			if fingerprintCache == nil {
				return 0
			}
			return fingerprintCache.Forget(prefix)
		},
		// Inspector byte-route path resolvers (loopback-only routes;
		// ids are regex-validated in the handlers before these run).
		// Cover + artist-image paths are wired unconditionally — the
		// caches exist with or without Atlas (CAA / local extraction).
		// Booklet closures are gated on the harvest client, mirroring
		// the /v1/health `booklets` flag condition.
		ArtworkPath: func(mbid string, size int) string {
			return enrich.ArtworkCachePath(artworkDir, mbid, size)
		},
		ArtistImagePath: func(mbid string) string {
			return enrich.ArtistImagePath(artworkDir, mbid)
		},
		BookletPath: func() func(string) string {
			if harvestClient == nil {
				return nil
			}
			return func(mbid string) string { return api.BookletPath(bookletsDir, mbid) }
		}(),
		BookletNudge: func() func(string) {
			if harvestClient == nil {
				return nil
			}
			return harvestClient.NudgeBookletFetch
		}(),
		UpscaleStats: func() *admin.UpscalePoolStats {
			// Snapshot the pool's live counters when the
			// feature is active. Two off-paths return nil
			// so the admin handler omits the `pool` field
			// entirely instead of surfacing zero-padded
			// clutter on the Settings page:
			//
			//   1. upscalePool == nil — sox-precheck demoted
			//      the feature at startup OR the operator
			//      never enabled it.
			//   2. cfg.Upscale.Enabled == false — operator
			//      just PATCHed the flag off; the long-
			//      lived Pool is still alive until restart,
			//      but the contract is "feature is off
			//      live", so don't surface live counters
			//      (CodeRabbit minor on PR #110 — the iOS-
			//      facing /v1/health.upscaleEnabled and the
			//      admin tile's `enabled` field both gate
			//      on this).
			live := cfgHolder.Load()
			if upscalePool == nil || live == nil || !live.Upscale.Enabled {
				return nil
			}
			s := upscalePool.Stats()
			// Map the pool's live per-worker grid into the admin DTO
			// (keeps internal/admin from importing internal/transcode).
			aw := upscalePool.ActiveWorkers()
			workers := make([]admin.ActiveWorkerView, len(aw))
			for i, w := range aw {
				workers[i] = admin.ActiveWorkerView{
					WorkerID:         w.WorkerID,
					Busy:             w.Busy,
					SourceRel:        w.SourceRel,
					SourceSampleRate: w.SourceSampleRate,
					SourceBits:       w.SourceBits,
					TargetSampleRate: w.TargetSampleRate,
					TargetBits:       w.TargetBits,
					Quality:          w.Quality,
					Kind:             w.Kind,
					StartedAtUnixMs:  w.StartedAtUnixMs,
				}
			}
			return &admin.UpscalePoolStats{
				Workers:       s.Workers,
				QueueCap:      s.QueueCap,
				QueueLen:      s.QueueLen,
				Inflight:      s.Inflight,
				Enqueued:      s.Enqueued,
				Done:          s.Done,
				Failed:        s.Failed,
				ActiveWorkers: workers,
			}
		},
		UpscaleBusy: func() bool {
			// Cheap atomic probe (Stats() = atomic counters + a map-len,
			// no DB) gating the fast-tick worker grid. Mirror the
			// UpscaleStats live-vs-persisted gate so a PATCHed-off feature
			// reports not-busy even while the long-lived pool drains.
			live := cfgHolder.Load()
			if upscalePool == nil || live == nil || !live.Upscale.Enabled {
				return false
			}
			st := upscalePool.Stats()
			return st.Inflight > 0 || st.QueueLen > 0
		},
		// Library Inspector projection closures (v1.3). Wired
		// to nil when upscale is disabled so the admin
		// projection handler's existing `nil` check fires (503
		// `upscale-disabled`). The prior shape wired non-nil
		// closures that returned an error string — admin
		// handler then surfaced 500 `disk-probe` instead of
		// 503. Per CodeRabbit major on PR #203 round 2.
		ProjectedSize: func() func(int64, int, int, int, int) int64 {
			live := cfgHolder.Load()
			if live == nil || !live.Upscale.Enabled {
				return nil
			}
			return func(sourceSize int64, sourceRate, sourceBits, targetRate, targetBits int) int64 {
				return transcode.ProjectedSize(sourceSize, sourceRate, sourceBits,
					targetRate, targetBits,
					transcode.DefaultCompressionFactor(targetBits))
			}
		}(),
		AvailableDiskSpace: func() func(string) (int64, error) {
			live := cfgHolder.Load()
			if live == nil || !live.Upscale.Enabled {
				return nil
			}
			// Nearest-existing-ancestor probe: the variants dir is
			// created lazily, so a bare statfs on it would ENOENT
			// before the first sidecar lands.
			return transcode.AvailableDiskSpaceNearest
		}(),
		// CarPlay-optimize deps closures. Gated on BOTH
		// `Upscale.Enabled` AND `EffectiveOptimizeEnabled()` so
		// the projection endpoint surfaces 503 in lockstep with
		// the api.Server's `WithCarPlayOptimize(upscaleActive
		// && cfg.Upscale.EffectiveOptimizeEnabled())` advertisement
		// (line ~1543 above). Pre-fix gated on Upscale.Enabled
		// alone, so a bridge with optimize explicitly DISABLED in
		// the config would still serve `?kind=optimize` projection
		// data — divergent from what /v1/health advertises and
		// from what POST /v1/upscale (kind=optimize) accepts. Per
		// CodeRabbit major on PR #276.
		OptimizeEligible: func() func(string, string, int, int) bool {
			live := cfgHolder.Load()
			if live == nil || !live.Upscale.Enabled || !live.Upscale.EffectiveOptimizeEnabled() {
				return nil
			}
			return transcode.OptimizeEligible
		}(),
		TargetRateForOptimize: func() func(int) int {
			live := cfgHolder.Load()
			if live == nil || !live.Upscale.Enabled || !live.Upscale.EffectiveOptimizeEnabled() {
				return nil
			}
			return transcode.TargetRateForOptimize
		}(),
		BatchCoordinator: func() admin.AdminBatchCoordinator {
			// Closure-resolved so admin doesn't see a typed-nil
			// pointer when upscale is disabled at boot — returning
			// the interface as untyped-nil keeps the admin handler's
			// `nil` check honest. Returns a real adapter only when
			// the Coordinator was constructed (i.e. cfg.Upscale.Enabled
			// AND the sox precheck passed).
			if upscaleCoordinator == nil {
				return nil
			}
			return &adminBatchCoordinatorAdapter{
				coord: upscaleCoordinator,
				store: manifestStore,
				// Live-resolved so hot variants-dir changes apply
				// without restart (see upscaleEnqueuerAdapter). Nil
				// snapshot → "" → coordinator falls back to dataDir.
				outputDir: func() string {
					live := cfgHolder.Load()
					if live == nil {
						return ""
					}
					return live.Upscale.EffectiveVariantsDir(live.DataDir)
				},
			}
		}(),
		VariantDeleter: func() admin.AdminVariantDeleter {
			// Same untyped-nil pattern as BatchCoordinator: when the
			// api.Server was constructed without
			// `WithVariantDeleter` (pre-feature build, sox precheck
			// failed, etc.), surface untyped-nil so the admin
			// handler's `s.deps.VariantDeleter == nil` short-circuit
			// returns 503. The adapter itself defends against this
			// too — both layers gate so a misconfigured boot can't
			// reach a panicking call site.
			if apiSrv == nil {
				return nil
			}
			return &adminVariantDeleterAdapter{apiSrv: apiSrv}
		}(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "admin: %v\n", err)
		return 1
	}
	adminCtx, adminCancel := context.WithCancel(context.Background())
	defer adminCancel()
	adminErr := make(chan error, 1)
	bgWriters.Add(1)
	go func() {
		defer bgWriters.Done()
		adminErr <- adminSrv.Serve(adminCtx)
	}()
	// Listen first so we can report the actual bound address (useful when
	// cfg.ListenAddress is ":0" — which test code uses).
	lis, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		fmt.Fprintf(stderr, "listen %s: %v\n", cfg.ListenAddress, err)
		return 1
	}

	// Format string uses bare %s — ServerVersion already carries the
	// "v" prefix when the Makefile / goreleaser inject it via -ldflags
	// (the Makefile reads `git describe --tags` which emits "v0.1.3..."
	// verbatim; goreleaser's {{.Version}} is also v-prefixed at release
	// time). Pre-fix this format string prepended an extra "v" and the
	// banner read "1-bit-bridge vv0.1.3-9-..." on every Makefile-built
	// bridge. The other format sites that print ServerVersion
	// (main.go's `bridge version` subcommand, styles.go's logo) now
	// agree on bare %s; tests in main_test.go's TestVersion pin the
	// same shape.
	fmt.Fprintf(stdout, "1-bit-bridge %s (protocol v%d) — listening on https://%s\n",
		version.ServerVersion, version.ProtocolVersion, lis.Addr())
	// Public-mode banner (PR 5): shorter shape — operator's
	// iOS clients dial the public domain (not the bound LAN
	// address), iOS doesn't pin the LE cert (so the fingerprint
	// box would mislead more than help), and the mDNS line
	// doesn't apply on a VPS. Loopback installs see the
	// historical banner unchanged.
	if cfg.IsPublic() {
		fmt.Fprintf(stdout, "Public mode — domain: %s\n", cfg.Autocert.Domain)
		if len(cfg.LibraryRoots) == 0 {
			fmt.Fprintf(stdout, "Library: %q (no roots — add one via the admin console or `bridge library add` once your storage is mounted)\n", cfg.LibraryName)
		} else {
			fmt.Fprintf(stdout, "Library: %q (roots: %v)\n", cfg.LibraryName, cfg.LibraryRoots)
		}
		// Operator-reachable URL — derived from the
		// configured public domain, NOT cfg.AdminAddress
		// (which is a bind target like 0.0.0.0:7789 or
		// 127.0.0.1:7789 — neither helps an operator
		// browsing from elsewhere). CodeRabbit Major review
		// post-PR-#295.
		//
		// **Proxy-mode special case** (CodeRabbit Major
		// review post-PR-#296): when admin TLS is terminated
		// by a reverse proxy, the bridge's local transport
		// is plain http on a private port (e.g.
		// http://bridge.example.com:7789/) — but THAT is the
		// backend, not the URL the operator visits. The
		// reverse proxy fronts admin at https://<domain>/
		// (or some operator-chosen path). The bridge can't
		// know what the proxy maps externally, so we print
		// the canonical assumption: https + domain, no port.
		// Operators with a non-standard proxy mapping will
		// recognise this is just the prompt and substitute
		// their actual URL.
		adminURL := operatorAdminURL(cfg, adminScheme)
		fmt.Fprintf(stdout, "Admin console: %s — log in with the credentials from `bridge admin reset-password`\n", adminURL)
	} else {
		fmt.Fprintf(stdout, "Library: %q (roots: %v)\n", cfg.LibraryName, cfg.LibraryRoots)
		fmt.Fprintf(stdout, "TLS fingerprint (pin this on the iOS side):\n  %s\n", fingerprint)
		fmt.Fprintf(stdout, "Admin console: %s — add library folders, pair devices, view stats\n", operatorAdminURL(cfg, adminScheme))
	}

	// Advertise on mDNS so iOS clients on the same LAN auto-discover
	// this server. Failures are non-fatal — mDNS is a nice-to-have,
	// and the server runs fine without it (users connect by IP).
	//
	// PR 4 promotes the advertiser into a hot-reloadable
	// lifecycle struct: the admin Settings PATCH path can flip
	// mdns.enabled on/off without a restart by firing the
	// `mdnsLife.Set(bool)` callback. Initial state is gated on
	// `EffectiveMDNSEnabled()` so an operator who set
	// `mdns.enabled: false` in YAML doesn't get a Bonjour
	// service emitted briefly at boot.
	boundAddr, _ := lis.Addr().(*net.TCPAddr)
	if boundAddr != nil {
		// nameSource closure reads the LIVE library name on every
		// Set(true) so a Settings PATCH that renamed the library
		// before toggling mDNS off→on picks up the new name
		// (Gemini medium on PR #294).
		nameSource := func() string {
			if live := cfgHolder.Load(); live != nil {
				return live.LibraryName
			}
			return cfg.LibraryName
		}
		ml := newMDNSLifecycle(boundAddr.Port, version.ProtocolVersion, nameSource, stdout, stderr)
		// Resolve the effective initial state. Catch-up apply:
		// if an admin PATCH arrived during the pre-listener
		// window, mdnsDesired carries its target value — that
		// wins over the cfg-on-disk default (operator pressed
		// the button; runtime should honour it).
		want := cfg.EffectiveMDNSEnabled()
		if d := mdnsDesired.Load(); d != nil {
			want = *d
		}
		if want {
			ml.Set(true)
		} else if cfg.IsPublic() {
			fmt.Fprintf(stdout, "mDNS: disabled by public-mode default\n")
		}
		mdnsLife.Store(ml)
	}
	if ml := mdnsLife.Load(); ml != nil {
		defer ml.Close()
	}

	var lanH3Srv *http3.Server
	var udpConn *net.UDPConn
	// tsH3 pairs the tsnet HTTP/3 server with its UDP PacketConn so
	// shutdown reads them atomically together. Stored from inside the
	// tsnet startup goroutine AFTER tsnetServer.Start succeeds — the
	// wrapper's ListenPacket guard at internal/tsnet/tsnet.go returns
	// "called before Start" otherwise. Pre-fix the HTTP/3 setup ran
	// synchronously above (before tsnetServer.Start fired in its
	// goroutine), so HTTP/3 over tailnet ALWAYS failed to bind on
	// every boot — the WARN log fired but the bridge silently fell
	// through to HTTP/2-only on the tailnet endpoint. Mirrors the
	// `tsnetHTTPSrv atomic.Pointer[http.Server]` race-safe pattern.
	var tsH3 atomic.Pointer[tsnetH3State]

	if !cfg.DisableHTTP3 {
		// 1. Resilient LAN Listener
		udpAddr, err := net.ResolveUDPAddr("udp", cfg.ListenAddress)
		if err != nil {
			logger.Warn("Failed to resolve UDP address, bypassing HTTP/3", "err", err)
		} else {
			udpConn, err = net.ListenUDP("udp", udpAddr)
			if err != nil {
				logger.Warn("Failed to bind LAN UDP socket, running HTTP/2 only", "err", err)
			} else {
				// Expand the UDP window to accommodate heavy FLAC/PCM streaming arrays
				const socketBufferSize = 2500 * 1024 // 2.5 MB
				if err := udpConn.SetReadBuffer(socketBufferSize); err != nil {
					logger.Debug("Could not expand UDP read buffer size", "err", err)
				}
				if err := udpConn.SetWriteBuffer(socketBufferSize); err != nil {
					logger.Debug("Could not expand UDP write buffer size", "err", err)
				}

				var lanTLSConfig *tls.Config
				if tlsConfig != nil {
					lanTLSConfig = tlsConfig.Clone()
					lanTLSConfig.NextProtos = []string{"h3"} // Force HTTP/3 ALPN exclusively
					lanTLSConfig.MinVersion = tls.VersionTLS13
				}

				if lanTLSConfig != nil {
					lanH3Srv = &http3.Server{
						Handler:   apiSrv.Handler(), // Crucial: Extract the compiled http.Handler
						TLSConfig: lanTLSConfig,
					}
					go func() {
						if err := lanH3Srv.Serve(udpConn); err != nil &&
							!errors.Is(err, http.ErrServerClosed) &&
							!strings.Contains(err.Error(), "server closed") {
							logger.Error("h3 serve direct", "err", err)
						}
					}()
				} else {
					logger.Warn("LAN TLS configuration is missing; bypassing LAN HTTP/3 initialization")
					if udpConn != nil {
						_ = udpConn.Close()
						udpConn = nil
					}
				}
			}
		}

		// 2. Tailscale HTTP/3 listener is set up INSIDE the tsnet
		// startup goroutine below, after tsnetServer.Start succeeds.
		// The wrapper's ListenPacket guard returns an error if called
		// before Start, so the original synchronous shape here always
		// failed at boot for tsnet-mode bridges (PR #264 regression).
	}

	// LAN HTTP/3 teardown as a defer so EVERY exit path drains the QUIC
	// listener and closes the bound UDP socket — not just the ctx.Done
	// graceful branch. The three startup-error branches below (serveErr /
	// adminErr / tsnetServeErr) `return 1` without touching lanH3Srv or
	// udpConn, and lanH3Srv.Serve(udpConn) doesn't observe ctx; because
	// runServe can return to the launcher menu (the process stays alive),
	// a leaked Serve goroutine + a still-bound UDP port made the next
	// "Start now" fail net.ListenUDP with "address already in use" and
	// silently fall back to HTTP/2-only. Mirrors the tsnet-H3 defer below.
	// Idempotent against the ctx.Done branch's explicit graceful drain:
	// http3.Server.Shutdown + udpConn.Close both tolerate a second call
	// (the tsnet-H3 listeners are already double-shut-down the same way).
	// Nil-guarded — either bind may have failed or HTTP/3 may be disabled.
	defer func() {
		if lanH3Srv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			_ = lanH3Srv.Shutdown(shutdownCtx)
		}
		if udpConn != nil {
			_ = udpConn.Close()
		}
	}()

	fmt.Fprintln(stdout, "Press Ctrl-C to shut down.")

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpSrv.ServeTLS(lis, "", "")
	}()

	// Tsnet path: spin up a SECOND http.Server bound to the embedded
	// tsnet listener. tsnet.Server.ListenTLS auto-renews LE certs
	// in-process; the listener accepts only on the tailnet virtual
	// interface, so dual-binding the same logical port (cfg.ListenAddress)
	// is safe — the LAN listener bind above sees the host's real
	// interface, this listener sees the tsnet stack only.
	//
	// Up() blocks on interactive auth on first run; spawn it in a
	// goroutine so the LAN listener is already serving while the
	// operator visits the AuthURL. Errors are logged but non-fatal —
	// the LAN listener keeps the bridge usable even if tsnet never
	// comes up.
	//
	// `tsnetHTTPSrv` is published via atomic.Pointer because the
	// startup goroutine writes it AFTER Up() succeeds and the
	// shutdown path (any exit branch below) reads it. Pre-fix, this
	// was a plain pointer with no synchronization — Qodo bug #1 +
	// Gemini high + CodeRabbit major all flagged it as a real race.
	//
	// Cleanup is via `defer` so EVERY exit path (serveErr, adminErr,
	// tsnetServeErr, ctx.Done) runs the same teardown sequence —
	// pre-fix only ctx.Done called Close(), so error-exit paths
	// leaked tsnet goroutines (Qodo bug #2 + Gemini medium).
	var tsnetHTTPSrv atomic.Pointer[http.Server]
	tsnetServeErr := make(chan error, 1)
	if tsnetServer != nil {
		defer func() {
			// Drain HTTP/3 first (if up), then HTTP/2, so in-flight
			// requests on either listener get a clean
			// http.ErrServerClosed instead of a mid-flight socket /
			// QUIC reset. THEN Close the tsnet.Server (drains
			// magicsock / netcheck / control plane goroutines per
			// CLAUDE.md plan correction #5). Each drain step gates on
			// a non-nil Load — failure to start either listener (e.g.
			// LAN-only tsnet timeout) leaves that slot nil.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			if state := tsH3.Load(); state != nil {
				for _, l := range state.listeners {
					_ = l.srv.Shutdown(shutdownCtx)
					_ = l.conn.Close()
				}
			}
			if srv := tsnetHTTPSrv.Load(); srv != nil {
				_ = srv.Shutdown(shutdownCtx)
			}
			if err := tsnetServer.Close(); err != nil {
				fmt.Fprintf(stderr, "tsnet close: %v\n", err)
			}
		}()

		go func() {
			startCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			if err := tsnetServer.Start(startCtx); err != nil {
				fmt.Fprintf(stderr, "tsnet: bring node up: %v (LAN listener still active)\n", err)
				return
			}
			// Wire the metrics tsnet collector so /metrics +
			// /v1/diagnostics surfaces report the live tailnet
			// state. The provider is a structural interface match:
			// tsnet.Server's `MetricsState` / `MetricsPeersOnline` /
			// `MetricsDERPLatencies` methods satisfy
			// `metrics.tsnetStatusProvider` without an explicit
			// import in either direction at the interface level.
			metrics.RegisterTsnetProvider(tsnetServer)

			// HTTP/3 (QUIC) over tailnet — set up here, AFTER Start
			// succeeded, because the wrapper's ListenPacket returns
			// "called before Start" otherwise (the synchronous PR
			// #264 placement always tripped that guard at boot).
			//
			// Three things this loop is responsible for that the
			// pre-fix shape got wrong:
			//
			//  1. Upstream tsnet.Server.ListenPacket requires an
			//     explicit tailnet IP (not the ":port" shorthand the
			//     LAN path uses with net.ListenUDP) — the listener
			//     binds to the virtual tailnet interface specifically,
			//     and a bare ":port" fails with "address must be a
			//     valid IP". We query Status() for the assigned IPs
			//     and bind one PacketConn per IP. Status() can take a
			//     few hundred ms to settle right after Start so use
			//     a bounded context.
			//
			//  2. Dual-stack tailnet nodes carry both IPv4 (100.x.y.z)
			//     and IPv6 (fd7a:...). HTTP/2 via
			//     `ListenTLS(cfg.ListenAddress)` accepts on both for
			//     free (port-only unspecified-IP form); HTTP/3 needs
			//     one explicit bind per IP — otherwise dual-stack
			//     clients connecting over the unbound address family
			//     fall back to HTTP/2 silently.
			//
			//  3. The H3 port MUST match the H2 port (extracted from
			//     cfg.ListenAddress). The Alt-Svc header `apiSrv`
			//     emits advertises h3 at the request's port; if H3
			//     listened on a different port (the pre-fix shape
			//     hardcoded :443) clients would dial the wrong port
			//     and never upgrade.
			//
			// Per-IP bind failure is non-fatal — we record the
			// successful listeners and continue. Total bind failure
			// degrades to HTTP/2 over tailnet via tsnetHTTPSrv below.
			if !cfg.DisableHTTP3 {
				statusCtx, statusCancel := context.WithTimeout(ctx, 5*time.Second)
				status, statusErr := tsnetServer.Status(statusCtx)
				statusCancel()
				_, h3Port, splitErr := net.SplitHostPort(cfg.ListenAddress)
				switch {
				case statusErr != nil:
					logger.Warn("Failed to query tsnet status for h3 bind, running HTTP/2 only on tailnet", "err", statusErr)
				case status == nil || status.Self == nil || len(status.Self.TailscaleIPs) == 0:
					logger.Warn("tsnet status returned no tailnet IPs for h3 bind, running HTTP/2 only on tailnet")
				case splitErr != nil || h3Port == "":
					logger.Warn("Failed to parse port from cfg.ListenAddress for tsnet h3 bind, running HTTP/2 only on tailnet", "addr", cfg.ListenAddress, "err", splitErr)
				default:
					listeners := make([]tsnetH3Listener, 0, len(status.Self.TailscaleIPs))
					for _, ip := range status.Self.TailscaleIPs {
						bindAddr := net.JoinHostPort(ip.String(), h3Port)
						pconn, err := tsnetServer.ListenPacket("udp", bindAddr)
						if err != nil {
							logger.Warn("Failed to bind tsnet UDP socket for h3, continuing with remaining IPs", "addr", bindAddr, "err", err)
							continue
						}
						h3srv := &http3.Server{
							Handler:   apiSrv.Handler(),
							TLSConfig: tsnetServer.HTTP3TLSConfig(),
						}
						listeners = append(listeners, tsnetH3Listener{srv: h3srv, conn: pconn})
						// Loop-local copies for the goroutine — without
						// these, every iteration's goroutine closes over
						// the same `h3srv` / `pconn` slot (Go ≤1.21
						// semantics; 1.22+ scopes per-iteration but
						// being explicit keeps the contract local).
						srvLocal, connLocal := h3srv, pconn
						go func() {
							if err := srvLocal.Serve(connLocal); err != nil &&
								!errors.Is(err, http.ErrServerClosed) &&
								!strings.Contains(err.Error(), "server closed") {
								logger.Error("h3 serve tsnet", "err", err)
							}
						}()
					}
					if len(listeners) > 0 {
						tsH3.Store(&tsnetH3State{listeners: listeners})
						logger.Info("tsnet HTTP/3 listeners bound", "count", len(listeners), "ipsReported", len(status.Self.TailscaleIPs), "port", h3Port)
					} else {
						logger.Warn("No tsnet HTTP/3 listeners bound on any tailnet IP, running HTTP/2 only on tailnet")
					}
				}
			}

			lis, err := tsnetServer.ListenTLS(cfg.ListenAddress)
			if err != nil {
				fmt.Fprintf(stderr, "tsnet: ListenTLS: %v\n", err)
				return
			}
			// Build a sibling http.Server pointing at the same handler
			// as httpSrv. Read/write timeout shape mirrors the LAN srv —
			// see the rationale comment there.
			srv := &http.Server{
				Handler: apiSrv.Handler(),
				// Same slow-loris defence as the LAN listener
				// (PR-C tightened ReadHeaderTimeout 10s → 5s).
				ReadHeaderTimeout: 5 * time.Second,
				ReadTimeout:       60 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
			tsnetHTTPSrv.Store(srv)
			tsnetServeErr <- srv.Serve(lis)
		}()
	}

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "server error: %v\n", err)
			return 1
		}
	case err := <-adminErr:
		// The admin console's bind can fail after main's first listen
		// succeeds (e.g. another process already owns :7789). Previously
		// this was swallowed via a fire-and-forget goroutine, leaving the
		// operator with a silently-broken admin URL — this case surfaces
		// it at startup, matches the signal the serveErr branch gives for
		// the main API listener.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "admin server: %v\n", err)
			// Tear down the main API listener cleanly before exit so
			// in-flight iOS requests get `http.ErrServerClosed` rather
			// than a socket RST mid-stream. Without this, a 404 on
			// :7789 binds → process-exit on 1 leaves the :7788 server
			// to be killed by the runtime's ungraceful goroutine halt.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
			return 1
		}
	case err := <-tsnetServeErr:
		// tsnet's secondary listener errored. LAN listener is still
		// running, but a dead tsnet listener means *.ts.net iOS
		// clients are silently failing — surface and exit so the
		// operator notices.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "tsnet listener: %v\n", err)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
			return 1
		}
	case <-ctx.Done():
		fmt.Fprintln(stdout, "\nShutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		// No defer cancel() here — we need to control the release timing manually
		// to avoid leaks during reboots while guaranteeing resource release.

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				fmt.Fprintf(stderr, "http shutdown: %v\n", err)
			}
		}()

		if lanH3Srv != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := lanH3Srv.Shutdown(shutdownCtx); err != nil {
					fmt.Fprintf(stderr, "lan h3 shutdown: %v\n", err)
				}
			}()
		}
		// tsnet HTTP/3 shutdown reads through atomic.Pointer — the
		// slot is stored from inside the tsnet startup goroutine and
		// may still be nil here if Start() never completed or every
		// per-IP bind failed. Each per-IP listener gets its own
		// goroutine so the WaitGroup releases as soon as the slowest
		// listener's Shutdown returns. Idempotent against the tsnet
		// defer's drain (http.Server.Shutdown returns ErrServerClosed
		// on a server already shut down).
		if state := tsH3.Load(); state != nil {
			for _, l := range state.listeners {
				lis := l // loop-local copy for the goroutine
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := lis.srv.Shutdown(shutdownCtx); err != nil {
						fmt.Fprintf(stderr, "tsnet h3 shutdown: %v\n", err)
					}
				}()
			}
		}

		wg.Wait()
		cancel() // Explicitly release context resources immediately

		if udpConn != nil {
			_ = udpConn.Close()
		}
		if state := tsH3.Load(); state != nil {
			for _, l := range state.listeners {
				_ = l.conn.Close()
			}
		}
	}
	return 0
}

func pairCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	name := fs.String("name", "", "client name (e.g. \"iPhone 15 Pro\")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "pair: --name is required")
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, configLoadFailedFormat, err)
		return 2
	}
	store, err := auth.OpenStore(filepath.Join(cfg.DataDir, tokensFileName))
	if err != nil {
		fmt.Fprintf(stderr, "open token store: %v\n", err)
		return 1
	}
	raw, tok, err := store.Mint(*name)
	if err != nil {
		fmt.Fprintf(stderr, "mint token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Paired successfully.")
	fmt.Fprintf(stdout, "  Device: %s\n", tok.Name)
	fmt.Fprintf(stdout, "  ID:     %s\n", tok.ID)
	fmt.Fprintf(stdout, "\nBearer token (copy this into the 1-bit iOS app; it won't be shown again):\n")
	fmt.Fprintf(stdout, "  %s\n", raw)
	return 0
}

func scanCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, configLoadFailedFormat, err)
		return 2
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()
	// Same artwork-cache directory the long-running serve mode uses.
	// Standalone `bridge scan` runs a one-shot full pass and exits;
	// without this, scanner-side local-artwork extraction would be a
	// no-op for the CLI scan path.
	artworkDir := filepath.Join(cfg.DataDir, "artwork")
	scanner := manifest.NewScanner(cfg.LibraryRoots, store, artworkDir)
	// Same missing_count grace period `bridge serve` wires. Without
	// this the CLI scan falls back to effectiveDeleteThreshold's
	// unwired default of 1 — immediate delete — so a single flaky
	// enumeration under a manual `bridge scan` reaps rows that the
	// documented 3-scan grace period exists to spare.
	scanner.SetDeleteThreshold(cfg.Scanner.DeleteAfterMissingScans)
	// Same duplicates policy `bridge serve` wires. Scan's success tail
	// ALWAYS runs the stamping pass, and an unwired scanner reports
	// FilterOff — so a single `bridge scan` under the default
	// `duplicates.filter: highest-quality` would clear every suppression
	// in the library, strict-advance indexed_at on each cleared row (a
	// full-library delta to every paired device), and rewrite the
	// dupe_summary document with `policy: "off"`. Exactly the failure
	// SetDupePolicy's "wire it BEFORE the scan starts" contract exists to
	// prevent, reached through the other entry point.
	//
	// The CLI has no live config holder — it loads once and exits — so a
	// closure over the loaded cfg is the whole story here.
	scanner.SetDupePolicy(func() dupes.Policy { return dupePolicyFromConfig(cfg) })

	fmt.Fprintf(stdout, "Scanning %v ...\n", cfg.LibraryRoots)
	start := time.Now()
	// Honor the signal-wired ctx from run() so Ctrl-C cancels the scan
	// (signal.NotifyContext intercepts the first SIGINT without killing
	// the process; a fresh context.Background() here would ignore it).
	n, err := scanner.Scan(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "scan error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Scan complete: %d tracks indexed in %s\n", n, time.Since(start).Round(time.Millisecond))
	return 0
}
