package main

// The version a rendition records, and when a render may start.
//
// A rendition records the version of its source it was made from
// (`track_variants.source_mtime_ns` / `source_size`, and a DSD rendition's
// peak in `dsd_peaks`). Three readers ask whether that version is current,
// on two clocks: the auto-optimize sweeper's candidate query and the album
// gain's manifest.FreshDSDPeaks compare it with the TRACK ROW, and the serve
// path (api.serveVariant) compares it with the FILE ON DISK, because a
// sidecar built from other bytes must never be served.
//
// The two clocks agree whenever the scanner is caught up, and between a
// change to a file and the scan that reads it no stamp satisfies both: a
// render reads the file's new bytes, so one stamped with the row names a
// version it was not made from and the serve path refuses it, and one
// stamped with the file is stale to the sweeper, which renders it again.
// So:
//
//   - Every writer stamps the ROW: the on-demand requests (POST /v1/upscale),
//     the CLI, the sweeper, the batch coordinator and the album survey.
//   - A render is queued only while the file still matches its row
//     (transcode.SourceIsAtRow, the one check). The on-demand path refuses
//     otherwise, and asks for a rescan of the file's directory so the next
//     request can render; the sweeper, the CLI and the batch walks pass the
//     file over until a scan reads it, the batch walks asking for the
//     rescan too. The album survey measures a changed album-mate all the
//     same, since a peak describes the bytes on disk and its row stamp
//     makes the next version measure it again.
//   - transcode.Run checks again when it starts and before it publishes, so
//     a file that changes while its job waits in a queue, or while it
//     renders, is not rendered under the row's older stamp either: the job
//     fails with transcode.ErrSourceChanged, strikes nothing, and the pool
//     asks for the same rescan (backlog B53).
//   - A download that finds a rendition stale renders it again
//     (staleRenditionHeal, backlog B82): at once when the source's row is
//     current, through the on-demand path under the live gate of the
//     rendition's kind (staleRerender), and, when the source changed after
//     its row was written, once the rescan it asks for has read the change.
//     Every rescan drops the album-gain index and nudges the auto-optimize
//     sweep too. With the sweep off, the default, nothing else renders a
//     stale rendition: a batch counts it covered, and a client asks for a
//     new one only once it stops listing the old one.
//
// Before this, the on-demand path and the CLI stamped a live stat while the
// rest stamped the row, and each writer undid the other: measured on a real
// bridge, three rounds of a request and a sweep against two changed files
// made twelve renders, a manifest delta for both tracks on every step, and
// the download answered 200 and 410 in turn (ops/engineering-log.md,
// 2026-09-28).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// errSourceAheadOfRow is the on-demand refusal of a file that changed on
// disk after the scan that wrote its row. It wraps api.ErrUpscaleIneligible,
// which the POST /v1/upscale handler counts as `rejected`: no wire change,
// and the phone plays the source as it does for any refusal.
var errSourceAheadOfRow = fmt.Errorf("the file changed on disk after its last scan: %w", api.ErrUpscaleIneligible)

// sourceRescanQueueCap bounds the directories waiting for a rescan, and so
// the work one burst of requests can queue. Each costs one ScanSubtree, and
// one that re-reads a changed file also runs the whole-library duplicate
// restamp: on the dev Mac, 1.1 s for an album with one changed file over
// 50,012 rows (7 ms with nothing changed), so a full queue there is about
// nineteen minutes of work. Ordinary use stays far below it: the app asks
// for one file at a time, and a folder POST names one directory per album
// under it. A request that finds the queue full is dropped, and its
// directory waits for the periodic scan, or for a later request once there
// is room; a request never waits on a scan.
const sourceRescanQueueCap = 1024

// sourceRescan is one queued directory, under both spellings: the absolute
// one to scan and the library-relative one to log (a log line names a
// library file library-relative).
type sourceRescan struct {
	abs, rel string
}

