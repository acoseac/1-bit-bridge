package admin

import (
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// playerContentType is the BROWSER MIME table.
//
// Deliberately NOT dlna.defaultMIMEForExtension. That table is
// renderer-interop truth, chosen from field reports about hardware
// players, and it maps .flac to audio/x-flac — which is right for a
// DLNA renderer and WRONG for a browser: measured in Chromium,
// canPlayType("audio/flac") is "probably" while canPlayType(
// "audio/x-flac") is "". Reusing it would have made FLAC — 88% of the
// reference library — look unplayable in the web player.
//
// Two tables, two contracts. TestPlayerMIMEDivergesFromDLNA enumerates
// every delta so the divergence stays deliberate.
// octetStream is the "opaque bytes" answer: what the audio route
// announces for a format no browser decodes, and what a download always
// announces regardless of format.
const octetStream = "application/octet-stream"

func playerContentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".flac":
		return "audio/flac"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a", ".mp4", ".m4b":
		return "audio/mp4"
	case ".wav", ".wave":
		return "audio/wav"
	case ".aif", ".aiff":
		return "audio/aiff"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".opus":
		return "audio/ogg"
	default:
		// DSD included: no browser decodes a 1-bit stream, and
		// announcing audio/x-dsf would only invite an engine to try.
		return octetStream
	}
}

// variantIDPattern bounds the ?variant= parameter before it reaches a
// store lookup. Variant ids are minted as <kind>-v<schema>-<rate>-<bits>
// so the alphabet is narrow by construction; pinning it here keeps an
// arbitrary string out of a query and out of any future path join.
var variantIDPattern = regexp.MustCompile(`^[a-z]+-v[0-9]+-[0-9]+-[0-9]+$`)

// Playability kinds. The server reports what it KNOWS; the browser
// decides what it can decode.
const (
	// playUniversal — every current engine decodes this.
	playUniversal = "universal"
	// playEngineDependent — Safari yes, Chromium/Firefox usually not
	// (ALAC, AIFF). canPlayType is an unreliable oracle here (it
	// answers "" for codec strings an engine doesn't RECOGNISE even
	// when it can decode the file), so the client attempts playback
	// and treats MEDIA_ERR_SRC_NOT_SUPPORTED as the answer.
	playEngineDependent = "engine-dependent"
	// playNone — structurally impossible. DSD only.
	playNone = "none"
)

// The DSD answer is guarded TWICE — the isDSD flag here and the
// .dsf/.dff extension case below — because the two inputs come from
// different places and either can be absent. A routed row's tags are
// DIDL-derived and may carry no isDSD flag at all, while a mis-tagged
// local file may claim a codec its extension contradicts. Neither
// guard alone is sufficient, so a negative control has to break both
// to prove the test isn't vacuous (it does).
func playabilityKind(codec, ext string, isDSD bool) string {
	if isDSD {
		return playNone
	}
	switch strings.ToLower(ext) {
	case ".flac", ".mp3", ".ogg", ".oga", ".opus", ".wav", ".wave":
		return playUniversal
	case ".m4a", ".mp4", ".m4b":
		// AAC in MP4 is universal; ALAC in MP4 is Safari-only, and the
		// container extension can't tell them apart — the codec can.
		if strings.EqualFold(strings.TrimSpace(codec), "ALAC") {
			return playEngineDependent
		}
		return playUniversal
	case ".aif", ".aiff":
		return playEngineDependent
	case ".dsf", ".dff":
		return playNone
	default:
		return playEngineDependent
	}
}

// variantFreshFor mirrors api.serveVariant's freshness gate exactly: a
// sidecar is only offered when its recorded source mtime is within 2 s
// of the file on disk AND the recorded size matches exactly. Anything
// looser hands the player a sidecar of a file that has since been
// re-encoded.
const variantMTimeToleranceNS = 2_000_000_000

func variantFresh(v manifest.VariantRow, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	delta := v.SourceMTimeNS - info.ModTime().UnixNano()
	if delta < 0 {
		delta = -delta
	}
	return delta <= variantMTimeToleranceNS && v.SourceSize == info.Size()
}

