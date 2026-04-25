package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ReleaseMeta is the shape of `release-meta.json`, a small sidecar
// the goreleaser config publishes alongside each release archive.
// It carries the floors a candidate release expects from its
// counterparties — currently the iOS app version (`MinClientVersion`)
// and the protocol version a paired bridge would speak. Older
// releases (anything before Phase C) don't publish this asset; the
// fetch helper treats absence as "no floor" so auto-install on
// pre-Phase-C releases stays unblocked.
//
// The asset name is fixed (`release-meta.json`) — operators don't
// need to find it by template, and the fetcher matches by basename.
type ReleaseMeta struct {
	// Version is the release tag stripped of the leading `v`,
	// e.g. "0.2.0". Used to sanity-check we fetched the meta for
	// the candidate the updater is about to install (a CDN cache
	// or a partial publication could theoretically deliver an
	// older meta against a newer archive).
	Version string `json:"version"`

	// MinClientVersion is the lowest iOS app version this bridge
	// release supports. The auto-install compat gate refuses to
	// install if any paired token's `LastClientVersion` is below
	// this value. Empty / "0.0.0" means "no floor".
	MinClientVersion string `json:"minClientVersion"`

	// ProtocolVersion is the wire protocol version this release
	// will speak. Currently advisory — the bridge already enforces
	// protocol version on the iOS side at /v1/health. Captured
	// here so a future operator-facing diff ("upgrade will bump
	// protocol from N to N+1") has the source data.
	ProtocolVersion int `json:"protocolVersion,omitempty"`
}

// ReleaseMetaAssetName is the file name the goreleaser config
// publishes the metadata under. Centralised so the publish side
// (`.goreleaser.yaml::release.extra_files`), the fetch side
// (`releaseMetaFor`), and the CLI / tests all agree.
const ReleaseMetaAssetName = "release-meta.json"

// ErrReleaseMetaMissing means the candidate release didn't ship a
// `release-meta.json` asset. NOT a hard error — the auto-installer
// treats it as "no floor", matching the behaviour of pre-Phase-C
// releases. Callers that want to log the absence (rather than
// silently treat as no-floor) inspect this sentinel.
var ErrReleaseMetaMissing = errors.New("release-meta.json not published with this release")

// releaseMetaFor finds the `release-meta.json` asset on a Release
// and fetches+decodes it. Returns `ErrReleaseMetaMissing` when the
// asset is absent (so the auto-installer can treat it as "no
// floor" without escalating); other errors (HTTP failure, JSON
// decode) surface verbatim.
func releaseMetaFor(ctx context.Context, hc *http.Client, rel *Release) (ReleaseMeta, error) {
	for i := range rel.Assets {
		if rel.Assets[i].Name == ReleaseMetaAssetName {
			return fetchReleaseMeta(ctx, hc, rel.Assets[i].BrowserDownloadURL)
		}
	}
	return ReleaseMeta{}, ErrReleaseMetaMissing
}

func fetchReleaseMeta(ctx context.Context, hc *http.Client, url string) (ReleaseMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ReleaseMeta{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return ReleaseMeta{}, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return ReleaseMeta{}, fmt.Errorf("fetch status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // tiny file
	if err != nil {
		return ReleaseMeta{}, fmt.Errorf("read body: %w", err)
	}
	var meta ReleaseMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		return ReleaseMeta{}, fmt.Errorf("decode: %w", err)
	}
	return meta, nil
}
