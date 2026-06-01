package admin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
	qrcode "github.com/skip2/go-qrcode"
)

// buildPairURL composes the bridge://pair?... URL that the iOS app
// consumes via its onOpenURL handler. Shape is deliberately flat and
// additive so the iOS side can tolerate future fields by ignoring them.
//
//	bridge://pair
//	  ?url=<https URL to the bridge — primary/most-likely>
//	  &urls=<newline-joined alternates including the primary>
//	  &token=<base64url bearer token>
//	  &fingerprint=<AB:CD:...:EF>
//	  &name=<library display name>
//
// `urls` is the v1-additive extension that lets iOS learn every address
// the bridge self-reports at pairing time, so a phone paired on Wi-Fi
// still has a Tailscale fallback recorded and can roam without a
// re-pair. Older iOS builds ignore unknown query params and keep using
// `url` alone.
func buildPairURL(bridgeURL, rawToken, fingerprint, libraryName string, alternates []string) string {
	q := url.Values{}
	q.Set("url", bridgeURL)
	// Only emit `urls` when there are actual alternates to ship. An
	// `urls` with just the primary URL is noise. `strings.Join` on a
	// newline matches what the iOS parser expects (one URL per line,
	// percent-decoded at the URLComponents layer).
	if len(alternates) > 1 ||
		(len(alternates) == 1 && alternates[0] != bridgeURL) {
		q.Set("urls", strings.Join(alternates, "\n"))
	}
	q.Set("token", rawToken)
	q.Set("fingerprint", fingerprint)
	q.Set("name", libraryName)
	return "bridge://pair?" + q.Encode()
}

// pairAlternates returns every URL the admin console should bake into
// the QR — bridge's self-advertised `/v1/health` endpoint list, with
// the operator's explicit `primary` URL moved to the front so older
// iOS builds (which only read `url`) pick the same default the
// operator saw in the admin modal.
//
// **Public-mode short-circuit (PR 5)**: when cfg.IsPublic(), skip
// the LAN / mDNS / Tailscale enumeration and use only the
// operator-declared customEndpoints plus the autocert public
// domain. Avoids baking VPS-internal hostnames into the iOS
// pair QR (an iOS device that connects to the public bridge
// from outside the VPS network would then keep retrying the
// useless LAN URL on every fail-over attempt). Matches the
// `reachableEndpoints` filter on the iOS-facing /v1/health
// side, so both surfaces agree.
func pairAlternates(primary string, cfg *config.Config) []string {
	listenAddress := cfg.ListenAddress
	_, portStr, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return []string{primary}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return []string{primary}
	}

	var urls []string
	if cfg.IsPublic() {
		// Public mode: operator-declared customEndpoints +
		// the autocert public domain only. Synthesize the
		// autocert URL without `:443` when the listen port is
		// the https default — matches the shape
		// cmd/bridge/init.go writes into customEndpoints so
		// the downstream dedupe in `ensurePrimaryFirst`
		// collapses near-duplicate entries (Gemini medium on
		// PR #295: pre-fix the synthesized URL always carried
		// the port suffix and would differ from a
		// customEndpoint of bare `https://host`).
		for _, e := range cfg.CustomEndpoints {
			if e = strings.TrimSpace(e); e != "" {
				urls = append(urls, e)
			}
		}
		if d := strings.TrimSpace(cfg.Autocert.Domain); d != "" {
			u := "https://" + d
			if portStr != "443" {
				u += ":" + portStr
			}
			urls = append(urls, u)
		}
	} else {
		// Loopback mode (historical behaviour): the
		// auto-discovered LAN / mDNS / Tailscale enumeration.
		// CustomEndpoints aren't seeded here because the
		// deep-link/QR baker traditionally relied on the
		// caller passing one via `primary`.
		urls = advertise.URLs(advertise.Params{Port: port})
	}
	if len(urls) == 0 {
		return []string{primary}
	}

	// Move `primary` to the head if already present; else prepend.
	out := make([]string, 0, len(urls)+1)
	out = append(out, primary)
	seen := map[string]bool{primary: true}
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

