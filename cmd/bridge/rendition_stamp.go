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
//     (sourceIsAtRow). The on-demand path refuses otherwise, and asks for a
//     rescan of the file's directory so the next request can render; the
//     sweeper and the CLI pass the file over until a scan reads it. The
//     batch coordinator does not check: it renders only a track with no
//     rendition of the family, so it takes no part in the loop below, but
//     a render it makes of a changed file is one the serve path refuses
//     until something renders that file again after its scan.
//     The album survey measures a changed album-mate all the same, since a
//     peak describes the bytes on disk and its row stamp makes the next
//     version measure it again.
//   - The check is made when a render is queued, not when the pool starts
//     it: a file that changes while its job waits, or while it renders, is
//     rendered from new bytes under the row's older stamp, and the serve
//     path refuses the result. The live stamp this replaced was taken at
//     enqueue too, so the window is not new (backlog B53).
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
	"path/filepath"
	"strings"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
)

// sourceIsAtRow reports whether the file on disk is still the version its
// track row records. It is the scanner's own test for a changed file (its
// skip gate compares size and mtime exactly), so a file this answers false
// for is one the next scan re-reads, and the serve path's 2 s tolerance does
// not belong here: that tolerance is about stamps taken through different
// mounts, and a row and a stat of the same file are taken through one.
func sourceIsAtRow(info os.FileInfo, rowMTimeNS, rowSize int64) bool {
	return info.Size() == rowSize && info.ModTime().UnixNano() == rowMTimeNS
}

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
// it, when an on-demand request for a rendition of that file is refused. It
// makes the refusal cost the client one play of the source rather than every
// play until the periodic scan (six hours by default): the scan it runs
// writes the row the next request stamps from.
//
// A directory is queued at most once at a time, and scanned by one loop
// (run), oldest first, one after another and behind any scan in progress:
// ScanSubtree takes the scanner's lock. The queue is the pending set
// itself, so a directory asked for while others wait is kept until the
// loop reaches it, up to sourceRescanQueueCap.
type sourceRescanner struct {
	wake    chan struct{} // one slot: something was queued since run last looked
	mu      sync.Mutex
	waiting []sourceRescan      // queued, oldest first
	pending map[string]struct{} // the absolute directories in waiting
}

func newSourceRescanner() *sourceRescanner {
	return &sourceRescanner{
		wake:    make(chan struct{}, 1),
		pending: map[string]struct{}{},
	}
}

// request queues the directory holding the file at abs (library-relative
// rel) unless it is already waiting or the queue is full. It never blocks.
func (r *sourceRescanner) request(abs, rel string) {
	dir := sourceRescan{abs: filepath.Dir(abs), rel: path.Dir(rel)}
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
func (r *sourceRescanner) run(ctx context.Context, scan func(ctx context.Context, absDir string) (int, error)) {
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
		}
	}
}
