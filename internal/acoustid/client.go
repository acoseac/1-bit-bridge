package acoustid

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

// DefaultBaseURL is the public AcoustID web service root.
const DefaultBaseURL = "https://api.acoustid.org/v2"

const (
	// PublicMinInterval paces requests to api.acoustid.org. AcoustID asks for
	// no more than 3 requests/second; 350ms is a deliberate margin over the
	// 333ms that implies.
	PublicMinInterval = 350 * time.Millisecond

	// SelfHostedMinInterval paces any OTHER host — a caching proxy, or a test
	// server. Mirrors enrich.SelfHostedMinInterval's rationale: a host that is
	// not the public service has not asked us for anything.
	SelfHostedMinInterval = 150 * time.Millisecond
)

// publicHosts are the hosts that get PublicMinInterval. Suffix matches are
// dot-anchored so "notacoustid.org" and "acoustid.org.example.com" stay
// third-party (same trap enrich.minIntervalForBase documents).
var publicHosts = []string{"acoustid.org"}

// maxResponseBytes bounds a lookup response. A fingerprint matching a heavily
// compiled recording can carry a lot of release groups, but never megabytes;
// this only stops a hostile or broken upstream streaming forever.
const maxResponseBytes = 8 << 20

// maxDrainBytes bounds how much of a discarded error body we read back before
// Close so the keep-alive connection returns to the idle pool. Twin of
// enrich.maxDrainBytes — keep in lockstep.
const maxDrainBytes = 64 << 10

// maxRetryAfter caps an upstream's advisory wait so a hostile or
// misconfigured header can't park a sweep for arbitrary durations.
const maxRetryAfter = time.Hour

// sharedHTTPTransport mirrors internal/enrich.sharedHTTPTransport's tuning.
// Duplicated rather than imported because internal/enrich will import THIS
// package (the enricher is the consumer), so depending on it here would be a
// cycle. Keep the two in lockstep.
//
// Pool sizing is smaller than enrich's: this client talks to exactly one host,
// and the sweeper's concurrency is deliberately low (default 1 worker), so a
// handful of warm connections is the whole requirement.
var sharedHTTPTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          8,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       60 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// Artist is one credited artist on a recording.
type Artist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ReleaseGroup is one release group a recording appears on.
type ReleaseGroup struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// Recording is one MusicBrainz recording linked to a fingerprint.
type Recording struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Duration is AcoustID's recorded length in seconds. Zero or absent means
	// "unknown", which the gate treats as disqualifying — an unverifiable
	// length is exactly what we cannot accept an MBID on.
	Duration      float64        `json:"duration"`
	Artists       []Artist       `json:"artists"`
	ReleaseGroups []ReleaseGroup `json:"releasegroups"`
}

// Result is one AcoustID match cluster.
type Result struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
	// Sources counts how many users submitted this fingerprint→recording
	// link. It grades the LINK rather than the audio, which makes it the only
	// reliability signal in the payload — see minSources.
	//
	// It is requested via `meta=sources` and arrives per-RESULT, so it cannot
	// discriminate between two recordings hanging off one cluster.
	Sources    int         `json:"sources"`
	Recordings []Recording `json:"recordings"`
}

// lookupResponse is the /v2/lookup envelope. AcoustID signals failure via the
// `status` field, and can do so under an HTTP 200 — so the status is checked
// on every response, not only on non-2xx.
type lookupResponse struct {
	Status  string   `json:"status"`
	Error   *apiErr  `json:"error"`
	Results []Result `json:"results"`
}

type apiErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// httpError carries an upstream status so IsTransient can classify it
// STRUCTURALLY. The message format is load-bearing: IsTransient parses it back
// with HasPrefix + Atoi rather than substring-matching, so a persistent 4xx
// whose body happens to mention "HTTP 503" cannot be misclassified as
// transient and retried forever (the trap enrich.httpError documents).
type httpError struct {
	StatusCode int
	Body       string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("acoustid: HTTP %d: %s", e.StatusCode, e.Body)
}

// StatusCode exposes the upstream status for callers that want to branch on it
// without re-parsing the message.
func (e *httpError) Status() int { return e.StatusCode }

// ErrNoMatch reports that AcoustID answered cleanly and knows nothing about
// this fingerprint. Distinct from an error: it is a fact about the audio, and
// callers record it rather than retrying.
var ErrNoMatch = errors.New("acoustid: no match")

// Client talks to the AcoustID web service.
type Client struct {
	base      string
	apiKey    string
	userAgent string
	http      *http.Client

	// minInterval is the politeness pacing the CALLER must apply before each
	// request. It lives here, derived from base at construction, so it can
	// never drift out of sync with the host it protects — the same reason
	// enrich.MusicBrainzClient carries its own (see internal/enrich/pacing.go).
	minInterval time.Duration
}

