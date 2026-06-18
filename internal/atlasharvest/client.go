package atlasharvest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ArtistMeta is the harvested bio the client hands to the sink. Found=false is a
// tombstone (Atlas checked, nothing) so the overlay stops re-querying.
type ArtistMeta struct {
	MBID       string
	Found      bool
	Bio        string
	BioSummary string
	Genres     []string
	Source     string // "lastfm" | "tadb" (attribution provenance)
	SourceURL  string
}

// MBIDSource enumerates the library's distinct artist + release GIDs to submit.
type MBIDSource interface {
	DistinctArtistMBIDs(ctx context.Context) ([]string, error)
	DistinctReleaseMBIDs(ctx context.Context) ([]string, error)
}

// CoverRefetcher re-fetches a release's premium cover from Atlas, overwriting
// the local cache ONLY when Atlas now serves a premium (non-CAA) one. Returns
// true when a premium cover was written. Satisfied by a thin adapter over the
// enricher's authenticated premium-cover fetcher. Nil disables cover harvest.
//
// Error contract: (false, nil) is a genuine "premium not ready yet" miss (it
// counts toward the attempt cap). A non-nil error is transient (the cover stays
// pending, attempt NOT burned) — the adapter MUST return ErrNoCredential when no
// usable credential is available so the sweep stops the whole tick.
type CoverRefetcher interface {
	RefetchPremium(ctx context.Context, releaseMBID string) (bool, error)
}

// ErrNoCredential signals a CoverRefetcher had no usable Atlas credential — the
// refresh sweep stops the tick on it (every remaining release would fail the
// same way) rather than spamming the log.
var ErrNoCredential = errors.New("atlasharvest: refetcher has no usable credential")

// MetaSink caches a harvested (or tombstoned) artist bio. manifest.Store
// satisfies this via a thin adapter in the wiring.
type MetaSink interface {
	UpsertArtistMeta(ctx context.Context, m ArtistMeta) error
}

// errUnauthorized signals Atlas rejected the token (401/403) — the client wipes
// the credential so it stops hammering a dead token until the app re-provisions.
var errUnauthorized = errors.New("atlasharvest: token rejected by atlas")

// Client runs the bridge side of the bulk harvest: submit library artist GIDs,
// delta-sync the harvested bios into the overlay. One long-lived goroutine; all
// pacing is interval-based + bounded by Atlas's own per-source rate limits.
type Client struct {
	State     *StateStore
	MBIDs     MBIDSource
	Sink      MetaSink
	Refetcher CoverRefetcher // optional; nil = artist-bio harvest only (no cover harvest)
	HTTP      *http.Client
	Log       *slog.Logger
	Now       func() time.Time

	// SubmitInterval is the re-submit cadence (re-submitting is idempotent at
	// Atlas, but cheap to skip) — catches artists added since the last submit.
	SubmitInterval time.Duration
	PollInterval   time.Duration // tick cadence
	SubmitChunk    int           // artist GIDs per submit POST
	ResultsLimit   int           // results page size
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// defaultHarvestHTTPClient carries a finite timeout so the background harvester
// can't hang forever on an unresponsive Atlas (http.DefaultClient has none).
var defaultHarvestHTTPClient = &http.Client{Timeout: 30 * time.Second}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHarvestHTTPClient
}

func (c *Client) submitInterval() time.Duration {
	if c.SubmitInterval > 0 {
		return c.SubmitInterval
	}
	return 6 * time.Hour
}

func (c *Client) pollInterval() time.Duration {
	if c.PollInterval > 0 {
		return c.PollInterval
	}
	return 60 * time.Second
}

func (c *Client) submitChunk() int {
	if c.SubmitChunk > 0 {
		return c.SubmitChunk
	}
	return 500
}

func (c *Client) resultsLimit() int {
	if c.ResultsLimit > 0 {
		return c.ResultsLimit
	}
	return 200
}