// pickPlayableVariant chooses a FLAC sidecar to prefer when the source
// itself isn't universally playable.
//
// Prefers `optimized-` over `upscaled-`: optimized is the small
// CarPlay-floor copy, while an upscaled sidecar can be 176.4/24 — a 5x
// bandwidth hit for a browser that will resample to the device rate
// anyway. Don't "fix" this toward higher fidelity; the web player is
// explicitly not a bit-exact path.
func pickPlayableVariant(rows []manifest.VariantRow, info os.FileInfo) *manifest.VariantRow {
	var upscaled *manifest.VariantRow
	for i := range rows {
		v := rows[i]
		if !strings.EqualFold(v.Format, "flac") || !variantFresh(v, info) {
			continue
		}
		switch {
		case strings.HasPrefix(v.VariantID, "optimized-"):
			return &rows[i]
		case strings.HasPrefix(v.VariantID, "upscaled-") && upscaled == nil:
			upscaled = &rows[i]
		}
	}
	return upscaled
}

// hydrateTracks turns a catalog path list into wire rows, resolving
// playability per track.
//
// Order follows the catalog's TrackPaths (already sorted by resolved
// disc, track, path), not the store's return order — the store filters
// by a set and makes no ordering promise.
func (s *Server) hydrateTracks(r *http.Request, cat *librarycat.Catalog,
	paths []string) ([]playerTrackDTO, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	ctx := r.Context()
	rows, err := s.deps.Manifest.CatalogTrackRowsForPaths(ctx, paths)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]manifest.CatalogTrackRow, len(rows))
	for _, row := range rows {
		byPath[row.Path] = row
	}
	variants, err := s.deps.Manifest.VariantsForPaths(ctx, paths)
	if err != nil {
		// A variant-table fault must not fail the album: without it
		// every track simply reports no variant, which is the same
		// answer a library with no variants gives.
		logger.Warn("player: variant lookup", "err", err)
		variants = nil
	}

	// Hoisted: SoxCanDecode is the 30s-TTL toolchain probe behind a
	// mutex, and its answer is fixed for the life of this request. A
	// playlist can carry tens of thousands of paths, so calling it
	// per track is a lock per track for one stable value. Matches
	// browseTrackRow's call site, which already takes it as an
	// argument. (Gemini on PR #814)
	canDecode := s.soxCanDecode()

	out := make([]playerTrackDTO, 0, len(paths))
	for _, p := range paths {
		row, ok := byPath[p]
		if !ok {
			// Deleted or newly suppressed since the snapshot. Skipping
			// is correct: the alternative is a row the player can't
			// play and the phone doesn't have.
			continue
		}
		albumID, _ := cat.AlbumIDForPath(p)
		ext := path.Ext(p)
		dto := playerTrackDTO{
			Path: p, Title: row.Title, Artist: row.Artist, AlbumID: albumID,
			Disc: row.Disc, Track: row.Track, Duration: row.Duration,
			SizeBytes: row.Size, Codec: row.Codec, RateHz: row.SampleRate,
			Bits: row.BitsPerSample, IsDSD: row.IsDSD,
			Routed: row.RoutedUDN != "", SourceID: sourceIDForRow(row.RoutedUDN),
			Play: playerPlayabilityDTO{
				Kind:         playabilityKind(row.Codec, ext, row.IsDSD),
				ContentType:  playerContentType(ext),
				Downloadable: true,
			},
		}
		if dto.Play.Kind != playUniversal {
			s.attachVariant(&dto.Play, p, variants[p])
		}
		// Independent of playback: what this track HAS, and whether it
		// could ever have more. A universally-playable FLAC needs no
		// substitute sidecar but may well own two.
		dto.Variants = describeVariants(variants[p], row)
		dto.VariantSkip = fundamentalSkipReason(
			row.RoutedUDN != "", row.IsDSD, row.Codec,
			floatOrNil(row.SampleRate), intOrNil(row.BitsPerSample),
			p, canDecode)
		out = append(out, dto)
	}
	return out, nil
}

// ---- The byte routes ----

