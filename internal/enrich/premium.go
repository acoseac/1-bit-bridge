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
	// RefetchPremium OVERWRITES the cache at path only when Atlas now serves a
	// premium (non-CAA) cover — the cover bulk-harvest's upgrade path. Returns
	// true when a premium cover was written.
	RefetchPremium(ctx context.Context, path, mbid string, size int) (bool, error)
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
		// 5xx = Atlas hiccup. The PUBLIC artwork proxy doesn't 401/403 a bad
		// bearer — it silently serves CAA — so in normal operation those never
		// arrive here. Defense-in-depth (Gemini HIGH on PR #413): if a fronting
		// proxy or a future Atlas change ever DOES reject the token, clear the
		// credential so we stop retrying a dead token on every subsequent
		// enriched track rather than waiting for the harvest client's next poll
		// to clear it. AtlasCredential then returns ok=false and the premium
		// path goes dormant until the iOS app re-provisions. Clear is safe to
		// call concurrently with the harvest client (StateStore.Clear holds its
		// own mutex; both setting Token="" is idempotent).
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			logger.Warn("atlas premium cover: credential rejected, clearing", "status", resp.StatusCode)
			if clearable, ok := f.cred.(interface{ Clear() error }); ok {
				_ = clearable.Clear()
			}
		case resp.StatusCode >= 500:
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

// RefetchPremium re-fetches the authenticated Atlas cover for (mbid, size) and
// OVERWRITES the cache at path ONLY when Atlas now serves a premium (non-CAA)
// cover — used by the cover bulk-harvest to upgrade an already-cached CAA cover
// once Atlas has reverse-resolved a premium one. Unlike TryCache (which writes
// any 200), this gates on the `X-Atlas-Asset-Source` header, so a release Atlas
// is still serving CAA for leaves the existing cache untouched and the caller
// retries later. Returns true when a premium cover was written. Never an error
// for a "not premium yet" miss; only for a write/transport failure worth
// surfacing.
func (f *atlasPremiumFetcher) RefetchPremium(ctx context.Context, path, mbid string, size int) (bool, error) {
	token, baseURL, ok := f.cred.AtlasCredential()
	if !ok {
		return false, nil
	}
	url := strings.TrimRight(baseURL, "/") + fmt.Sprintf("/release/%s/front-%d", mbid, size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if clearable, ok := f.cred.(interface{ Clear() error }); ok {
				_ = clearable.Clear()
			}
		}
		return false, nil
	}
	if !isPremiumSource(resp.Header.Get("X-Atlas-Asset-Source")) {
		// Atlas hasn't reverse-resolved a premium cover yet (still CAA) — leave
		// the existing cache; the caller retries on a later sweep.
		return false, nil
	}
	if err := writeArtworkAtomicStream(path, resp.Body, MaxCoverArtBytes); err != nil {
		return false, err
	}
	logger.Info("atlas premium cover upgraded", "mbid", mbid, "size", size)
	return true, nil
}

// isPremiumSource reports whether the X-Atlas-Asset-Source header names a
// premium (non-CAA) source. Empty / "caa" → false; "tidal" / "qobuz" / etc.
// → true. Atlas sets this header on the cover proxy response.
func isPremiumSource(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "caa":
		return false
	default:
		return true
	}
}
