package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Client talks to the GitHub Releases REST API. One per Updater.
//
// The default endpoint is https://api.github.com; tests override
// `baseURL` to point at an httptest.Server.
type Client struct {
	repo    string
	baseURL string
	http    *http.Client
}

// NewClient builds a Client targeting api.github.com with the given
// per-request timeout. Use Client.WithBaseURL in tests.
func NewClient(repo string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		repo:    repo,
		baseURL: "https://api.github.com",
		http:    &http.Client{Timeout: timeout},
	}
}

// WithBaseURL overrides the API host. Test-only; production should use
// the default. Returns the client for chaining.
func (c *Client) WithBaseURL(u string) *Client {
	c.baseURL = strings.TrimRight(u, "/")
	return c
}

// Release is the subset of the GitHub Releases API response we care
// about. Additive fields are silently ignored — GitHub adds new keys
// over time without bumping the API version.
type Release struct {
	TagName    string         `json:"tag_name"`
	Name       string         `json:"name"`
	Body       string         `json:"body"`
	HTMLURL    string         `json:"html_url"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []ReleaseAsset `json:"assets"`
}

// ReleaseAsset describes one binary / archive attached to a release.
// Phase B uses BrowserDownloadURL + Name + Size; Phase A only needs
// the release shell.
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

// ErrRateLimited indicates the GitHub REST API has refused the request
// because the unauthenticated source-IP budget is exhausted. Surfaced
// to the admin UI so operators understand the cause; not retried
// internally because retrying inside the same poll cycle wouldn't help.
var ErrRateLimited = errors.New("github rate limit exceeded")

// LatestRelease returns the most recent non-draft, non-prerelease
// release on the configured repo. Drafts are never visible to the
// unauthenticated REST endpoint anyway, but we double-check so a
// pre-release tagged at the top of /releases doesn't get treated as a
// stable update.
//
// Errors are returned verbatim — the caller (Updater.checkOnce) records
// the message in Status.LastError.
func (c *Client) LatestRelease(ctx context.Context) (*Release, error) {
	endpoint, err := url.Parse(fmt.Sprintf("%s/repos/%s/releases/latest",
		c.baseURL, c.repo))
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// Identify ourselves so GitHub's abuse heuristics don't flag the
	// User-Agent-less request. Carry the bridge's own version so the
	// access logs distinguish update-poll traffic from human curl.
	req.Header.Set("User-Agent", fmt.Sprintf("1-bit-bridge-updater/%s", version.ServerVersion))
	// Pin to v3 so a future API revision doesn't change the JSON shape
	// out from under us.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		// 403 with X-RateLimit-Remaining: 0 is the canonical rate-limit
		// signal on the unauthenticated path. Other 403s (private repo,
		// abuse blocking) are also possible — we conflate because the
		// remediation from the operator's POV is "wait and retry".
		if remaining := resp.Header.Get("X-RateLimit-Remaining"); remaining == "0" {
			return nil, ErrRateLimited
		}
		return nil, fmt.Errorf("github 403: %s", strings.TrimSpace(readShortBody(resp.Body)))
	}
	if resp.StatusCode == http.StatusNotFound {
		// Empty repo or no releases yet. Treat as a non-error from the
		// updater's POV: we just don't have a candidate. Return a
		// sentinel-ish empty release so checkOnce records "no update
		// available" rather than a flapping LastError.
		return &Release{}, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("github status %d: %s", resp.StatusCode, strings.TrimSpace(readShortBody(resp.Body)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	if rel.Draft || rel.Prerelease {
		// Latest endpoint shouldn't return either, but if GitHub
		// changes that or the response was somehow forwarded, reject
		// rather than treat the prerelease as stable.
		return &Release{}, nil
	}
	return &rel, nil
}

// readShortBody reads up to 1 KiB of response body for an error message.
// We don't want to spew a multi-MB HTML 403 page into the operator's
// LastError field.
func readShortBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 1024))
	return string(b)
}

// rateLimitWindow returns the time at which the unauthenticated rate
// limit window resets, parsed from the X-RateLimit-Reset header. Zero
// time if the header is missing or malformed. Currently unused — kept
// here so Phase B's UI can surface "next check possible at HH:MM".
func rateLimitWindow(h http.Header) time.Time {
	raw := h.Get("X-RateLimit-Reset")
	if raw == "" {
		return time.Time{}
	}
	sec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