// Run drives the harvest until ctx is cancelled. Ticks immediately, then on
// PollInterval. A tick is a no-op when no usable credential is provisioned, so
// the loop runs cheaply (one snapshot) until the app provisions one.
func (c *Client) Run(ctx context.Context) {
	t := time.NewTicker(c.pollInterval())
	defer t.Stop()
	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

func (c *Client) tick(ctx context.Context) {
	st := c.State.Snapshot()
	if !credentialUsable(st, c.now()) {
		return
	}
	if submitDue(st, c.now(), c.submitInterval()) {
		if err := c.submitAll(ctx, st); err != nil {
			c.handleErr(ctx, "submit", err)
		} else if err := c.State.SetLastSubmit(c.now()); err != nil {
			c.log().WarnContext(ctx, "atlasharvest.state.persist_failed", "phase", "submit", "error", err)
		}
	}
	if err := c.pollResults(ctx); err != nil {
		c.handleErr(ctx, "poll", err)
	}
	c.refreshCovers(ctx)
}

const (
	// maxCoverRefetchAttempts is how many CAA misses a pending release tolerates
	// before it's dropped — Tidal likely has no album for that barcode, so the
	// cover will never go premium and re-fetching it forever is waste.
	maxCoverRefetchAttempts = 6
	// coverRefreshPerTick bounds how many pending covers a single tick re-fetches
	// so a large backlog paces against Atlas's public-tier rate limit over ticks
	// rather than bursting.
	coverRefreshPerTick = 100
)

// refreshCovers re-fetches a bounded batch of pending release covers from Atlas
// (premium-gated): a premium hit settles the entry (the local cache is upgraded);
// a CAA miss increments its attempt count and drops it at the cap. No-op without
// a wired refetcher or pending entries.
func (c *Client) refreshCovers(ctx context.Context) {
	if c.Refetcher == nil {
		return
	}
	pending := c.State.PendingCoversSnapshot()
	if len(pending) == 0 {
		return
	}
	var resolved, missed []string
	processed := 0
	for mbid := range pending {
		if processed >= coverRefreshPerTick || ctx.Err() != nil {
			break
		}
		processed++
		got, err := c.Refetcher.RefetchPremium(ctx, mbid)
		if err != nil {
			// Transient: leave the cover pending WITHOUT burning an attempt. No
			// credential means every remaining release fails the same way, so
			// stop the tick rather than spam; other errors (5xx) just skip this
			// one and try the rest.
			if errors.Is(err, ErrNoCredential) {
				break
			}
			c.log().WarnContext(ctx, "atlasharvest.cover_refetch_failed", "mbid", mbid, "error", err.Error())
			continue
		}
		if got {
			resolved = append(resolved, mbid)
		} else {
			missed = append(missed, mbid)
		}
	}
	if len(resolved) == 0 && len(missed) == 0 {
		return
	}
	if err := c.State.SettlePendingCovers(resolved, missed, maxCoverRefetchAttempts); err != nil {
		c.log().WarnContext(ctx, "atlasharvest.pending_covers.settle_failed", "error", err)
	}
	if len(resolved) > 0 {
		c.log().InfoContext(ctx, "atlasharvest.covers_upgraded", "count", len(resolved))
	}
}

func credentialUsable(st State, now time.Time) bool {
	if st.Token == "" || st.AtlasBaseURL == "" {
		return false
	}
	return st.ExpiresAt.IsZero() || now.Before(st.ExpiresAt)
}

func submitDue(st State, now time.Time, interval time.Duration) bool {
	return st.LastSubmitAt.IsZero() || now.Sub(st.LastSubmitAt) >= interval
}

// handleErr wipes the credential on an auth rejection (so the app re-provisions)
// and otherwise just logs — transient failures retry on the next tick.
func (c *Client) handleErr(ctx context.Context, phase string, err error) {
	if errors.Is(err, errUnauthorized) {
		c.log().WarnContext(ctx, "atlasharvest.token_rejected", "phase", phase)
		if cerr := c.State.Clear(); cerr != nil {
			c.log().WarnContext(ctx, "atlasharvest.clear_failed", "error", cerr)
		}
		return
	}
	c.log().WarnContext(ctx, "atlasharvest.tick_error", "phase", phase, "error", err)
}

type submitRequest struct {
	MBIDs        []string `json:"mbids"`
	ReleaseMBIDs []string `json:"releaseMbids,omitempty"`
}

type submitResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

func (c *Client) submitAll(ctx context.Context, st State) error {
	artists, err := c.MBIDs.DistinctArtistMBIDs(ctx)
	if err != nil {
		return fmt.Errorf("enumerate artist mbids: %w", err)
	}
	// Release covers are only submitted when a refetcher is wired — otherwise
	// Atlas would resolve covers the bridge never picks up locally.
	var releases []string
	if c.Refetcher != nil {
		releases, err = c.MBIDs.DistinctReleaseMBIDs(ctx)
		if err != nil {
			return fmt.Errorf("enumerate release mbids: %w", err)
		}
	}
	if len(artists) == 0 && len(releases) == 0 {
		return nil
	}
	chunk := c.submitChunk()
	postChunks := func(mbids []string, asRelease bool) error {
		for i := 0; i < len(mbids); i += chunk {
			end := i + chunk
			if end > len(mbids) {
				end = len(mbids)
			}
			req := submitRequest{}
			if asRelease {
				req.ReleaseMBIDs = mbids[i:end]
			} else {
				req.MBIDs = mbids[i:end]
			}
			var resp submitResponse
			if err := c.postJSON(ctx, st, "/v1/atlas/harvest/submit", req, &resp); err != nil {
				return err
			}
		}
		return nil
	}
	if err := postChunks(artists, false); err != nil {
		return err
	}
	if err := postChunks(releases, true); err != nil {
		return err
	}
	c.log().InfoContext(ctx, "atlasharvest.submitted", "artists", len(artists), "releases", len(releases))
	return nil
}

type resultItem struct {
	MBID       string   `json:"mbid"`
	Kind       string   `json:"kind"` // "artist" | "release" (older atlas omits → "")
	Status     string   `json:"status"`
	Found      bool     `json:"found"`
	Bio        string   `json:"bio"`
	BioSummary string   `json:"bioSummary"`
	Genres     []string `json:"genres"`
	Source     string   `json:"source"`
	SourceURL  string   `json:"sourceUrl"`
	Cursor     int64    `json:"cursor"`
}

type resultsResponse struct {
	Results    []resultItem `json:"results"`
	NextCursor int64        `json:"nextCursor"`
}

func (c *Client) pollResults(ctx context.Context) error {
	limit := c.resultsLimit()
	for {
		st := c.State.Snapshot()
		q := url.Values{}
		q.Set("since", strconv.FormatInt(st.ResultCursor, 10))
		q.Set("limit", strconv.Itoa(limit))
		var resp resultsResponse
		if err := c.getJSON(ctx, st, "/v1/atlas/harvest/results?"+q.Encode(), &resp); err != nil {
			return err
		}
		var pendingReleases []string
		for _, it := range resp.Results {
			if it.Kind == "release" {
				// Cover reverse-resolve result: "done" means Atlas enqueued the
				// resolve, so record it for the refresh sweep to re-fetch the
				// (now premium) cover. "exhausted" (no barcode / not mirrored) →
				// nothing to fetch.
				if it.Found {
					pendingReleases = append(pendingReleases, it.MBID)
				}
				continue
			}
			if err := c.Sink.UpsertArtistMeta(ctx, ArtistMeta{
				MBID:       it.MBID,
				Found:      it.Found,
				Bio:        it.Bio,
				BioSummary: it.BioSummary,
				Genres:     it.Genres,
				Source:     it.Source,
				SourceURL:  it.SourceURL,
			}); err != nil {
				return fmt.Errorf("store %s: %w", it.MBID, err)
			}
		}
		if err := c.State.AddPendingCovers(pendingReleases); err != nil {
			c.log().WarnContext(ctx, "atlasharvest.pending_covers.persist_failed", "error", err)
		}
		advanced := resp.NextCursor > st.ResultCursor
		if advanced {
			if err := c.State.SetCursor(resp.NextCursor); err != nil {
				return fmt.Errorf("advance cursor: %w", err)
			}
		}
		if len(resp.Results) < limit {
			return nil // drained
		}
		// A full page that didn't advance the cursor would re-request the same
		// page forever — stop rather than hammer Atlas in a tight loop.
		if !advanced {
			return fmt.Errorf("harvest results cursor stuck at %d on a full page", st.ResultCursor)
		}
	}
}

// --- HTTP helpers ---

func (c *Client) postJSON(ctx context.Context, st State, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, joinURL(st.AtlasBaseURL, path), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(st, req, out)
}

func (c *Client) getJSON(ctx context.Context, st State, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, joinURL(st.AtlasBaseURL, path), nil)
	if err != nil {
		return err
	}
	return c.do(st, req, out)
}

func (c *Client) do(st State, req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+st.Token)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("atlas %s: http %d: %s", req.URL.Path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}