// sourceRescanner reads again the directory of a file whose row is behind
// it: when an on-demand request for a rendition of that file is refused,
// when a render job or a batch walk finds the file changed since its row
// (transcode.Pool.SetSourceRescan), and when a download finds a rendition
// of it stale (staleRenditionHeal). It makes the refusal cost the client
// one play of the source rather than every play until the periodic scan
// (six hours by default): the scan it runs writes the row the next request
// stamps from.
//
// A directory is queued at most once at a time, and scanned by one loop
// (run), oldest first, one after another and behind any scan in progress:
// ScanSubtree takes the scanner's lock. The queue is the pending set
// itself, so a directory asked for while others wait is kept until the
// loop reaches it, up to sourceRescanQueueCap.
type sourceRescanner struct {
	// resolve maps a library-relative directory onto the directory on disk
	// (the bridge's fs.Resolver). A request names its file by the path its
	// ROW records, and the directory scanned is resolved from that, never
	// taken from a path a client sent: the scanner makes each row's path
	// from the spelling of the directory it is handed, so a case variant of
	// the directory, which a case-insensitive filesystem opens all the same,
	// indexed its files a second time (measured on main at 6dfba62c: a
	// request naming `fixture/dsd/01.dsf` left the rows `Fixture/DSD/01.dsf`
	// and `fixture/dsd/01.dsf`).
	resolve func(rel string) (string, error)
	wake    chan struct{} // one slot: something was queued since run last looked
	mu      sync.Mutex
	waiting []sourceRescan      // queued, oldest first
	pending map[string]struct{} // the absolute directories in waiting
}

func newSourceRescanner(resolve func(rel string) (string, error)) *sourceRescanner {
	return &sourceRescanner{
		resolve: resolve,
		wake:    make(chan struct{}, 1),
		pending: map[string]struct{}{},
	}
}

// request queues the directory holding the file its track row records at
// rel, unless it is already waiting, the queue is full, or rel's directory
// names no library root this bridge has. It never blocks.
//
// An empty rel names no file and is refused before anything resolves it:
// path.Dir("") is ".", which the resolver maps onto a library root, so it
// would queue a walk of the whole root (CLAUDE.md, the rule on reapers that
// refuse an empty root). No caller passes one today; every rescan funnels
// through here, so here is where it is refused. A file AT the root is
// different: its directory is the root, and that walk is the one it needs.
func (r *sourceRescanner) request(rel string) {
	r.queue(rel)
}

// queue is request, reporting whether the directory will be scanned: true
// when it was queued now or was already waiting, false when the request was
// refused (an empty path, a directory that names no root) or dropped
// because the queue is full. The download path's debounce reads it
// (staleRenditionRescan).
func (r *sourceRescanner) queue(rel string) bool {
	if rel == "" {
		return false
	}
	relDir := path.Dir(rel)
	abs, err := r.resolve(relDir)
	if err != nil {
		return false
	}
	dir := sourceRescan{abs: abs, rel: relDir}
	r.mu.Lock()
	_, waiting := r.pending[dir.abs]
	queued := !waiting && len(r.waiting) < sourceRescanQueueCap
	if queued {
		r.pending[dir.abs] = struct{}{}
		r.waiting = append(r.waiting, dir)
	}
	r.mu.Unlock()
	if queued {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	return queued || waiting
}

// next takes the oldest waiting directory. It leaves the pending set here,
// as its scan starts, so a file that changes again while its directory is
// being read is queued again rather than folded into a scan that may
// already have passed it.
func (r *sourceRescanner) next() (sourceRescan, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.waiting) == 0 {
		return sourceRescan{}, false
	}
	dir := r.waiting[0]
	r.waiting[0] = sourceRescan{}
	r.waiting = r.waiting[1:]
	if len(r.waiting) == 0 {
		r.waiting = nil
	}
	delete(r.pending, dir.abs)
	return dir, true
}

// run scans every queued directory until ctx ends. One wake can stand for
// many requests, since its slot holds one, so each wake drains the queue.
//
// after, when set, is called after each rescan with the library-relative
// directory it read, unless the context ended during it. runServe passes
// afterRescan: a rescan that read a changed file has made that file's
// renditions stale against its row, so the renders downloads asked for while
// the row was behind are queued there, and the sweep it nudges renders the
// rest from the version the scan read; without it they wait for the sweep's
// next tick, which by default is the next periodic scan. Every rescan, not
// only one that committed rows: ScanSubtree counts the rows it wrote, and a
// rescan whose file was deleted before it ran deletes a row and counts none,
// which changes an album's membership all the same (review round 4). It must
// not block for long: the next directory waits for it.
func (r *sourceRescanner) run(ctx context.Context, scan func(ctx context.Context, absDir string) (int, error), after func(ctx context.Context, relDir string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		for ctx.Err() == nil {
			dir, ok := r.next()
			if !ok {
				break
			}
			if _, err := scan(ctx, dir.abs); err != nil {
				if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
					logger.Warn("rescan of a changed source's directory failed",
						"dir", dir.rel, "err", strings.ReplaceAll(failure.Error(), dir.abs, dir.rel))
				}
			}
			if after != nil && ctx.Err() == nil {
				after(ctx, dir.rel)
			}
		}
	}
}

