package enrich

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// AtlasCredentialSource supplies the bridge's runtime bulk_harvest credential
// for authenticated premium-cover fetches: the Atlas bearer token + base URL,
// provisioned by the iOS app over the Phase-H channel and held in
// atlasharvest.StateStore (NOT baked into the open-source binary). ok=false
// means no usable credential (none provisioned, or expired) → the premium
// fetch is skipped and the caller falls through to the public CAA path.
// *atlasharvest.StateStore satisfies this.
type AtlasCredentialSource interface {
	AtlasCredential() (token, baseURL string, ok bool)
}

// PremiumCoverFetcher fetches an authenticated premium (Qobuz/Tidal-grade)
// cover from Atlas and caches it at a given path. Returns true only on a
// cached hit; false (the caller falls through to the CAA chain) on any
// miss/error. A nil PremiumCoverFetcher on the Enricher = premium covers not
// configured (CAA-only enrichment, the default).
type PremiumCoverFetcher interface {
	TryCache(ctx context.Context, path, mbid string, size int) bool
}

// atlasPremiumFetcher implements PremiumCoverFetcher against Atlas's
// authenticated artwork proxy (GET <base>/release/{mbid}/front-{size} with the
// bulk_harvest bearer). Atlas serves the cross-source premium canonical when
// one exists (its 1200² blob satisfies any size ≤ 1200) and otherwise falls
// through to CAA, so a single authed request yields the best available cover —
// which is why this slots ahead of the bridge's own CAA chain.
type atlasPremiumFetcher struct {
	cred      AtlasCredentialSource
	userAgent string
	http      *http.Client
}

// NewAtlasPremiumFetcher builds the premium-cover fetcher. A nil httpClient
// gets a 30s-timeout default — an image fetch over a high-latency relay link
// must not hang background enrichment forever (http.DefaultClient has no
// timeout).
func NewAtlasPremiumFetcher(cred AtlasCredentialSource, userAgent string, httpClient *http.Client) PremiumCoverFetcher {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &atlasPremiumFetcher{cred: cred, userAgent: userAgent, http: httpClient}
}

// TryCache fetches the premium cover for (mbid, size) and streams it to path.
//
// INVARIANT: the image BYTES are fetched directly from the authenticated
// artwork endpoint and streamed straight to disk — NEVER ferried through the
// harvest JSON. Returns false on any non-200 (no credential, 404, 401/403,
// 5xx, network) so the caller falls through to the CAA chain; only a streamed
// 200 returns true. Never propagates an error — a premium miss must not fail
// the whole artwork-caching step.
func (f *atlasPremiumFetcher) TryCache(ctx context.Context, path, mbid string, size int) bool {
	token, baseURL, ok := f.cred.AtlasCredential()
	if !ok {
		return false // no usable credential → CAA path
	}
	url := strings.TrimRight(baseURL, "/") + fmt.Sprintf("/release/%s/front-%d", mbid, size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		logger.Warn("atlas premium cover fetch", "mbid", mbid, "size", size, "err", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 = no cover at Atlas (normal — fall through to CAA rg + iTunes);
		// 401/403 = token rejected (the harvest client owns clearing the
		// credential); 5xx = Atlas hiccup. In every case fall through rather
		// than failing enrichment; only 5xx is noisy enough to log.
		if resp.StatusCode >= 500 {
			logger.Warn("atlas premium cover fetch", "mbid", mbid, "size", size, "status", resp.StatusCode)
		}
		return false
	}
	if err := writeArtworkAtomicStream(path, resp.Body, MaxCoverArtBytes); err != nil {
		logger.Warn("atlas premium cover write", "mbid", mbid, "size", size, "err", err)
		return false
	}
	logger.Debug("atlas premium cover cached", "mbid", mbid, "size", size)
	return true
}