// apiPlayerAudio handles GET|HEAD /api/player/audio?path=&variant=.
//
// /v1/download is not reusable for two independent reasons: it is
// bearer-authed (an <audio src> cannot set a header), and it
// deliberately announces application/octet-stream for every format — a
// contract with iOS that stays put. So this is a loopback twin with
// browser MIME semantics, built on the same primitives.
//
// Order of operations mirrors api.serveFile: routing is checked BEFORE
// any filesystem touch, and a routing-lookup ERROR is a 500, not a 404.
// A false 404 is indistinguishable from "track deleted" and teaches the
// UI to give up on a track that is fine.
func (s *Server) apiPlayerAudio(w http.ResponseWriter, r *http.Request) {
	s.servePlayerBytes(w, r, false)
}

// apiPlayerDownload is the same resolution with attachment headers —
// the "not playable in this browser, download instead" affordance.
func (s *Server) apiPlayerDownload(w http.ResponseWriter, r *http.Request) {
	s.servePlayerBytes(w, r, true)
}

func (s *Server) servePlayerBytes(w http.ResponseWriter, r *http.Request, download bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or HEAD only")
		return
	}
	rel, ok := normaliseBrowsePath(safeQuery(r).Get("path"))
	if !ok || rel == "" {
		writeError(w, http.StatusBadRequest, "bad_path", "invalid path")
		return
	}
	variantID := safeQuery(r).Get("variant")
	if variantID != "" && !variantIDPattern.MatchString(variantID) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid variant id")
		return
	}

	// The updater refuses to swap the binary mid-stream while a session
	// is open — /v1/serveFile takes the same lock for the same reason.
	// Without it a self-update kills the operator's playback mid-track.
	if s.deps.BeginPlaybackSession != nil {
		end := s.deps.BeginPlaybackSession()
		defer end()
	}

	if s.deps.Manifest != nil && variantID == "" {
		rt, err := s.deps.Manifest.GetUPnPRouting(r.Context(), rel)
		switch {
		case err != nil:
			logger.Error("player audio: routing lookup", "path", rel, "err", err)
			writeError(w, http.StatusInternalServerError, "internal", "routing lookup failed")
			return
		case rt != nil:
			s.serveRoutedAudio(w, r, rt, rel, download)
			return
		}
	}

	abs, info, resolveErr := s.deps.Resolver.ResolveChecked(rel)
	if resolveErr != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such track")
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_path", "not a file")
		return
	}

	servePath, serveInfo := abs, info
	if variantID != "" {
		v, err := s.deps.Manifest.LookupVariant(r.Context(), rel, variantID)
		if err != nil || v == nil {
			writeError(w, http.StatusNotFound, "not_found", "no such variant")
			return
		}
		if !variantFresh(*v, info) {
			// 410, not 404: "was here, fall back to the source" — the
			// same contract api.serveVariant and the DLNA file handler
			// use, and the client's cue to retry without ?variant=.
			writeError(w, http.StatusGone, "variant_stale",
				"the variant is stale; request the source instead")
			return
		}
		// Confine the sidecar to the variants directory before opening
		// it. The path is server-authored — the transcode pool wrote it,
		// and the request only SELECTS a row by a validated (path,
		// variantID) key — so this is not reachable from user input
		// today. It is enforced rather than argued because the check is
		// two lines and turns "no caller can do that" into "no caller
		// CAN do that": a hand-edited row, a restored DB from a host
		// with a different variants dir, or a future writer that stores
		// an absolute path from elsewhere would otherwise all end in an
		// os.Open of whatever the row says.
		cfg := s.deps.CfgHolder.Load()
		variantsDir := cfg.Upscale.EffectiveVariantsDir(cfg.DataDir)
		if fsutil.IsUnderAny(v.SidecarPath, []string{variantsDir}) == "" {
			logger.Error("player audio: sidecar outside the variants dir",
				"variantID", v.VariantID, "variantsDir", variantsDir)
			writeError(w, http.StatusGone, "variant_missing_on_disk",
				"the variant row exists but its sidecar does not")
			return
		}
		vi, err := os.Stat(v.SidecarPath)
		if err != nil {
			writeError(w, http.StatusGone, "variant_missing_on_disk",
				"the variant row exists but its sidecar does not")
			return
		}
		servePath, serveInfo = v.SidecarPath, vi
	}

	f, err := os.Open(servePath)
	if err != nil {
		logger.Error("player audio: open", "path", servePath, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not open the file")
		return
	}
	defer f.Close()

	if download {
		setAttachmentHeaders(w, path.Base(rel))
	} else {
		w.Header().Set("Content-Type", playerContentType(path.Ext(servePath)))
		// private, not no-store: audio bytes are re-requested on every
		// scrub, and no-store would re-download the file each time.
		// ServeContent's Last-Modified handles revalidation.
		w.Header().Set("Cache-Control", "private, max-age=3600")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, serveInfo.Name(), serveInfo.ModTime(), f)
}