// afterRescan is what runServe runs after each rescan (sourceRescanner.run's
// `after`), in this order. It drops the album-gain index first (invalidate,
// nil when the album gain is not wired): the rescan may have read a retag
// that moved a DSD track to another album, or deleted one, and the index is
// otherwise dropped only when a FULL scan lands, so it can be up to its
// two-minute TTL old, and every render queued after this step takes its
// album-mates from it. Then it queues the renders downloads asked for while
// the directory's rows were behind (rescanned, staleRenditionHeal's, nil when
// no hook is wired), on the foreground lane, ahead of the sweep. Then it
// nudges the auto-optimize sweep. The nudge never blocks: its channel holds
// one, and a pending nudge already covers this one.
func afterRescan(invalidate func(), rescanned func(ctx context.Context, relDir string), nudge chan<- struct{}) func(ctx context.Context, relDir string) {
	return func(ctx context.Context, relDir string) {
		if invalidate != nil {
			invalidate()
		}
		if rescanned != nil {
			rescanned(ctx, relDir)
		}
		select {
		case nudge <- struct{}{}:
		default:
		}
	}
}

// staleRenditionRescanEvery is how often downloads may ask for a rescan of
// one directory, and for a render of one rendition. A stale rendition is
// asked for on every play of its track, by every paired device, and in range
// requests, so without it a file whose rescan does not bring its row level
// with it (one still being written, a directory the scan cannot read) would
// keep the rescanner busy with that directory for as long as GETs arrive:
// each rescan that reads a changed file also runs the whole-library
// duplicate restamp (1.1 s over 50,012 rows on the dev Mac). And a render
// that fails would be tried again on every GET: a failure writes no row, so
// the rendition stays stale.
const staleRenditionRescanEvery = time.Minute

// staleRenditionWaitMax is how long a render a download asked for may wait
// for a rescan of its directory that brings its row level: past it, a
// rescan that leaves the row behind drops it, and a full table forgets it
// first. Longer than a full rescan queue (about nineteen minutes of work at
// its cap, see sourceRescanQueueCap) behind a full scan holding the
// scanner's lock.
const staleRenditionWaitMax = time.Hour

// staleRenditionHeal is the download path's side of a stale rendition
// (api.StaleRenditionFunc): told of every rendition a GET found stale, it has
// it rendered again (backlog B82). Until then a phone plays the source, and
// nothing else renders it on a bridge whose auto-optimize sweep is off, the
// default: a batch counts a track with any rendition of the family as
// covered, and the app asks for a new one only once it stops listing the
// old one, which it does after a 410 on some of its playback routes and never
// for an offline download.
//
//   - While the file's row is behind it (the file changed since the scan
//     that wrote the row), it asks for a rescan of the file's directory
//     (backlog B53), and the render waits for a rescan of that directory
//     that brings the row level (rescanned), for at most
//     staleRenditionWaitMax: a render now would record a version the serve
//     path refuses. The rescan is asked for at most once per directory per
//     staleRenditionRescanEvery, a minute spent only on a request the
//     rescanner queued or already had waiting: one it dropped (its queue
//     full) leaves the next GET free to ask, and a rescan that brought every
//     waiting file level frees it.
//   - Once the row is current it asks for the render (rerender) at once, at
//     most once per version of the file and rendition per
//     staleRenditionRescanEvery, a minute not spent when nothing was tried
//     (the pool's queue full, the kind switched off). The render is stamped
//     with the row, so the rendition it writes is fresh to the serve path,
//     the sweep and the album gain alike, and no later download finds it
//     stale: no loop (#1077's one clock). A render that fails writes no row,
//     so the next download after the minute asks again, until the failures
//     suppress the file (staleRerender).
type staleRenditionHeal struct {
	lookup func(ctx context.Context, rel string) (*manifest.Track, error)
	// request is sourceRescanner.queue: whether the directory will be
	// scanned.
	request func(rel string) bool
	// rerender asks for the render of the rendition variantID names, for the
	// file whose track row records rel (staleRerender.rerender). Nil: a stale
	// download asks for rescans only.
	rerender func(ctx context.Context, rel, variantID string) error
	// stat is the file a row names at rel, as a download stats it (the
	// resolver's ResolveChecked). A render that waited for a rescan is asked
	// for only if the rescan brought the row level with it.
	stat func(rel string) (os.FileInfo, error)
	now  func() time.Time
	mu   sync.Mutex
	// asked is when a download last asked for each library-relative
	// directory's rescan; rendered, when one last asked for each rendition's
	// render (keyed by path, id and the row's size and mtime).
	asked, rendered recentKeys
	// waiting holds the renders downloads asked for while their rows were
	// behind, keyed by path and id, until the rescan of their directory.
	waiting map[string]staleRenditionWant
}

