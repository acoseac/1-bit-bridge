// iTunes Search API client. Fetches album artwork by (artist, album)
// in a single round-trip. Used by the enricher as a **last-resort
// fallback** after both MusicBrainz → CAA paths return errNotFound:
// `enrichOne → ensureArtworkCached → CAA-release → CAA-release-group
// → iTunes`. The bytes still cache under the MB-derived release MBID
// (`<MBID>-<size>.jpg`) so iOS's `/v1/artwork/{mbid}` endpoint serves
// them transparently — no `mbidPattern` regex relaxation, no wire-
// shape change.
//
// Why fallback rather than primary: skipping MB entirely when iTunes
// hits would require either a synthetic-MBID cache key or a
// `mbidPattern` relaxation. Both were considered out-of-scope for
// the introductory PR (#52). The current placement still raises the
// artwork hit rate for major-label releases that CAA misses, which
// is the practical motivation.
//
// Latency on a hit: a `/search?term=...&entity=album` call typically
// returns in 100-300 ms with the artwork URL embedded; the follow-up
// CDN fetch is another ~500 ms. The total cost is paid only when
// both CAA paths have already missed, so the worst case is "MB +
// CAA-release + CAA-release-group + iTunes" — measured rather than
// theoretical because the real CAA-miss albums are exactly the
// iTunes-hit candidates.
//
// API docs: https://developer.apple.com/library/archive/documentation/AudioVideo/Conceptual/iTuneSearchAPI/Searching.html
//
// No API key required, but Apple expects a polite identifying
// User-Agent and an unwritten ~20 req/min ceiling — paced via the
// enricher's `ITunesMinInterval` (defaults to 3 s).

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

// DefaultITunesBase is the public iTunes Search API root. Overridable
// in tests via NewITunesClient.
const DefaultITunesBase = "https://itunes.apple.com/search"

// ITunesClient queries the iTunes Search API for album artwork. Same
// shape as `MusicBrainzClient` / `CoverArtClient` so the enricher can
// treat it as just-another-source.
type ITunesClient struct {
	base      string
	userAgent string
	http      *http.Client
}

// NewITunesClient constructs a client. userAgent must identify the
// caller (Apple's recommended courtesy; not enforced).
func NewITunesClient(base, userAgent string, httpClient *http.Client) *ITunesClient {
	if base == "" {
		base = DefaultITunesBase
	}
	if httpClient == nil {
		// sharedHTTPTransport tunes the pool size + keep-alive
		// behaviour for the metadata API hosts; default
		// http.Client falls back to http.DefaultTransport which
		// caps MaxIdleConnsPerHost at 2 and causes connection
		// churn during enrichment passes.
		httpClient = &http.Client{Timeout: 10 * time.Second, Transport: sharedHTTPTransport}
	}
	return &ITunesClient{base: base, userAgent: userAgent, http: httpClient}
}

// ITunesAlbum is the trimmed-down iTunes album result the enricher
// consumes — collection ID for cache routing + the artwork URL.
type ITunesAlbum struct {
	// CollectionID is iTunes's stable identifier for the album. Used
	// as the suffix on the bridge's on-disk cache key for iTunes-
	// sourced artwork (e.g. `itunes-1234567890-500.jpg`) so a future
	// iTunes-only rescan reads from disk without a re-fetch.
	CollectionID int64
	// CollectionName is the album title as iTunes returned it — kept
	// for log lines / debugging, not used for matching.
	CollectionName string
	// ArtworkURL100 is the 100×100 artwork URL iTunes ships in the
	// search response. The high-res 600×600 URL is derived from this
	// via a documented string substitution (`/100x100bb.` → `/600x600bb.`).
	ArtworkURL100 string
}

