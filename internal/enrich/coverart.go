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

// FetchReleaseFront returns the JPEG bytes for a release's front cover at
// the given size. Returns errNotFound if CAA has no front cover for this
// release (falling back to the release-group level is the caller's
// choice).
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

func (c *CoverArtClient) fetch(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coverart: HTTP %d", resp.StatusCode)
	}
	// Cap at a generous 20 MB so a runaway redirect can't exhaust memory.
	return io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
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
