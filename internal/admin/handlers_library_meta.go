// Admin Library Inspector metadata layer (Atlas surfaces on tiles +
// the About panel card). Four surfaces:
//
//   - GET /api/library/enrichment?path=<parent>
//     One batched response with per-CHILD-FOLDER artwork/booklet refs
//     for tile decoration. Grouped in Go over a streamed indexed
//     path-LIKE scan (no SUBSTR/INSTR GROUP BY in SQLite); cached
//     60s + single-flighted per path (the root walk is a full-table
//     json_extract — composition cost class).
//
//   - GET /api/library/enrichment/detail?path=<folder>
//     The About card: dominant artist bio + album description from
//     the artist_atlas / release_atlas overlays (with the mandatory
//     "Read more on <source>" attribution), booklet rows, artist-
//     image presence. Fetched lazily when the card expands.
//
//   - POST /api/library/enrichment/retry {path}
//     Folder-scoped "retry missing metadata": prefix-scoped
//     enriched_at resets (covers), artist-image re-queues, booklet
//     re-checks + fetch nudges, and the (inherently library-wide)
//     harvest re-submit. Rate-guarded 60s PER PATH.
//
//   - GET /api/library/{artwork|artist-image|booklet}/{mbid}
//     Loopback byte routes over the same cache dirs the /v1 twins
//     serve — the admin console can't call /v1 (bearer-authed), and
//     everything on this mux is loopback-gated by boundaryMiddleware,
//     so no auth layer is added here (CLAUDE.md admin posture).
//
// Both JSON endpoints send Cache-Control: no-store — the server-side
// TTL cache is the intended cache; a browser disk-cache hit after a
// retry would resurrect the stale "missing" state.
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// adminMBIDPattern / adminArtworkMBIDPattern mirror the /v1 twins in
// internal/api/artwork.go (mbidPattern :37 / artworkMBIDPattern :47 —
// keep in lockstep). The bounded alphabets ([0-9a-fA-F-] / plus the
// lowercase local-<sha256> arm) are the traversal defense: no path
// separator or dot can survive the match, so the values are safe to
// join into cache-file paths. Never serve a byte off disk from an id
// that hasn't passed one of these.
var (
	adminMBIDPattern        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	adminArtworkMBIDPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|local-[0-9a-f]{64})$`)
)

// libMetaCacheTTL matches the composition/enrichment-meta snapshot
// TTLs — the refs walk is the same json_extract cost class.
const libMetaCacheTTL = 60 * time.Second

// libMetaCacheMaxEntries bounds the per-path refs cache. Navigation
// is depth-first-ish in practice; 64 paths ≈ minutes of browsing.
const libMetaCacheMaxEntries = 64

// libraryMetaRetryMinInterval is the per-path retry guard window.
const libraryMetaRetryMinInterval = 60 * time.Second

// bookletNudgeCap bounds fetch nudges per retry to the harvest
// client's priority-channel buffer — extra nudges would be dropped
// anyway (rows still drain via BookletsToFetch in order, unless they
// have exhausted maxBookletAttempts, in which case re-running this
// retry is what re-tries them).
const bookletNudgeCap = 32

type libraryChildRef struct {
	// Kind is a rendering heuristic: "album" (one distinct release
	// under the child → show its cover), "artist" (one distinct
	// artist, several releases → show the portrait), "mixed"
	// (anything else → icon only unless a cover ref exists).
	Kind           string `json:"kind"`
	ArtworkMBID    string `json:"artworkMBID,omitempty"`
	ArtworkVersion string `json:"artworkVersion,omitempty"`
	ArtistMBID     string `json:"artistMBID,omitempty"`
	HasBooklet     bool   `json:"hasBooklet,omitempty"`
}

type libraryMetaRefsResponse struct {
	Path            string                     `json:"path"`
	AtlasEnabled    bool                       `json:"atlasEnabled"`
	BookletsEnabled bool                       `json:"bookletsEnabled"`
	Children        map[string]libraryChildRef `json:"children"`
}

type libMetaEntry[T any] struct {
	resp T
	at   time.Time
}

// libMetaCache is a bounded, TTL'd, single-flighted per-path response
// cache. The zero value is ready to use (the map is lazily created).
//
// Both walks it fronts (refs + detail) are json_extract subtree scans
// — full-table at the library root, the AtlasMetaBreakdownCounts cost
// class — so serving one uncached is the bug this type exists to make
// unrepresentable: `serve` owns the whole ceremony (TTL check →
// single-flight → detached-but-bounded compute → store → write), and a
// handler physically cannot skip a step by forgetting to write it out.
//
// Each instantiation owns its OWN singleflight.Group. That's
// structural, not stylistic: one Group shared across two response
// types would let a concurrent refs+detail request for the same path
// collapse into a single flight and hand the joiner the wrong struct.
// Separate Groups make that unrepresentable, and drop the key
// namespacing a shared Group would need.
type libMetaCache[T any] struct {
	mu sync.Mutex
	m  map[string]libMetaEntry[T]
	sf singleflight.Group
}

// get returns the cached response for key when it's within the TTL.
func (c *libMetaCache[T]) get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.m[key]; ok && time.Since(e.at) < libMetaCacheTTL {
		return e.resp, true
	}
	var zero T
	return zero, false
}

// put stores resp under key, bounding the map at
// libMetaCacheMaxEntries.
func (c *libMetaCache[T]) put(key string, resp T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]libMetaEntry[T])
	}
	// Overwriting an existing key can't grow the map, so it must not
	// evict: a redundant put (two callers racing the same miss) would
	// otherwise drop an unrelated live entry for nothing.
	_, replacing := c.m[key]
	if !replacing && len(c.m) >= libMetaCacheMaxEntries {
		for k, e := range c.m {
			if time.Since(e.at) >= libMetaCacheTTL {
				delete(c.m, k)
			}
		}
		// Still full of fresh entries (64 distinct paths inside one
		// TTL). Evict the entry CLOSEST TO EXPIRY rather than resetting
		// the map: a reset drops all 64, so an operator browsing fast
		// enough to fill the cache would re-walk all 64 paths, refill,
		// and reset again — the cache doing least exactly when it's
		// needed most. The scan is O(libMetaCacheMaxEntries) on a
		// branch that only fires under that sustained load.
		//
		// `at` is insert time and entries expire on it (get() never
		// refreshes it), so the smallest `at` is the entry with the
		// least remaining useful life — the cheapest one to lose. That
		// makes this expiry-ordered, not LRU; for a TTL cache the two
		// coincide on what matters.
		for len(c.m) >= libMetaCacheMaxEntries {
			oldestKey, oldestAt, found := "", time.Time{}, false
			for k, e := range c.m {
				if !found || e.at.Before(oldestAt) {
					oldestKey, oldestAt, found = k, e.at, true
				}
			}
			if !found {
				break // unreachable under c.mu; guards the loop regardless
			}
			delete(c.m, oldestKey)
		}
	}
	c.m[key] = libMetaEntry[T]{resp: resp, at: time.Now()}
}

// invalidateUnder drops every entry whose path overlaps prefix.
//
// Deliberately NOT fenced against in-flight computations (no epoch
// counter in the flight keys). A walk that started before a retry can
// land after this sweep and re-populate the entry — but it stores
// IDENTICAL data, because the retry writes nothing either cached
// response reads:
//
//   - ResetEnrichedMisses* / ResetEnrichedByArtistMBIDs write
//     tracks.enriched_at; the walks read tracks.tags_json.
//   - HarvestForceSubmit only zeroes an in-memory submit stamp.
//   - ResetBookletChecks writes booklets.check_attempts; the About
//     card reads Available / Fetched.
//   - BookletNudge queues a fetch — Fetched flips later, when the
//     download lands.
//
// The retry RE-QUEUES work; it doesn't perform it. The data it leads
// to changes minutes later at MB/CAA/Deezer pacing, far outside any
// flight window. The sweep exists so the next open isn't served a
// pre-retry entry from the remaining TTL, not to order against flights.
//
// If a future retry facet ever writes something a walk reads
// synchronously, that reasoning lapses and this needs a real fence.
func (c *libMetaCache[T]) invalidateUnder(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.m {
		if pathOverlaps(p, prefix) {
			delete(c.m, p)
		}
	}
}

// serve answers one request from the cache, computing + storing on a
// miss. compute runs UNLOCKED inside the flight, so a slow walk on one
// path can't block cache lookups for another.
//
// The compute context is detached from the request
// (context.WithoutCancel) because the result is shared with joined
// callers — the first caller hanging up must not poison everyone
// else's response (reachability-probe precedent, PR #373). It's then
// re-bounded with a timeout: detaching without substituting a deadline
// would leave a pathological walk with no ceiling at all. Order
// matters — WithoutCancel first, or WithTimeout's deadline is stripped
// right back off.
//
// Errors are NOT cached: a failed walk must not park a degraded
// response for the whole TTL. (A best-effort facet that degraded but
// still returned a response IS cached — that's the TTL contract, not a
// leak.)
func (c *libMetaCache[T]) serve(w http.ResponseWriter, r *http.Request,
	key, errCode string, compute func(context.Context) (T, error)) {
	resp, err := c.fetch(r, key, compute)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errCode, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// fetch is serve's value-returning half: same TTL + singleflight +
// re-check-inside-the-flight discipline, but it hands the response back
// instead of writing it. Callers that need to post-process a cached
// snapshot before responding (filtering it by a query parameter that
// must NOT become part of the cache key, or the expensive walk would run
// once per parameter value) use this directly.
func (c *libMetaCache[T]) fetch(r *http.Request, key string,
	compute func(context.Context) (T, error)) (T, error) {
	if resp, ok := c.get(key); ok {
		return resp, nil
	}
	v, err, _ := c.sf.Do(key, func() (any, error) {
		// Re-check inside the flight: a caller that missed above can be
		// delayed past a prior flight's completion and would otherwise
		// repeat the whole subtree walk against an already-fresh entry
		// (the getEnrichmentMetaSnapshot precedent). Concurrent callers
		// arriving DURING a flight are already deduped by singleflight;
		// this covers the ones that arrive just after one ends.
		if resp, ok := c.get(key); ok {
			return resp, nil
		}
		ctx, cancel := context.WithTimeout(
			context.WithoutCancel(r.Context()), enrichmentDBTimeout)
		defer cancel()
		resp, err := compute(ctx)
		if err != nil {
			return nil, err
		}
		c.put(key, resp)
		return resp, nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return v.(T), nil
}

// pathOverlaps reports whether two normalised browse paths cover
// overlapping subtrees: identical, or either an ancestor of the other.
//
// "" is the library root and overlaps everything — both the "retry at
// the root" case (b == "") and a cached root entry (a == ""). Neither
// arm is redundant with the HasPrefix pair below: normaliseBrowsePath
// guarantees no leading slash, so HasPrefix(x, ""+"/") is dead. That
// same guarantee (path.Clean'd, forward slashes, no traversal) is what
// makes the "/" boundary check sound — it's why this can't false-match
// "A/Bee" against "A/B".
func pathOverlaps(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// libMetaInvalidateUnder drops both per-path caches for the subtree.
//
// Each cache takes its own lock, so this is two acquisitions rather
// than one. Nothing needs them to be atomic together: they're
// independent best-effort TTL caches, so a request landing between the
// two sweeps just gets one fresh + one stale, and the next open (a
// second later) gets both fresh.
func (s *Server) libMetaInvalidateUnder(prefix string) {
	s.libMetaRefs.invalidateUnder(prefix)
	s.libMetaDetail.invalidateUnder(prefix)
	// The misses snapshot reads the same three MBID fields a retry is
	// about to re-populate, so it goes stale on exactly the same events.
	s.libMetaMisses.invalidateUnder(prefix)
}

// apiLibraryEnrichmentRefs handles GET /api/library/enrichment?path=.
func (s *Server) apiLibraryEnrichmentRefs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	normalised, ok := normaliseBrowsePath(r.URL.Query().Get("path"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
		return
	}
	s.libMetaRefs.serve(w, r, normalised, "meta-refs",
		func(ctx context.Context) (libraryMetaRefsResponse, error) {
			return s.computeLibraryMetaRefs(ctx, normalised)
		})
}

// computeLibraryMetaRefs walks the subtree once and groups the MBID
// refs by immediate child folder. Loose tracks (files directly in the
// parent — no further '/' in the remainder) are EXCLUDED: they render
// as track tiles, not folder tiles, and their metadata surfaces via
// the CURRENT folder's detail endpoint.
func (s *Server) computeLibraryMetaRefs(ctx context.Context, parent string) (libraryMetaRefsResponse, error) {
	cfg := s.deps.CfgHolder.Load()
	resp := libraryMetaRefsResponse{
		Path:            parent,
		AtlasEnabled:    cfg != nil && cfg.Atlas.Enabled,
		BookletsEnabled: s.deps.BookletPath != nil,
		Children:        map[string]libraryChildRef{},
	}

	type agg struct {
		artists  map[string]struct{}
		releases map[string]struct{}
		covers   map[string]int    // artworkMBID → votes
		coverVer map[string]string // artworkMBID → artwork_version
	}
	children := map[string]*agg{}
	prefixLen := 0
	if parent != "" {
		prefixLen = len(parent) + 1 // + "/"
	}
	err := s.deps.Manifest.StreamTrackMetaRefsUnderPrefix(ctx, parent, func(ref manifest.TrackMetaRef) error {
		rest := ref.Path[prefixLen:]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			return nil // loose track directly in parent — not a folder tile
		}
		name := rest[:slash]
		a := children[name]
		if a == nil {
			a = &agg{
				artists:  map[string]struct{}{},
				releases: map[string]struct{}{},
				covers:   map[string]int{},
				coverVer: map[string]string{},
			}
			children[name] = a
		}
		if ref.ArtistMBID != "" {
			a.artists[ref.ArtistMBID] = struct{}{}
		}
		if ref.ReleaseMBID != "" {
			a.releases[ref.ReleaseMBID] = struct{}{}
		}
		if ref.ArtworkMBID != "" {
			a.covers[ref.ArtworkMBID]++
			if ref.ArtworkVersion != "" {
				a.coverVer[ref.ArtworkMBID] = ref.ArtworkVersion
			}
		}
		return nil
	})
	if err != nil {
		return libraryMetaRefsResponse{}, err
	}

	// One booklet probe over the union of every child's releases.
	releaseUnion := map[string]struct{}{}
	for _, a := range children {
		for m := range a.releases {
			releaseUnion[m] = struct{}{}
		}
	}
	var bookletStates map[string]manifest.BookletState
	if resp.BookletsEnabled && len(releaseUnion) > 0 {
		mbids := make([]string, 0, len(releaseUnion))
		for m := range releaseUnion {
			mbids = append(mbids, m)
		}
		bookletStates, err = s.deps.Manifest.BookletStatesIn(ctx, mbids)
		if err != nil {
			// Degrade: tiles just lose the booklet chip this pass.
			logger.Warn("meta refs: booklet states unavailable", "err", err)
			bookletStates = nil
		}
	}

	for name, a := range children {
		ref := libraryChildRef{}
		switch {
		case len(a.releases) == 1:
			ref.Kind = "album"
		case len(a.artists) == 1:
			ref.Kind = "artist"
		default:
			ref.Kind = "mixed"
		}
		if len(a.artists) == 1 {
			for m := range a.artists {
				ref.ArtistMBID = m
			}
		}
		// Representative cover: most-voted artworkMBID, preferring
		// UUID (enricher/premium) refs over local-<sha> sentinels,
		// with a deterministic lexicographic tie-break. local- refs
		// are still perfectly servable — they're only outranked.
		ref.ArtworkMBID = pickCoverRef(a.covers)
		ref.ArtworkVersion = a.coverVer[ref.ArtworkMBID]
		for m := range a.releases {
			if st, ok := bookletStates[m]; ok && st.Available {
				ref.HasBooklet = true
				break
			}
		}
		resp.Children[name] = ref
	}
	return resp, nil
}

// pickCoverRef picks the representative artworkMBID from a vote map:
// highest vote count wins; UUID refs outrank local-<sha> sentinels at
// equal votes; final tie-break is lexicographic for determinism.
func pickCoverRef(votes map[string]int) string {
	best := ""
	bestVotes := -1
	bestLocal := false
	for m, n := range votes {
		isLocal := strings.HasPrefix(m, "local-")
		switch {
		case n > bestVotes:
		case n == bestVotes && bestLocal && !isLocal:
		case n == bestVotes && bestLocal == isLocal && m < best:
		default:
			continue
		}
		best, bestVotes, bestLocal = m, n, isLocal
	}
	return best
}

// --- About-card detail ---

type aboutArtistDTO struct {
	MBID string `json:"mbid"`
	// State: "found" (text to show) / "missing" (tombstone, or found
	// with empty text — nothing the UI can render) / "unchecked"
	// (Atlas never asked — the harvest will fill it in).
	State      string   `json:"state"`
	BioSummary string   `json:"bioSummary,omitempty"`
	Bio        string   `json:"bio,omitempty"`
	Genres     []string `json:"genres,omitempty"`
	// Source/SourceURL attribute the bio for the mandatory
	// "Read more on <source>" link (CC-BY-SA / ToS compliance —
	// render whenever the text renders).
	Source    string `json:"source,omitempty"`
	SourceURL string `json:"sourceUrl,omitempty"`
}

type aboutReleaseDTO struct {
	MBID        string   `json:"mbid"`
	State       string   `json:"state"`
	Description string   `json:"description,omitempty"`
	RecordLabel string   `json:"recordLabel,omitempty"`
	Genres      []string `json:"genres,omitempty"`
	Source      string   `json:"source,omitempty"`
	SourceURL   string   `json:"sourceUrl,omitempty"`
}

type aboutBookletDTO struct {
	MBID  string `json:"mbid"`
	State string `json:"state"` // "cached" (PDF on disk) | "pending" (available, download queued)
}

type libraryMetaDetailResponse struct {
	Path            string            `json:"path"`
	AtlasEnabled    bool              `json:"atlasEnabled"`
	BookletsEnabled bool              `json:"bookletsEnabled"`
	Artist          *aboutArtistDTO   `json:"artist,omitempty"`
	Release         *aboutReleaseDTO  `json:"release,omitempty"`
	Booklets        []aboutBookletDTO `json:"booklets,omitempty"`
	HasArtistImage  bool              `json:"hasArtistImage"`
	CoverMBID       string            `json:"coverMBID,omitempty"`
	CoverVersion    string            `json:"coverVersion,omitempty"`
	// MoreArtists / MoreReleases count the distinct MBIDs BEYOND the
	// dominant one shown — the UI renders "and N more…".
	MoreArtists  int `json:"moreArtists,omitempty"`
	MoreReleases int `json:"moreReleases,omitempty"`
}

// apiLibraryEnrichmentDetail handles GET /api/library/enrichment/detail?path=.
func (s *Server) apiLibraryEnrichmentDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	normalised, ok := normaliseBrowsePath(r.URL.Query().Get("path"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
		return
	}
	s.libMetaDetail.serve(w, r, normalised, "meta-detail",
		func(ctx context.Context) (libraryMetaDetailResponse, error) {
			return s.computeLibraryMetaDetail(ctx, normalised)
		})
}

// computeLibraryMetaDetail builds the About card for one folder: the
// dominant artist's bio + the dominant release's description from the
// Atlas overlays, booklet rows, artist-image presence.
//
// The Atlas/booklet facets are best-effort: a read failure logs and
// leaves that facet at its "unchecked" default rather than failing the
// whole card. Only the subtree walk itself is fatal.
func (s *Server) computeLibraryMetaDetail(ctx context.Context, normalised string) (libraryMetaDetailResponse, error) {
	cfg := s.deps.CfgHolder.Load()
	resp := libraryMetaDetailResponse{
		Path:            normalised,
		AtlasEnabled:    cfg != nil && cfg.Atlas.Enabled,
		BookletsEnabled: s.deps.BookletPath != nil,
	}

	// One scoped walk: dominant artist (most tracks), dominant release,
	// representative cover, and the distinct-release set for booklets.
	artistVotes := map[string]int{}
	releaseVotes := map[string]int{}
	coverVotes := map[string]int{}
	coverVer := map[string]string{}
	err := s.deps.Manifest.StreamTrackMetaRefsUnderPrefix(ctx, normalised, func(ref manifest.TrackMetaRef) error {
		if ref.ArtistMBID != "" {
			artistVotes[ref.ArtistMBID]++
		}
		if ref.ReleaseMBID != "" {
			releaseVotes[ref.ReleaseMBID]++
		}
		if ref.ArtworkMBID != "" {
			coverVotes[ref.ArtworkMBID]++
			if ref.ArtworkVersion != "" {
				coverVer[ref.ArtworkMBID] = ref.ArtworkVersion
			}
		}
		return nil
	})
	if err != nil {
		return resp, err
	}
	resp.CoverMBID = pickCoverRef(coverVotes)
	resp.CoverVersion = coverVer[resp.CoverMBID]

	dominantArtist := pickDominant(artistVotes)
	dominantRelease := pickDominant(releaseVotes)
	if n := len(artistVotes); n > 1 {
		resp.MoreArtists = n - 1
	}
	if n := len(releaseVotes); n > 1 {
		resp.MoreReleases = n - 1
	}

	if resp.AtlasEnabled && dominantArtist != "" {
		dto := &aboutArtistDTO{MBID: dominantArtist, State: "unchecked"}
		if meta, err := s.deps.Manifest.GetArtistAtlasMeta(ctx, dominantArtist); err != nil {
			logger.Warn("meta detail: artist atlas read", "mbid", dominantArtist, "err", err)
		} else if meta != nil {
			if meta.Found && strings.TrimSpace(meta.Bio)+strings.TrimSpace(meta.BioSummary) != "" {
				dto.State = "found"
				dto.Bio = meta.Bio
				dto.BioSummary = meta.BioSummary
				dto.Genres = meta.Genres
				dto.Source = meta.Source
				dto.SourceURL = meta.SourceURL
			} else {
				// Tombstone, or found-with-empty-text: nothing the UI
				// can show (the AtlasMetaBreakdownCounts rule).
				dto.State = "missing"
			}
		}
		resp.Artist = dto
	}
	if resp.AtlasEnabled && dominantRelease != "" {
		dto := &aboutReleaseDTO{MBID: dominantRelease, State: "unchecked"}
		if meta, err := s.deps.Manifest.GetReleaseAtlasMeta(ctx, dominantRelease); err != nil {
			logger.Warn("meta detail: release atlas read", "mbid", dominantRelease, "err", err)
		} else if meta != nil {
			if meta.Found && strings.TrimSpace(meta.Description) != "" {
				dto.State = "found"
				dto.Description = meta.Description
				dto.RecordLabel = meta.RecordLabel
				dto.Genres = meta.Genres
				dto.Source = meta.Source
				dto.SourceURL = meta.SourceURL
			} else {
				dto.State = "missing"
			}
		}
		resp.Release = dto
	}

	if dominantArtist != "" && s.deps.ArtistImageMBIDs != nil {
		if files, err := s.deps.ArtistImageMBIDs(); err == nil {
			_, resp.HasArtistImage = files[strings.ToLower(dominantArtist)]
		}
	}

	if resp.BookletsEnabled && len(releaseVotes) > 0 {
		mbids := make([]string, 0, len(releaseVotes))
		for m := range releaseVotes {
			mbids = append(mbids, m)
		}
		states, err := s.deps.Manifest.BookletStatesIn(ctx, mbids)
		if err != nil {
			logger.Warn("meta detail: booklet states", "err", err)
		} else {
			for m, st := range states {
				if !st.Available {
					continue
				}
				state := "pending"
				if st.Fetched {
					state = "cached"
				}
				resp.Booklets = append(resp.Booklets, aboutBookletDTO{MBID: m, State: state})
			}
			// Deterministic order for the UI + tests.
			sort.Slice(resp.Booklets, func(i, j int) bool {
				return resp.Booklets[i].MBID < resp.Booklets[j].MBID
			})
		}
	}
	return resp, nil
}

// pickDominant returns the most-voted key (ties broken
// lexicographically for determinism); "" for an empty map.
func pickDominant(votes map[string]int) string {
	best := ""
	bestVotes := -1
	for k, n := range votes {
		if n > bestVotes || (n == bestVotes && k < best) {
			best, bestVotes = k, n
		}
	}
	return best
}

// --- Folder-scoped retry ---

type libraryMetaRetryRequest struct {
	Path string `json:"path"`
}

type libraryMetaRetryResponse struct {
	ResetTracks        int64 `json:"resetTracks"`
	ArtistImageResets  int64 `json:"artistImageResets"`
	HarvestResubmitted bool  `json:"harvestResubmitted"`
	BookletChecksReset int64 `json:"bookletChecksReset"`
	BookletFetchNudges int   `json:"bookletFetchNudges"`
}

// apiLibraryEnrichmentRetryScoped handles POST /api/library/enrichment/retry.
//
// Every facet is best-effort (a failing one degrades to zero rather
// than failing the request), none bumps indexed_at (no iOS delta
// churn), and the enriched_at writes route exclusively through the
// sanctioned ResetEnrichedMisses* / ResetEnrichedByArtistMBIDs
// primitives. The per-path 60s guard is UX politeness — the real
// upstream protection is the enricher/harvest clients' own pacing
// (1.1s MB / 500ms CAA / 120ms Deezer): resets deepen the queue, they
// don't raise the call rate.
func (s *Server) apiLibraryEnrichmentRetryScoped(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	var req libraryMetaRetryRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}
	normalised, ok := normaliseBrowsePath(req.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path contains traversal segments or is otherwise invalid")
		return
	}

	// Per-path rate guard (+ opportunistic prune of expired entries).
	s.libMetaRetryMu.Lock()
	if s.libMetaRetryAt == nil {
		s.libMetaRetryAt = make(map[string]time.Time)
	}
	for p, at := range s.libMetaRetryAt {
		if time.Since(at) >= libraryMetaRetryMinInterval {
			delete(s.libMetaRetryAt, p)
		}
	}
	// The window re-check is independent of the prune above by design —
	// don't couple them. It's unreachable while the prune runs
	// unconditionally on the line before, but the moment that moves to
	// a background sweep (or gets gated to every Nth call) this becomes
	// the only thing standing between a panic-clicked button and the
	// enrichers.
	if at, hot := s.libMetaRetryAt[normalised]; hot && time.Since(at) < libraryMetaRetryMinInterval {
		s.libMetaRetryMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "rate_limited",
			"a retry for this folder ran less than a minute ago — give the enrichers a moment")
		return
	}
	s.libMetaRetryAt[normalised] = time.Now()
	// The harvest facet is library-wide — gate it globally so two
	// different-folder retries inside the window fire it once.
	harvestAllowed := time.Since(s.libMetaHarvestAt) >= libraryMetaRetryMinInterval
	if harvestAllowed {
		s.libMetaHarvestAt = time.Now()
	}
	s.libMetaRetryMu.Unlock()

	ctx := r.Context()
	var resp libraryMetaRetryResponse

	// Facet 1: covers / artist-match gaps under the folder.
	if n, err := s.deps.Manifest.ResetEnrichedMissesUnderPrefix(ctx, normalised); err != nil {
		logger.Warn("library retry: reset misses", "path", normalised, "err", err)
	} else {
		resp.ResetTracks = n
	}

	// Facet 1b: fingerprint suppression markers under the folder, so a scoped
	// "try again" reaches the files AcoustID declined and the ones whose answer
	// was vetoed against their tags, rather than only the ones the enricher
	// alone could fix. Scoped to this prefix rather than clearing everything:
	// the feature exists to avoid needless whole-object reads, and re-opening
	// unrelated folders would force them to re-decode for a retry that never
	// named them. Deliberately NOT counted in ResetTracks — that field reports
	// rows re-queued for the ENRICHER; these re-enter the fingerprint sweep.
	s.clearFingerprintSuppression(ctx, "library retry", normalised)

	// Facet 2: artist images — MBIDs under the folder minus the
	// on-disk image set (resetArtistImageGaps shape, scoped).
	if s.deps.ArtistImageMBIDs != nil {
		if files, err := s.deps.ArtistImageMBIDs(); err == nil {
			if mbids, err := s.deps.Manifest.DistinctArtistMBIDsUnderPrefix(ctx, normalised); err == nil {
				var missing []string
				for _, m := range mbids {
					if _, ok := files[strings.ToLower(m)]; !ok {
						missing = append(missing, m)
					}
				}
				if n, err := s.deps.Manifest.ResetEnrichedByArtistMBIDs(ctx, missing); err == nil {
					resp.ArtistImageResets = n
				}
			}
		}
	}

	// Facet 3: bios / descriptions — the harvest submit is library-
	// wide by construction (no per-folder submit exists); the UI copy
	// says so.
	if harvestAllowed && s.deps.HarvestForceSubmit != nil {
		resp.HarvestResubmitted = s.deps.HarvestForceSubmit()
	}

	// Facet 4: booklets — re-arm availability checks for the folder's
	// releases and nudge the fetch sweep for available-but-unfetched
	// ones (capped at the priority channel's buffer).
	if s.deps.BookletPath != nil {
		if releases, err := s.deps.Manifest.DistinctReleaseMBIDsUnderPrefix(ctx, normalised); err == nil && len(releases) > 0 {
			if n, err := s.deps.Manifest.ResetBookletChecks(ctx, releases); err == nil {
				resp.BookletChecksReset = n
			}
			if s.deps.BookletNudge != nil {
				if states, err := s.deps.Manifest.BookletStatesIn(ctx, releases); err == nil {
					for m, st := range states {
						if resp.BookletFetchNudges >= bookletNudgeCap {
							break
						}
						if st.Available && !st.Fetched {
							s.deps.BookletNudge(m)
							resp.BookletFetchNudges++
						}
					}
				}
			}
		}
	}

	// Invalidate the dashboard enrichment snapshot + this subtree's
	// meta caches so the "pending" jump lands promptly. The DETAIL
	// sweep is load-bearing: the About card deliberately doesn't
	// auto-refetch after a retry (enrichment runs at the MB/CAA/Deezer
	// clients' pacing, so an immediate refetch loses the race and reads
	// as failure) — it refreshes on next open, which a 60s-stale cache
	// entry would defeat.
	s.enrichmentMu.Lock()
	s.enrichmentAt = time.Time{}
	s.enrichmentMu.Unlock()
	s.libMetaInvalidateUnder(normalised)

	logger.Info("library metadata retry", "path", normalised,
		"resetTracks", resp.ResetTracks, "artistImageResets", resp.ArtistImageResets,
		"harvestResubmitted", resp.HarvestResubmitted,
		"bookletChecksReset", resp.BookletChecksReset, "bookletFetchNudges", resp.BookletFetchNudges)
	writeJSON(w, http.StatusOK, resp)
}

// --- Byte routes ---

// serveCacheFile opens and serves one validated cache file with the
// given content type. Missing file → plain 404 (the tile/card falls
// back — no 202 dance for admin cover/portrait requests).
func serveCacheFile(w http.ResponseWriter, r *http.Request, path, contentType, cacheControl string) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "not_found", "not cached")
			return
		}
		logger.Error("open cache file", "path", path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat cache file", "path", path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// artworkCacheControl picks the caching mode for a cover response:
// content-keyed requests (a `v=` cache-buster from artwork_version,
// or a local-<sha256> id — the hash IS the content key) are immutable
// for a year; a bare UUID cover can change on a premium refetch, so
// it gets a day + mtime revalidation via ServeContent.
func artworkCacheControl(mbid string, hasVersionParam bool) string {
	if hasVersionParam || strings.HasPrefix(mbid, "local-") {
		return "private, max-age=31536000, immutable"
	}
	return "private, max-age=86400"
}

// apiLibraryArtwork handles GET /api/library/artwork/{mbid}?size=&v=.
// Only size 500 exists in the cache (enrich.ArtworkCachePath's
// default) — other sizes 404 naturally; don't invent a resize path.
func (s *Server) apiLibraryArtwork(w http.ResponseWriter, r *http.Request) {
	if s.deps.ArtworkPath == nil {
		writeError(w, http.StatusNotFound, "not_found", "artwork cache not wired")
		return
	}
	mbid := r.PathValue("mbid")
	if !adminArtworkMBIDPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"mbid must be a MusicBrainz UUID or local-<sha256> sentinel")
		return
	}
	size := 500
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 4096 {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid size")
			return
		}
		size = n
	}
	cc := artworkCacheControl(mbid, r.URL.Query().Get("v") != "")
	serveCacheFile(w, r, s.deps.ArtworkPath(mbid, size), "image/jpeg", cc)
}

// apiLibraryArtistImage handles GET /api/library/artist-image/{mbid}.
// Deliberately capped at max-age=86400 with NO version scheme: artist
// portraits are enricher-fetched (Deezer), have no version column,
// and change only via the operator's own retry; ServeContent's mtime
// revalidation covers replacement after expiry.
func (s *Server) apiLibraryArtistImage(w http.ResponseWriter, r *http.Request) {
	if s.deps.ArtistImagePath == nil {
		writeError(w, http.StatusNotFound, "not_found", "artwork cache not wired")
		return
	}
	mbid := r.PathValue("mbid")
	if !adminMBIDPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request", "mbid must be a MusicBrainz UUID")
		return
	}
	serveCacheFile(w, r, s.deps.ArtistImagePath(mbid), "image/jpeg", "private, max-age=86400")
}

// apiLibraryBooklet handles GET /api/library/booklet/{mbid} — the
// loopback twin of /v1/booklet (internal/api/booklet.go): 200 PDF
// inline, 202+Retry-After with a fetch nudge when available but not
// yet downloaded, 404 otherwise.
func (s *Server) apiLibraryBooklet(w http.ResponseWriter, r *http.Request) {
	if s.deps.BookletPath == nil || s.deps.Manifest == nil {
		writeError(w, http.StatusNotFound, "not_found", "booklets not enabled on this bridge")
		return
	}
	mbid := r.PathValue("mbid")
	if !adminMBIDPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request", "mbid must be a MusicBrainz UUID")
		return
	}
	row, err := s.deps.Manifest.GetBooklet(r.Context(), mbid)
	if err != nil {
		logger.Error("booklet lookup", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if row == nil || !row.Available {
		writeError(w, http.StatusNotFound, "not_found", "no booklet for release")
		return
	}
	f, err := os.Open(s.deps.BookletPath(mbid))
	if err != nil {
		if os.IsNotExist(err) {
			if s.deps.BookletNudge != nil {
				s.deps.BookletNudge(mbid)
			}
			w.Header().Set("Retry-After", "30")
			writeError(w, http.StatusAccepted, "pending",
				"booklet download pending; retry after the Retry-After window")
			return
		}
		logger.Error("open booklet", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat booklet", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="booklet-`+mbid+`.pdf"`)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
