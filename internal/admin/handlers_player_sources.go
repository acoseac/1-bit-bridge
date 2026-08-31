package admin

import (
	"errors"
	"net/http"
	"sort"

	"github.com/acoseac/1-bit-bridge/internal/librarycat"
)

// The library-source facet: which places this library's tracks come
// from, whether each is reachable right now, and a filter that narrows
// every browse axis to one of them.
//
// A hybrid bridge serves its own filesystem AND one or more upstream
// UPnP MediaServers, and until this existed the two were blended into
// the same grids with nothing to tell them apart or to browse one on
// its own. On the reference library that is 15,283 routed tracks
// beside 87 local ones, so "which of these is actually mine" was not a
// question the player could answer.

// UPnPSource is one upstream MediaServer as the source facet sees it.
//
// It carries BOTH identities on purpose, because they are different
// strings and each opens a different door:
//
//   - Key is what `upnp_track_routing.server_udn` holds, which is the
//     ingest's StableServerKey — the LOWERCASED UDN, or
//     "manual:<sha256(url)>" for an upstream configured by description
//     URL alone. It is what the catalog's rows carry, so it is the
//     only thing that can decide membership.
//   - Online is resolved by the provider against the SSDP cache, which
//     is keyed on the RAW UDN as the device reported it.
//
// Asking the liveness lookup for a Key is therefore wrong whenever the
// device's UDN is not already lowercase, and always wrong for a manual
// entry — it silently answers "offline" for an upstream that is up.
// Resolving it on the provider's side, where the config row holds both
// spellings, is what keeps the two apart.
type UPnPSource struct {
	Key    string
	Name   string
	Online bool
}

// playerSourceDTO is one row of GET /api/player/sources.
//
// Counts come from the catalog rather than from a COUNT(*) per source:
// every other number the player shows is a slice of that same
// snapshot, and a source panel that disagreed with the grid it links
// to would be worse than a slightly stale one.
type playerSourceDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Kind is "filesystem" or "upnp" — the two behave differently
	// enough (one can go offline, one cannot) that the client branches
	// on it rather than inferring from the id.
	Kind       string `json:"kind"`
	TrackCount int    `json:"trackCount"`
	AlbumCount int    `json:"albumCount"`
	// Online is absent when the answer is unknown, which is NOT the
	// same as offline: an unwired discovery cache must not paint every
	// upstream red. Always absent for the filesystem source, which has
	// no such state.
	Online *bool `json:"online,omitempty"`
	// Configured is false for an upstream that has tracks in the
	// manifest but no config row — rows left behind by a server the
	// operator removed, before the next run's orphan sweep reaps them.
	// Naming them is better than hiding them: they are why the album
	// count adds up the way it does.
	Configured bool `json:"configured"`
}

type playerSourcesResponse struct {
	Sources    []playerSourceDTO `json:"sources"`
	SnapshotAt string            `json:"snapshotAt"`
}

const (
	sourceKindFilesystem = "filesystem"
	sourceKindUPnP       = "upnp"
)

// localSourceName is what the bridge's own library is called in the
// facet. Not the configured library name: a bridge with three roots
// has one filesystem source, and borrowing one root's name for it
// would be wrong two thirds of the time.
const localSourceName = "This bridge"

// parseSourceFilter validates the `source` query token.
//
// "" and "all" mean no filter. Everything else must be either the
// local sentinel or a catalog id, checked against the same
// bounded-alphabet pattern the other id filters use BEFORE any lookup.
func parseSourceFilter(v string) (string, error) {
	switch v {
	case "", "all":
		return "", nil
	case librarycat.LocalSourceID:
		return v, nil
	}
	if !playerIDPattern.MatchString(v) {
		return "", errors.New("invalid source")
	}
	return v, nil
}

// sourceAlbumSet returns the album ids belonging to one source, or nil
// when no source filter is active.
//
// An id the snapshot no longer knows yields an EMPTY (non-nil) set,
// which the caller reads as "no results" — the same answer axisAllowSet
// gives an aged-out artist id, and for the same reason: an upstream
// removed between two page loads should empty the view, not fault it.
func sourceAlbumSet(cat *librarycat.Catalog, sourceID string) map[string]struct{} {
	if sourceID == "" {
		return nil
	}
	out := map[string]struct{}{}
	for _, a := range cat.Albums {
		for _, id := range a.SourceIDs {
			if id == sourceID {
				out[a.ID] = struct{}{}
				break
			}
		}
	}
	return out
}