// setAttachmentHeaders writes a Content-Disposition that survives
// non-ASCII filenames. RFC 5987's filename* is not optional here: this
// library is full of them, and a bare filename= would hand the browser
// mojibake or nothing.
func setAttachmentHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", octetStream)
	w.Header().Set("Cache-Control", "private, max-age=0")
	w.Header().Set("Content-Disposition",
		"attachment; filename=\""+asciiFallbackName(name)+"\"; filename*=UTF-8''"+
			url.PathEscape(name))
}

// asciiFallbackName strips a filename down to something every client
// can render, for the legacy filename= parameter. Quotes and
// backslashes must go or they terminate the quoted-string early.
func asciiFallbackName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f || r == '"' || r == '\\':
			b.WriteByte('_')
		case r > 0x7f:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "download"
	}
	return b.String()
}

// serveRoutedAudio proxies an upstream UPnP MediaServer's bytes.
//
// Reuses the same upnpproxy the /v1 download path and the DLNA file
// handler use, so all three observe one SSDP cache. Two things this
// route must do that they don't:
//
//  1. Override Content-Type. upnpproxy relays the upstream's headers
//     with Header().Add, so pre-setting a value yields TWO
//     ("audio/flac, audio/x-flac"). The upstream sends the DLNA
//     spelling, which browsers don't claim. The override happens in a
//     wrapper at WriteHeader time; upnpproxy itself is NOT changed —
//     its verbatim relay is the bit-exact contract for iOS and for
//     hardware renderers.
//
//  2. Report whether ranges survived. Embedded DLNA servers routinely
//     ignore Range and answer 200 with the whole stream, which breaks
//     browser scrubbing. The wrapper records what came back so the
//     client can disable the scrubber rather than bind it to a
//     duration it cannot honour — a scrubber that lies is worse than
//     no scrubber.
func (s *Server) serveRoutedAudio(w http.ResponseWriter, r *http.Request,
	rt *manifest.UPnPRouting, rel string, download bool) {
	if s.deps.ProxyUPnPAudio == nil {
		writeError(w, http.StatusServiceUnavailable, "upstream_unavailable",
			"this track lives on a UPnP upstream and the proxy is not wired")
		return
	}
	ct := playerContentType(path.Ext(rel))
	wrapped := &routedAudioWriter{ResponseWriter: w, contentType: ct}
	if download {
		wrapped.disposition = "attachment; filename=\"" + asciiFallbackName(path.Base(rel)) +
			"\"; filename*=UTF-8''" + url.PathEscape(path.Base(rel))
		wrapped.contentType = octetStream
	}
	if err := s.deps.ProxyUPnPAudio(wrapped, r, rt); err != nil {
		// Only safe to write an error if nothing went out yet.
		if !wrapped.wroteHeader {
			writeError(w, err.Status, err.Code, err.Message)
		}
	}
}

// routedAudioWriter rewrites Content-Type (and, for a download,
// Content-Disposition) on the way out, and records whether the upstream
// honoured the Range request.
type routedAudioWriter struct {
	http.ResponseWriter
	contentType  string
	disposition  string
	wroteHeader  bool
	rangeHonored bool
}

