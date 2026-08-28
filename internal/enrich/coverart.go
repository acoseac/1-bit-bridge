package enrich

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultCoverArtBase is the public Cover Art Archive root. Overridable
// in tests via NewCoverArtClient.
const DefaultCoverArtBase = "https://coverartarchive.org"

// SupportedCoverSizes lists the sizes CAA exposes natively. 1200 is full-
// size, 500 is the canonical "detail view" resolution, 250 is thumbnail.
var SupportedCoverSizes = []int{250, 500, 1200}

// DefaultCoverSize is the tier NEW cover fetches use when the enricher's
// CoverSize is unset (see Enricher.CoverSize + config `enrich.coverSize`).
// 1200 matches the scan-time local-artwork ceiling
// (manifest.localArtMaxDimensionPx) so enriched and curated covers land
// in the same quality class.
const DefaultCoverSize = 1200

// CoverArtClient fetches a release's front cover in one of the supported
// sizes from Cover Art Archive.
type CoverArtClient struct {
	base      string
	userAgent string
	http      *http.Client

	// minInterval is the politeness pacing the ENRICHER must apply before
	// each call, derived from base at construction — see pacing.go.
	minInterval time.Duration
	// liveBase, when non-nil, supersedes base and is read per use — see
	// WithLiveBase. The pacing re-derives from it too.
	liveBase func() string
}

// NewCoverArtClient constructs a client.
func NewCoverArtClient(base, userAgent string, httpClient *http.Client) *CoverArtClient {
	if base == "" {
		base = DefaultCoverArtBase
	}
	if httpClient == nil {
		// CAA redirects to Internet Archive; these can occasionally be
		// slow, so give ourselves more headroom than the MB client.
		// sharedHTTPTransport tunes the pool size — see transport.go.
		httpClient = &http.Client{Timeout: 30 * time.Second, Transport: sharedHTTPTransport}
	}
	return &CoverArtClient{
		base:        base,
		userAgent:   userAgent,
		http:        httpClient,
		minInterval: minIntervalForBase(base, PublicCAAMinInterval, publicCAAHosts),
	}
}

// MinInterval is the pacing the caller should sleep between requests:
// PublicCAAMinInterval against coverartarchive.org, SelfHostedMinInterval
// against an operator's own mirror. See pacing.go.
func (c *CoverArtClient) MinInterval() time.Duration {
	if c.liveBase != nil {
		return minIntervalForBase(c.resolveBase(), PublicCAAMinInterval, publicCAAHosts)
	}
	return c.minInterval
}

// MaxCoverArtBytes caps individual cover-art body reads. 20 MB is
// generous — the largest 1200×1200 JPEG observed in the wild is
// ~3 MB, so the cap exists to protect against a hostile/runaway
// redirect serving a multi-GB body, not to bound a real-world image.
const MaxCoverArtBytes = 20 * 1024 * 1024

// FetchReleaseFront returns the JPEG bytes for a release's front cover at
// the given size. Returns errNotFound if CAA has no front cover for this
// release (falling back to the release-group level is the caller's
// choice).
//
// **Buffered API** — kept for callers that genuinely need bytes in
// hand (existing tests, byte-equivalence collision checks). Production
// enrichment uses `FetchReleaseFrontStream` to avoid loading multi-MB
// images into RAM on Pi-class hosts.
func (c *CoverArtClient) FetchReleaseFront(ctx context.Context, mbid string, size int) ([]byte, error) {
	if mbid == "" {
		return nil, fmt.Errorf("coverart: empty mbid")
	}
	if !validSize(size) {
		return nil, fmt.Errorf("coverart: unsupported size %d, want one of %v", size, SupportedCoverSizes)
	}
	u := fmt.Sprintf("%s/release/%s/front-%d", c.resolveBase(), mbid, size)
	return c.fetch(ctx, u)
}

// FetchReleaseGroupFront is a fallback for when a specific release has no
// cover uploaded — the release group (the abstract "album") may still have
// one from a sibling pressing.
func (c *CoverArtClient) FetchReleaseGroupFront(ctx context.Context, rgMBID string, size int) ([]byte, error) {
	if rgMBID == "" {
		return nil, fmt.Errorf("coverart: empty release-group mbid")
	}
	if !validSize(size) {
		return nil, fmt.Errorf("coverart: unsupported size %d", size)
	}
	u := fmt.Sprintf("%s/release-group/%s/front-%d", c.resolveBase(), rgMBID, size)
	return c.fetch(ctx, u)
}

