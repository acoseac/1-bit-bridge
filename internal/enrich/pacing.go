package enrich

import (
	"net/url"
	"strings"
	"time"
)

// Politeness pacing for the metadata clients.
//
// The intervals below exist to respect the PUBLIC services' published limits:
// MusicBrainz asks anonymous clients for at most 1 req/s (we pace at 1.1s for
// margin), and Cover Art Archive is Internet Archive infrastructure where 500ms
// is the polite floor. That contract is load-bearing and must not be weakened.
//
// It is also, however, a contract with two SPECIFIC HOSTS — and both clients
// accept an operator-supplied base URL. When the bridge is pointed at a
// self-hosted mirror (the Atlas deployment at `<host>/ws/2` is the motivating
// case), the same sleeps are pure dead time against the operator's own server:
// measured on a 19,482-track library, ~60% of the enricher's wall clock was
// spent asleep waiting on a private host that has no such limit.
//
// So the pacing travels WITH the base URL rather than being a fixed property of
// the enricher. A default-configured bridge — no `enrich.musicbrainzBaseURL` /
// `enrich.coverArtBaseURL` set — resolves to the public defaults and paces
// exactly as before, byte for byte.
const (
	// PublicMBMinInterval paces requests to musicbrainz.org. MB's anonymous
	// limit is 1 req/s; 1.1s is a deliberate margin.
	PublicMBMinInterval = 1100 * time.Millisecond

	// PublicCAAMinInterval paces requests to coverartarchive.org, which is
	// hosted on Internet Archive infrastructure with more lenient limits.
	PublicCAAMinInterval = 500 * time.Millisecond

	// SelfHostedMinInterval paces requests to any OTHER host — an Atlas
	// mirror, a local musicbrainz-docker, a caching reverse proxy.
	//
	// Not zero, for two reasons. (1) Atlas's own public-tier gate throttles
	// /ws/2/, /release/, /release-group/ and /artist/ at 600 req/min per IP
	// (10 rps sustained, burst 100) and the enricher SHARES that per-IP bucket
	// with the harvest client's cover sweep — 150ms is ~6.7 rps, which leaves
	// headroom. (2) A self-hosted mirror is still someone's server; hammering
	// it as fast as the loop can spin is not obviously better than saturating
	// its useful throughput.
	SelfHostedMinInterval = 150 * time.Millisecond
)

// publicMBHosts / publicCAAHosts are the registrable domains whose published
// rate limits the constants above encode. A subdomain counts (beta.musicbrainz.org
// is the same operator under the same policy).
var (
	publicMBHosts  = []string{"musicbrainz.org"}
	publicCAAHosts = []string{"coverartarchive.org", "archive.org"}
)

// minIntervalForBase picks the pacing interval for a client pointed at base.
//
// FAILS SAFE: an unparseable or host-less base resolves to publicInterval, so a
// malformed config can only ever make us MORE polite, never less. Config's
// normalizeBaseURL already requires an absolute http(s) URL, so this is a
// backstop rather than a live path.
func minIntervalForBase(base string, publicInterval time.Duration, publicHosts []string) time.Duration {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || u.Hostname() == "" {
		return publicInterval
	}
	host := strings.ToLower(u.Hostname())
	for _, ph := range publicHosts {
		if host == ph || strings.HasSuffix(host, "."+ph) {
			return publicInterval
		}
	}
	return SelfHostedMinInterval
}
