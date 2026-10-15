package upnp

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
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

	// MaxContainers caps the containers the walk Browses (backlog B164).
	// MaxItems counts audio items only, so a tree of empty containers —
	// from a hostile, broken or spoofed upstream — never met it, and
	// MaxDepth bounds a chain, not a wide tree: four children a level is
	// 16.7M containers by depth 12. Defaults to defaultMaxContainers.
	// Reaching it ends the walk with ErrWalkContainerBudget, an
	// ErrWalkTruncated, so the ingest reaps nothing and walks again on its
	// next tick.
	MaxContainers int

	// MaxDuration caps the walk's wall time (backlog B164): an upstream that
	// answers each Browse just inside its request timeout is bounded by no
	// count. Defaults to defaultMaxDuration. Checked between Browse calls;
	// reaching it ends the walk with ErrWalkTimeBudget, an ErrWalkTruncated.
	MaxDuration time.Duration

	// now is the walk's clock, for tests; nil is time.Now.
	now func() time.Time

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
	// CyclesSkipped counts child containers not walked because they are an
	// ancestor of the container that lists them (backlog B164). Skipping one
	// loses nothing: its subtree is the one being walked already.
	CyclesSkipped int
}

// defaultMaxContainers is the container budget of a walk whose caller sets
// none: a Browse per container, as many as the default MaxItems allows tracks.
// A real folder view holds an album or so per folder, so it never comes near.
const defaultMaxContainers = 50_000

// defaultMaxDuration is the time budget of a walk whose caller sets none.
const defaultMaxDuration = 4 * time.Hour

// ErrWalkTruncated is returned when MaxItems is hit before EOF — the
// partial results MUST NOT be treated as authoritative (a sync-style
// ingest would otherwise misread the truncation as "every track past N
// was deleted"). Mirrors ErrBrowseLimit's contract.
var ErrWalkTruncated = errors.New("upnp: Browse Folders walk hit MaxItems ceiling")

// ErrWalkContainerBudget and ErrWalkTimeBudget end a walk that reached
// MaxContainers or MaxDuration (backlog B164). Each is an ErrWalkTruncated to
// errors.Is, so every guard that refuses to reap a truncated walk refuses
// these too, with a message that names the budget rather than MaxItems.
var (
	ErrWalkContainerBudget error = walkBudgetError("upnp: Browse Folders walk hit its container budget (MaxContainers)")
	ErrWalkTimeBudget      error = walkBudgetError("upnp: Browse Folders walk ran past its time budget (MaxDuration)")
)

type walkBudgetError string

func (e walkBudgetError) Error() string { return string(e) }