// ensurePrimaryFirst is the response-boundary defence-in-depth that
// guarantees the JSON contract `pairResult.alternates[0] ==
// pairResult.url` AND that `primary` appears exactly once. The helper
// always rebuilds the slice — pre-fix an early-return on
// `alternates[0] == primary` let `[primary, other, primary]` pass
// through with the duplicate intact (CodeRabbit on PR #101 round 2).
// Non-primary duplicates are NOT deduped here; that would expand the
// helper's responsibility past the head-position contract and into
// territory `pairAlternates` itself owns.
//
// Cheap to apply at every emission site; expensive to debug if the
// invariant ever drifts. Do not inline at consumers — keep the single
// helper so the contract has one definition.
func ensurePrimaryFirst(primary string, alternates []string) []string {
	out := make([]string, 0, len(alternates)+1)
	out = append(out, primary)
	for _, u := range alternates {
		if u != primary {
			out = append(out, u)
		}
	}
	return out
}

// defaultBridgeURL is the best-guess URL the admin UI pre-fills in the
// pairing modal.
//
// **Public mode**: the device dials the public endpoint from off-network,
// so a `<hostname>.local` mDNS default is useless (it only resolves on
// the bridge's own LAN). Prefer the operator's configured autocert domain,
// then the first customEndpoint. Synthesize the autocert URL without
// `:443` when the listen port is the https default — mirrors the shape
// `pairAlternates` builds so downstream dedupe collapses near-duplicates.
//
// **Loopback / LAN mode** (historical): `https://<hostname>.local:<port>`.
// Users on networks where mDNS is flaky override in the modal input.
// Falls back to "localhost" if os.Hostname errors, which still works for
// same-machine simulator pairing.
func defaultBridgeURL(cfg *config.Config) string {
	_, port, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil || port == "" {
		port = "7788"
	}
	if cfg.IsPublic() {
		if d := strings.TrimSpace(cfg.Autocert.Domain); d != "" {
			if port == "443" {
				return "https://" + d
			}
			return "https://" + d + ":" + port
		}
		for _, e := range cfg.CustomEndpoints {
			if e = strings.TrimSpace(e); e != "" {
				return e
			}
		}
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	// macOS hostnames already carry the `.local` suffix (e.g.
	// `mac-mini.local`); Linux hostnames usually don't. Always tacking
	// on `.local` would be wrong for non-mDNS networks, so only append
	// if the hostname doesn't already contain a dot.
	return fmt.Sprintf("https://%s:%s", ensureMDNSHost(host), port)
}

// pairFingerprint resolves the cert fingerprint to bake into the pairing
// QR for a given dial URL. The QR MUST carry the fingerprint the device
// will actually capture when it dials bridgeURL: in public mode a device
// connecting to the public domain receives the autocert LE cert, whose
// fingerprint differs from the self-signed LAN pin — baking the
// self-signed value makes the iOS first-contact MITM check reject the
// pairing ("TLS fingerprint doesn't match the pairing link"). `resolve`
// (certManager.FingerprintForServerName) answers using the same SNI cert
// switcher the listener serves with, so the QR can't drift from reality.
// Falls back to the self-signed fingerprint when the resolver is absent
// (loopback wiring / tests) or returns "" for the host.
func pairFingerprint(bridgeURL, selfSigned string, resolve func(string) string) string {
	if resolve == nil {
		return selfSigned
	}
	host := pairURLHost(bridgeURL)
	if host == "" {
		return selfSigned
	}
	if fp := resolve(host); fp != "" {
		return fp
	}
	return selfSigned
}

// pairURLHost extracts the bare hostname (no port) from a dial URL, for
// use as the SNI key into the fingerprint resolver. Returns "" on a URL
// that doesn't parse or carries no host.
func pairURLHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil {
		return ""
	}
	return u.Hostname()
}

func ensureMDNSHost(host string) string {
	for _, c := range host {
		if c == '.' {
			return host
		}
	}
	return host + ".local"
}

// qrPNG renders text as a 256x256 PNG QR code. Medium error correction is
// the default compromise — low would shrink the code but survive fewer
// printed-screen reads; high is overkill for a same-room workflow.
func qrPNG(text string) ([]byte, error) {
	var buf bytes.Buffer
	q, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	// DisableBorder = false (keep the quiet zone) so the default camera
	// framing works.
	if err := q.Write(256, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// qrDataURL encodes the rendered PNG as a base64 data URL so the admin
// page can <img src="..."/> it inline without a second HTTP round-trip.
func qrDataURL(text string) (string, error) {
	png, err := qrPNG(text)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}
