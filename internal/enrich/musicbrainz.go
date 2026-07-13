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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
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
		// sharedHTTPTransport tunes the pool size for metadata
		// API hosts — see transport.go for the rationale.
		httpClient = &http.Client{Timeout: 10 * time.Second, Transport: sharedHTTPTransport}
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
		// Drain the (usually tiny) 404 body so the deferred Close returns the
		// HTTP/1.1 connection to the idle pool instead of dropping it — MB 404
		// (album not on MB) is the common cold-cache case.
		drainBody(resp.Body)
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
			// Use sleepCtx (time.NewTimer + Stop) rather than a bare
			// time.After select: a long Retry-After (capped at 1h via
			// maxRetryAfter) plus a SIGTERM would otherwise leave the
			// time.After timer in the runtime heap until the full delay
			// elapsed — the same leak the enricher's pacing loops avoid.
			if !sleepCtx(ctx, delay) {
				return ctx.Err()
			}
		}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// Drain any remainder past the 512-byte error snippet so the deferred
		// Close can reuse the connection.
		drainBody(resp.Body)
		return &httpError{StatusCode: resp.StatusCode, Body: string(b)}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// httpError carries the upstream MB HTTP status code in a typed shape
// `IsTransient` (and any future classifier) consumes via `errors.As`,
// so the transient/persistent decision doesn't depend on `strings.HasPrefix`
// over the formatted message. Pre-fix `IsTransient` parsed the status
// out of "musicbrainz: HTTP NNN: …" with a load-bearing string-prefix
// check; any caller that wrapped the error (`fmt.Errorf("foo: %w", err)`)
// silently broke the prefix match, made `IsTransient` return false on a
// real 5xx, and reintroduced the PR #74 poisoning regression.
//
// `Error()` preserves the prior format byte-for-byte so log lines and
// existing string-shape assertions don't drift.
type httpError struct {
	StatusCode int
	Body       string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("musicbrainz: HTTP %d: %s", e.StatusCode, e.Body)
}