// staleRenditionWant is a render a download asked for while the file's row
// was behind it.
type staleRenditionWant struct {
	dir, rel, variantID string
	at                  time.Time
}

func newStaleRenditionHeal(lookup func(ctx context.Context, rel string) (*manifest.Track, error), request func(rel string) bool,
	rerender func(ctx context.Context, rel, variantID string) error, stat func(rel string) (os.FileInfo, error)) *staleRenditionHeal {
	return &staleRenditionHeal{
		lookup: lookup, request: request, rerender: rerender, stat: stat, now: time.Now,
		asked: recentKeys{}, rendered: recentKeys{}, waiting: map[string]staleRenditionWant{},
	}
}

// observe is the api.StaleRenditionFunc: clientPath is the source path the
// GET named (any spelling the store's lookup accepts), variantID the stale
// rendition's id, info the stat the freshness check compared against.
func (h *staleRenditionHeal) observe(ctx context.Context, clientPath, variantID string, info os.FileInfo) {
	track, err := h.lookup(ctx, clientPath)
	if err != nil || track == nil {
		return
	}
	if transcode.SourceIsAtRow(info, track.ModTime.UnixNano(), track.Size) {
		h.render(ctx, track, variantID)
		return
	}
	dir := path.Dir(track.Path)
	h.await(dir, track.Path, variantID)
	if !h.admit(h.asked, dir) {
		return
	}
	if !h.request(track.Path) {
		// Nothing will scan it (the rescanner's queue is full): the minute
		// is not spent on a request that did not happen.
		h.forget(h.asked, dir)
	}
}

// rescanned is sourceRescanner.run's `after` step for the renders downloads
// asked for while the rows of dir's files were behind: each whose row the
// rescan brought level with its file is asked for now. One still behind (the
// file changed again, the scan failed or could not read it) keeps waiting,
// with the time it was first asked for, for a later rescan of dir; one whose
// row is gone is dropped.
//
// A rescan that brought every waiting file level frees the directory's
// minute: it did what the downloads asked for, and a later change to one of
// those files is a new change, whose download may ask for a rescan at once.
// One that left a file behind keeps it, which is what the minute is for: a
// file still being written, or a directory the scan cannot read, would
// otherwise have every download ask for a rescan that changes nothing.
func (h *staleRenditionHeal) rescanned(ctx context.Context, dir string) {
	wants := h.takeWaiting(dir)
	behind := false
	for _, w := range wants {
		track, err := h.lookup(ctx, w.rel)
		if err != nil {
			behind = true
			h.keep(w)
			continue
		}
		if track == nil {
			continue
		}
		info, err := h.stat(track.Path)
		if err != nil || !transcode.SourceIsAtRow(info, track.ModTime.UnixNano(), track.Size) {
			behind = true
			h.keep(w)
			continue
		}
		h.render(ctx, track, w.variantID)
	}
	if len(wants) > 0 && !behind {
		h.forget(h.asked, dir)
	}
}

