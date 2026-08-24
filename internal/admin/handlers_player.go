package admin

import (
	"errors"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The web player's read surface.
//
// Every response is a slice of ONE cached catalog snapshot (catalog.go),
// so two panels on the same screen can never disagree about the
// library. Filters and sorts narrow that snapshot here in the handler
// and never enter the cache key.
//
// DTOs are admin-local by the wire-type discipline: nothing in
// internal/librarycat or internal/manifest is encoded directly, so a
// future field added to a domain type can't silently reach a client.

// playerIDPattern validates a catalog id before any lookup. The ids are
// librarycat.HashID output — 16 lowercase hex — so the alphabet is
// bounded and a path separator cannot survive it. Same discipline as
// the artwork routes, applied here even though these ids only ever
// index a map, because "only ever" is a property that changes.
var playerIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

const (
	playerDefaultLimit = 60
	playerMaxLimit     = 200

	// playerTimeLayout is the snapshot timestamp format every player
	// response echoes. A named layout rather than five copies of the
	// same magic string: the client compares snapshotAt across
	// responses to notice a rebuild mid-scroll, so the format is a
	// contract between the two, not incidental formatting.
	playerTimeLayout = "2006-01-02T15:04:05Z"
)

// snapshotStamp renders a catalog build time for the wire.
func snapshotStamp(t time.Time) string { return t.UTC().Format(playerTimeLayout) }

type playerPageMeta struct {
	Total      int    `json:"total"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	SnapshotAt string `json:"snapshotAt"`

	// Buckets is the A–Z jump index for the CURRENT filter and sort,
	// emitted on the FIRST page only — the same first-page-only
	// convention Folders/Total already use, and for the same reason:
	// it describes the whole result set, so repeating it on every page
	// is pure wire cost.
	//
	// Omitted entirely when the ordering is not alphabetical (an album
	// grid sorted by year or recency has no meaningful letter index),
	// which is also how the client knows not to draw a rail.
	Buckets []playerBucketDTO `json:"buckets,omitempty"`
}

// playerBucketDTO is one letter of the jump index: where that letter
// starts in the filtered+sorted list, and how many entries it holds.
//
// Offset is an index into that list, so the client jumps by re-fetching
// at that offset rather than scrolling — which is the only thing that
// works when the target is past the pages loaded so far.
type playerBucketDTO struct {
	Key    string `json:"key"`
	Offset int    `json:"offset"`
	Count  int    `json:"count"`
}

// buildBuckets walks an already-sorted list and records where each
// letter starts. Runs only for the first page, so it is O(n) once per
// filter change rather than per page.
//
// letterAt returns the bucket for one entry; a caller that sorts by a
// different key passes a different letterAt, which is why this takes a
// function instead of reading a Bucket field.
func buildBuckets(n int, letterAt func(i int) string) []playerBucketDTO {
	if n == 0 {
		return nil
	}
	out := make([]playerBucketDTO, 0, 27)
	for i := 0; i < n; i++ {
		k := letterAt(i)
		if len(out) > 0 && out[len(out)-1].Key == k {
			out[len(out)-1].Count++
			continue
		}
		out = append(out, playerBucketDTO{Key: k, Offset: i, Count: 1})
	}
	return out
}

type playerAlbumDTO struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	AlbumArtist string  `json:"albumArtist"`
	ArtistID    string  `json:"artistId"`
	Year        int     `json:"year,omitempty"`
	TrackCount  int     `json:"trackCount"`
	DiscCount   int     `json:"discCount,omitempty"`
	Duration    float64 `json:"duration,omitempty"`
	SizeBytes   int64   `json:"sizeBytes,omitempty"`

	Quality   string   `json:"quality"`
	Qualities []string `json:"qualities,omitempty"`
	RateHz    int      `json:"rateHz,omitempty"`
	Bits      int      `json:"bits,omitempty"`

	ArtworkMBID    string `json:"artworkMBID,omitempty"`
	ArtworkVersion string `json:"artworkVersion,omitempty"`
	FolderPath     string `json:"folderPath,omitempty"`

	AddedAt int64 `json:"addedAt,omitempty"`

	Routed       bool  `json:"routed,omitempty"`
	RoutedOnline *bool `json:"routedOnline,omitempty"`
}

type playerAlbumsResponse struct {
	playerPageMeta
	Albums []playerAlbumDTO `json:"albums"`
}

type playerArtistDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Bucket     string `json:"bucket"`
	TrackCount int    `json:"trackCount"`
	AlbumCount int    `json:"albumCount"`
	ArtistMBID string `json:"artistMBID,omitempty"`

	// HasImage says a cached portrait exists, so the client can ask for
	// one instead of firing a request per artist and eating a 404 for
	// everyone without one. Sourced from a single ReadDir, not a stat
	// per row.
	HasImage bool `json:"hasImage,omitempty"`
	// ArtworkMBID/ArtworkVersion are the artist's TOP album's cover —
	// the fallback tile when there is no portrait, which most artists in
	// a real library are.
	//
	// The cover REF travels rather than the album id so the client can
	// reuse coverURL() unchanged (same field names as playerAlbumDTO)
	// and inherit its content-key caching. Sending an id instead would
	// have needed a new id-addressed cover endpoint for no gain.
	ArtworkMBID    string `json:"artworkMBID,omitempty"`
	ArtworkVersion string `json:"artworkVersion,omitempty"`
}

type playerArtistsResponse struct {
	playerPageMeta
	Artists []playerArtistDTO `json:"artists"`
}

type playerAxisDTO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Bucket       string `json:"bucket"`
	TrackCount   int    `json:"trackCount"`
	AlbumCount   int    `json:"albumCount"`
	CoverAlbumID string `json:"coverAlbumId,omitempty"`
}

type playerAxisResponse struct {
	playerPageMeta
	Entries []playerAxisDTO `json:"entries"`
}

type playerStatsDTO struct {
	Tracks       int `json:"tracks"`
	Albums       int `json:"albums"`
	Artists      int `json:"artists"`
	Genres       int `json:"genres"`
	Composers    int `json:"composers"`
	RoutedTracks int `json:"routedTracks"`
}

func albumDTO(a librarycat.Album, online func(string) *bool) playerAlbumDTO {
	d := playerAlbumDTO{
		ID: a.ID, Title: a.Title, AlbumArtist: a.AlbumArtist, ArtistID: a.ArtistID,
		Year: a.Year, TrackCount: a.TrackCount, DiscCount: a.DiscCount,
		Duration: a.Duration, SizeBytes: a.SizeBytes,
		Quality: a.Quality.String(), RateHz: a.RateHz, Bits: a.Bits,
		ArtworkMBID: a.ArtworkMBID, ArtworkVersion: a.ArtworkVersion,
		FolderPath: a.FolderPath, AddedAt: a.AddedAt, Routed: a.Routed,
	}
	// Only send the mask when it is genuinely mixed — a single-value
	// list on every tile is noise the UI would have to filter anyway.
	if buckets := a.QualityMask.Buckets(); len(buckets) > 1 {
		d.Qualities = make([]string, len(buckets))
		for i, b := range buckets {
			d.Qualities[i] = b.String()
		}
	}
	if a.Routed && online != nil && len(a.RoutedUDNs) > 0 {
		d.RoutedOnline = online(a.RoutedUDNs[0])
	}
	return d
}

// routedOnline reports upstream reachability for a UDN, or nil when
// the answer is unknown (the dep is unwired). nil is NOT false: a
// player that greyed out every routed album because it couldn't ask
// would be worse than one that lets the user try.
func (s *Server) routedOnline(udn string) *bool {
	if s.deps.UPnPHostOnline == nil {
		return nil
	}
	v := s.deps.UPnPHostOnline(udn)
	return &v
}

func (s *Server) playerCatalog(w http.ResponseWriter, r *http.Request) (*librarycat.Catalog, bool) {
	cat, err := s.libraryCatalog(r.Context())
	if err != nil {
		if errors.Is(err, errCatalogTooLarge) {
			writeError(w, http.StatusServiceUnavailable, "catalog_too_large",
				"this library is too large for the in-memory catalog the player uses")
			return nil, false
		}
		logger.Error("build library catalog", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not build the library catalog")
		return nil, false
	}
	return cat, true
}

func playerPaging(r *http.Request) (offset, limit int, err error) {
	q := r.URL.Query()
	limit = playerDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n <= 0 {
			return 0, 0, errors.New("invalid limit")
		}
		limit = min(n, playerMaxLimit)
	}
	if v := q.Get("offset"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 {
			return 0, 0, errors.New("invalid offset")
		}
		offset = n
	}
	return offset, limit, nil
}

// pageSlice clamps an offset/limit window onto a length. An offset past
// the end yields an empty page rather than an error — a client racing a
// rebuild that shrank the library should see "no more", not a fault.
func pageSlice(total, offset, limit int) (start, end int) {
	if offset >= total {
		return total, total
	}
	end = offset + limit
	if end > total {
		end = total
	}
	return offset, end
}

// apiPlayerAlbums handles GET /api/player/albums.
func (s *Server) apiPlayerAlbums(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	offset, limit, err := playerPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	q := r.URL.Query()

	idx, err := s.filterAlbums(cat, q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sortAlbumIndices(cat, idx, q.Get("sort"))

	start, end := pageSlice(len(idx), offset, limit)
	out := make([]playerAlbumDTO, 0, end-start)
	for _, i := range idx[start:end] {
		out = append(out, albumDTO(cat.Albums[i], s.routedOnline))
	}
	meta := playerPageMeta{Total: len(idx), Offset: start, Limit: limit,
		SnapshotAt: snapshotStamp(cat.BuiltAt)}
	if start == 0 {
		meta.Buckets = albumBuckets(cat, idx, q.Get("sort"))
	}
	writeJSON(w, http.StatusOK, playerAlbumsResponse{playerPageMeta: meta, Albums: out})
}

// albumBuckets builds the A–Z jump index for a sorted album window, or
// nil when the ordering has no letters to index.
//
// The letter depends on the SORT, not on the row: Album.Bucket is
// derived from SortArtist (librarycat), which is right under an artist
// sort and wrong under a title sort — "Abbey Road" by The Beatles files
// under B by artist and A by title. Recency and year orderings get no
// rail at all, and the omitted field is how the client knows not to draw
// one.
func albumBuckets(cat *librarycat.Catalog, idx []int, mode string) []playerBucketDTO {
	var letterAt func(i int) string
	switch mode {
	case "artist":
		letterAt = func(i int) string { return cat.Albums[idx[i]].Bucket }
	case "title":
		letterAt = func(i int) string { return librarycat.BucketOf(cat.Albums[idx[i]].SortTitle) }
	default:
		return nil // recent / year — not alphabetical
	}
	return buildBuckets(len(idx), letterAt)
}

// filterAlbums narrows the snapshot to the indices matching the query.
// Returns a fresh slice every call — the catalog's own ordering must
// never be mutated, since it is shared by every concurrent reader.
func (s *Server) filterAlbums(cat *librarycat.Catalog, q map[string][]string) ([]int, error) {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	quality := get("quality")
	artistID := get("artist")
	genreID := get("genre")
	composerID := get("composer")

	var allow map[string]struct{}
	for _, spec := range []struct {
		id   string
		load func(string) ([]string, bool)
	}{
		{artistID, func(id string) ([]string, bool) {
			a, ok := cat.ArtistByID(id)
			return a.AlbumIDs, ok
		}},
		{genreID, func(id string) ([]string, bool) {
			e, ok := cat.GenreByID(id)
			return e.AlbumIDs, ok
		}},
		{composerID, func(id string) ([]string, bool) {
			e, ok := cat.ComposerByID(id)
			return e.AlbumIDs, ok
		}},
	} {
		if spec.id == "" {
			continue
		}
		if !playerIDPattern.MatchString(spec.id) {
			return nil, errors.New("invalid id")
		}
		ids, ok := spec.load(spec.id)
		if !ok {
			// A filter naming something that isn't there yields an
			// EMPTY result, not an error: the id may simply have aged
			// out of a rebuilt snapshot.
			return []int{}, nil
		}
		next := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			if allow == nil {
				next[id] = struct{}{}
				continue
			}
			if _, in := allow[id]; in {
				next[id] = struct{}{}
			}
		}
		allow = next
	}

	wantQuality, err := parseQualityFilter(quality)
	if err != nil {
		return nil, err
	}

	idx := make([]int, 0, len(cat.Albums))
	for i, a := range cat.Albums {
		if allow != nil {
			if _, in := allow[a.ID]; !in {
				continue
			}
		}
		if wantQuality != nil && !wantQuality(a) {
			continue
		}
		idx = append(idx, i)
	}
	return idx, nil
}

// parseQualityFilter maps the query token to a predicate. "" and "all"
// mean no filter. "dsd" is the bridge-only any-DSD value that makes the
// rate-less DSD rows selectable; the seven others mirror iOS exactly.
func parseQualityFilter(v string) (func(librarycat.Album) bool, error) {
	switch v {
	case "", "all":
		return nil, nil
	case "dsd":
		return func(a librarycat.Album) bool {
			for _, b := range a.QualityMask.Buckets() {
				if b.IsDSD() {
					return true
				}
			}
			return false
		}, nil
	}
	for b := librarycat.QualityUnknown; b <= librarycat.QualityDSDUnknownRate; b++ {
		if b.String() == v {
			want := b
			// Match on the MASK, not the dominant bucket: a mostly-FLAC
			// album with three DSD tracks should appear under DSD too,
			// or those tracks are unreachable by filter.
			return func(a librarycat.Album) bool { return a.QualityMask.Has(want) }, nil
		}
	}
	return nil, errors.New("unknown quality filter")
}

// sortAlbumIndices orders in place. Every comparator ends in a total
// tie-break on ID so a page boundary can't duplicate or drop a row.
func sortAlbumIndices(cat *librarycat.Catalog, idx []int, mode string) {
	al := cat.Albums
	switch mode {
	case "", "recent":
		// AddedAt is MIN(indexed_at) over the album's tracks — see
		// librarycat.Album. It is "recently indexed", not a true
		// first-seen date; the store has no such column.
		sort.SliceStable(idx, func(i, j int) bool {
			x, y := al[idx[i]], al[idx[j]]
			if x.AddedAt != y.AddedAt {
				return x.AddedAt > y.AddedAt
			}
			return x.ID < y.ID
		})
	case "title":
		sort.SliceStable(idx, func(i, j int) bool {
			x, y := al[idx[i]], al[idx[j]]
			if c := strings.Compare(x.SortTitle, y.SortTitle); c != 0 {
				return c < 0
			}
			return x.ID < y.ID
		})
	case "year":
		sort.SliceStable(idx, func(i, j int) bool {
			x, y := al[idx[i]], al[idx[j]]
			if x.Year != y.Year {
				// Unknown years (0) sort last under a year sort rather
				// than leading it, which is what a listener expects.
				if x.Year == 0 || y.Year == 0 {
					return y.Year == 0
				}
				return x.Year > y.Year
			}
			return x.ID < y.ID
		})
	default: // "artist" — the catalog's own canonical order
	}
}

func (s *Server) apiPlayerArtists(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	offset, limit, err := playerPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	start, end := pageSlice(len(cat.Artists), offset, limit)
	// One ReadDir for the whole page, not one stat per artist.
	withImages := s.cachedArtistImageMBIDs()
	out := make([]playerArtistDTO, 0, end-start)
	for _, a := range cat.Artists[start:end] {
		d := playerArtistDTO{
			ID: a.ID, Name: a.Name, Bucket: a.Bucket,
			TrackCount: a.TrackCount, AlbumCount: a.AlbumCount, ArtistMBID: a.ArtistMBID,
		}
		if a.ArtistMBID != "" {
			_, d.HasImage = withImages[strings.ToLower(a.ArtistMBID)]
		}
		// AlbumIDs[0] is the artist's top album by in-group track count
		// (librarycat.rankAlbums), so this is a stable tile.
		if len(a.AlbumIDs) > 0 {
			if top, ok := cat.AlbumByID(a.AlbumIDs[0]); ok {
				d.ArtworkMBID, d.ArtworkVersion = top.ArtworkMBID, top.ArtworkVersion
			}
		}
		out = append(out, d)
	}
	meta := playerPageMeta{Total: len(cat.Artists), Offset: start, Limit: limit,
		SnapshotAt: snapshotStamp(cat.BuiltAt)}
	if start == 0 {
		meta.Buckets = buildBuckets(len(cat.Artists), func(i int) string { return cat.Artists[i].Bucket })
	}
	writeJSON(w, http.StatusOK, playerArtistsResponse{playerPageMeta: meta, Artists: out})
}

func (s *Server) apiPlayerGenres(w http.ResponseWriter, r *http.Request) {
	s.serveAxis(w, r, func(c *librarycat.Catalog) []librarycat.AxisEntry { return c.Genres })
}

func (s *Server) apiPlayerComposers(w http.ResponseWriter, r *http.Request) {
	s.serveAxis(w, r, func(c *librarycat.Catalog) []librarycat.AxisEntry { return c.Composers })
}

func (s *Server) serveAxis(w http.ResponseWriter, r *http.Request,
	pick func(*librarycat.Catalog) []librarycat.AxisEntry) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	offset, limit, err := playerPaging(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	entries := pick(cat)
	start, end := pageSlice(len(entries), offset, limit)
	out := make([]playerAxisDTO, 0, end-start)
	for _, e := range entries[start:end] {
		d := playerAxisDTO{ID: e.ID, Name: e.DisplayName, Bucket: e.Bucket,
			TrackCount: e.TrackCount, AlbumCount: len(e.AlbumIDs)}
		if len(e.AlbumIDs) > 0 {
			// AlbumIDs[0] is the group's stable primary tile — see
			// librarycat.rankAlbums.
			d.CoverAlbumID = e.AlbumIDs[0]
		}
		out = append(out, d)
	}
	meta := playerPageMeta{Total: len(entries), Offset: start, Limit: limit,
		SnapshotAt: snapshotStamp(cat.BuiltAt)}
	if start == 0 {
		// Genres are ordered by track count, not alphabetically
		// (librarycat.finishAxis), so a letter rail would point at
		// scattered offsets and mislead. Composers ARE alphabetical.
		if axisIsAlphabetical(entries) {
			meta.Buckets = buildBuckets(len(entries), func(i int) string { return entries[i].Bucket })
		}
	}
	writeJSON(w, http.StatusOK, playerAxisResponse{playerPageMeta: meta, Entries: out})
}

// artistImageSetTTL matches the sox-availability cache: short enough
// that a portrait fetched by the enricher shows up on the next page
// load, long enough that scrolling a large artist grid does not re-read
// the artwork directory per page.
const artistImageSetTTL = 30 * time.Second

// cachedArtistImageMBIDs returns the set of artist MBIDs with a cached
// portrait, behind a short TTL.
//
// The underlying closure is ONE os.ReadDir over the artwork cache — the
// alternative being one os.Stat per artist, which on a 469-artist
// library is 469 syscalls per page of an infinite scroll. The TTL is
// there because portraits appear asynchronously as the enricher runs,
// so an unbounded cache would leave a freshly-fetched artist looking
// image-less until a restart, while an uncached one re-reads the
// directory on every page.
//
// A read error degrades to "no portraits", never to an error: the
// fallback tile is the artist's top album cover, so a failure here costs
// a nicer image, not a broken grid.
func (s *Server) cachedArtistImageMBIDs() map[string]struct{} {
	if s.deps.ArtistImageMBIDs == nil {
		return nil
	}
	now := time.Now()
	s.artistImagesMu.Lock()
	if !s.artistImagesAt.IsZero() && now.Sub(s.artistImagesAt) < artistImageSetTTL {
		v := s.artistImages
		s.artistImagesMu.Unlock()
		return v
	}
	s.artistImagesMu.Unlock()

	// Read UNLOCKED — a cold directory read must not block concurrent
	// page requests, the cachedSoxAvailability convention.
	files, err := s.deps.ArtistImageMBIDs()
	if err != nil {
		logger.Warn("artist image set", "err", err)
		files = nil
	}
	s.artistImagesMu.Lock()
	s.artistImages, s.artistImagesAt = files, now
	s.artistImagesMu.Unlock()
	return files
}

// axisIsAlphabetical reports whether an axis list is actually in
// letter order, which is what makes a jump rail meaningful.
//
// Checked rather than assumed per axis: finishAxis orders GENRES by
// track count and COMPOSERS by surname, and a rail drawn over a
// count-ordered list sends the reader to the wrong place while looking
// authoritative. Cheap — one pass over an already-materialised slice,
// first page only.
func axisIsAlphabetical(entries []librarycat.AxisEntry) bool {
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Bucket > entries[i].Bucket {
			return false
		}
	}
	return true
}

func (s *Server) apiPlayerStats(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, playerStatsDTO{
		Tracks: cat.Stats.Tracks, Albums: cat.Stats.Albums, Artists: cat.Stats.Artists,
		Genres: len(cat.Genres), Composers: len(cat.Composers),
		RoutedTracks: cat.Stats.RoutedTracks,
	})
}

// ---- Detail ----

type playerTrackDTO struct {
	Path      string  `json:"path"`
	Title     string  `json:"title"`
	Artist    string  `json:"artist,omitempty"`
	AlbumID   string  `json:"albumId,omitempty"`
	Disc      int     `json:"disc,omitempty"`
	Track     int     `json:"track,omitempty"`
	Duration  float64 `json:"duration,omitempty"`
	SizeBytes int64   `json:"sizeBytes,omitempty"`
	Codec     string  `json:"codec,omitempty"`
	RateHz    int     `json:"rateHz,omitempty"`
	Bits      int     `json:"bits,omitempty"`
	IsDSD     bool    `json:"isDsd,omitempty"`
	Routed    bool    `json:"routed,omitempty"`

	Play playerPlayabilityDTO `json:"play"`
}

// playerPlayabilityDTO tells the client what the SERVER knows. The
// browser still decides — canPlayType and a real decode attempt are the
// only authorities on whether a given engine can play a given file, and
// they disagree across Safari, Chromium and Firefox (notably on ALAC
// and AIFF). So this reports facts, not a verdict:
//
//	Source      what the audio route will announce for the source
//	Kind        a coarse hint: "universal" | "engine-dependent" | "none"
//	VariantID   a FRESH FLAC sidecar to prefer, when one exists
//
// "none" is reserved for DSD, where the answer really is universal: no
// browser decodes a 1-bit stream.
type playerPlayabilityDTO struct {
	Kind        string `json:"kind"`
	ContentType string `json:"contentType"`

	VariantID          string `json:"variantId,omitempty"`
	VariantContentType string `json:"variantContentType,omitempty"`
	VariantRateHz      int    `json:"variantRateHz,omitempty"`
	VariantBits        int    `json:"variantBits,omitempty"`

	Downloadable bool `json:"downloadable"`
}

type playerAlbumDetailResponse struct {
	Album        playerAlbumDTO   `json:"album"`
	Tracks       []playerTrackDTO `json:"tracks"`
	Release      *aboutReleaseDTO `json:"release,omitempty"`
	Artist       *aboutArtistDTO  `json:"artist,omitempty"`
	Booklet      *aboutBookletDTO `json:"booklet,omitempty"`
	AtlasEnabled bool             `json:"atlasEnabled"`
	SnapshotAt   string           `json:"snapshotAt"`
}

type playerArtistDetailResponse struct {
	Artist       playerArtistDTO  `json:"artist"`
	Albums       []playerAlbumDTO `json:"albums"`
	About        *aboutArtistDTO  `json:"about,omitempty"`
	HasImage     bool             `json:"hasImage"`
	AtlasEnabled bool             `json:"atlasEnabled"`
	SnapshotAt   string           `json:"snapshotAt"`
}

func (s *Server) apiPlayerAlbumDetail(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !playerIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid album id")
		return
	}
	album, found := cat.AlbumByID(id)
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such album")
		return
	}
	tracks, err := s.hydrateTracks(r, cat, album.TrackPaths)
	if err != nil {
		logger.Error("album detail hydrate", "album", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not read album tracks")
		return
	}
	cfg := s.deps.CfgHolder.Load()
	resp := playerAlbumDetailResponse{
		Album: albumDTO(album, s.routedOnline), Tracks: tracks,
		AtlasEnabled: cfg.Atlas.Enabled,
		SnapshotAt:   snapshotStamp(cat.BuiltAt),
	}
	if cfg.Atlas.Enabled {
		resp.Release = s.releaseAbout(r.Context(), album.ReleaseMBID)
	}
	if album.ReleaseMBID != "" && s.deps.Manifest != nil {
		if row, err := s.deps.Manifest.GetBooklet(r.Context(), album.ReleaseMBID); err == nil &&
			row != nil && row.Available {
			// "cached" only when the PDF is actually on disk; the
			// player's booklet button 202s otherwise and the byte
			// route nudges the fetch.
			state := "pending"
			if s.deps.BookletPath != nil {
				if _, statErr := os.Stat(s.deps.BookletPath(album.ReleaseMBID)); statErr == nil {
					state = "cached"
				}
			}
			resp.Booklet = &aboutBookletDTO{MBID: album.ReleaseMBID, State: state}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) apiPlayerArtistDetail(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !playerIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid artist id")
		return
	}
	artist, found := cat.ArtistByID(id)
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such artist")
		return
	}
	albums := make([]playerAlbumDTO, 0, len(artist.AlbumIDs))
	for _, aid := range artist.AlbumIDs {
		if a, ok := cat.AlbumByID(aid); ok {
			albums = append(albums, albumDTO(a, s.routedOnline))
		}
	}
	cfg := s.deps.CfgHolder.Load()
	resp := playerArtistDetailResponse{
		Artist: playerArtistDTO{ID: artist.ID, Name: artist.Name, Bucket: artist.Bucket,
			TrackCount: artist.TrackCount, AlbumCount: artist.AlbumCount,
			ArtistMBID: artist.ArtistMBID},
		Albums:       albums,
		AtlasEnabled: cfg.Atlas.Enabled,
		SnapshotAt:   snapshotStamp(cat.BuiltAt),
	}
	if cfg.Atlas.Enabled {
		resp.About = s.artistAbout(r.Context(), artist.ArtistMBID)
	}
	if artist.ArtistMBID != "" && s.deps.ArtistImagePath != nil {
		if _, err := os.Stat(s.deps.ArtistImagePath(artist.ArtistMBID)); err == nil {
			resp.HasImage = true
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- Search ----

type playerSearchHitDTO struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Detail         string `json:"detail,omitempty"`
	ArtworkMBID    string `json:"artworkMBID,omitempty"`
	ArtworkVersion string `json:"artworkVersion,omitempty"`
}

type playerSearchTrackDTO struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Artist  string `json:"artist,omitempty"`
	Album   string `json:"album,omitempty"`
	AlbumID string `json:"albumId,omitempty"`
}

type playerSearchResponse struct {
	Query           string                 `json:"query"`
	Albums          []playerSearchHitDTO   `json:"albums"`
	Artists         []playerSearchHitDTO   `json:"artists"`
	Tracks          []playerSearchTrackDTO `json:"tracks"`
	TracksAvailable bool                   `json:"tracksAvailable"`
}

// apiPlayerSearch is the player's one search surface, and it is
// deliberately two-tier.
//
// Albums and artists come from the CACHED CATALOG — a linear scan over
// a few thousand value structs, matched on the sort key so "beatles"
// finds "The Beatles" and "eric" finds "Éric Serra". Tracks come from
// FTS5, which is the right tool for a full-text match over 25k rows and
// already exists.
//
// The catalog half is answered server-side rather than by shipping the
// whole catalog to the browser and filtering there: on a large library
// that payload is megabytes, and the scan is microseconds either way.
// The client debounces, so the cost is one request per pause in typing.
//
// A bridge whose SQLite lacks FTS5 still gets album and artist results;
// tracksAvailable tells the UI to say so rather than imply the library
// has no matching tracks.
func (s *Server) apiPlayerSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(safeQuery(r).Get("q"))
	resp := playerSearchResponse{
		Query:  q,
		Albums: []playerSearchHitDTO{}, Artists: []playerSearchHitDTO{},
		Tracks: []playerSearchTrackDTO{},
	}
	// Two runes, matching the FTS handler's floor — a one-character
	// query matches most of a library and helps nobody.
	if len([]rune(q)) < 2 {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	limit := 12
	if v := safeQuery(r).Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 50)
		}
	}

	albums, artists := cat.Search(q, limit)
	for _, h := range albums {
		d := playerSearchHitDTO{ID: h.ID, Name: h.Name, Detail: h.Detail}
		if h.Album != nil {
			d.ArtworkMBID, d.ArtworkVersion = h.Album.ArtworkMBID, h.Album.ArtworkVersion
		}
		resp.Albums = append(resp.Albums, d)
	}
	for _, h := range artists {
		resp.Artists = append(resp.Artists, playerSearchHitDTO{ID: h.ID, Name: h.Name, Detail: h.Detail})
	}

	hits, err := s.deps.Manifest.SearchTracks(r.Context(), q, limit)
	switch {
	case errors.Is(err, manifest.ErrSearchUnavailable):
		// Not an error for this surface: the catalog half still
		// answered. tracksAvailable=false is the UI's cue.
	case err != nil:
		logger.Error("player search: tracks", "err", err)
	default:
		resp.TracksAvailable = true
		for _, h := range hits {
			t := playerSearchTrackDTO{Path: h.Path, Title: h.Title, Artist: h.Artist, Album: h.Album}
			if id, ok := cat.AlbumIDForPath(h.Path); ok {
				t.AlbumID = id
			}
			resp.Tracks = append(resp.Tracks, t)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
