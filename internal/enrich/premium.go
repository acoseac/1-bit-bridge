package enrich

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ErrNoCredential is returned by RefetchPremium when no usable Atlas credential
// is provisioned — distinct from a genuine "premium not ready" (CAA) miss so the
// cover-harvest sweep doesn't burn a retry attempt on it.
var ErrNoCredential = errors.New("enrich: no usable atlas credential")

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
	resp, err := f.authedCoverGet(ctx, mbid, size)
	if err != nil {
		if !errors.Is(err, ErrNoCredential) {
			logger.Warn("atlas premium cover fetch", "mbid", mbid, "size", size, "err", err)
		}
		return false // no credential / transport error → CAA path
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 = no cover at Atlas (normal — fall through to CAA rg + iTunes);
		// 5xx = Atlas hiccup. The public artwork proxy doesn't 401/403 a bad
		// bearer (it serves CAA), so those are defense-in-depth — see
		// clearOnAuthReject.
		f.clearOnAuthReject(resp.StatusCode)
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

// authedCoverGet builds + sends the authenticated cover GET for (mbid, size).
// Shared by TryCache + RefetchPremium. Returns ErrNoCredential when no usable
// credential is provisioned (no request is made). The caller owns the response
// body + status handling.
func (f *atlasPremiumFetcher) authedCoverGet(ctx context.Context, mbid string, size int) (*http.Response, error) {
	token, baseURL, ok := f.cred.AtlasCredential()
	if !ok {
		return nil, ErrNoCredential
	}
	url := strings.TrimRight(baseURL, "/") + fmt.Sprintf("/release/%s/front-%d", mbid, size)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	return f.http.Do(req)
}

// clearOnAuthReject clears the credential on a 401/403 (token rejected) so a
// dead token isn't retried on every subsequent fetch — it goes dormant until
// the iOS app re-provisions. Defense-in-depth: the public artwork proxy doesn't
// actually 401/403 a bad bearer (it serves CAA), but a fronting proxy or a
// future Atlas change might. Safe to call concurrently with the harvest client
// (StateStore.Clear holds its own mutex; both setting Token="" is idempotent).
func (f *atlasPremiumFetcher) clearOnAuthReject(status int) {
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return
	}
	logger.Warn("atlas premium cover: credential rejected, clearing", "status", status)
	if clearable, ok := f.cred.(interface{ Clear() error }); ok {
		_ = clearable.Clear()
	}
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
	resp, err := f.authedCoverGet(ctx, mbid, size)
	if err != nil {
		// ErrNoCredential or a transport error — the sweep leaves the cover
		// pending WITHOUT burning an attempt (these are transient, not a genuine
		// "premium not ready" miss).
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.clearOnAuthReject(resp.StatusCode)
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			// Token rejected (now cleared) — surface as ErrNoCredential so the
			// sweep stops this tick rather than counting it as a miss.
			return false, ErrNoCredential
		case resp.StatusCode >= 500:
			return false, fmt.Errorf("atlas premium cover: http %d", resp.StatusCode)
		default:
			return false, nil // 404 etc → no cover at Atlas; nothing to retry
		}
	}
	if !isPremiumSource(resp.Header.Get("X-Atlas-Asset-Source")) {
		// Atlas hasn't reverse-resolved a premium cover yet (still CAA) — leave
		// the existing cache. This IS a genuine "not ready" miss (it counts
		// toward the attempt cap), distinct from the transient errors above.
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
