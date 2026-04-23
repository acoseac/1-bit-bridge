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
}

// NewDeezerClient constructs a client.
func NewDeezerClient(base, userAgent string, httpClient *http.Client) *DeezerClient {
	if base == "" {
		base = DefaultDeezerBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &DeezerClient{base: base, userAgent: userAgent, http: httpClient}
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
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
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

// FetchImage downloads the JPEG at url (which must be a Deezer-hosted
// picture URL returned by SearchArtist). Cap at 20 MB.
func (c *DeezerClient) FetchImage(ctx context.Context, u string) ([]byte, error) {
	if u == "" {
		return nil, errors.New("deezer: empty image URL")
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
	return io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
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
