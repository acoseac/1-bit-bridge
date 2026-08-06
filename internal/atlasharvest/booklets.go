package atlasharvest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// PDF album booklets (v1.8). Two loops ride the harvest client's existing
// tick, both no-ops without a wired BookletSink:
//
//   - The CHECK cycle (SubmitInterval cadence, like the library submit)
//     asks Atlas which of the library's release MBIDs have a booklet
//     (POST /v1/atlas/harvest/booklets/check), records verdicts, stamps
//     the wire tag on availability flips (which bumps indexed_at so iOS
//     delta-sync picks the change up), and GC's rows+files for releases
//     that left the library.
//   - The FETCH sweep (every tick, budget-bounded) downloads available
//     booklets (GET /v1/atlas/release/{mbid}/booklet with the harvest
//     bearer) into the local cache dir so /v1/booklet serves from disk.
//     The API handler nudges a priority channel on a 202 so a tapped
//     booklet jumps the queue.

// BookletSink is the manifest-store surface the booklet loops consume.
// *manifest.Store satisfies it directly.
type BookletSink interface {
	DistinctAlbumReleaseMBIDs(ctx context.Context) ([]string, error)
	BookletsToCheck(ctx context.Context, candidates []string, maxAttempts int) ([]string, error)
	UpsertBookletAvailability(ctx context.Context, mbid string, available bool, etag string, size int64) error
	SetBookletTagAndBumpIndex(ctx context.Context, releaseMBID, tag string) (int64, error)
	BookletsToFetch(ctx context.Context, limit, maxAttempts int) ([]BookletFetchItem, error)
	MarkBookletFetched(ctx context.Context, mbid string) error
	MarkBookletFetchFailed(ctx context.Context, mbid string) error
	MarkBookletUnavailable(ctx context.Context, mbid string) error
	DeleteBookletsNotIn(ctx context.Context, universe []string) ([]string, error)
}

// BookletFetchItem is the (mbid, etag) pair the fetch sweep downloads.
type BookletFetchItem struct {
	ReleaseMBID string
	Etag        string
}

// BookletFileStore persists fetched PDFs (and removes GC'd ones). Satisfied
// by a small disk adapter in cmd/bridge (atomicwrite into
// <dataDir>/booklets/<mbid>.pdf).
type BookletFileStore interface {
	WriteBooklet(mbid string, r io.Reader) error
	RemoveBooklet(mbid string) error
}

const (
	// maxBookletAttempts bounds BOTH per-release attempt budgets, which
	// share the booklets.check_attempts column in disjoint states (see
	// Store.MarkBookletFetchFailed):
	//
	//   - CHECK (available = 0): how many cycles keep asking Atlas about a
	//     release it answers "no booklet" for. At the 6h cadence, 8 ≈ 2 days
	//     — enough for Atlas's name-match ingest to link a freshly submitted
	//     library and fetch its goodies. A library rescan that mints new
	//     MBIDs naturally re-seeds candidates.
	//   - FETCH (available = 1, not yet downloaded): how many failed
	//     downloads a release tolerates before it stops entering the sweep,
	//     so a permanently-failing PDF can't burn the budget forever.
	//
	// A capped-out release is only out of the BACKGROUND sweep: both
	// on-demand paths reach fetchOneBooklet through NudgeBookletFetch's
	// priority channel, which never consults BookletsToFetch — the
	// /v1/booklet 202 (a user opening that album) and the admin About-card
	// folder retry, which nudges every available-but-unfetched release in
	// scope. So the cap bounds unattended waste, not recovery.
	maxBookletAttempts = 8
	// bookletCheckChunk mirrors Atlas's per-request MBID cap headroom
	// (submit uses 500 against the server's 2000 cap).
	bookletCheckChunk = 500
	// bookletFetchPerTick bounds downloads per 60s tick — polite pacing
	// against the operator's own Atlas while a large backlog drains
	// (hundreds of booklets ≈ a few hours).
	bookletFetchPerTick = 3
	// maxBookletBytes caps a single PDF download — mirrors Atlas's own
	// 64 MiB goodie-fetch cap.
	maxBookletBytes = 64 << 20
)

// bookletsCheckDue mirrors submitDue for the booklet cycle.
func bookletsCheckDue(st State, now time.Time, interval time.Duration) bool {
	return st.LastBookletCheckAt.IsZero() || now.Sub(st.LastBookletCheckAt) >= interval
}