// render asks for the render of variantID for the file whose row is track,
// unless it was asked for within the minute for this VERSION of the file:
// the minute bounds the tries of one render that fails (a failure writes no
// row, so the rendition stays stale), and a file changed again is a render
// that has not been tried.
func (h *staleRenditionHeal) render(ctx context.Context, track *manifest.Track, variantID string) {
	if h.rerender == nil {
		return
	}
	rel := track.Path
	key := fmt.Sprintf("%s\x00%s\x00%d\x00%d", rel, variantID, track.ModTime.UnixNano(), track.Size)
	if !h.admit(h.rendered, key) {
		return
	}
	err := h.rerender(ctx, rel, variantID)
	switch {
	case err == nil:
		logger.Info("a download found a rendition stale; rendering it again", "path", rel, "variant", variantID)
	case errors.Is(err, api.ErrUpscaleQueueFull):
		// Nothing was queued: the next download may ask again.
		h.forget(h.rendered, key)
	case errors.Is(err, api.ErrUpscaleNoRoom):
		// Nothing was queued, for want of free space, and a probe costs
		// little to ask again: the first download after room is made
		// renders. At Debug: every download of the rendition asks, and a
		// full volume is the Jobs card's to report (the sweep's disk floor).
		h.forget(h.rendered, key)
		logger.Debug("a download found a rendition stale; no room to render it again", "path", rel, "variant", variantID, "err", err)
	case errors.Is(err, errSourceAheadOfRow):
		// The file changed again between the check here and the one in the
		// enqueue, and the enqueue asked for a rescan. The render waits for
		// it, and asks for the rescan again once it is waiting, which the
		// rescanner folds into the one already queued: the enqueue's request
		// came first, so a quick rescan could have run, and its step gone
		// by, before the wait was recorded. The minute stays spent: the
		// rescan reads a new version, whose render is another key.
		h.await(path.Dir(rel), rel, variantID)
		h.request(rel)
	case errors.Is(err, errRerenderInactive):
		// Refused on a gate, before any work: the minute stays free, so the
		// first download after the kind is switched on renders.
		h.forget(h.rendered, key)
	case errors.Is(err, errRerenderSuppressed),
		errors.Is(err, api.ErrUpscaleIneligible), errors.Is(err, api.ErrUpscaleSourceMissing):
		logger.Debug("a download found a rendition stale; not rendering it again", "path", rel, "variant", variantID, "reason", err)
	default:
		logger.Warn("a download found a rendition stale, and asking for its render failed", "path", rel, "variant", variantID, "err", err)
	}
}

// await keeps the render of variantID for rel until the rescan of dir, when
// a render can be asked for at all. At most sourceRescanQueueCap renders
// wait: past that, the ones older than staleRenditionWaitMax are forgotten
// first, and a render finding none to forget is dropped, to be asked for by
// the next download once the row is current.
func (h *staleRenditionHeal) await(dir, rel, variantID string) {
	if h.rerender == nil {
		return
	}
	h.wait(staleRenditionWant{dir: dir, rel: rel, variantID: variantID, at: h.now()}, true)
}

// keep puts back a render a rescan left behind, with the time it was first
// asked for, so it still ages out an hour after that: a file that never
// comes level is not carried through every rescan for ever. A download that
// asked for it again meanwhile has already put back a newer one.
func (h *staleRenditionHeal) keep(w staleRenditionWant) {
	if h.now().Sub(w.at) >= staleRenditionWaitMax {
		return
	}
	h.wait(w, false)
}

// wait records w, over one already waiting for the same render when replace
// is set, under await's bound.
func (h *staleRenditionHeal) wait(w staleRenditionWant, replace bool) {
	now := h.now()
	key := w.rel + "\x00" + w.variantID
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.waiting[key]
	if ok && !replace {
		return
	}
	if !ok && len(h.waiting) >= sourceRescanQueueCap {
		for k, old := range h.waiting {
			if now.Sub(old.at) >= staleRenditionWaitMax {
				delete(h.waiting, k)
			}
		}
		if len(h.waiting) >= sourceRescanQueueCap {
			return
		}
	}
	h.waiting[key] = w
}

// takeWaiting removes and returns the renders waiting for dir's rescan.
func (h *staleRenditionHeal) takeWaiting(dir string) []staleRenditionWant {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []staleRenditionWant
	for k, w := range h.waiting {
		if w.dir == dir {
			out = append(out, w)
			delete(h.waiting, k)
		}
	}
	return out
}

// admit records in keys that key goes now, unless it went within
// staleRenditionRescanEvery.
func (h *staleRenditionHeal) admit(keys recentKeys, key string) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	return keys.admit(key, now, staleRenditionRescanEvery)
}

// forget drops what admit recorded for key.
func (h *staleRenditionHeal) forget(keys recentKeys, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(keys, key)
}

// recentKeys is when each key was last let through, for at most
// sourceRescanQueueCap keys. Its caller holds the lock.
type recentKeys map[string]time.Time

// admit records that key goes at now, unless it went within window. Past
// sourceRescanQueueCap keys, the ones older than window are forgotten first,
// and a key finding none to forget is refused.
func (r recentKeys) admit(key string, now time.Time, window time.Duration) bool {
	if at, ok := r[key]; ok && now.Sub(at) < window {
		return false
	}
	if _, ok := r[key]; !ok && len(r) >= sourceRescanQueueCap {
		for k, at := range r {
			if now.Sub(at) >= window {
				delete(r, k)
			}
		}
		if len(r) >= sourceRescanQueueCap {
			return false
		}
	}
	r[key] = now
	return true
}

