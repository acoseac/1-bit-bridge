package enrich

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
)

// DefaultDeezerBase is the public Deezer API root. Overridable in tests
// via NewDeezerClient.
const DefaultDeezerBase = "https://api.deezer.com"

// DeezerClient resolves artist images. The public Deezer search endpoint
// is unauthenticated and returns ~50 req / 5s; we pace well under that.
// Same client the iOS app already uses — keeping the fallback path
// consistent across platforms.
type DeezerClient struct {
	base      string
	userAgent string
	http      *http.Client
	// allowedImageHosts suffix-matches on the Deezer image URL host.
	// Production values (".dzcdn.net", ".deezer.com") are set by
	// NewDeezerClient; tests may append their httptest host via
	// SetAllowedImageHostsForTest.
	allowedImageHosts []string
}

// NewDeezerClient constructs a client.
//
// The returned client re-validates every HTTP redirect target against the
// image allowlist. Without this gate, the initial `hostAllowed` check in
// FetchImage/SearchArtist would only cover the *first* URL — a malicious
// or misconfigured CDN could 30x-redirect to 169.254.169.254 (cloud
// metadata) or an RFC1918 address and we'd happily fetch it. The check
// fires for every hop, keeping the allowlist authoritative for the whole
// redirect chain.
func NewDeezerClient(base, userAgent string, httpClient *http.Client) *DeezerClient {
	if base == "" {
		base = DefaultDeezerBase
	}
	if httpClient == nil {
		// sharedHTTPTransport tunes the pool size for metadata
		// API hosts — see transport.go for the rationale.
		httpClient = &http.Client{Timeout: 10 * time.Second, Transport: sharedHTTPTransport}
	}
	// Shallow-copy the caller's client before installing the redirect
	// guard. installRedirectGuard mutates CheckRedirect, and *http.Client
	// instances are routinely shared across goroutines/services in Go;
	// without this copy a caller passing http.DefaultClient or a client
	// shared with NewMusicBrainzClient would suddenly see Deezer's
	// SSRF allowlist constrain redirects across the whole process.
	clientCopy := *httpClient
	c := &DeezerClient{
		base:              base,
		userAgent:         userAgent,
		http:              &clientCopy,
		allowedImageHosts: append([]string(nil), deezerAllowedHosts...),
	}
	c.installRedirectGuard()
	return c
}

// SetAllowedImageHostsForTest replaces the client's image-host allowlist.
// Production code should never call this — it exists so httptest-backed
// tests can let FetchImage reach 127.0.0.1.
func (c *DeezerClient) SetAllowedImageHostsForTest(hosts []string) {
	c.allowedImageHosts = append([]string(nil), hosts...)
}

// installRedirectGuard wires CheckRedirect on c.http so every hop's host
// is re-validated against the current allowlist. Returning a non-nil
// error here causes http.Client to abort the redirect *and* return the
// error — no silent follow-through.
func (c *DeezerClient) installRedirectGuard() {
	prev := c.http.CheckRedirect
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("deezer: stopped after 10 redirects")
		}
		host := req.URL.Hostname()
		if !hostAllowed(host, c.allowedImageHosts) {
			return fmt.Errorf("deezer: refusing redirect to non-allowlisted host %q", host)
		}
		if prev != nil {
			return prev(req, via)
		}
		return nil
	}
}

// DeezerArtist is the subset of Deezer's artist shape we consume.
type DeezerArtist struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	PictureXL  string `json:"picture_xl"`
	PictureBig string `json:"picture_big"`
}

// SearchArtist queries Deezer for the best artist image URL. Returns
// ("", nil) if no match, errNotFound if the API 404s.
func (c *DeezerClient) SearchArtist(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	u := fmt.Sprintf("%s/search/artist?q=%s&limit=5", c.base, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", errNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deezer: HTTP %d", resp.StatusCode)
	}

	var body struct {
		Data []DeezerArtist `json:"data"`
	}
	err = json.NewDecoder(limitJSONBody(resp.Body)).Decode(&body)
	// Drain any bytes past the decoded JSON document so the deferred Close
	// returns the keep-alive connection to the idle pool — json.Decoder
	// stops at the closing token, not EOF. Mirrors MusicBrainzClient.get.
	drainBody(resp.Body)
	if err != nil {
		return "", err
	}
	match := pickDeezerArtist(body.Data, name)
	if match == nil {
		return "", nil
	}
	// Prefer picture_xl when present (1000×1000); fall back to big (500×500).
	if match.PictureXL != "" {
		return match.PictureXL, nil
	}
	return match.PictureBig, nil
}