type bookletsCheckRequest struct {
	MBIDs []string `json:"mbids"`
}

type bookletsCheckItem struct {
	MBID  string `json:"mbid"`
	Etag  string `json:"etag"`
	Bytes int64  `json:"bytes"`
}

type bookletsCheckResponse struct {
	Booklets []bookletsCheckItem `json:"booklets"`
}

// errBookletEndpointMissing marks an Atlas that pre-dates the booklet API —
// the cycle logs once and skips; it must NOT trip the credential-wipe path
// (only 401/403 do, handled inside do()).
var errBookletEndpointMissing = errors.New("atlasharvest: atlas has no booklets endpoint")

// checkBooklets runs one availability cycle. Chunked POSTs; verdicts are
// recorded per-MBID (available rows get their wire tag stamped; misses burn
// an attempt). Ends with the orphan GC over the same universe enumeration.
func (c *Client) checkBooklets(ctx context.Context, st State) error {
	universe, err := c.Booklets.DistinctAlbumReleaseMBIDs(ctx)
	if err != nil {
		return fmt.Errorf("enumerate booklet universe: %w", err)
	}
	candidates, err := c.Booklets.BookletsToCheck(ctx, universe, maxBookletAttempts)
	if err != nil {
		return fmt.Errorf("filter booklet candidates: %w", err)
	}
	for i := 0; i < len(candidates); i += bookletCheckChunk {
		end := min(i+bookletCheckChunk, len(candidates))
		if err := c.checkBookletChunk(ctx, st, candidates[i:end]); err != nil {
			return err
		}
	}
	c.gcBooklets(ctx, universe)
	c.log().InfoContext(ctx, "atlasharvest.booklets_checked",
		"universe", len(universe), "checked", len(candidates))
	return nil
}

// checkBookletChunk POSTs one candidate chunk and records every verdict:
// present in the response → available (+ the wire-tag stamp, whose
// strict-advance indexed_at bump is what surfaces the flip to iOS);
// absent → a miss that burns one attempt.
func (c *Client) checkBookletChunk(ctx context.Context, st State, chunk []string) error {
	var resp bookletsCheckResponse
	if err := c.postJSON(ctx, st, "/v1/atlas/harvest/booklets/check", bookletsCheckRequest{MBIDs: chunk}, &resp); err != nil {
		if isHTTPNotFound(err) {
			return errBookletEndpointMissing
		}
		return err
	}
	available := make(map[string]bookletsCheckItem, len(resp.Booklets))
	for _, b := range resp.Booklets {
		if b.MBID != "" {
			available[b.MBID] = b
		}
	}
	for _, mbid := range chunk {
		b, ok := available[mbid]
		if !ok {
			if err := c.Booklets.UpsertBookletAvailability(ctx, mbid, false, "", 0); err != nil {
				return fmt.Errorf("record booklet miss %s: %w", mbid, err)
			}
			continue
		}
		// ORDER IS LOAD-BEARING: stamp the wire tag BEFORE marking the
		// release available.
		//
		// BookletsToCheck only re-queues rows with available = 0, so
		// availability is the latch that takes a release out of the
		// check rotation. Marking it first meant a failing tag stamp
		// left the row available = 1 with NO tag — permanently outside
		// the queue, so the booklet stayed invisible to iOS with no
		// recovery short of editing the DB by hand.
		//
		// Stamping first makes both failure modes self-heal: a failed
		// stamp returns before availability is touched, and a failed
		// availability upsert leaves the (already-correct) tag with
		// available still 0. Either way the release is re-queued next
		// cycle and the surviving half of the write is idempotent —
		// SetBookletTagAndBumpIndex no-ops when the tag is unchanged.
		if _, err := c.Booklets.SetBookletTagAndBumpIndex(ctx, mbid, b.Etag); err != nil {
			return fmt.Errorf("stamp booklet tag %s: %w", mbid, err)
		}
		if err := c.Booklets.UpsertBookletAvailability(ctx, mbid, true, b.Etag, b.Bytes); err != nil {
			return fmt.Errorf("record booklet %s: %w", mbid, err)
		}
	}
	return nil
}

// scanInProgress reports whether a library scan is currently running (nil hook
// = never). Gates the booklet orphan GC — see gcBooklets.
func (c *Client) scanInProgress() bool {
	return c.ScanInProgress != nil && c.ScanInProgress()
}