// Is makes a budget error an ErrWalkTruncated.
func (e walkBudgetError) Is(target error) bool { return target == ErrWalkTruncated }

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
	if yield == nil {
		return WalkStats{}, errors.New("upnp: yield function is required")
	}
	if opts.MaxItems <= 0 {
		opts.MaxItems = 50_000
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 12
	}
	if opts.MaxContainers <= 0 {
		opts.MaxContainers = defaultMaxContainers
	}
	if opts.MaxDuration <= 0 {
		opts.MaxDuration = defaultMaxDuration
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	skip := make(map[string]struct{}, len(opts.SkipContainerTitles))
	for _, t := range opts.SkipContainerTitles {
		// Drop empty/whitespace-only entries — an empty key would skip
		// any container whose title trims to "". The admin API sanitises
		// this list, but a hand-edited bridge.yaml reaches the walker raw
		// (ingest passes srv.SkipTopLevelContainers directly).
		if key := strings.ToLower(strings.TrimSpace(t)); key != "" {
			skip[key] = struct{}{}
		}
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
		now:        now,
		deadline:   now().Add(opts.MaxDuration),
		ancestors:  map[string]struct{}{},
	}
	// The time budget also bounds a single container's pages: BrowseAll
	// follows one container's pages until the server says it is done (up to
	// MaxBrowseAllItems), checking only its context between them, so a
	// deadline checked only between containers let one container's one-item
	// pages run past it (CodeRabbit on #1241). The context's deadline is the
	// wall clock's; the check between containers keeps w.now, so a test can
	// drive it.
	walkCtx, cancelWalk := context.WithDeadline(ctx, time.Now().Add(opts.MaxDuration))
	defer cancelWalk()
	w.parent = ctx
	if err := w.recurse(walkCtx, opts.RootObjectID, prefix, 0, true); err != nil {
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
	now        func() time.Time
	deadline   time.Time
	// parent is the caller's context: the walk's own (recurse's) is the
	// caller's with the time budget's deadline, and only an expiry the
	// caller's does not share is the budget's (timeBudgetSpent).
	parent context.Context
	// ancestors holds the ids of the containers on the path being walked,
	// the one being Browsed included (backlog B164).
	ancestors map[string]struct{}
}

// recurse Browses one container, recurses into child containers, yields
// child items. `pathSoFar` is the joined directory chain (no leading /).
// `atRoot` flips the SkipContainerTitles gate on/off.
func (w *walker) recurse(ctx context.Context, objectID, pathSoFar string, depth int, atRoot bool) error {
	if err := ctx.Err(); err != nil {
		if w.timeBudgetSpent(ctx) {
			return ErrWalkTimeBudget
		}
		return err
	}
	if depth > w.opts.MaxDepth {
		return fmt.Errorf("upnp: walk exceeded MaxDepth=%d at %q", w.opts.MaxDepth, pathSoFar)
	}
	if w.stats.Containers >= w.opts.MaxContainers {
		return ErrWalkContainerBudget
	}
	if !w.now().Before(w.deadline) {
		return ErrWalkTimeBudget
	}
	w.stats.Containers++
	w.ancestors[objectID] = struct{}{}
	defer delete(w.ancestors, objectID)

	containers, items, err := w.client.BrowseAll(ctx, w.controlURL, objectID)
	if err != nil && w.timeBudgetSpent(ctx) {
		return ErrWalkTimeBudget
	}
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
		// A container that lists one of its own ancestors is a cycle: walking
		// into it would Browse the same subtree again until MaxDepth, and a
		// cycle with two ways round multiplies at every level. The same
		// container under two different parents is not a cycle and is still
		// walked under each (backlog B164).
		if _, cycle := w.ancestors[ct.ID]; cycle {
			w.stats.CyclesSkipped++
			continue
		}
		childPath := joinPath(pathSoFar, containerPathComponent(ct.Title, ct.ID))
		if err := w.recurse(ctx, ct.ID, childPath, depth+1, false); err != nil {
			return err
		}
	}
	return nil
}

// timeBudgetSpent reports whether the walk's context has expired while the
// caller's has not: the time budget, not the caller, ended it. Decided from
// the two contexts rather than the error chain, since a Browse cut short
// mid-request may report its cancellation in a form errors.Is cannot see.
func (w *walker) timeBudgetSpent(ctx context.Context) bool {
	return ctx.Err() != nil && (w.parent == nil || w.parent.Err() == nil)
}

// looksLikeAudioItem filters out non-audio items (the 2Go's MediaServer
// also serves Pictures + Video). We can't fully trust upnp:class — some
// servers leave it blank — so also accept items whose protocolInfo
// claims an audio MIME or whose res URL ends with a known audio ext.
func looksLikeAudioItem(it Object) bool {
	if it.Res == "" {
		return false
	}
	// class / protocolInfo are overwhelmingly already-lowercase in the
	// wild, so match the raw string first and only pay the
	// strings.ToLower allocation on a miss — this runs per item across
	// the whole walk. Gemini r4.
	if classIsAudioItem(it.Class) {
		return true
	}
	if protocolInfoIsAudio(it.ProtocolInfo) {
		return true
	}
	return isKnownAudioExt(extFromURL(it.Res))
}

// classIsAudioItem reports whether class names an audioItem
// (case-insensitive), allocation-free for ALL inputs: strings.EqualFold
// on the leading len(want) bytes avoids the strings.ToLower copy a
// mixed-case input would otherwise trigger (Gemini r4 round 1). `want`
// is pure ASCII, so the byte-length prefix slice is rune-safe — a
// multi-byte rune in those bytes can't equal-fold the ASCII target and
// correctly returns false.
func classIsAudioItem(class string) bool {
	const want = "object.item.audioitem"
	if len(class) < len(want) {
		return false
	}
	return strings.EqualFold(class[:len(want)], want)
}