// maxRetryAfter caps the wait to protect against a hostile or
// misconfigured upstream parking the enricher for arbitrary durations.
// MB has never asked for more than a few minutes in practice; 1h is
// the comfortable upper bound.
const maxRetryAfter = time.Hour

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
	// Apply the cap in the *seconds* domain before multiplying by
	// time.Second — a Retry-After of e.g. 2^33 would otherwise overflow
	// time.Duration (int64 nanoseconds) during the multiplication and
	// silently bypass the cap. Use ParseInt(64) so platforms where
	// `int` is 32-bit don't lose values in the [2^31, 2^63) range
	// either; clamp before the conversion either way.
	//
	// Numeric values that exceed int64 (>2^63 − 1) make ParseInt return
	// `*NumError{Err: ErrRange}`. Treat those as "absurdly large
	// advisory" → clamp to maxRetryAfter rather than falling through to
	// 0, which would defeat the cap entirely for hostile or
	// misconfigured upstreams. Negative-overflow ("-9999...") still
	// falls through to 0 like other malformed inputs.
	//
	// A non-compliant upstream may send a fractional delta-seconds
	// ("86400.5"). RFC 9110 mandates an integer, so ParseInt would reject
	// the whole string with a *syntax* error (not ErrRange) — without
	// trimming, it then falls through http.ParseTime, fails again, and
	// returns 0, silently dropping the backoff. Strip at the first '.' so
	// the integer prefix is honoured (and the ErrRange cap below still
	// fires for a giant fractional value). Guarded on -1 so a header with
	// no '.' — including every HTTP-date form, none of which contain one —
	// is left untouched and never mis-sliced.
	if idx := strings.IndexByte(header, '.'); idx != -1 {
		header = header[:idx]
	}
	secs, err := strconv.ParseInt(header, 10, 64)
	if err == nil && secs >= 0 {
		maxSecs := int64(maxRetryAfter / time.Second)
		if secs > maxSecs {
			secs = maxSecs
		}
		return time.Duration(secs) * time.Second
	}
	if numErr, ok := err.(*strconv.NumError); ok &&
		errors.Is(numErr.Err, strconv.ErrRange) &&
		!strings.HasPrefix(header, "-") {
		return maxRetryAfter
	}
	if t, err := http.ParseTime(header); err == nil {
		d := t.Sub(now)
		if d <= 0 {
			return 0
		}
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

// IsTransient reports whether err is a transient infrastructure
// failure (network blip, timeout, server overload) that the
// enricher should NOT permanently mark a track as skipped for.
// Pre-fix (PR #N), the enricher called `markSkipped` on every
// SearchRelease error — a 30-second MusicBrainz outage permanently
// poisoned every track currently being enriched, with no retry path
// short of bumping `enriched_at` back to 0 in SQL.
//
// Transient signals:
//   - HTTP 5xx (server-side overload / restart)
//   - HTTP 429 (rate limit; we already honor Retry-After but the
//     batch-level skip stamp must not fire)
//   - net.Error.Timeout() (connect timeout, read timeout)
//   - context.DeadlineExceeded (per-request deadline; distinct from
//     ctx.Err() cancellation which is handled separately)
//   - syscall.ECONNRESET / ECONNABORTED / EPIPE / ETIMEDOUT (TCP-level
//     resets)
//   - syscall.ECONNREFUSED / ENETUNREACH / EHOSTUNREACH (MB restart
//     window, boot-before-network, route flap — all clear in seconds)
//   - *net.DNSError other than NXDOMAIN (local resolver restart,
//     boot-before-network, SERVFAIL/REFUSED from a captive portal or
//     Pi-hole — environmental against our hardcoded-valid hosts)
//
// Persistent (NOT transient):
//   - errNotFound (HTTP 404 — the album genuinely isn't on MB)
//   - *net.DNSError with IsNotFound (NXDOMAIN — the host is genuinely gone)
//   - JSON decode errors (schema drift; will fail every retry)
//   - HTTP 4xx other than 429 (bad request shape, auth, etc.)
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// Don't double-classify a pure cancel as transient.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// net.Error covers most timeout / connect-refused shapes.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// DNS resolution failures against our hardcoded-valid hosts are
	// environmental (local resolver restart, boot-before-network, a captive
	// portal or Pi-hole returning SERVFAIL / REFUSED), NOT a data-contract
	// problem — treat any *net.DNSError as transient EXCEPT a hard NXDOMAIN
	// (IsNotFound), the one shape that says "this host is genuinely gone."
	// This COMPLEMENTS the errno arm below: a fully-down network surfaces as
	// EITHER a *net.DNSError (resolution failed) OR a raw ENETUNREACH /
	// ECONNREFUSED (route/connect failed), and the two arms together cover
	// both. Gated on IsNotFound rather than the Go-1.18-deprecated
	// IsTemporary / Temporary() (which trips staticcheck SA1019 and is
	// platform-leaky). Gemini-consulted 2026-07-13.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && !dnsErr.IsNotFound {
		return true
	}
	// TCP/route-level failures surface as wrapped syscall errnos
	// (net.OpError → os.SyscallError → errno; errors.Is unwraps the
	// chain). ECONNREFUSED / ENETUNREACH / EHOSTUNREACH belong here:
	// a MusicBrainz restart (refused) or a boot-before-network window
	// (unreachable) clears in seconds, and classifying them persistent
	// markSkipped-stamps every in-flight track — the exact PR #74
	// poisoning class.
	if errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}
	// HTTP status is consulted via the typed `httpError` carried by
	// `MusicBrainzClient.get`. `errors.As` survives any number of
	// `fmt.Errorf("...: %w", err)` wraps a future caller might add,
	// where the prior `strings.HasPrefix(msg, "musicbrainz: HTTP ")`
	// shape would silently misclassify a wrapped 5xx as non-transient
	// and reintroduce the PR #74 poisoning regression.
	//
	// The pre-fix substring guard against an HTML error body that
	// mentions "HTTP 503" elsewhere is now structural — the typed
	// StatusCode is the response status code, not anything text-
	// matched out of the body.
	var herr *httpError
	if errors.As(err, &herr) {
		if herr.StatusCode >= 500 || herr.StatusCode == 429 {
			return true
		}
	}
	return false
}

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
	// Hoist the query-side lowercasing out of the candidate loop — album
	// and artist are fixed across every candidate.
	albumLower := strings.ToLower(album)
	artistLower := strings.ToLower(artist)
	var best *releaseCandidate
	bestScore := 0
	for i := range candidates {
		c := &candidates[i]
		// Matches the >=80 contract in the SearchRelease docstring.
		// Lower scores tend to be artist-collision false positives
		// (e.g. a Dire Straits album mis-attributed to a tribute band).
		if c.Score < 80 {
			continue
		}
		if !caseInsensitiveContains(c.Title, albumLower) {
			continue
		}
		if !anyArtistMatchesLower(c.ArtistCredit, artistLower) {
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
		// Linear max-scan: keep the FIRST candidate that reaches the top
		// score (strict `>` preserves the stable "first of equal score
		// wins" tie-break the previous sort.SliceStable produced).
		if best == nil || s > bestScore {
			best = c
			bestScore = s
		}
	}
	return best
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

// caseInsensitiveContains reports whether a and bLower overlap as
// case-insensitive substrings (either direction). bLower MUST already be
// lowercased by the caller (hoisted out of the candidate loop); only a —
// which varies per candidate — is lowered here.
func caseInsensitiveContains(a, bLower string) bool {
	if a == "" || bLower == "" {
		// Neither side may be empty: strings.Contains with an empty needle is
		// trivially true, which would over-match. In practice bLower is the
		// always-non-empty album query, but guard both sides for future callers.
		return false
	}
	if strings.EqualFold(a, bLower) {
		return true // exact (case-insensitive) match — skip the ToLower alloc
	}
	la := strings.ToLower(a)
	return strings.Contains(la, bLower) || strings.Contains(bLower, la)
}

// anyArtistMatchesLower reports whether any credit name overlaps
// artistLower (already lowercased by the caller) as a case-insensitive
// substring, either direction.
func anyArtistMatchesLower(credits []artistCredit, artistLower string) bool {
	for _, c := range credits {
		lc := strings.ToLower(c.Name)
		if strings.Contains(lc, artistLower) || strings.Contains(artistLower, lc) {
			return true
		}
	}
	return false
}

// luceneSpecialChars are the characters MusicBrainz's Lucene-style query
// parser treats as special in text field values. All single-byte ASCII.
const luceneSpecialChars = `+-&|!(){}[]^"~*?:\/`

// luceneSpecials is the byte-indexed lookup table for escapeLucene. A
// [256]bool indexed by a raw byte is both complete (every special is ASCII)
// and impossible to index out of bounds — which is why escapeLucene scans
// bytes rather than runes (a [128]bool indexed by a non-ASCII rune like
// 'é'/CJK would panic). The lead/continuation bytes of a multi-byte rune
// are all >=0x80 and never set here, so byte-scanning is exactly equivalent
// to the old rune scan but skips the O(len(specials)) ContainsRune probe.
var luceneSpecials = func() [256]bool {
	var t [256]bool
	for i := 0; i < len(luceneSpecialChars); i++ {
		t[luceneSpecialChars[i]] = true
	}
	return t
}()

// escapeLucene escapes the small set of characters that MusicBrainz's
// Lucene-style query parser treats as special in text field values.
func escapeLucene(s string) string {
	// Fast path: the vast majority of artist / album names carry no Lucene
	// specials, so a single scan lets the common case return the input
	// verbatim — zero alloc, zero copy (gemini-code-assist on PR #424).
	needEscape := false
	for i := 0; i < len(s); i++ {
		if luceneSpecials[s[i]] {
			needEscape = true
			break
		}
	}
	if !needEscape {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if luceneSpecials[c] {
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}