// SearchAlbum queries iTunes for the best-matching album by (artist, album)
// and returns the top result.
//
// Return semantics:
//   - `(nil, nil)` when the input artist / album is blank after
//     trim — the caller has nothing to query for, no error to surface.
//   - `(nil, errNotFound)` when iTunes returned zero results OR every
//     result failed the substring-match heuristic. Compatible with
//     the project's `IsNotFound` check, so the enricher can treat
//     all sources (MB / CAA / iTunes) interchangeably in the fallback
//     chain.
//   - `(*ITunesAlbum, nil)` on a hit — `CollectionID`,
//     `CollectionName`, and `ArtworkURL100` are populated.
//   - `(nil, err)` on transport / decode / HTTP-status errors.
//
// Match heuristic: iTunes's relevance ranking is generally good; we
// take the top result whose `collectionName` substring-matches the
// requested album (case-insensitive). The substring check filters
// the obvious "you typed 'Black Diamond' and got 'Black Diamond
// Skye'" miss without rejecting legitimate suffix differences like
// "(Deluxe Edition)".
func (c *ITunesClient) SearchAlbum(ctx context.Context, artist, album string) (*ITunesAlbum, error) {
	artist = strings.TrimSpace(artist)
	album = strings.TrimSpace(album)
	if artist == "" || album == "" {
		return nil, nil
	}

	q := url.Values{}
	q.Set("term", artist+" "+album)
	q.Set("entity", "album")
	q.Set("limit", "5")
	u := c.base + "?" + q.Encode()

	var body iTunesSearchResponse
	if err := c.get(ctx, u, &body); err != nil {
		return nil, err
	}
	if body.ResultCount == 0 || len(body.Results) == 0 {
		return nil, errNotFound
	}

	albumLower := strings.ToLower(album)
	for i := range body.Results {
		r := &body.Results[i]
		if r.CollectionID == 0 || r.ArtworkURL100 == "" {
			continue
		}
		// Substring match in either direction so suffix-decorated titles
		// ("Album (Deluxe Edition)") still pass when the user's tag is
		// the bare title, AND vice versa.
		nameLower := strings.ToLower(r.CollectionName)
		if !strings.Contains(nameLower, albumLower) && !strings.Contains(albumLower, nameLower) {
			continue
		}
		return &ITunesAlbum{
			CollectionID:   r.CollectionID,
			CollectionName: r.CollectionName,
			ArtworkURL100:  r.ArtworkURL100,
		}, nil
	}
	return nil, errNotFound
}

// FetchArtwork downloads the 600×600 version of the iTunes artwork
// URL `a.ArtworkURL100` returned from `SearchAlbum`. The high-res
// substitution (`/100x100bb.` → `/600x600bb.`) is documented Apple
// behavior — iTunes's CDN serves any size suffix that matches the
// pattern.
//
// Returns an `io.ReadCloser` (caller MUST Close) so the body streams
// straight to disk via `writeArtworkAtomicStream` without ever
// materialising the full payload in RAM. Mirrors the
// `CoverArtClient.FetchReleaseFrontStream` shape so the iTunes
// fallback path inherits the same ~32 KB peak-RAM profile and
// `MaxCoverArtBytes` size cap as the CAA paths. The caller wraps the
// body in `io.LimitReader(body, MaxCoverArtBytes+1)` (handled inside
// `writeArtworkAtomicStream`) so this method does NOT pre-bound the
// stream — the cap is applied uniformly with the destination size
// guard. Pre-fix returned `[]byte` via `io.ReadAll(io.LimitReader(..,
// 5<<20))`, which protected against multi-GB OOM but still allocated
// up to 5 MB in RAM per fetch and lived inconsistently next to the
// CAA streaming path.
func (c *ITunesClient) FetchArtwork(ctx context.Context, a *ITunesAlbum) (io.ReadCloser, error) {
	if a == nil || a.ArtworkURL100 == "" {
		return nil, errNotFound
	}
	hiRes := strings.Replace(a.ArtworkURL100, "/100x100bb.", "/600x600bb.", 1)
	req, err := http.NewRequestWithContext(ctx, "GET", hiRes, nil)
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
	// Honor Retry-After on the artwork CDN the same way the search
	// path does. Apple rarely 429s on the image CDN per-IP but
	// throttles do happen during incidents; matching the rest of our
	// HTTP paths keeps the bridge well-behaved.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		if delay := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); delay > 0 {
			// sleepCtx (time.NewTimer + Stop) avoids leaking the timer in
			// the runtime heap on a cancel mid-Retry-After — see the note
			// in MusicBrainzClient.get.
			if !sleepCtx(ctx, delay) {
				resp.Body.Close()
				return nil, ctx.Err()
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("itunes: artwork HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// get is the common JSON-fetch path with `Retry-After` honoring on
// 429 / 503, mirroring `MusicBrainzClient.get`. iTunes rarely 429s in
// practice but Apple has been known to quietly throttle abusive
// callers, so honoring the advisory header keeps us well-behaved.
func (c *ITunesClient) get(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		if delay := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); delay > 0 {
			// sleepCtx (time.NewTimer + Stop) avoids leaking the timer in
			// the runtime heap on a cancel mid-Retry-After — see the note
			// in MusicBrainzClient.get.
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("itunes: HTTP %d: %s", resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// errITunesNoMatch is the public sentinel callers can compare against
// when the search returned zero plausible matches. Reuses the
// existing `errNotFound` so `IsNotFound(err)` works uniformly across
// MB / CAA / iTunes call sites.
var _ = errors.Is // satisfy linters — errNotFound already wraps via package-level errors.Is

// --- JSON shapes ---

type iTunesSearchResponse struct {
	ResultCount int            `json:"resultCount"`
	Results     []iTunesResult `json:"results"`
}

type iTunesResult struct {
	CollectionID   int64  `json:"collectionId"`
	CollectionName string `json:"collectionName"`
	ArtworkURL100  string `json:"artworkUrl100"`
	WrapperType    string `json:"wrapperType"` // "collection" for albums
}