// narrowGroup restates one artist / genre / composer group against an
// allowed album set: which of its albums survive, and how many of its
// tracks sit in those.
//
// The album ids are narrowed, not merely counted, because AlbumIDs[0]
// is the group's cover tile — a filtered list whose tiles showed
// artwork from the source the reader just filtered OUT would undo the
// filter visually while the counts said otherwise.
//
// The track total is summed from AlbumTracks rather than scaled from
// TrackCount, because a group's tracks are not spread evenly across
// its albums and an estimate would be a number on screen that no query
// would reproduce. `ok` is false when nothing survives, which is how
// the caller drops the group entirely.
func narrowGroup(albumIDs []string, albumTracks []int, allow map[string]struct{}) (
	keptIDs []string, keptTracks []int, tracks int, ok bool) {
	for i, id := range albumIDs {
		if _, in := allow[id]; !in {
			continue
		}
		keptIDs = append(keptIDs, id)
		// Defensive on length: the two slices are emitted together by
		// rankAlbums and cannot disagree, but this reads a second slice
		// by the first one's index and a panic here would take down a
		// whole browse page.
		n := 0
		if i < len(albumTracks) {
			n = albumTracks[i]
		}
		keptTracks = append(keptTracks, n)
		tracks += n
	}
	return keptIDs, keptTracks, tracks, len(keptIDs) > 0
}

// apiPlayerSources serves GET /api/player/sources.
//
// The filesystem row is emitted whenever the library holds a
// filesystem track, and upstream rows are derived from the catalog —
// so a source with no tracks yet (configured, never walked) does not
// appear. That is deliberate: this is a facet over the library, and a
// row that filters to nothing is a dead end.
func (s *Server) apiPlayerSources(w http.ResponseWriter, r *http.Request) {
	cat, ok := s.playerCatalog(w, r)
	if !ok {
		return
	}
	albums := albumCountsBySource(cat)

	out := make([]playerSourceDTO, 0, len(cat.SourceTracks))
	if n := cat.SourceTracks[librarycat.LocalSourceID]; n > 0 {
		out = append(out, playerSourceDTO{
			ID: librarycat.LocalSourceID, Name: localSourceName,
			Kind: sourceKindFilesystem, Configured: true,
			TrackCount: n, AlbumCount: albums[librarycat.LocalSourceID],
		})
	}
	writeJSON(w, http.StatusOK, playerSourcesResponse{
		Sources:    append(out, s.upstreamSourceRows(cat, albums)...),
		SnapshotAt: snapshotStamp(cat.BuiltAt),
	})
}

// albumCountsBySource counts album MEMBERSHIP per source, so an album
// holding tracks from two places is counted by both — "has music here" is
// the question the row answers. The track totals still sum to the library
// because those ARE attributed per track, in the catalog build.
func albumCountsBySource(cat *librarycat.Catalog) map[string]int {
	albums := map[string]int{}
	for _, a := range cat.Albums {
		for _, id := range a.SourceIDs {
			albums[id]++
		}
	}
	return albums
}