// maxDeezerImageBytes caps image downloads so a compromised CDN can't
// exhaust memory. 20 MB comfortably holds a 1000x1000 JPEG.
const maxDeezerImageBytes = 20 * 1024 * 1024

// deezerAllowedHosts is the SSRF allowlist for FetchImage. The public
// search API returns picture URLs under these suffixes (verified against
// live /search/artist responses 2026-04); reject anything else to prevent
// a crafted response pointing at cloud-metadata (169.254.169.254) or RFC1918.
var deezerAllowedHosts = []string{".dzcdn.net", ".deezer.com"}

// FetchImage downloads the JPEG at url (which must be a Deezer-hosted
// picture URL returned by SearchArtist). Cap at maxDeezerImageBytes.
func (c *DeezerClient) FetchImage(ctx context.Context, u string) ([]byte, error) {
	if u == "" {
		return nil, errors.New("deezer: empty image URL")
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("deezer: parse image URL: %w", err)
	}
	if !hostAllowed(parsed.Hostname(), c.allowedImageHosts) {
		return nil, fmt.Errorf("deezer: refusing non-Deezer image host %q", parsed.Hostname())
	}
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
		return nil, fmt.Errorf("deezer: image HTTP %d", resp.StatusCode)
	}
	// Read one byte past the cap so an over-sized body surfaces as an
	// explicit error rather than silently truncating mid-JPEG.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxDeezerImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(buf) > maxDeezerImageBytes {
		return nil, fmt.Errorf("deezer: image exceeds %d-byte limit", maxDeezerImageBytes)
	}
	// A 200 OK with an empty body must NOT be written to disk: existence
	// is checked via os.Stat (size-blind), so a 0-byte file becomes a
	// permanent broken cache hit that the enricher never retries. Treat
	// an empty body as "no image" — the same outcome as a 404 — so the
	// next enrich pass can try again. The CAA/iTunes streaming path has
	// the equivalent n==0 guard; this is the buffered path's version.
	if len(buf) == 0 {
		return nil, errNotFound
	}
	return buf, nil
}

func hostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(host)
	for _, suf := range allowed {
		suf = strings.ToLower(suf)
		// Exact-match path covers dot-less allowlist entries like
		// "127.0.0.1" or explicit apex domains.
		if host == suf {
			return true
		}
		// Leading-dot entries match the apex (".deezer.com" → "deezer.com")
		// and any proper subdomain (".deezer.com" → "cdn.deezer.com"). The
		// bare-HasSuffix path this replaces was SSRF-fragile: without the
		// leading-dot gate an allowlist entry like "deezer.com" would
		// have matched "attackerdeezer.com", and "127.0.0.1" would have
		// matched "evil.127.0.0.1" — both let a compromised/ misconfigured
		// allowlist exfiltrate through a look-alike hostname. Require the
		// suffix to start with "." so the match is anchored at a label
		// boundary.
		if strings.HasPrefix(suf, ".") {
			apex := strings.TrimPrefix(suf, ".")
			if host == apex || strings.HasSuffix(host, suf) {
				return true
			}
		}
	}
	return false
}

// pickDeezerArtist chooses the best candidate. Exact-name match wins;
// otherwise the first candidate whose name contains or is contained in
// the search query.
func pickDeezerArtist(list []DeezerArtist, query string) *DeezerArtist {
	q := strings.ToLower(query)
	for i := range list {
		if strings.EqualFold(list[i].Name, query) {
			return &list[i]
		}
	}
	for i := range list {
		n := strings.ToLower(list[i].Name)
		if strings.Contains(n, q) || strings.Contains(q, n) {
			return &list[i]
		}
	}
	if len(list) > 0 {
		return &list[0]
	}
	return nil
}