// gcBooklets removes rows + cached PDFs for releases no longer in the
// library. Best-effort: file-removal failures log and continue (the row is
// already gone; a stale file is disk-only and unreachable via the API).
func (c *Client) gcBooklets(ctx context.Context, universe []string) {
	// Skip the orphan GC while a library scan is running. DeleteBookletsNotIn
	// treats any release MBID absent from `universe` as "left the library" —
	// but a wipe+rescan (admin root add/remove) briefly leaves
	// DistinctAlbumReleaseMBIDs returning ONLY the UPnP-routed albums (the
	// filesystem tracks are wiped, not yet re-indexed): a NON-empty-but-partial
	// universe the len()==0 guard below can't catch. GC'ing against it would
	// delete every filesystem album's booklet row + cached PDF, then re-fetch
	// them next cycle (iOS booklet chips blink off/on). The next check cycle
	// after the scan completes GCs against the full universe. nil hook
	// (production default until the cmd/bridge wire lands) = always run,
	// preserving prior behavior.
	if c.scanInProgress() {
		c.log().InfoContext(ctx, "atlasharvest.booklet_gc_skipped_scan_in_progress")
		return
	}
	orphans, err := c.Booklets.DeleteBookletsNotIn(ctx, universe)
	if err != nil {
		c.log().WarnContext(ctx, "atlasharvest.booklet_gc_failed", "error", err)
		return
	}
	if len(orphans) == 0 {
		return
	}
	if c.BookletFiles != nil {
		for _, mbid := range orphans {
			if err := c.BookletFiles.RemoveBooklet(mbid); err != nil {
				c.log().WarnContext(ctx, "atlasharvest.booklet_gc_file", "mbid", mbid, "error", err)
			}
		}
	}
	c.log().InfoContext(ctx, "atlasharvest.booklets_gc", "removed", len(orphans))
}

// fetchBooklets downloads a budget-bounded batch of available booklets.
// Priority-nudged MBIDs (the API's 202 path) drain first, then the oldest
// pending rows. A 404 from Atlas flips the row unavailable and CLEARS the
// wire tag (the flip is a real state change iOS must see); transient errors
// leave the row pending for the next tick. Returns errUnauthorized when Atlas
// rejects the token so the caller (tickBooklets) routes it to handleErr and
// wipes the credential — the fetch leg's analog of the check leg's wipe.
func (c *Client) fetchBooklets(ctx context.Context) error {
	if c.BookletFiles == nil {
		return nil
	}
	st := c.State.Snapshot()
	budget := bookletFetchPerTick
	fetched := 0
	seen := make(map[string]struct{}, budget)

	// Priority first: tap-driven requests from the API's 202 path.
	for budget > len(seen) {
		select {
		case mbid := <-c.bookletPriority():
			landed, err := c.fetchOneBooklet(ctx, st, mbid, seen)
			if err != nil {
				return err // errUnauthorized → abort the sweep; caller wipes
			}
			if landed {
				fetched++
			}
			continue
		default:
		}
		break
	}
	if budget > len(seen) {
		items, err := c.Booklets.BookletsToFetch(ctx, budget-len(seen), maxBookletAttempts)
		if err != nil {
			c.log().WarnContext(ctx, "atlasharvest.booklet_fetch_list", "error", err)
			return nil
		}
		for _, it := range items {
			if ctx.Err() != nil {
				return nil
			}
			landed, err := c.fetchOneBooklet(ctx, st, it.ReleaseMBID, seen)
			if err != nil {
				return err // errUnauthorized → abort the sweep; caller wipes
			}
			if landed {
				fetched++
			}
		}
	}
	if fetched > 0 {
		c.log().InfoContext(ctx, "atlasharvest.booklets_fetched", "count", fetched)
	}
	return nil
}

