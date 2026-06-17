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

// MBIDSource enumerates the library's distinct artist GIDs to submit.
type MBIDSource interface {
	DistinctArtistMBIDs(ctx context.Context) ([]string, error)
}

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
	State *StateStore
	MBIDs MBIDSource
	Sink  MetaSink
	HTTP  *http.Client
	Log   *slog.Logger
	Now   func() time.Time

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

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
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
	MBIDs []string `json:"mbids"`
}

type submitResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

func (c *Client) submitAll(ctx context.Context, st State) error {
	mbids, err := c.MBIDs.DistinctArtistMBIDs(ctx)
	if err != nil {
		return fmt.Errorf("enumerate artist mbids: %w", err)
	}
	if len(mbids) == 0 {
		return nil
	}
	chunk := c.submitChunk()
	var totalAccepted int
	for i := 0; i < len(mbids); i += chunk {
		end := i + chunk
		if end > len(mbids) {
			end = len(mbids)
		}
		var resp submitResponse
		if err := c.postJSON(ctx, st, "/v1/atlas/harvest/submit", submitRequest{MBIDs: mbids[i:end]}, &resp); err != nil {
			return err
		}
		totalAccepted += resp.Accepted
	}
	c.log().InfoContext(ctx, "atlasharvest.submitted", "artists", len(mbids), "accepted", totalAccepted)
	return nil
}

type resultItem struct {
	MBID       string   `json:"mbid"`
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
		for _, it := range resp.Results {
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
		if resp.NextCursor > st.ResultCursor {
			if err := c.State.SetCursor(resp.NextCursor); err != nil {
				return fmt.Errorf("advance cursor: %w", err)
			}
		}
		if len(resp.Results) < limit {
			return nil // drained
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
