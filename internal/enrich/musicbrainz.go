// Package enrich looks up external metadata (MusicBrainz, Cover Art
// Archive) for tracks that are missing MBIDs or artwork, then updates the
// manifest store.
//
// Rate limits are respected without authentication: MusicBrainz asks for
// at most 1 request per second from anonymous clients, so the Enricher
// paces itself to 1.1s (comfortable margin). Cover Art Archive is hosted
// on Internet Archive infrastructure with more lenient limits; we pace
// at 500ms for politeness. Both services require a User-Agent identifying
// the app + a contact URL.
//
// License: MusicBrainz data is CC0 (public domain). Cover Art Archive
// images are predominantly CC-BY or CC0; attribution may be required
// depending on the specific image — the iOS app carries the link back to
// the canonical MusicBrainz release page so users can verify licensing.
package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultMusicBrainzBase is the public MusicBrainz API root. Overridable
// in tests via NewMusicBrainzClient.
const DefaultMusicBrainzBase = "https://musicbrainz.org/ws/2"

// MusicBrainzClient queries the MusicBrainz release-search endpoint to
// resolve an album MBID from (artist, album) and an artist MBID from
// (artist).
type MusicBrainzClient struct {
	base      string
	userAgent string
	http      *http.Client
}

// NewMusicBrainzClient constructs a client. userAgent MUST be set to
// something identifying the caller (MB requires this).
func NewMusicBrainzClient(base, userAgent string, httpClient *http.Client) *MusicBrainzClient {
	if base == "" {
		base = DefaultMusicBrainzBase
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &MusicBrainzClient{base: base, userAgent: userAgent, http: httpClient}
}

// SearchResult is the trimmed-down result of a release or artist search.
type SearchResult struct {
	MBID  string
	Score int
	Title string // release: album title; artist: artist name
	// ReleaseGroupMBID is populated for release searches when the MB
	// response carries a release-group reference. Enables the CAA
	// release-group fallback for releases without a front cover at
	// the specific release level but where the release-group has
	// cover art from a sibling pressing. Empty for artist searches
	// and for releases without a release-group association.
	ReleaseGroupMBID string
}

// SearchRelease queries MusicBrainz for the best release matching
// (artist, album). Returns ("", nil) if no plausible match exists.
// Matching strategy:
//  1. Pass the query to MB's Lucene-style release search.
//  2. MB returns up to 25 scored candidates.
//  3. We pick the top candidate whose score >= 80 and whose title +
//     artist-credit substring-match the inputs (case-insensitive).
func (c *MusicBrainzClient) SearchRelease(ctx context.Context, artist, album string) (*SearchResult, error) {
	artist = strings.TrimSpace(artist)
	album = strings.TrimSpace(album)
	if artist == "" || album == "" {
		return nil, nil
	}

	q := fmt.Sprintf(`release:"%s" AND artist:"%s"`, escapeLucene(album), escapeLucene(artist))
	u := fmt.Sprintf("%s/release/?query=%s&fmt=json&limit=10", c.base, url.QueryEscape(q))

	var body releaseSearchResponse
	if err := c.get(ctx, u, &body); err != nil {
		return nil, err
	}
	best := pickBestRelease(body.Releases, album, artist)
	if best == nil {
		return nil, nil
	}
	result := &SearchResult{MBID: best.ID, Score: best.Score, Title: best.Title}
	if best.ReleaseGroup != nil {
		result.ReleaseGroupMBID = best.ReleaseGroup.ID
	}
	return result, nil
}

// ReleaseGroupMBID fetches the release-group MBID for a given release
// MBID. Used by the CAA release-group fallback when a track had an
// embedded release MBID (no prior SearchRelease call that would have
// populated the association during enrichment).
//
// Returns ("", nil) if the release has no release-group association
// (rare — most MB releases do). Returns errNotFound wrapped when the
// release MBID is unknown to MB (handle with IsNotFound).
func (c *MusicBrainzClient) ReleaseGroupMBID(ctx context.Context, releaseMBID string) (string, error) {
	releaseMBID = strings.TrimSpace(releaseMBID)
	if releaseMBID == "" {
		return "", fmt.Errorf("musicbrainz: empty release mbid")
	}
	u := fmt.Sprintf("%s/release/%s?fmt=json&inc=release-groups", c.base, url.PathEscape(releaseMBID))
	var body releaseLookupResponse
	if err := c.get(ctx, u, &body); err != nil {
		return "", err
	}
	if body.ReleaseGroup == nil {
		return "", nil
	}
	return body.ReleaseGroup.ID, nil
}

// SearchArtist returns the best-matching artist MBID for the given
// artist name. Used for artist-image lookup in a later PR.
func (c *MusicBrainzClient) SearchArtist(ctx context.Context, artist string) (*SearchResult, error) {
	artist = strings.TrimSpace(artist)
	if artist == "" {
		return nil, nil
	}
	q := fmt.Sprintf(`artist:"%s"`, escapeLucene(artist))
	u := fmt.Sprintf("%s/artist/?query=%s&fmt=json&limit=5", c.base, url.QueryEscape(q))

	var body artistSearchResponse
	if err := c.get(ctx, u, &body); err != nil {
		return nil, err
	}
	best := pickBestArtist(body.Artists, artist)
	if best == nil {
		return nil, nil
	}
	return &SearchResult{MBID: best.ID, Score: best.Score, Title: best.Name}, nil
}

func (c *MusicBrainzClient) get(ctx context.Context, u string, out any) error {
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
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	// MB asks anonymous clients to back off when it's overloaded. Honor
	// `Retry-After` (delta-seconds OR HTTP-date) by sleeping here before
	// returning the error — the enricher's batch loop is single-threaded
	// through this method (one MB call at a time per Run goroutine) so a
	// process-wide pause isn't needed; pacing the next call from the
	// same client is sufficient. Without this, a sustained 429 plus the
	// 15s `PollInterval` had us re-hit MB at ~4 calls/min through the
	// advisory window — well-meaning but rude.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		if delay := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("musicbrainz: HTTP %d: %s", resp.StatusCode, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// parseRetryAfter returns the duration to wait per RFC 9110 §10.2.3
// (HTTP semantics) — a delta-seconds non-negative integer OR an
// HTTP-date. Returns zero if the header is absent or unparseable; the
// caller must treat zero as "no advice given" and fall through to its
// default behavior (don't sleep on a missing/malformed header).
//
// The `now` parameter is injected for testability; production callers
// pass `time.Now()`.
func parseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		// Cap at 1h to avoid a hostile / misconfigured upstream parking
		// the enricher for its full advised duration. MB has never asked
		// for more than ~5 minutes in practice; 1h is the comfortable
		// upper bound.
		const maxRetryAfter = time.Hour
		d := time.Duration(secs) * time.Second
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d
	}
	if t, err := http.ParseTime(header); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
		const maxRetryAfter = time.Hour
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d
	}
	return 0
}