// protocolInfoIsAudio reports whether pi carries an ":audio/" MIME token
// (case-insensitive), allocation-free on the common all-lowercase input.
func protocolInfoIsAudio(pi string) bool {
	if strings.Contains(pi, ":audio/") {
		return true
	}
	return strings.Contains(strings.ToLower(pi), ":audio/")
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
	// Sanitize, and if the title had nothing to keep, take the ID form the
	// empty-title branch takes — the same fallback for the same fact,
	// applied AFTER the sanitizer as well as before it, so the stem is
	// never empty (".flac", "01 - .flac") or the bare floor when an ID is
	// there to name the track.
	if stem := sanitizePathComponentOrEmpty(title); stem != "" {
		title = stem
	} else {
		title = sanitizePathComponent("_" + strings.TrimSpace(it.ID))
	}
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

// pathComponentNeedsSanitize reports whether s carries a byte that
// sanitizePathComponent must rewrite: a path separator ('/' or '\\') or
// a control char (< 0x20). Allocation-free; safe on UTF-8 because
// multi-byte runes are all >= 0x80 and never collide with these ASCII
// targets. Extracted from sanitizePathComponent to keep that function
// under the cognitive-complexity ceiling (Sonar S3776).
func pathComponentNeedsSanitize(s string) bool {
	for i := 0; i < len(s); i++ {
		if b := s[i]; b == '/' || b == '\\' || b < 0x20 {
			return true
		}
	}
	return false
}

// sanitizePathComponent strips '/' and '\\' (path separators on POSIX +
// Windows) + control chars from a directory or filename component so the
// joined Path never breaks the path.Clean invariant we rely on.
// ASCII-conservative: anything else (Unicode letters, spaces, punctuation
// in titles) is preserved.
func sanitizePathComponent(s string) string {
	if out := sanitizePathComponentOrEmpty(s); out != "" {
		return out
	}
	if strings.TrimSpace(s) == "" {
		return ""
	}
	// A non-empty input that was nothing but the bytes the sanitizer drops
	// used to come back "", which joinPath then skipped — a child folder
	// whose path was its PARENT's. Unreachable through the DIDL walk
	// (encoding/xml refuses every dropped byte except the whitespace
	// TrimSpace removes first), but this is a plain function and the next
	// caller may not parse XML; a non-empty input never sanitizes to
	// nothing. Callers that can do better take the OrEmpty form and fall
	// back to the ObjectID, which is unique where this floor is not.
	return "_"
}

// sanitizePathComponentOrEmpty is sanitizePathComponent without the floor:
// "" when nothing survives, so a caller holding a better fallback (the
// ObjectID) can tell "nothing to keep" from a title that IS "_".
func sanitizePathComponentOrEmpty(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Fast path: clean components — the overwhelming majority across a
	// full library walk — return the (already-trimmed) string without
	// allocating a Builder. The dirty-byte scan lives in a helper so
	// this function stays under the cognitive-complexity ceiling
	// (Gemini r4 fast-path; Sonar S3776 round 1).
	if !pathComponentNeedsSanitize(s) {
		// Still neutralize the two path.Clean-special segments (see below).
		if s == "." || s == ".." {
			return "_"
		}
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			// '\\' too: the bridge ships for Windows, where a backslash
			// is a path separator — a "..\\.." title would otherwise
			// survive the "."/".." check below and traverse (Gemini
			// security-high on PR #359).
			b.WriteByte('-')
		case r == 0x00 || r < 0x20:
			// drop control chars silently
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	// Neutralize the two segments path.Clean treats specially: a
	// container title of "." or ".." is attacker-controllable (a
	// malicious or misconfigured upstream MediaServer) and would
	// otherwise survive into the joined Path as a traversal segment,
	// breaking the path.Clean invariant this sanitizer exists to protect.
	if out == "." || out == ".." {
		return "_"
	}
	return out
}

// containerPathComponent is the path segment a container contributes: its
// sanitized title, or — when the title is empty or has nothing the sanitizer
// keeps — the volatile ObjectID in the `_<id>` form. A folder with no usable
// title would collide with its siblings, or worse collapse onto its PARENT's
// path when joinPath skipped an empty component; the ObjectID is unique where
// the sanitizer's "_" floor is not. Decided AFTER sanitizing, so a title that
// only looked non-empty takes the same fallback the empty one does.
func containerPathComponent(title, id string) string {
	if name := sanitizePathComponentOrEmpty(title); name != "" {
		return name
	}
	return sanitizePathComponent("_" + id)
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
	// protocolInfo MIME tokens are overwhelmingly already lowercase;
	// match the raw string first and only pay the strings.ToLower
	// allocation on a miss. Gemini r4.
	if ext := matchProtocolInfoExt(pi); ext != "" {
		return ext
	}
	return matchProtocolInfoExt(strings.ToLower(pi))
}

func matchProtocolInfoExt(pi string) string {
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
	case "flac", "dsf", "dff", "wav", "aiff", "aif", "aifc", "m4a", "mp3", "mp4", "ogg", "opus":
		return true
	}
	return false
}
