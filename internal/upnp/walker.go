package upnp

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
)

// Walked is a single track yielded by a Browse-Folders walk. Path is the
// stable, filesystem-derived form the bridge persists as identity (the
// SHA-256 trackID hashes on it); ObjectID + Res are the volatile locators
// the file proxy re-resolves at fetch time.
type Walked struct {
	// Path is the stable identity: pathPrefix + joined directory chain +
	// "<NN - Title>.<ext>". Directory titles come from the actual
	// filesystem tree (MiniDLNA's Browse Folders view); track filename
	// is synthesized from the metadata-title + the <res> file extension
	// (the upstream's DIDL never exposes the source filename). Stable
	// across MiniDLNA DB rebuilds + reboots; CHANGES when the user
	// edits a title tag (rare, acceptable per the plan).
	Path string

	// ObjectID is the ContentDirectory item id at walk time. VOLATILE
	// (position-based on MiniDLNA) — stored as a Tier-2 resolution
	// hint, NOT identity.
	ObjectID string

	// ParentObjectID is the container the item lives under. Powers the
	// "amortized parent re-resolution" cache-heal path.
	ParentObjectID string

	// Res is the upstream HTTP URL captured at walk time. VOLATILE
	// (host:port can float on a Wi-Fi/hotspot swap, numeric file id
	// renumbers on a full DB rebuild). The file proxy reconstructs
	// host:port from the live SSDP registry + re-Browses on 404.
	Res           string
	ProtocolInfo  string
	Size          int64
	Duration      string // raw "H:MM:SS.mmm"
	SampleRate    int
	BitsPerSample int
	Channels      int

	// Metadata fields populated from DIDL.
	Title       string
	Artist      string
	Album       string
	Genre       string
	Date        string
	TrackNumber int
	AlbumArtURI string

	// AlbumPath is the parent-folder Path of this item (the album
	// container's stable path). Lets the proxy's parent-re-Browse heal
	// every sibling track in one round-trip on a global id scramble.
	AlbumPath string
}

// WalkOptions configures BrowseFoldersWalk.
type WalkOptions struct {
	// RootObjectID is the ContentDirectory id whose subtree is walked.
	// MiniDLNA exposes the filesystem view under id "64" ("Browse
	// Folders"); other servers may differ. Caller picks the right id
	// by inspecting the root Browse (id "0").
	RootObjectID string

	// PathPrefix is prepended to every yielded Path so multi-server
	// setups can't collide (e.g. "Chord 2Go" + "/Music/Artist/...").
	// Trimmed of surrounding slashes; "" allowed.
	PathPrefix string

	// MaxItems caps the total tracks yielded as a defense-in-depth
	// against a runaway server. Defaults to 50_000.
	MaxItems int

	// MaxDepth caps recursion. Defaults to 12 — deep enough for any
	// realistic library hierarchy; shallow enough to fail loudly on a
	// pathological/cyclic server.
	MaxDepth int

	// SkipContainerTitles is an optional set of (case-insensitive) folder
	// titles to skip at the TOP LEVEL of the walk — MiniDLNA exposes
	// `System Volume Information` alongside `Music` and we don't want
	// to walk it. Matched only against direct children of RootObjectID.
	SkipContainerTitles []string
}

// WalkStats is the post-walk summary (logging + telemetry).
type WalkStats struct {
	Containers int // total containers visited (incl. RootObjectID)
	Items      int // total tracks yielded
	Truncated  bool
}

// ErrWalkTruncated is returned when MaxItems is hit before EOF — the
// partial results MUST NOT be treated as authoritative (a sync-style
// ingest would otherwise misread the truncation as "every track past N
// was deleted"). Mirrors ErrBrowseLimit's contract.
var ErrWalkTruncated = errors.New("upnp: Browse Folders walk hit MaxItems ceiling")