// NewClient builds an AcoustID client. An empty baseURL uses DefaultBaseURL;
// a nil httpClient gets a 20s-timeout default over the shared transport.
//
// The 20s timeout covers the whole exchange including the body read, which is
// safe here because responses are small and bounded — unlike a binary
// download, where a whole-exchange timeout is the trap that PR #374 fixed.
func NewClient(baseURL, apiKey, userAgent string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second, Transport: sharedHTTPTransport}
	}
	return &Client{
		base:        baseURL,
		apiKey:      apiKey,
		userAgent:   userAgent,
		http:        httpClient,
		minInterval: minIntervalForBase(baseURL),
	}
}

// MinInterval is the minimum spacing the caller must leave between requests.
func (c *Client) MinInterval() time.Duration { return c.minInterval }

// minIntervalForBase picks the pacing for a client pointed at base.
//
// FAILS SAFE: an unparseable or host-less base resolves to PublicMinInterval,
// so a malformed config can only ever make us MORE polite, never less.
func minIntervalForBase(base string) time.Duration {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" {
		return PublicMinInterval
	}
	host := strings.ToLower(u.Hostname())
	for _, ph := range publicHosts {
		if host == ph || strings.HasSuffix(host, "."+ph) {
			return PublicMinInterval
		}
	}
	return SelfHostedMinInterval
}

// lookupMeta is the metadata the gate needs, and it must be requested
// explicitly — AcoustID returns bare AcoustID IDs by default.
//
//   - recordings:    the recording MBIDs and their artist credits
//   - releasegroups: nested per recording; backs the unambiguous-album case
//   - sources:       the submission count the gate's reliability clause grades
//   - compress:      gzip the response body
//
// Dropping any of the first three silently disables a gate clause rather than
// failing loudly, which is why they are one constant and not a builder.
const lookupMeta = "recordings+releasegroups+sources+compress"

// Lookup submits a fingerprint and returns the matching clusters, best score
// first (AcoustID orders them, and the gate re-checks rather than assuming).
//
// Returns ErrNoMatch when AcoustID answers cleanly with no results.
func (c *Client) Lookup(ctx context.Context, fp Fingerprint) ([]Result, error) {
	if fp.Value == "" {
		return nil, fmt.Errorf("acoustid: empty fingerprint")
	}
	if c.apiKey == "" {
		return nil, fmt.Errorf("acoustid: no API key configured")
	}

	q := url.Values{}
	q.Set("client", c.apiKey)
	q.Set("duration", strconv.Itoa(fp.DurationSeconds()))
	q.Set("fingerprint", fp.Value)
	q.Set("format", "json")
	// url.Values.Encode() would percent-escape the '+' separators that the
	// meta parameter uses as its own delimiter, so it is appended raw.
	endpoint := c.base + "/lookup?" + q.Encode() + "&meta=" + lookupMeta

	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	var payload lookupResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("acoustid: decoding response: %w", err)
	}
	// AcoustID can report failure under an HTTP 200, so status is checked on
	// every response rather than only on a non-2xx.
	if payload.Status != "ok" {
		code, msg := 0, "unknown error"
		if payload.Error != nil {
			code, msg = payload.Error.Code, payload.Error.Message
		}
		return nil, fmt.Errorf("acoustid: upstream error %d: %s", code, msg)
	}
	if len(payload.Results) == 0 {
		return nil, ErrNoMatch
	}
	return payload.Results, nil
}

// get performs the request and returns the body bytes.
//
// The URL is NEVER logged or embedded in an error: it carries `client=<key>`
// and a multi-kilobyte fingerprint. Errors name the endpoint's status only.
func (c *Client) get(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		// err from NewRequest embeds the URL — replace it wholesale rather
		// than wrapping, so the API key cannot reach a log.
		return nil, fmt.Errorf("acoustid: malformed request URL")
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error stringifies with the full URL. Unwrap to the cause so the
		// API key and fingerprint never reach a log line.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return nil, fmt.Errorf("acoustid: %s: %w", urlErr.Op, urlErr.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		drainBody(resp.Body)
		herr := &httpError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(b))}
		// Surface the upstream's own advice so the caller can honour it
		// instead of guessing a backoff.
		if d := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); d > 0 {
			return nil, &RateLimitError{RetryAfter: d, err: herr}
		}
		return nil, herr
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	drainBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("acoustid: reading response: %w", err)
	}
	return body, nil
}

