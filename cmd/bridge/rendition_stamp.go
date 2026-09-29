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
//   - A download that finds a rendition stale because its source changed
//     after its row was written asks for the rescan as well
//     (staleRenditionRescan), and a rescan that wrote rows nudges the
//     auto-optimize sweep, so a pre-generated rendition of a retagged file
//     is rendered again without waiting for the next scan: the phone never
//     asks again for a family the manifest lists.
//
// Before this, the on-demand path and the CLI stamped a live stat while the
// rest stamped the row, and each writer undid the other: measured on a real
// bridge, three rounds of a request and a sweep against two changed files
// made twelve renders, a manifest delta for both tracks on every step, and
// the download answered 200 and 410 in turn (ops/engineering-log.md,
// 2026-09-28).

import (
	"context"
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
// of it stale (staleRenditionRescan). It makes the refusal cost the client
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
func (r *sourceRescanner) request(rel string) {
	relDir := path.Dir(rel)
	abs, err := r.resolve(relDir)
	if err != nil {
		return
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
// wrote, when set, is called after each scan that committed rows. runServe
// passes a nudge of the auto-optimize sweep: a rescan that read a changed
// file has made that file's renditions stale against its row, and the sweep
// renders them again from the version the scan read. Without it they wait
// for the sweep's next tick, which by default is the next periodic scan. It
// must not block.
func (r *sourceRescanner) run(ctx context.Context, scan func(ctx context.Context, absDir string) (int, error), wrote func()) {
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
			n, err := scan(ctx, dir.abs)
			if err != nil {
				if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
					logger.Warn("rescan of a changed source's directory failed",
						"dir", dir.rel, "err", strings.ReplaceAll(failure.Error(), dir.abs, dir.rel))
				}
			}
			if n > 0 && wrote != nil {
				wrote()
			}
		}
	}
}

// staleRenditionRescanEvery is how often downloads may ask for a rescan of
// one directory. A stale rendition is asked for on every play of its track,
// by every paired device, and in range requests, so without it a file whose
// rescan does not bring its row level with it (one still being written, a
// directory the scan cannot read) would keep the rescanner busy with that
// directory for as long as GETs arrive: each rescan that reads a changed
// file also runs the whole-library duplicate restamp (1.1 s over 50,012 rows
// on the dev Mac).
const staleRenditionRescanEvery = time.Minute

// staleRenditionRescan is the download path's side of the rescans
// (api.StaleRenditionFunc): told of every rendition a GET found stale, it
// asks for a rescan of the source's directory when the file has changed
// since its row was written, so its row catches up and the auto-optimize
// sweep renders the rendition again. Nothing else would: the phone never
// asks again for a family the manifest lists, and a batch counts a track
// with any rendition of the family as covered.
//
// It asks only while the row is behind the file. Once a scan has read the
// change, the rendition is stale against the row too, and a rescan would
// change nothing; the sweep (with auto-optimize on) renders it on its next
// pass. And at most once per directory per staleRenditionRescanEvery.
type staleRenditionRescan struct {
	lookup  func(ctx context.Context, rel string) (*manifest.Track, error)
	request func(rel string)
	now     func() time.Time
	mu      sync.Mutex
	// asked is when a download last asked for each library-relative
	// directory, bounded at sourceRescanQueueCap directories: past that,
	// the ones older than the window are forgotten first, and a request
	// finding none to forget is dropped.
	asked map[string]time.Time
}

func newStaleRenditionRescan(lookup func(ctx context.Context, rel string) (*manifest.Track, error), request func(rel string)) *staleRenditionRescan {
	return &staleRenditionRescan{lookup: lookup, request: request, now: time.Now, asked: map[string]time.Time{}}
}

// observe is the api.StaleRenditionFunc: clientPath is the source path the
// GET named (any spelling the store's lookup accepts), info the stat the
// freshness check compared against.
func (h *staleRenditionRescan) observe(ctx context.Context, clientPath string, info os.FileInfo) {
	track, err := h.lookup(ctx, clientPath)
	if err != nil || track == nil {
		return
	}
	if transcode.SourceIsAtRow(info, track.ModTime.UnixNano(), track.Size) {
		return
	}
	if !h.due(path.Dir(track.Path)) {
		return
	}
	h.request(track.Path)
}

// due records that a download asks for dir now, unless one did within
// staleRenditionRescanEvery.
func (h *staleRenditionRescan) due(dir string) bool {
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	if at, ok := h.asked[dir]; ok && now.Sub(at) < staleRenditionRescanEvery {
		return false
	}
	if len(h.asked) >= sourceRescanQueueCap {
		for d, at := range h.asked {
			if now.Sub(at) >= staleRenditionRescanEvery {
				delete(h.asked, d)
			}
		}
		if len(h.asked) >= sourceRescanQueueCap {
			return false
		}
	}
	h.asked[dir] = now
	return true
}