// errRerenderInactive and errRerenderSuppressed are staleRerender's two
// refusals that are no fault: the rendition's kind is switched off on this
// bridge (or its id names no family it renders, or the bridge is a demo),
// and the file's renders have failed often enough to be suppressed.
var (
	errRerenderInactive   = errors.New("this rendition's kind is not active on this bridge")
	errRerenderSuppressed = errors.New("renders of this file keep failing (suppressed)")
)

// renditionKindOf is the job kind that renders a rendition of this id's
// family, read off its prefix (the manifest.VariantKindPrefix* names every
// coverage query and the app route by): `optimized-`, the CarPlay tier of a
// PCM source and, by the shared prefix, the compact tier of a DSD source;
// `pcm-`, the faithful DSD tier; `upscaled-`. ok is false for any other id.
func renditionKindOf(variantID string) (kind transcode.JobKind, ok bool) {
	switch {
	case strings.HasPrefix(variantID, manifest.VariantKindPrefixOptimized+"-"):
		return transcode.JobKindOptimize, true
	case strings.HasPrefix(variantID, manifest.VariantKindPrefixPCM+"-"):
		return transcode.JobKindPCMRender, true
	case strings.HasPrefix(variantID, manifest.VariantKindPrefixUpscaled+"-"):
		return transcode.JobKindUpscale, true
	}
	return "", false
}

// renditionGates are the live gates of the three kinds: the closures runServe
// hands POST /v1/upscale (api.WithUpscale, WithCarPlayOptimize, WithDSDRender),
// so a stale download renders nothing a client's request for the family
// would be refused (TestAStaleDownloadRendersUnderTheV1KindGates).
type renditionGates struct {
	upscale, optimize, pcm func() bool
}

// open reports whether kind's gate is open. A nil gate is closed.
func (g renditionGates) open(kind transcode.JobKind) bool {
	var gate func() bool
	switch kind {
	case transcode.JobKindOptimize:
		gate = g.optimize
	case transcode.JobKindPCMRender:
		gate = g.pcm
	case transcode.JobKindUpscale:
		gate = g.upscale
	}
	return gate != nil && gate()
}

// staleRerender is how a stale download has a rendition rendered again: the
// way a client's POST /v1/upscale for the family would, and with the
// refusals every automatic path makes.
//
//   - The rendition's kind must be active (open): its live gate is the one
//     POST /v1/upscale reads.
//   - Never on a demo bridge (demo): POST /v1/upscale answers 403 there,
//     since every bearer on a demo bridge is public, and a download is not a
//     way around that. The demo's auto-optimize sweep renders its stale
//     renditions instead.
//   - Never for a file whose renders keep failing (suppressed: the
//     transcode-failure suppression the sweep and the batch walks skip by),
//     or a failing file would be rendered on every play of it.
//   - enqueue is the adapter's entry point for the kind
//     (upscaleEnqueuerAdapter.enqueueKind), which builds the spec from the
//     TRACK ROW, refuses a file ahead of its row (and asks for its rescan),
//     refuses a family already fresh, and puts the job on the lane a
//     client's request takes. It renders the family's CURRENT id for the
//     source, which is the stale id itself unless the id was minted under
//     an older schema or another target (a DSD `v1` rendition, an upscale
//     target the operator has since moved): then it is the id a request for
//     the family would render, and the app, which picks the newest of a
//     family, plays it.
type staleRerender struct {
	enqueue    func(kind transcode.JobKind, rel string) error
	open       func(kind transcode.JobKind) bool
	suppressed func(ctx context.Context, rel string) (bool, error)
	demo       bool
}

func (r staleRerender) rerender(ctx context.Context, rel, variantID string) error {
	kind, ok := renditionKindOf(variantID)
	if !ok || r.demo || r.open == nil || !r.open(kind) {
		return errRerenderInactive
	}
	if r.suppressed != nil {
		suppressed, err := r.suppressed(ctx, rel)
		if err != nil {
			return fmt.Errorf("read the file's render failures: %w", err)
		}
		if suppressed {
			return errRerenderSuppressed
		}
	}
	return r.enqueue(kind, rel)
}
