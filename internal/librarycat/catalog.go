package librarycat

import (
	"crypto/sha256"
	"encoding/hex"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// Row is one track's catalog-relevant projection.
//
// Deliberately a SEPARATE struct from dupes.Row rather than an
// embedding or an alias: dupes.Row is a mirror struct whose field set
// is part of the iOS contract, and growing it with genre / composer /
// MBIDs — none of which the client key uses — would invite keying the
// mirror on non-mirror fields. dupeRow() adapts.
type Row struct {
	Path        string
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	Year        int
	Disc        int
	DiscTagged  bool
	Track       int
	TrackTagged bool
	Size        int64
	Duration    float64

	SampleRate    int
	BitsPerSample int
	IsDSD         bool
	Codec         string

	Genre          string
	Composer       string
	ArtworkMBID    string
	ArtworkVersion string
	ReleaseMBID    string
	ArtistMBID     string
	IndexedAt      int64
	RoutedUDN      string // "" for a filesystem track
}

func (r Row) dupeRow() dupes.Row {
	return dupes.Row{
		Path: r.Path, Title: r.Title, Album: r.Album, AlbumArtist: r.AlbumArtist,
		Artist: r.Artist, Year: r.Year,
		Disc: r.Disc, DiscTagged: r.DiscTagged,
		Track: r.Track, TrackTagged: r.TrackTagged,
		Size: r.Size, Duration: r.Duration,
		SampleRate: r.SampleRate, BitsPerSample: r.BitsPerSample,
		IsDSD: r.IsDSD, Codec: r.Codec,
	}
}

// Album is one grouped release.
type Album struct {
	ID          string
	Title       string
	AlbumArtist string
	ArtistID    string
	Year        int
	TrackCount  int
	DiscCount   int
	Duration    float64
	SizeBytes   int64

	Quality     QualityBucket
	QualityMask QualityMask
	RateHz      int
	Bits        int

	ArtworkMBID    string
	ArtworkVersion string
	ReleaseMBID    string
	FolderPath     string

	AddedAt   int64
	UpdatedAt int64

	SortTitle  string
	SortArtist string
	Bucket     string

	Routed     bool
	RoutedUDNs []string

	// TrackPaths are the member paths, pre-sorted by (disc, track,
	// path). Paths only, not hydrated rows: album detail re-queries by
	// path so variant / analysis / favourite state is fresh without a
	// catalog rebuild, and holding 50k full rows in the snapshot is the
	// memory the OOM discipline exists to avoid.
	TrackPaths []string
}

// Artist is one artist row. Every "; " segment of a track's artist AND
// album-artist gets one, mirroring the client's secondary-artist rows —
// a featured credit is findable without owning album identity.
type Artist struct {
	ID         string
	Name       string
	SortName   string
	Bucket     string
	TrackCount int
	AlbumCount int
	AlbumIDs   []string
	ArtistMBID string
	AddedAt    int64
}

// AxisEntry mirrors iOS LibraryAxisEntry field-for-field: one genre or
// composer group.
type AxisEntry struct {
	ID          string
	DisplayName string
	SortName    string
	Bucket      string
	TrackCount  int
	AlbumIDs    []string
	RawVariants []string
}

// Stats are the whole-library totals a header can render without
// walking the slices.
type Stats struct {
	Tracks       int
	Albums       int
	Artists      int
	RoutedTracks int
}

// Catalog is one immutable snapshot. Never mutated after Build
// returns — the admin server publishes it through an atomic.Pointer,
// so a reader holding an older snapshot must stay correct.
type Catalog struct {
	BuiltAt   time.Time
	Albums    []Album
	Artists   []Artist
	Genres    []AxisEntry
	Composers []AxisEntry
	Stats     Stats

	albumIndex  map[string]int
	artistIndex map[string]int
	genreIndex  map[string]int
	compIndex   map[string]int
	trackAlbum  map[string]string
}

// AlbumByID returns the album and whether it exists.
func (c *Catalog) AlbumByID(id string) (Album, bool) {
	i, ok := c.albumIndex[id]
	if !ok {
		return Album{}, false
	}
	return c.Albums[i], true
}

// ArtistByID returns the artist and whether it exists.
func (c *Catalog) ArtistByID(id string) (Artist, bool) {
	i, ok := c.artistIndex[id]
	if !ok {
		return Artist{}, false
	}
	return c.Artists[i], true
}

// GenreByID / ComposerByID resolve an axis entry.
func (c *Catalog) GenreByID(id string) (AxisEntry, bool) {
	i, ok := c.genreIndex[id]
	if !ok {
		return AxisEntry{}, false
	}
	return c.Genres[i], true
}

func (c *Catalog) ComposerByID(id string) (AxisEntry, bool) {
	i, ok := c.compIndex[id]
	if !ok {
		return AxisEntry{}, false
	}
	return c.Composers[i], true
}

// AlbumIDForPath maps a track path to its album, for hydrating
// favourites, playlists, mixes and search hits.
func (c *Catalog) AlbumIDForPath(p string) (string, bool) {
	id, ok := c.trackAlbum[p]
	return id, ok
}

// HashID is the URL-safe stable id for a natural key. The natural keys
// here (an albumID is "<artist>|<album>|<year>", an axis key is
// arbitrary tag text) contain "|", "/", spaces and any Unicode, so
// they cannot be path segments. A 16-hex digest is bounded-alphabet,
// so a route regex can validate it BEFORE any lookup — the same
// discipline the artwork routes use.
func HashID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// Builder accumulates rows into a Catalog. Not safe for concurrent
// use; build one per snapshot.
type Builder struct {
	albums    map[string]*albumAcc
	artists   map[string]*artistAcc
	genres    map[string]*axisAcc
	composers map[string]*axisAcc
	stats     Stats
}

type voteMap map[string]int

func (v voteMap) add(s string) {
	if s != "" {
		v[s]++
	}
}

// pickVoted returns the most-voted value; ties break on the
// lexicographically smallest so the result never depends on map
// iteration or arrival order.
func (v voteMap) pick() string {
	best, bestN := "", -1
	for s, n := range v {
		if n > bestN || (n == bestN && s < best) {
			best, bestN = s, n
		}
	}
	return best
}

type albumAcc struct {
	id           string
	titleVotes   voteMap
	artistVotes  voteMap
	yearVotes    map[int]int
	coverVotes   voteMap
	versionVotes voteMap
	releaseVotes voteMap
	qualityVotes map[QualityBucket]int
	rateVotes    map[int]int
	bitsVotes    map[int]int
	mask         QualityMask
	discs        map[int]struct{}
	udns         map[string]struct{}
	routedAll    bool
	tracks       []albumTrack
	duration     float64
	size         int64
	addedAt      int64
	updatedAt    int64
	dirs         []string
}

type albumTrack struct {
	path  string
	disc  int
	track int
}

type artistAcc struct {
	id        string
	nameVotes voteMap
	mbidVotes voteMap
	tracks    int
	albums    map[string]int
	addedAt   int64
}

type axisAcc struct {
	id          string
	nameVotes   voteMap
	tracks      int
	albums      map[string]int
	rawVariants map[string]struct{}
}

// New returns an empty Builder.
func New() *Builder {
	return &Builder{
		albums:    map[string]*albumAcc{},
		artists:   map[string]*artistAcc{},
		genres:    map[string]*axisAcc{},
		composers: map[string]*axisAcc{},
	}
}

// Add folds one row in. Order-independent: every tie-break resolves on
// values, never on arrival, which TestFoldIsInputOrderIndependent pins.
func (b *Builder) Add(r Row) {
	res := dupes.Resolve(r.dupeRow())
	albumID := dupes.AlbumIDOf(res)

	b.stats.Tracks++
	if r.RoutedUDN != "" {
		b.stats.RoutedTracks++
	}

	a := b.albums[albumID]
	if a == nil {
		a = &albumAcc{
			id: albumID, titleVotes: voteMap{}, artistVotes: voteMap{},
			yearVotes: map[int]int{}, coverVotes: voteMap{}, versionVotes: voteMap{},
			releaseVotes: voteMap{}, qualityVotes: map[QualityBucket]int{},
			rateVotes: map[int]int{}, bitsVotes: map[int]int{},
			discs: map[int]struct{}{}, udns: map[string]struct{}{},
			routedAll: true, addedAt: r.IndexedAt, updatedAt: r.IndexedAt,
		}
		b.albums[albumID] = a
	}
	// Title votes use the CLEANED display form, matching iOS
	// Album.displayName — the routed library's folder names carry
	// bracket cruft like "[A] What's Up- [134283123] [2019]".
	a.titleVotes.add(dupes.CleanDisplayName(res.Album))
	a.artistVotes.add(res.AlbumArtist)
	if res.Year > 0 {
		a.yearVotes[res.Year]++
	}
	a.coverVotes.add(r.ArtworkMBID)
	a.versionVotes.add(r.ArtworkVersion)
	a.releaseVotes.add(r.ReleaseMBID)

	q := Classify(r.Codec, r.SampleRate, r.BitsPerSample, r.IsDSD)
	a.qualityVotes[q]++
	a.mask.add(q)
	if r.SampleRate > 0 {
		a.rateVotes[r.SampleRate]++
	}
	if r.BitsPerSample > 0 {
		a.bitsVotes[r.BitsPerSample]++
	}
	a.discs[res.Disc] = struct{}{}
	if r.RoutedUDN != "" {
		a.udns[r.RoutedUDN] = struct{}{}
	} else {
		a.routedAll = false
	}
	a.tracks = append(a.tracks, albumTrack{path: r.Path, disc: res.Disc, track: res.Track})
	a.duration += r.Duration
	a.size += r.Size
	if r.IndexedAt > 0 {
		if a.addedAt == 0 || r.IndexedAt < a.addedAt {
			a.addedAt = r.IndexedAt
		}
		if r.IndexedAt > a.updatedAt {
			a.updatedAt = r.IndexedAt
		}
	}
	a.dirs = append(a.dirs, path.Dir(r.Path))

	// Artists: every segment of BOTH credits, so a featured artist is
	// findable without owning album identity.
	for _, name := range artistSegmentsFor(res) {
		key := dupes.Normalize(name)
		if key == "" {
			continue
		}
		ar := b.artists[key]
		if ar == nil {
			ar = &artistAcc{id: key, nameVotes: voteMap{}, mbidVotes: voteMap{},
				albums: map[string]int{}, addedAt: r.IndexedAt}
			b.artists[key] = ar
		}
		ar.nameVotes.add(name)
		ar.mbidVotes.add(r.ArtistMBID)
		ar.tracks++
		ar.albums[albumID]++
		if r.IndexedAt > 0 && (ar.addedAt == 0 || r.IndexedAt < ar.addedAt) {
			ar.addedAt = r.IndexedAt
		}
	}

	addAxis(b.genres, genreSegments(r.Genre), albumID)
	addAxis(b.composers, composerSegments(r.Composer), albumID)
}

// artistSegmentsFor returns the deduped credit segments for one row —
// album artist first so it wins the display vote on ties.
func artistSegmentsFor(res dupes.Resolved) []string {
	segs := dupes.SplitArtistDisplayName(res.AlbumArtist)
	segs = append(segs, dupes.SplitArtistDisplayName(res.Artist)...)
	seen := map[string]struct{}{}
	out := segs[:0]
	for _, s := range segs {
		k := dupes.Normalize(s)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
}

func addAxis(dst map[string]*axisAcc, segs []Segment, albumID string) {
	for _, seg := range segs {
		e := dst[seg.Key]
		if e == nil {
			e = &axisAcc{id: seg.Key, nameVotes: voteMap{},
				albums: map[string]int{}, rawVariants: map[string]struct{}{}}
			dst[seg.Key] = e
		}
		e.nameVotes.add(seg.Display)
		e.rawVariants[seg.Display] = struct{}{}
		e.tracks++
		e.albums[albumID]++
	}
}

// Build finalises the snapshot. Every ordering below is total, so two
// builds from the same rows in any order are DeepEqual.
func (b *Builder) Build(now time.Time) *Catalog {
	c := &Catalog{
		BuiltAt:     now,
		albumIndex:  make(map[string]int, len(b.albums)),
		artistIndex: make(map[string]int, len(b.artists)),
		genreIndex:  make(map[string]int, len(b.genres)),
		compIndex:   make(map[string]int, len(b.composers)),
		trackAlbum:  make(map[string]string, b.stats.Tracks),
	}

	c.Albums = make([]Album, 0, len(b.albums))
	for _, a := range b.albums {
		c.Albums = append(c.Albums, a.finish(c.trackAlbum))
	}
	sort.Slice(c.Albums, func(i, j int) bool {
		x, y := c.Albums[i], c.Albums[j]
		if cmp := naturalCompare(x.SortArtist, y.SortArtist); cmp != 0 {
			return cmp < 0
		}
		if cmp := naturalCompare(x.SortTitle, y.SortTitle); cmp != 0 {
			return cmp < 0
		}
		return x.ID < y.ID
	})
	for i, a := range c.Albums {
		c.albumIndex[a.ID] = i
	}

	c.Artists = make([]Artist, 0, len(b.artists))
	for _, ar := range b.artists {
		c.Artists = append(c.Artists, ar.finish())
	}
	sort.Slice(c.Artists, func(i, j int) bool {
		x, y := c.Artists[i], c.Artists[j]
		if cmp := naturalCompare(x.SortName, y.SortName); cmp != 0 {
			return cmp < 0
		}
		return x.ID < y.ID
	})
	for i, ar := range c.Artists {
		c.artistIndex[ar.ID] = i
	}

	// Genres order by track count desc then name — the iOS convention;
	// composers order alphabetically by surname-first sort name.
	c.Genres = finishAxis(b.genres, false)
	for i, g := range c.Genres {
		c.genreIndex[g.ID] = i
	}
	c.Composers = finishAxis(b.composers, true)
	for i, cp := range c.Composers {
		c.compIndex[cp.ID] = i
	}

	b.stats.Albums = len(c.Albums)
	b.stats.Artists = len(c.Artists)
	c.Stats = b.stats
	return c
}

func (a *albumAcc) finish(trackAlbum map[string]string) Album {
	title := a.titleVotes.pick()
	albumArtist := a.artistVotes.pick()

	sort.Slice(a.tracks, func(i, j int) bool {
		x, y := a.tracks[i], a.tracks[j]
		if x.disc != y.disc {
			return x.disc < y.disc
		}
		if x.track != y.track {
			return x.track < y.track
		}
		return x.path < y.path
	})
	// The index publishes the HASHED id, not the natural key: every
	// other public surface (Album.ID, AlbumIDs, the route parameter)
	// is the hash, and a lookup that returned the natural key would
	// resolve against nothing.
	publicID := HashID(a.id)
	paths := make([]string, len(a.tracks))
	for i, t := range a.tracks {
		paths[i] = t.path
		trackAlbum[t.path] = publicID
	}

	udns := make([]string, 0, len(a.udns))
	for u := range a.udns {
		udns = append(udns, u)
	}
	sort.Strings(udns)

	sortArtist := sortKey(albumArtist)
	return Album{
		ID:          publicID,
		Title:       title,
		AlbumArtist: albumArtist,
		ArtistID:    HashID(dupes.ArtistID(albumArtist)),
		Year:        pickModalInt(a.yearVotes),
		TrackCount:  len(a.tracks),
		DiscCount:   len(a.discs),
		Duration:    a.duration,
		SizeBytes:   a.size,

		Quality:     pickQuality(a.qualityVotes),
		QualityMask: a.mask,
		RateHz:      pickModalInt(a.rateVotes),
		Bits:        pickModalInt(a.bitsVotes),

		ArtworkMBID:    pickCoverRef(a.coverVotes),
		ArtworkVersion: a.versionVotes.pick(),
		ReleaseMBID:    a.releaseVotes.pick(),
		FolderPath:     commonDir(a.dirs),

		AddedAt:   a.addedAt,
		UpdatedAt: a.updatedAt,

		SortTitle:  sortKey(title),
		SortArtist: sortArtist,
		Bucket:     bucket(sortArtist),

		Routed:     a.routedAll && len(a.udns) > 0,
		RoutedUDNs: udns,
		TrackPaths: paths,
	}
}

func (ar *artistAcc) finish() Artist {
	name := ar.nameVotes.pick()
	albumIDs := rankAlbums(ar.albums)
	sn := sortKey(name)
	return Artist{
		ID:         HashID(ar.id),
		Name:       name,
		SortName:   sn,
		Bucket:     bucket(sn),
		TrackCount: ar.tracks,
		AlbumCount: len(albumIDs),
		AlbumIDs:   albumIDs,
		ArtistMBID: ar.mbidVotes.pick(),
		AddedAt:    ar.addedAt,
	}
}

func finishAxis(src map[string]*axisAcc, composer bool) []AxisEntry {
	out := make([]AxisEntry, 0, len(src))
	for _, e := range src {
		display := e.nameVotes.pick()
		variants := make([]string, 0, len(e.rawVariants))
		for v := range e.rawVariants {
			variants = append(variants, v)
		}
		// Sorted so composerSortName's "first comma-form variant wins"
		// pick is deterministic — the Swift docstring requires it.
		sort.Strings(variants)
		sn := display
		if composer {
			sn = composerSortName(display, variants)
		}
		key := sortKey(sn)
		out = append(out, AxisEntry{
			ID:          HashID(e.id),
			DisplayName: display,
			SortName:    key,
			Bucket:      bucket(key),
			TrackCount:  e.tracks,
			AlbumIDs:    rankAlbums(e.albums),
			RawVariants: variants,
		})
	}
	if composer {
		sort.Slice(out, func(i, j int) bool {
			if cmp := naturalCompare(out[i].SortName, out[j].SortName); cmp != 0 {
				return cmp < 0
			}
			return out[i].ID < out[j].ID
		})
		return out
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TrackCount != out[j].TrackCount {
			return out[i].TrackCount > out[j].TrackCount
		}
		if cmp := naturalCompare(out[i].SortName, out[j].SortName); cmp != 0 {
			return cmp < 0
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// rankAlbums orders an axis/artist's albums by in-group track count
// desc, then album key asc. albumIDs[0] is the group's primary tile
// cover, so a Set-order fold would reshuffle it on every rebuild —
// this ordering is what makes it stable.
func rankAlbums(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for i, k := range keys {
		keys[i] = HashID(k)
	}
	return keys
}

func pickModalInt(votes map[int]int) int {
	best, bestN := 0, 0
	for v, n := range votes {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

func pickQuality(votes map[QualityBucket]int) QualityBucket {
	best, bestN := QualityUnknown, -1
	for q, n := range votes {
		if n > bestN || (n == bestN && qualityRank(q) > qualityRank(best)) {
			best, bestN = q, n
		}
	}
	return best
}

// pickCoverRef mirrors the rule the Library Inspector already uses
// (moved here so the album grid and the inspector tiles cannot drift
// on which cover represents a group): most votes wins; a UUID outranks
// a local-<sha> sentinel at equal votes; lexicographic tie-break for
// determinism.
func pickCoverRef(votes voteMap) string {
	best, bestVotes, bestLocal := "", -1, true
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

// commonDir is the longest shared directory prefix of the members —
// the album's folder, used for booklet lookup and "reveal in browse".
// Compares whole segments so two sibling directories that merely share
// a name prefix ("Live" and "Live Deluxe") don't produce a path that
// exists on neither.
func commonDir(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	common := strings.Split(dirs[0], "/")
	for _, d := range dirs[1:] {
		segs := strings.Split(d, "/")
		n := len(common)
		if len(segs) < n {
			n = len(segs)
		}
		i := 0
		for i < n && common[i] == segs[i] {
			i++
		}
		common = common[:i]
		if len(common) == 0 {
			return ""
		}
	}
	return strings.Join(common, "/")
}