// FetchReleaseFrontStream returns an io.ReadCloser carrying the JPEG
// body. Caller MUST close it. Returns errNotFound on 404. The caller
// (typically `writeArtworkAtomicStream`) is expected to wrap the body
// in `io.LimitReader(body, MaxCoverArtBytes+1)` before reading; this
// method does NOT pre-bound the body so the caller can apply the cap
// uniformly with the destination size guard.
func (c *CoverArtClient) FetchReleaseFrontStream(ctx context.Context, mbid string, size int) (io.ReadCloser, error) {
	if mbid == "" {
		return nil, fmt.Errorf("coverart: empty mbid")
	}
	if !validSize(size) {
		return nil, fmt.Errorf("coverart: unsupported size %d, want one of %v", size, SupportedCoverSizes)
	}
	u := fmt.Sprintf("%s/release/%s/front-%d", c.resolveBase(), mbid, size)
	return c.fetchStream(ctx, u)
}

// FetchReleaseGroupFrontStream is the streaming counterpart to
// FetchReleaseGroupFront. Same semantics as FetchReleaseFrontStream.
func (c *CoverArtClient) FetchReleaseGroupFrontStream(ctx context.Context, rgMBID string, size int) (io.ReadCloser, error) {
	if rgMBID == "" {
		return nil, fmt.Errorf("coverart: empty release-group mbid")
	}
	if !validSize(size) {
		return nil, fmt.Errorf("coverart: unsupported size %d", size)
	}
	u := fmt.Sprintf("%s/release-group/%s/front-%d", c.resolveBase(), rgMBID, size)
	return c.fetchStream(ctx, u)
}

func (c *CoverArtClient) fetch(ctx context.Context, u string) ([]byte, error) {
	body, err := c.fetchStream(ctx, u)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, MaxCoverArtBytes))
}

// fetchStream is the shared HTTP path. Returns the raw body so the
// caller can stream straight to disk or buffer as needed. Status-code
// classification (404 → errNotFound) lives here so both Fetch and
// FetchStream paths surface the same error semantics.
func (c *CoverArtClient) fetchStream(ctx context.Context, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drainBody(resp.Body)
		resp.Body.Close()
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		drainBody(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("coverart: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func validSize(size int) bool {
	for _, s := range SupportedCoverSizes {
		if s == size {
			return true
		}
	}
	return false
}

// ParseSize parses a size query param ("500" etc.) and returns 500 as the
// default if unset/unparseable. Returns an error if the parsed size isn't
// in SupportedCoverSizes.
func ParseSize(s string) (int, error) {
	if s == "" {
		return 500, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("size must be an integer, got %q", s)
	}
	if !validSize(n) {
		return 0, fmt.Errorf("size %d unsupported (allowed: %v)", n, SupportedCoverSizes)
	}
	return n, nil
}

// WithLiveBase attaches a provider consulted per use instead of the base
// URL captured at construction, so `enrich.coverArtBaseURL` applies
// without a restart.
//
// **The pacing travels with it.** MinInterval re-derives from the same
// live value, because the politeness contract is with a HOST, not with a
// client instance: a live base whose interval stayed frozen at the
// self-hosted 150ms would start hammering public Cover Art Archive at ~6.7 rps
// the moment an operator cleared the mirror URL. That is the one mistake
// in this file that reaches a third party.
//
// Base and interval are read separately, and a change can land between
// the two. That is safe by construction rather than by luck: the pacing
// gap is measured since the last request to the OLD host, so the first
// request to the NEW one arrives with no prior traffic to it at all, and
// every request after it is paced by the new host's own interval. The
// worst case is a single request that waited longer or shorter than the
// new host requires while that host has seen nothing.
//
// An empty or whitespace-only live value falls back to the constructed
// base, so a cleared config resolves to the public default rather than to
// a broken empty-prefix URL. Nil keeps the captured base entirely, which
// is what every caller other than the serve path wants.
func (c *CoverArtClient) WithLiveBase(f func() string) *CoverArtClient {
	c.liveBase = f
	return c
}

// resolveBase returns the base URL to use right now.
func (c *CoverArtClient) resolveBase() string {
	if c.liveBase != nil {
		if v := strings.TrimRight(strings.TrimSpace(c.liveBase()), "/"); v != "" {
			return v
		}
	}
	return c.base
}
