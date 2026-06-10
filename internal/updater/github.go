package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Client talks to the GitHub Releases REST API. One per Updater.
//
// The default endpoint is https://api.github.com; tests override
// `baseURL` to point at an httptest.Server.
//
// Two http.Clients, deliberately: `http` carries the short poll
// Timeout and serves the small JSON fetches (LatestRelease,
// release-meta.json); `download` carries NO overall Timeout and
// serves the release-archive + checksums.txt asset fetches.
// http.Client.Timeout caps the ENTIRE exchange including the body
// read, so routing a multi-MiB archive through the 10 s poll client
// killed the download mid-stream on any link slower than ~1.5 MB/s —
// `bridge update` and the serve auto-installer then failed forever.
// The download phase is bounded by a generous context deadline at the
// Install call site (downloadTimeout) instead, so a hung CDN
// connection still can't wedge; transport-level dial/TLS limits come
// from http.DefaultTransport.
type Client struct {
	repo     string
	baseURL  string
	http     *http.Client
	download *http.Client
}

// NewClient builds a Client targeting api.github.com. timeout applies
// to the JSON API polls only — asset downloads use the separate
// timeout-free client (see the Client doc). Use Client.WithBaseURL in
// tests.
func NewClient(repo string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		repo:     repo,
		baseURL:  "https://api.github.com",
		http:     &http.Client{Timeout: timeout},
		download: &http.Client{},
	}
}

// downloadClient returns the timeout-free client for asset fetches,
// falling back to the API client when the Client was built as a bare
// struct literal (no production path does; symmetric defence so a
// future test-only literal can't nil-pointer inside http.Client.Do).
func (c *Client) downloadClient() *http.Client {
	if c.download != nil {
		return c.download
	}
	return c.http
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

// ErrNoReleasesPublished is returned when GitHub responds with 404 to
// the `/releases/latest` lookup. Two distinct realities collapse into
// this single sentinel because GitHub deliberately conflates them on
// the unauthenticated API to avoid leaking private-repo existence:
//
//  1. The repo is public but has zero published releases yet.
//  2. The repo is private (or doesn't exist), so the unauthenticated
//     caller can't see any releases.
//
// From the operator's POV the remediation is the same — either the
// maintainer hasn't shipped a release yet, or repo visibility was
// changed without realising the auto-updater would break. We surface
// a single human-readable message and let the operator investigate.
//
// Replaces the prior behaviour of returning `&Release{}, nil` on 404,
// which left the dashboard's Updates card stuck on "checking…"
// forever (LastError empty AND LatestVersion empty triggered the
// "checking…" branch in dashboard.html).
var ErrNoReleasesPublished = errors.New(
	"no releases published yet, or repository is private")

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
		// 404 means either (a) repo has no published releases yet, or
		// (b) the repo is private/missing — GitHub conflates both on
		// the unauthenticated REST endpoint. Surface as a typed sentinel
		// so checkOnce records LastError with a friendly explanation;
		// the dashboard's "check failed" branch then replaces the
		// permanent "checking…" badge that the prior empty-Release
		// short-circuit produced.
		return nil, ErrNoReleasesPublished
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
		// rather than treat the prerelease as stable. Surface as the
		// same sentinel as a 404 — the dashboard's "check failed"
		// branch fires correctly instead of the permanent "checking…"
		// badge that the prior empty-Release short-circuit produced
		// (Gemini bot review on PR #89: same root cause as 404).
		return nil, ErrNoReleasesPublished
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
