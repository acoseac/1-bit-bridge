package enrich

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultCoverArtBase is the public Cover Art Archive root. Overridable
// in tests via NewCoverArtClient.
const DefaultCoverArtBase = "https://coverartarchive.org"

// SupportedCoverSizes lists the sizes CAA exposes natively. 1200 is full-
// size, 500 is the canonical "detail view" resolution, 250 is thumbnail.
var SupportedCoverSizes = []int{250, 500, 1200}

// CoverArtClient fetches a release's front cover in one of the supported
// sizes from Cover Art Archive.
type CoverArtClient struct {
	base      string
	userAgent string
	http      *http.Client
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
	return &CoverArtClient{base: base, userAgent: userAgent, http: httpClient}
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
	u := fmt.Sprintf("%s/release/%s/front-%d", c.base, mbid, size)
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
	u := fmt.Sprintf("%s/release-group/%s/front-%d", c.base, rgMBID, size)
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
	u := fmt.Sprintf("%s/release/%s/front-%d", c.base, mbid, size)
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
	u := fmt.Sprintf("%s/release-group/%s/front-%d", c.base, rgMBID, size)
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
		resp.Body.Close()
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
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
