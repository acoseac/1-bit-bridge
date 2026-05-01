package enrich

import (
	"net"
	"net/http"
	"time"
)

// sharedHTTPTransport tunes the connection-pool defaults for the four
// metadata clients (MusicBrainz, CoverArt, Deezer, iTunes). Pre-fix
// each client built `&http.Client{Timeout: ...}` with no Transport,
// falling back to `http.DefaultTransport`'s `MaxIdleConnsPerHost = 2`.
// During a fresh-library enrichment pass the same handful of API
// hosts get hit thousands of times in quick succession; the default
// cap caused connection churn (TLS re-handshake per third request) and
// FD spikes on Pi-class hosts.
//
// The transport is a single package-level instance shared across all
// four default clients — Go's http.Transport is goroutine-safe by
// design and connection pools across hosts are slot-isolated, so the
// share doesn't introduce any cross-client coupling. Tests that pass
// their own `httpClient` (via the New*Client constructors) override
// this entirely; only the default-construction path picks it up.
//
// MaxIdleConnsPerHost: 8 (vs. default 2). Enrichment hits each API
// host with N tracks * <few endpoints>, so a small pool keeps
// connections warm without exploding the FD budget on small hosts.
//
// MaxIdleConns: 64 (global pool cap). Per-host is 8; the enricher
// rotates through MusicBrainz, iTunes, Deezer, CoverArt, the
// Internet Archive (CAA redirect target), Apple's image CDN, and
// occasional Deezer dzcdn hosts — easily 6+ active hosts during a
// fresh-library pass. 64 lets each of those hold its
// MaxIdleConnsPerHost=8 quota without forced eviction (gemini
// medium review on PR #118).
//
// IdleConnTimeout: 60 s. The enricher batches in 100-track windows
// with brief pauses between; 60 s spans the natural between-batch
// idle without aggressively closing.
//
// HTTP/2 enabled (ForceAttemptHTTP2). MusicBrainz / Deezer / iTunes
// support it; multiplexing reduces handshakes further.
//
// `Proxy: http.ProxyFromEnvironment` preserves $HTTPS_PROXY support
// for operators behind corporate proxies.
//
// `DialContext` uses a 5 s connect timeout — well under the 10-30 s
// per-client request timeouts so a dead host fails fast.
var sharedHTTPTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          64,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       60 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}