// upstreamSourceRows builds the non-filesystem rows, largest first.
//
// A configured upstream with no tracks is skipped: this is a facet over
// the library, and a row that filters to nothing is a dead end. Rows whose
// upstream is no longer configured are kept — they are why the counts add
// up the way they do — but get no name and no liveness rather than a guess.
func (s *Server) upstreamSourceRows(cat *librarycat.Catalog, albums map[string]int) []playerSourceDTO {
	out := make([]playerSourceDTO, 0, len(cat.SourceTracks))
	seen := map[string]struct{}{}
	for _, src := range s.upnpSources() {
		id := librarycat.SourceID(src.Key)
		seen[id] = struct{}{}
		if cat.SourceTracks[id] == 0 {
			continue
		}
		online := src.Online
		out = append(out, playerSourceDTO{
			ID: id, Name: src.Name, Kind: sourceKindUPnP, Configured: true,
			TrackCount: cat.SourceTracks[id], AlbumCount: albums[id], Online: &online,
		})
	}
	for id, n := range cat.SourceTracks {
		if id == librarycat.LocalSourceID {
			continue
		}
		if _, known := seen[id]; known {
			continue
		}
		out = append(out, playerSourceDTO{
			ID: id, Name: "Removed upstream", Kind: sourceKindUPnP,
			TrackCount: n, AlbumCount: albums[id],
		})
	}
	// Largest first, then by name, then id: the ordering must not depend
	// on map iteration or the rail reshuffles between reloads.
	sort.Slice(out, func(i, j int) bool {
		if out[i].TrackCount != out[j].TrackCount {
			return out[i].TrackCount > out[j].TrackCount
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// upnpSources returns the configured upstreams, or nil when the
// feature is unwired.
func (s *Server) upnpSources() []UPnPSource {
	if s.deps.UPnPSources == nil {
		return nil
	}
	return s.deps.UPnPSources()
}

// sourceOnlineByKey maps routing key -> liveness for the album badge.
//
// Built from the provider rather than by asking UPnPHostOnline with a
// routing key, which is the mismatch UPnPSource's doc describes: the
// cache is keyed on the raw UDN and the badge holds the stable key.
func (s *Server) sourceOnlineByKey() map[string]bool {
	srcs := s.upnpSources()
	if len(srcs) == 0 {
		return nil
	}
	out := make(map[string]bool, len(srcs))
	for _, src := range srcs {
		out[src.Key] = src.Online
	}
	return out
}

// narrowArtists returns the artist list scoped to one source, with the
// per-tile counts restated so they describe what is actually shown.
//
// Returns the catalog's own slice untouched when nothing filters —
// which is also why the result must be treated as read-only by the
// caller, exactly like cat.Artists itself.
func narrowArtists(cat *librarycat.Catalog, source string) ([]librarycat.Artist, error) {
	sourceID, err := parseSourceFilter(source)
	if err != nil {
		return nil, err
	}
	allow := sourceAlbumSet(cat, sourceID)
	if allow == nil {
		return cat.Artists, nil
	}
	out := make([]librarycat.Artist, 0, len(cat.Artists))
	for _, a := range cat.Artists {
		ids, counts, tracks, ok := narrowGroup(a.AlbumIDs, a.AlbumTracks, allow)
		if !ok {
			continue
		}
		a.AlbumIDs, a.AlbumTracks = ids, counts
		a.AlbumCount, a.TrackCount = len(ids), tracks
		out = append(out, a)
	}
	return out, nil
}

// narrowAxis is the genre / composer twin of narrowArtists.
//
// It RE-SORTS a narrowed genre list, because finishAxis orders genres
// by track count and the counts have just changed: leaving the original
// order would present a list sorted by a number the reader can no
// longer see. Composers are alphabetical, so their order is unaffected
// and the same re-sort is a no-op for them — detected the way
// axisIsAlphabetical detects it, rather than by trusting which axis
// this is.
func narrowAxis(cat *librarycat.Catalog, entries []librarycat.AxisEntry, source string) ([]librarycat.AxisEntry, error) {
	sourceID, err := parseSourceFilter(source)
	if err != nil {
		return nil, err
	}
	allow := sourceAlbumSet(cat, sourceID)
	if allow == nil {
		return entries, nil
	}
	alphabetical := axisIsAlphabetical(entries)
	out := make([]librarycat.AxisEntry, 0, len(entries))
	for _, e := range entries {
		ids, counts, tracks, ok := narrowGroup(e.AlbumIDs, e.AlbumTracks, allow)
		if !ok {
			continue
		}
		e.AlbumIDs, e.AlbumTracks, e.TrackCount = ids, counts, tracks
		out = append(out, e)
	}
	if !alphabetical {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].TrackCount != out[j].TrackCount {
				return out[i].TrackCount > out[j].TrackCount
			}
			return out[i].ID < out[j].ID
		})
	}
	return out, nil
}
