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

// httpsScheme is the URL scheme prefix every bridge dial URL carries.
// Factored out so the public-mode endpoint synthesis (pairAlternates,
// defaultBridgeURL) and the scheme-less-host fallback (pairURLHost)
// share one literal.
const httpsScheme = "https://"

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
// `endpoints` IS that list — `Deps.Endpoints`, the api layer's
// `ReachableEndpoints`, the one place the Tailscale MagicDNS + tailnet
// IPs are appended (from the TailscaleProvider, in `cli` and `tsnet`
// modes) and the one place customEndpoints join the loopback set.
// Before it was threaded through, this function called
// `advertise.Endpoints` directly — which stopped emitting anything
// Tailscale-classed in PR #269 — so a loopback bridge whose health
// advertised `nuc.sable-eagle.ts.net` + `100.x` + `fd7a:…` handed the
// phone a QR carrying only the `.local` name and the LAN IP: no
// Tailscale fallback recorded, roaming needed a re-pair, and the
// promise in buildPairURL's docblock was false for four months. It
// also dropped cfg.CustomEndpoints in loopback mode. Nil → the primary
// alone (see advertisedEndpoints for why not the old walk).
//
// **Public-mode short-circuit (PR 5)**: when cfg.IsPublic(), skip
// the LAN / mDNS / Tailscale enumeration and use only the
// operator-declared customEndpoints plus the autocert public
// domain. Avoids baking VPS-internal hostnames into the iOS
// pair QR (an iOS device that connects to the public bridge
// from outside the VPS network would then keep retrying the
// useless LAN URL on every fail-over attempt). Matches the
// `ReachableEndpoints` filter on the iOS-facing /v1/health
// side, so both surfaces agree — but is NOT delegated to it: the
// QR's autocert URL names its port explicitly (`:443` included)
// where health omits the https default, because a port-less dial
// URL trips the iOS 7788-default bug on shipped builds (see
// defaultBridgeURL). The two shapes differ on purpose.
func pairAlternates(primary string, cfg *config.Config, endpoints func() []advertise.Endpoint) []string {
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
		// Public mode: operator-declared customEndpoints + the autocert
		// public domain only. The synthesized autocert URL names its
		// port EXPLICITLY (incl. :443) — a port-less dial URL trips the
		// iOS 7788-default bug on shipped builds (see defaultBridgeURL).
		// customEndpoints are passed through verbatim (operator's values);
		// the explicit-port primary from defaultBridgeURL is what the
		// device dials first, so a bare-host customEndpoint only ever
		// shows up as a lower-priority failover entry.
		declared := map[string]bool{}
		for _, e := range cfg.CustomEndpoints {
			if e = strings.TrimSpace(e); e != "" {
				urls = append(urls, e)
				if h := endpointHost(e); h != "" {
					declared[h] = true
				}
			}
		}
		// SKIPPED when customEndpoints already names that HOST — the same
		// rule api.publicModeEndpoints applies to /v1/health, and the one
		// place the two public-mode enumerations have to agree even though
		// their URL SHAPES deliberately differ.
		//
		// `portStr` is the port THIS PROCESS listens on, which is the port
		// a client dials only when nothing remaps it. Behind a proxy that
		// does — the hosted layout, where each tenant listens on a loopback
		// high port and is published on one shared external port — the
		// synthesized URL is an address no client can reach, and iOS puts
		// every advertised URL into its failover rotation. The operator
		// declaring the host in customEndpoints is them saying what the
		// reachable address is.
		//
		// A HOST comparison, not a URL one: the two strings differ in
		// exactly the part that is wrong, so the dedupe below — which is
		// byte-equality — cannot see it. And `autocert.domain` cannot
		// simply be left unset: public mode refuses to start without it.
		//
		// #936 moved health onto ReachableEndpoints and left public mode
		// with its own synthesis here on purpose (the explicit `:443` the
		// iOS 7788-default bug needs), so the host skip added to health
		// never reached this copy: a tenant whose customEndpoints is
		// `https://demo…:8443` on listen port 20001 had a clean
		// /v1/health and an unreachable URL baked into its QR.
		if d := strings.TrimSpace(cfg.Autocert.Domain); d != "" && !declared[strings.ToLower(d)] {
			// Explicit port (incl. :443) — see defaultBridgeURL: a
			// port-less URL trips the iOS 7788-default bug on shipped
			// builds, so every dial URL the QR carries names its port.
			urls = append(urls, httpsScheme+d+":"+portStr)
		}
	} else {
		// Loopback mode: the auto-discovered LAN / mDNS / Tailscale /
		// custom enumeration, class-ordered the way the phone's
		// selector ranks it. Exactly what `/v1/health` says.
		for _, e := range advertisedEndpoints(endpoints) {
			urls = append(urls, e.URL)
		}
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

// declaredEndpointForHost returns the first customEndpoint whose host is
// `host`, or "" when the operator has declared none for it.
//
// The comparison is on the HOST alone, never the whole URL: what the
// caller needs to know is whether the operator has already said how this
// domain is reached, and the declaration differs from the synthesis in
// exactly the port — the part that is wrong behind a proxy.
func declaredEndpointForHost(endpoints []string, host string) string {
	want := strings.ToLower(strings.TrimSpace(host))
	if want == "" {
		return ""
	}
	for _, e := range endpoints {
		if e = strings.TrimSpace(e); e != "" && endpointHost(e) == want {
			return e
		}
	}
	return ""
}

// explicitHTTPSPort adds `:443` to an https URL that names no port.
//
// Every dial URL the pairing payload carries names its port, `:443`
// included, because the shipped iOS builds default a port-less bridge
// URL to 7788 — the LAN listen default — and time out against a public
// bridge. An operator's customEndpoint is written for a browser, where
// the port-less form is the normal one, so taking it as the primary
// without this would re-open that bug on exactly the deployments this
// change exists to fix. Anything not https-without-a-port is returned
// unchanged: the caller is choosing between operator-supplied strings,
// not normalising them.
func explicitHTTPSPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" {
		return raw
	}
	u.Host = net.JoinHostPort(u.Hostname(), "443")
	return u.String()
}