// fetchOneBooklet downloads one PDF (dedup'd via seen). Returns (landed,
// fatal): `landed` reports whether a fetch was stored; `fatal` is non-nil ONLY
// for errUnauthorized — a rejected token that MUST abort the sweep so the
// caller wipes the credential (mirrors the check leg + do()). An upstream 404
// flips the row unavailable AND clears the wire tag (a real state change iOS
// must see); any other failure records the attempt via MarkBookletFetchFailed,
// which rotates the row to the back of BookletsToFetch's checked_at ordering
// and burns one of its maxBookletAttempts. Recording the attempt is NOT
// optional bookkeeping: pre-fix this branch only logged, so a failing row kept
// its frozen checked_at, stayed at the head of the queue, and three of them
// consumed the whole bookletFetchPerTick budget on every tick — the background
// pre-cache sweep stopped making progress across the entire library while
// re-downloading the same failing bytes every minute.
func (c *Client) fetchOneBooklet(ctx context.Context, st State, mbid string, seen map[string]struct{}) (bool, error) {
	if _, dup := seen[mbid]; dup {
		return false, nil
	}
	seen[mbid] = struct{}{}
	if err := c.fetchBookletPDF(ctx, st, mbid); err != nil {
		switch {
		case errors.Is(err, errBookletGone):
			if uerr := c.Booklets.MarkBookletUnavailable(ctx, mbid); uerr != nil {
				c.log().WarnContext(ctx, "atlasharvest.booklet_mark_unavailable", "mbid", mbid, "error", uerr)
			}
			if _, terr := c.Booklets.SetBookletTagAndBumpIndex(ctx, mbid, ""); terr != nil {
				c.log().WarnContext(ctx, "atlasharvest.booklet_clear_tag", "mbid", mbid, "error", terr)
			}
			return false, nil
		case errors.Is(err, errUnauthorized):
			return false, err
		default:
			c.log().WarnContext(ctx, "atlasharvest.booklet_fetch_failed", "mbid", mbid, "error", err)
			if ferr := c.Booklets.MarkBookletFetchFailed(ctx, mbid); ferr != nil {
				c.log().WarnContext(ctx, "atlasharvest.booklet_mark_fetch_failed", "mbid", mbid, "error", ferr)
			}
			return false, nil
		}
	}
	if err := c.Booklets.MarkBookletFetched(ctx, mbid); err != nil {
		c.log().WarnContext(ctx, "atlasharvest.booklet_mark_fetched", "mbid", mbid, "error", err)
	}
	return true, nil
}

// errBookletGone marks an Atlas 404 on the PDF fetch — the booklet
// disappeared between check and fetch (upstream eviction).
var errBookletGone = errors.New("atlasharvest: booklet gone upstream")

// fetchBookletPDF streams one PDF from Atlas into the file store, capped at
// maxBookletBytes.
//
// The deadline is c.bulkTimeout(), NOT the ack-sized c.requestTimeout(): this
// body can legitimately run to 64 MiB, and the deferred cancel must outlive
// WriteBooklet because the transfer happens inside it.
func (c *Client) fetchBookletPDF(ctx context.Context, st State, mbid string) error {
	ctx, cancel := context.WithTimeout(ctx, c.bulkTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		joinURL(st.AtlasBaseURL, "/v1/atlas/release/"+mbid+"/booklet"), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+st.Token)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errBookletGone
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errUnauthorized
	case resp.StatusCode != http.StatusOK:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("atlas booklet %s: http %d: %s", mbid, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	// LimitReader at cap+1: a body that still has bytes past the cap is
	// refused rather than silently truncated (a truncated PDF is corrupt).
	lr := &io.LimitedReader{R: resp.Body, N: maxBookletBytes + 1}
	if err := c.BookletFiles.WriteBooklet(mbid, lr); err != nil {
		return err
	}
	if lr.N <= 0 {
		_ = c.BookletFiles.RemoveBooklet(mbid)
		return fmt.Errorf("atlas booklet %s: exceeds %d byte cap", mbid, maxBookletBytes)
	}
	return nil
}

// bookletPriority lazily builds the tap-priority channel. The API handler's
// 202 path pushes via NudgeBookletFetch; the fetch sweep drains it first.
func (c *Client) bookletPriority() chan string {
	c.bookletPriorityOnce.Do(func() {
		c.bookletPriorityCh = make(chan string, 32)
	})
	return c.bookletPriorityCh
}

// NudgeBookletFetch asks the fetch sweep to prioritize one release (the
// /v1/booklet 202 path — the user is looking at that album right now).
// Non-blocking: a full buffer drops the nudge, which is fine — the row is
// still in BookletsToFetch and drains in order.
func (c *Client) NudgeBookletFetch(mbid string) {
	if c == nil || c.Booklets == nil {
		return
	}
	select {
	case c.bookletPriority() <- mbid:
	default:
	}
}

// isHTTPNotFound matches do()'s formatted non-2xx error for a 404 — used to
// detect a pre-booklet Atlas without changing do()'s error contract.
func isHTTPNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), ": http 404:")
}