func (w *routedAudioWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		h := w.Header()
		h.Del("Content-Type")
		h.Set("Content-Type", w.contentType)
		if w.disposition != "" {
			h.Del("Content-Disposition")
			h.Set("Content-Disposition", w.disposition)
		}
		w.rangeHonored = status == http.StatusPartialContent
		// Tell the client what it can rely on. An upstream that ignored
		// Range leaves the browser unable to seek; saying so lets the
		// player disable the scrubber instead of appearing to hang.
		if !w.rangeHonored {
			h.Set("X-Bridge-Upstream-Ranges", "unsupported")
			h.Del("Accept-Ranges")
			h.Set("Accept-Ranges", "none")
		}
		h.Set("X-Content-Type-Options", "nosniff")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *routedAudioWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// attachVariant fills in the fresh-FLAC fallback for a track whose own
// format this browser may not decode. No-op when the track has no
// variants, which is the common case.
//
// Extracted from hydrateTracks to keep that function under the
// cognitive-complexity budget, and because the "resolve, stat, pick"
// sequence reads better named than inline.
func (s *Server) attachVariant(play *playerPlayabilityDTO, relPath string, rows []manifest.VariantRow) {
	if len(rows) == 0 {
		return
	}
	// Only resolve the source when a variant might actually be offered —
	// ResolveChecked stats, and doing that per track on every album row
	// would put filesystem latency on the hot path for no gain. A routed
	// track never resolves, and never has a sidecar, so it falls through
	// with no variant.
	var info os.FileInfo
	if _, fi, err := s.deps.Resolver.ResolveChecked(relPath); err == nil {
		info = fi
	}
	v := pickPlayableVariant(rows, info)
	if v == nil {
		return
	}
	play.VariantID = v.VariantID
	play.VariantContentType = playerContentType(".flac")
	play.VariantRateHz = v.SampleRate
	play.VariantBits = v.BitsPerSample
}

// describeVariants turns the stored sidecar rows for one track into the
// wire shape, newest-relevant kind first.
//
// Ordering is by KIND, not by whatever the store returned: the two
// chips always appear in the same places, so a reader scanning a track
// list is comparing like with like down the column.
func describeVariants(rows []manifest.VariantRow, row manifest.CatalogTrackRow) []playerVariantDTO {
	if len(rows) == 0 {
		return nil
	}
	out := make([]playerVariantDTO, 0, len(rows))
	for _, kind := range []struct{ prefix, name string }{
		{manifest.VariantKindPrefixUpscaled + "-", variantKindUpscale},
		{manifest.VariantKindPrefixOptimized + "-", variantKindOptimize},
	} {
		for _, v := range rows {
			if !strings.HasPrefix(v.VariantID, kind.prefix) {
				continue
			}
			out = append(out, playerVariantDTO{
				Kind:      kind.name,
				VariantID: v.VariantID,
				RateHz:    v.SampleRate,
				Bits:      v.BitsPerSample,
				SizeBytes: v.SizeBytes,
				// The scanner's record, not a live stat — see
				// playerVariantDTO.
				Fresh: v.SourceMTimeNS == row.MTimeNS && v.SourceSize == row.Size,
			})
		}
	}
	return out
}

// The kind vocabulary the batch / delete endpoints take. The store
// spells the same distinction as an id PREFIX (`upscaled-` /
// `optimized-`); keeping the two spellings apart in one place is what
// stops a UI sending "upscaled" to an endpoint that wants "upscale".
const (
	variantKindUpscale  = "upscale"
	variantKindOptimize = "optimize"
)

// floatOrNil / intOrNil adapt the catalog row's plain zero-means-absent
// integers to fundamentalSkipReason's pointer parameters, which come
// from the browse rows where the distinction between "no tag" and "zero"
// survives. Zero reaches the skip classifier as absent either way, which
// is the answer it wants: a track with no readable geometry is
// "unknown_format".
func floatOrNil(v int) *float64 {
	if v <= 0 {
		return nil
	}
	f := float64(v)
	return &f
}

func intOrNil(v int) *int {
	if v <= 0 {
		return nil
	}
	return &v
}

// sourceIDForRow maps a track's routing key to its facet id, or "" for a
// filesystem track.
//
// Empty rather than LocalSourceID so the field stays absent on every row
// of a pure-filesystem library — the client reads absence as local. The
// argument is upnp_track_routing.server_udn, which is the ingest's stable
// key and not the device UDN (see admin.UPnPSource).
func sourceIDForRow(routingKey string) string {
	if routingKey == "" {
		return ""
	}
	return librarycat.SourceID(routingKey)
}