// endpointHost is the lowercased hostname of an advertised URL, or ""
// if it has none.
//
// A deliberate second copy of api.endpointHost rather than an import:
// the api package does not export it, and admin importing api for one
// four-line URL helper would couple the console to the wire layer for
// nothing. What matters is that the two answer the same QUESTION —
// "has the operator already declared this host?" — not that they share
// a body; the enumerations they serve return different URL shapes on
// purpose. TestPublicPairingSkipsTheRemappedListenPort and the
// api-side test pin the shared behaviour from both ends.
func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// advertisedEndpoints is the classed list this bridge advertises to
// paired devices — `Deps.Endpoints`, the api layer's `/v1/health`
// enumeration — or nothing when no provider is wired. Shared by the
// pairing QR and the Settings "Reachable endpoints" panel so the two
// cannot disagree about what the phone will see.
//
// Nil is NOT a fallback to the host-network walk. That walk is what
// both consumers ran before, and it is exactly the degraded shape the
// provider replaces (no Tailscale entry since PR #269, no
// customEndpoints in the QR); reproducing it here would be a second
// enumeration to keep in step, and one that a forgotten wiring line
// could never be told apart from — on a host without Tailscale the two
// lists are byte-identical, which is how the first draft's boot control
// stayed green with the line deleted. An absent dependency answers the
// way every other Deps closure does: with nothing, never a guess. The
// QR then carries only the operator's primary, which always pairs, and
// the panel renders its "No external addresses detected" state, which
// in that wiring is the truth.
func advertisedEndpoints(provider func() []advertise.Endpoint) []advertise.Endpoint {
	if provider == nil {
		return nil
	}
	return provider()
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
// then the first customEndpoint.
//
// The autocert URL names the LISTEN port — which is the port a client
// dials only when nothing remaps it. So when customEndpoints already
// declares that HOST, the declaration wins: the operator naming the host
// there is them saying what the reachable address is, and on the hosted
// layout (each tenant on a loopback high port, published on one shared
// external port) the synthesized form is an address no client can reach.
// This is the PRIMARY — the `url=` field every shipped iOS build dials
// first, and the only one the oldest ones read — so getting it from the
// declaration matters more here than in the alternates beside it.
// Measured on the live demo tenant, whose /v1/health advertises exactly
// `https://bridge.1-bit.app` while this function was handing the QR the
// loopback port behind the proxy.
//
// Either way the result names a port explicitly, `:443` included: a
// port-less dial URL trips the iOS 7788-default bug on shipped builds,
// which is what `explicitHTTPSPort` is for.
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
			if declared := declaredEndpointForHost(cfg.CustomEndpoints, d); declared != "" {
				return explicitHTTPSPort(declared)
			}
			// Emit the port EXPLICITLY, including :443. The iOS app
			// (≤ the build that fixes this) defaults a port-less bridge
			// URL to 7788 (the LAN listenAddress default), which dials
			// the wrong port on a public bridge and times out. Baking
			// the real port into the dial URL + QR sidesteps that on
			// already-shipped apps; newer apps keep working too.
			return httpsScheme + d + ":" + port
		}
		// Left VERBATIM, unlike the declared-endpoint branch above. That
		// one replaces a form that always named its port, so dropping
		// the port there would re-open the iOS 7788 bug; this one has
		// returned the operator's string unchanged since it was written,
		// and it is reached only with NO autocert domain — a state
		// Validate refuses in public mode. Normalising a path nothing
		// takes, to fix a bug on it, is a behaviour change with no
		// deployment behind it.
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
	if rawURL == "" {
		return ""
	}
	// Operators may type a scheme-less host:port (e.g. "bridge.ars.md:8443")
	// in the admin dial-URL field. url.Parse would read the host as the
	// scheme and return an empty Hostname(), silently falling the
	// fingerprint resolver back to self-signed — reintroducing the exact
	// mismatch this fix closes. Prepend the scheme when none is present.
	if !strings.Contains(rawURL, "://") {
		rawURL = httpsScheme + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u == nil {
		return ""
	}
	return u.Hostname()
}

// ensureMDNSHost appends `.local` to a bare (dot-less) hostname so the
// pairing URL resolves over mDNS. Values returned unchanged: any host
// already containing a dot (FQDN, `mac.local`, or an IPv4 literal); the
// literal "localhost" — defaultBridgeURL falls back to "localhost" when
// os.Hostname() fails, and "localhost.local" wouldn't resolve to
// loopback (the documented same-machine simulator pairing path); and any
// IP literal, incl. bracketed / bare IPv6 (`[::1]`, `fe80::1`) which is
// dot-less and would otherwise become `::1.local`. The bracket-strip
// mirrors loopbackHostname's IPv6 handling.
func ensureMDNSHost(host string) string {
	// EqualFold: hostnames are case-insensitive, so an operator-entered
	// "LOCALHOST" or "LocalHost" must take the same carve-out as
	// "localhost" — otherwise it becomes "LOCALHOST.local", which doesn't
	// resolve, and the pairing URL silently points nowhere.
	if strings.EqualFold(host, "localhost") || strings.Contains(host, ".") || net.ParseIP(strings.Trim(host, "[]")) != nil {
		return host
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