// errNotFound is used internally; callers don't need to distinguish.
var errNotFound = errors.New("not found")

// IsNotFound reports whether err indicates the lookup returned 404.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }

// --- JSON shapes (only the fields we actually consume) ---

type releaseSearchResponse struct {
	Releases []releaseCandidate `json:"releases"`
}

// releaseLookupResponse decodes a `GET /release/{mbid}?inc=release-groups`
// single-release response. Only the release-group ID is consumed today;
// the rest of the release document is ignored.
type releaseLookupResponse struct {
	ReleaseGroup *releaseGroup `json:"release-group,omitempty"`
}

type releaseCandidate struct {
	ID           string         `json:"id"`
	Score        int            `json:"score"`
	Title        string         `json:"title"`
	ArtistCredit []artistCredit `json:"artist-credit"`
	// MusicBrainz returns release-group as an object
	// ({id, title, primary-type}) when present, not a bare string. The
	// previous `string` type silently blew up every enrichment against
	// the public API. Keeping it typed so we can reach `.ID` for the
	// future release-group artwork fallback.
	ReleaseGroup *releaseGroup `json:"release-group,omitempty"`
}

type releaseGroup struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	PrimaryType string `json:"primary-type"`
}

type artistCredit struct {
	Name string `json:"name"`
}

type artistSearchResponse struct {
	Artists []artistCandidate `json:"artists"`
}

type artistCandidate struct {
	ID    string `json:"id"`
	Score int    `json:"score"`
	Name  string `json:"name"`
}

// --- matching ---

func pickBestRelease(candidates []releaseCandidate, album, artist string) *releaseCandidate {
	type scored struct {
		r     *releaseCandidate
		score int
	}
	var out []scored
	for i := range candidates {
		c := &candidates[i]
		// Matches the >=80 contract in the SearchRelease docstring.
		// Lower scores tend to be artist-collision false positives
		// (e.g. a Dire Straits album mis-attributed to a tribute band).
		if c.Score < 80 {
			continue
		}
		if !caseInsensitiveMatch(c.Title, album) {
			continue
		}
		if !anyArtistMatches(c.ArtistCredit, artist) {
			continue
		}
		// Weight: MB score + exact-match bonuses.
		s := c.Score
		if strings.EqualFold(c.Title, album) {
			s += 10
		}
		if len(c.ArtistCredit) > 0 && strings.EqualFold(c.ArtistCredit[0].Name, artist) {
			s += 10
		}
		out = append(out, scored{r: c, score: s})
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out[0].r
}

func pickBestArtist(candidates []artistCandidate, artist string) *artistCandidate {
	for i := range candidates {
		c := &candidates[i]
		if c.Score < 80 {
			continue
		}
		if strings.EqualFold(c.Name, artist) {
			return c
		}
	}
	// No exact-name match — fall back to the top result if its score is
	// high enough for a fuzzy match.
	if len(candidates) > 0 && candidates[0].Score >= 90 {
		return &candidates[0]
	}
	return nil
}

func caseInsensitiveMatch(a, b string) bool {
	return strings.Contains(strings.ToLower(a), strings.ToLower(b)) ||
		strings.Contains(strings.ToLower(b), strings.ToLower(a))
}

func anyArtistMatches(credits []artistCredit, artist string) bool {
	al := strings.ToLower(artist)
	for _, c := range credits {
		if strings.Contains(strings.ToLower(c.Name), al) || strings.Contains(al, strings.ToLower(c.Name)) {
			return true
		}
	}
	return false
}

// escapeLucene escapes the small set of characters that MusicBrainz's
// Lucene-style query parser treats as special in text field values.
func escapeLucene(s string) string {
	specials := `+-&|!(){}[]^"~*?:\/`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(specials, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