// BrowseFoldersWalk recursively walks a ContentDirectory subtree
// SERIALLY (one Browse at a time — MiniDLNA's libmicrohttpd pool is
// tiny; parallel Browse bursts cause socket timeouts).
//
// For each track item it builds a stable Path from the directory chain
// + a synthesized filename (NN - Title.ext). For each container it
// recurses with the container's title appended to the chain. ctx
// cancellation is honored between every Browse call.
//
// Pure-data callback (yield) so the ingest layer can stream upserts to
// the manifest store without buffering the whole library in RAM.
func BrowseFoldersWalk(
	ctx context.Context,
	client *ContentDirectoryClient,
	controlURL string,
	opts WalkOptions,
	yield func(Walked) error,
) (WalkStats, error) {
	if client == nil {
		return WalkStats{}, errors.New("upnp: nil client")
	}
	if controlURL == "" {
		return WalkStats{}, errors.New("upnp: empty controlURL")
	}
	if opts.RootObjectID == "" {
		return WalkStats{}, errors.New("upnp: RootObjectID is required")
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = 50_000
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 12
	}
	skip := make(map[string]struct{}, len(opts.SkipContainerTitles))
	for _, t := range opts.SkipContainerTitles {
		skip[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	prefix := strings.Trim(strings.TrimSpace(opts.PathPrefix), "/")

	stats := WalkStats{}
	w := &walker{
		client:     client,
		controlURL: controlURL,
		opts:       opts,
		skip:       skip,
		yield:      yield,
		stats:      &stats,
	}
	if err := w.recurse(ctx, opts.RootObjectID, prefix, 0, true); err != nil {
		if errors.Is(err, ErrWalkTruncated) {
			stats.Truncated = true
			return stats, err
		}
		return stats, err
	}
	return stats, nil
}

type walker struct {
	client     *ContentDirectoryClient
	controlURL string
	opts       WalkOptions
	skip       map[string]struct{}
	yield      func(Walked) error
	stats      *WalkStats
}

// recurse Browses one container, recurses into child containers, yields
// child items. `pathSoFar` is the joined directory chain (no leading /).
// `atRoot` flips the SkipContainerTitles gate on/off.
func (w *walker) recurse(ctx context.Context, objectID, pathSoFar string, depth int, atRoot bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > w.opts.MaxDepth {
		return fmt.Errorf("upnp: walk exceeded MaxDepth=%d at %q", w.opts.MaxDepth, pathSoFar)
	}
	w.stats.Containers++

	containers, items, err := w.client.BrowseAll(ctx, w.controlURL, objectID)
	if err != nil && !errors.Is(err, ErrBrowseLimit) {
		// Real error — propagate. (ErrBrowseLimit is per-container;
		// keep going with what we got but mark truncated.)
		return fmt.Errorf("upnp: BrowseAll %q: %w", objectID, err)
	}
	if errors.Is(err, ErrBrowseLimit) {
		w.stats.Truncated = true
	}

	// Yield items first so the parent re-resolution path can heal
	// sibling tracks in one round-trip after a DB rebuild.
	for _, it := range items {
		if w.stats.Items >= w.opts.MaxItems {
			return ErrWalkTruncated
		}
		if !looksLikeAudioItem(it) {
			continue
		}
		filename := synthesizeFilename(it)
		if filename == "" {
			continue
		}
		trackPath := joinPath(pathSoFar, filename)
		if err := w.yield(Walked{
			Path:           trackPath,
			ObjectID:       it.ID,
			ParentObjectID: it.ParentID,
			Res:            it.Res,
			ProtocolInfo:   it.ProtocolInfo,
			Size:           it.Size,
			Duration:       it.Duration,
			SampleRate:     it.SampleRate,
			BitsPerSample:  it.BitsPerSample,
			Channels:       it.Channels,
			Title:          it.Title,
			Artist:         it.Artist,
			Album:          it.Album,
			Genre:          it.Genre,
			Date:           it.Date,
			TrackNumber:    it.TrackNumber,
			AlbumArtURI:    it.AlbumArtURI,
			AlbumPath:      pathSoFar,
		}); err != nil {
			return fmt.Errorf("upnp: yield: %w", err)
		}
		w.stats.Items++
	}

	for _, ct := range containers {
		if atRoot {
			if _, drop := w.skip[strings.ToLower(strings.TrimSpace(ct.Title))]; drop {
				continue
			}
		}
		title := strings.TrimSpace(ct.Title)
		if title == "" {
			// A folder with no title would collide with siblings —
			// fall back to the volatile ObjectID for the path
			// component to keep paths unique.
			title = "_" + ct.ID
		}
		childPath := joinPath(pathSoFar, sanitizePathComponent(title))
		if err := w.recurse(ctx, ct.ID, childPath, depth+1, false); err != nil {
			return err
		}
	}
	return nil
}

// looksLikeAudioItem filters out non-audio items (the 2Go's MediaServer
// also serves Pictures + Video). We can't fully trust upnp:class — some
// servers leave it blank — so also accept items whose protocolInfo
// claims an audio MIME or whose res URL ends with a known audio ext.
func looksLikeAudioItem(it Object) bool {
	if it.Res == "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(it.Class), "object.item.audioitem") {
		return true
	}
	if strings.Contains(strings.ToLower(it.ProtocolInfo), ":audio/") {
		return true
	}
	return isKnownAudioExt(extFromURL(it.Res))
}

// synthesizeFilename builds the leaf component of a track's stable Path.
// MiniDLNA's DIDL doesn't expose the source filename, so we synthesize
// "NN - Title.ext" — stable so long as the metadata title doesn't change.
// Falls back to "<ObjectID>.<ext>" when Title is empty so we never lose
// the track entirely.
func synthesizeFilename(it Object) string {
	ext := strings.ToLower(strings.TrimPrefix(extFromURL(it.Res), "."))
	if ext == "" {
		ext = extFromProtocolInfo(it.ProtocolInfo)
	}
	title := strings.TrimSpace(it.Title)
	if title == "" {
		title = strings.TrimSpace(it.ID)
	}
	if title == "" {
		return ""
	}
	title = sanitizePathComponent(title)
	if it.TrackNumber > 0 {
		return fmt.Sprintf("%02d - %s%s", it.TrackNumber, title, dotIfExt(ext))
	}
	return title + dotIfExt(ext)
}

func dotIfExt(ext string) string {
	if ext == "" {
		return ""
	}
	return "." + ext
}

// sanitizePathComponent strips '/' (path separator) + control chars from
// a directory or filename component so the joined Path never breaks the
// path.Clean invariant we rely on. ASCII-conservative: anything else
// (Unicode letters, spaces, punctuation in titles) is preserved.
func sanitizePathComponent(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '/':
			b.WriteByte('-')
		case r == 0x00 || r < 0x20:
			// drop control chars silently
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// joinPath joins path components with '/'. Empty components are skipped.
func joinPath(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p = strings.Trim(p, "/"); p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, "/")
}

// extFromURL returns the lowercased extension WITHOUT the leading dot
// from the URL's path. Empty string if absent.
func extFromURL(u string) string {
	if u == "" {
		return ""
	}
	// Trim query/fragment so "?id=..." doesn't bleed into the ext.
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	e := strings.TrimPrefix(strings.ToLower(path.Ext(u)), ".")
	return e
}

// extFromProtocolInfo gives a fallback ext from a DLNA protocolInfo
// string (e.g. "http-get:*:audio/x-flac:*" → "flac"). Covers the cases
// where the upstream's URL omits the extension (rare with MiniDLNA, but
// observed on other servers).
func extFromProtocolInfo(pi string) string {
	pi = strings.ToLower(pi)
	switch {
	case strings.Contains(pi, "audio/x-flac"), strings.Contains(pi, "audio/flac"):
		return "flac"
	case strings.Contains(pi, "audio/x-dsf"):
		return "dsf"
	case strings.Contains(pi, "audio/x-dff"), strings.Contains(pi, "audio/dsdiff"):
		return "dff"
	case strings.Contains(pi, "audio/wav"), strings.Contains(pi, "audio/x-wav"):
		return "wav"
	case strings.Contains(pi, "audio/aiff"), strings.Contains(pi, "audio/x-aiff"):
		return "aiff"
	case strings.Contains(pi, "audio/mp4"), strings.Contains(pi, "audio/m4a"), strings.Contains(pi, "audio/x-m4a"):
		return "m4a"
	case strings.Contains(pi, "audio/mpeg"), strings.Contains(pi, "audio/mp3"):
		return "mp3"
	case strings.Contains(pi, "audio/ogg"):
		return "ogg"
	}
	return ""
}

// isKnownAudioExt is a tight allowlist of audio extensions the bridge's
// downstream pipeline already handles bit-exact.
func isKnownAudioExt(ext string) bool {
	switch strings.ToLower(ext) {
	case "flac", "dsf", "dff", "wav", "aiff", "aif", "m4a", "mp3", "mp4", "ogg", "opus":
		return true
	}
	return false
}