// RateLimitError carries the upstream's Retry-After advice.
//
// It exists so the sweeper can pause its WHOLE pool rather than only the
// worker that hit the limit. That matters more here than it does for
// MusicBrainz: the expensive decode happens BEFORE the lookup, so continuing
// to fingerprint into a rate-limited wall burns CPU — and, on a
// network-backed library, egress that has already been paid for.
type RateLimitError struct {
	RetryAfter time.Duration
	err        error
}

func (e *RateLimitError) Error() string { return e.err.Error() }
func (e *RateLimitError) Unwrap() error { return e.err }

// IsTransient reports whether err is worth retrying later.
//
// Transient: 429, any 5xx, timeouts, and the connection-level errnos that a
// restarting upstream or a boot-before-network window produce. Persistent:
// context.Canceled, 4xx other than 429, and decode failures — those are
// guaranteed to fail identically on retry.
//
// The HTTP classification parses the status STRUCTURALLY out of
// httpError.Error()'s stable prefix. Substring-matching the message would
// misclassify a persistent 4xx whose body quotes "HTTP 503" as transient and
// retry it forever — the poisoning class documented on enrich.IsTransient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	// Checked FIRST: a cancel surfaces as context.Canceled, and treating it as
	// transient would have a shutting-down sweeper log a spurious retry.
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return true
	}
	var herr *httpError
	if errors.As(err, &herr) {
		return herr.StatusCode >= 500 || herr.StatusCode == http.StatusTooManyRequests
	}
	// Fall back to the stable message prefix for an httpError that reached us
	// through a formatting wrap rather than %w.
	if code, ok := statusFromMessage(err.Error()); ok {
		return code >= 500 || code == http.StatusTooManyRequests
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// A DNS failure against a hardcoded-valid host is environmental (resolver
	// restart, boot-before-network, a captive portal answering SERVFAIL), not
	// a data-contract problem — transient EXCEPT a hard NXDOMAIN, the one
	// shape that says the host is genuinely gone. Falls THROUGH on NXDOMAIN
	// rather than returning, so the errno arm still gets a look.
	//
	// Typed-nil guard: errors.As can report true with a nil pointer, and
	// reading IsNotFound off it would panic.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr != nil && !dnsErr.IsNotFound {
		return true
	}
	// TCP/route-level failures surface as wrapped errnos (net.OpError →
	// os.SyscallError → errno). ECONNREFUSED and ENETUNREACH belong here: an
	// upstream restart or a boot-before-network window clears in seconds, and
	// classifying them persistent is the PR #74 poisoning class.
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}

// statusFromMessage recovers the status code from httpError's stable message
// prefix. Anchored with HasPrefix so it can only ever read the code this
// package wrote, never a number quoted inside an upstream body.
func statusFromMessage(msg string) (int, bool) {
	const prefix = "acoustid: HTTP "
	i := strings.Index(msg, prefix)
	if i < 0 {
		return 0, false
	}
	rest := msg[i+len(prefix):]
	end := strings.IndexByte(rest, ':')
	if end < 0 {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(rest[:end]))
	if err != nil {
		return 0, false
	}
	return code, true
}

// drainBody discards up to maxDrainBytes so the underlying connection can be
// reused: net/http only returns an HTTP/1.1 connection to the idle pool if the
// body was read to EOF. Nil-safe for test mocks.
func drainBody(body io.Reader) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxDrainBytes))
}

// parseRetryAfter returns the duration to wait per RFC 9110 §10.2.3 — a
// delta-seconds non-negative integer OR an HTTP-date. Zero means "no advice
// given"; the caller must not sleep on it.
//
// Faithful twin of internal/enrich.parseRetryAfter — keep in lockstep. It is
// duplicated rather than shared because internal/enrich imports THIS package,
// so importing it back would be a cycle. Every subtlety below is load-bearing
// and was fixed once already there:
//
//   - the cap is applied in the SECONDS domain before multiplying, because a
//     header of 2^33 would otherwise overflow time.Duration's int64 nanoseconds
//     and silently bypass it;
//   - ParseInt(..., 64) so 32-bit platforms don't lose [2^31, 2^63);
//   - a value exceeding int64 returns ErrRange, which clamps to maxRetryAfter
//     rather than falling through to 0 (which would defeat the cap entirely);
//   - a non-compliant fractional value ("86400.5") is truncated at the '.', so
//     the integer prefix is honoured instead of the whole header being dropped.
//     Guarded on -1 so HTTP-dates, which contain no '.', are never mis-sliced.
func parseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
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
	var numErr *strconv.NumError
	if errors.As(err, &numErr) &&
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
