package upnp

// Manual-URL MediaServer resolution.
//
// The config has accepted `upnpUpstream.servers[].manualDescriptionURL`
// since PR #351 — validated at load, hashed into a StableServerKey — and
// the ingest refused it at runtime with "not yet supported". It is the
// only escape hatch for a network where the bridge's SSDP cannot reach
// the server (multicast filtered by the AP, the server on another
// subnet, iOS Local Network permission irrelevant because it is the
// BRIDGE that cannot see it), and it looked supported while doing
// nothing.
//
// **The design falls out of one observation: all three surfaces that need
// a manual server read the same ServerCache.** ResolveControlURL looks the
// cache up, api's LiveHost derives its host:port from the cached control
// URL, and the admin/health online chip reads the cache. So ONE insertion
// point makes all three work, rather than three parallel implementations
// that can disagree.
//
// **The cache entry is keyed under the ingest's StableServerKey** — the
// `manual:<sha256(url)>` form — NOT the device's real UDN. Routing rows,
// per-server telemetry, LiveHost and the status chip all key on that
// string; PR #807 already had to carry both spellings for exactly this
// reason. The real UDN is kept as a display field.

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// ManualServer is one configured manual-URL upstream.
type ManualServer struct {
	// Key is the ingest's StableServerKey for this server — the string
	// every other subsystem uses to refer to it. The cache entry is
	// stored under this, not under the device's own UDN.
	Key string
	// DescriptionURL is the operator-configured device-description URL.
	DescriptionURL string
	// Name is the operator's label, used only when the description
	// carries no friendlyName.
	Name string
}

// ManualPoller refreshes cache entries for manual-URL servers.
//
// It must run on the SAME cadence as the SSDP TTL, because EvictStale
// reaps entries the poller stops refreshing — and that eviction is
// CORRECT: a manual URL that stops answering should show as offline, and
// getting that for free is why the poller writes into the shared cache
// rather than a parallel map.
type ManualPoller struct {
	cache      *ServerCache
	dispatcher discovery.SOAPDispatcher
	servers    func() []ManualServer
	// knownUDNs reports the UDNs of SSDP-configured servers, so a device
	// configured BOTH ways is not walked twice under two routing keys.
	knownUDNs func() map[string]struct{}
	interval  time.Duration
	timeout   time.Duration
	log       *slog.Logger

	mu      sync.Mutex
	warned  map[string]struct{} // dedup for the duplicate-config warning
	nowFunc func() time.Time
}

// ManualPollerConfig configures a ManualPoller. Servers and KnownUDNs are
// closures so a config reload is picked up without reconstructing.
type ManualPollerConfig struct {
	Cache      *ServerCache
	Dispatcher discovery.SOAPDispatcher
	Servers    func() []ManualServer
	KnownUDNs  func() map[string]struct{}
	Interval   time.Duration
	Timeout    time.Duration
	Logger     *slog.Logger
}

// NewManualPoller constructs a poller. Returns nil when there is nothing
// to poll, so the caller can skip spawning a goroutine entirely.
func NewManualPoller(cfg ManualPollerConfig) *ManualPoller {
	if cfg.Cache == nil || cfg.Servers == nil {
		return nil
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultMediaServerDetailTimeout
	}
	if cfg.Dispatcher == nil {
		// The SSDP detail fetch's redirect guard, built here so it lives
		// in one place rather than at each wiring site. The URL is
		// operator-configured and therefore more trusted than an SSDP
		// Location header — but "more trusted" is not "trusted", and an
		// operator can paste a URL that redirects.
		//
		// On discovery.NewDeviceTransport, the transport of every request
		// to a device, and so under its dial check, with the approval
		// pollServer gives the fetch (discovery.ManualDescriptionFetch):
		// every address but a cloud metadata one (backlog B54). Not the
		// SSDP client's approval: a manual URL on this machine, or a name
		// resolving to it, is the operator pointing at a local server,
		// and the control URLs its description names are bounded by
		// discovery's host-kind rule instead. Like every other request to
		// a device since #1074, it takes no proxy from the environment and
		// keeps no connection alive.
		cfg.Dispatcher = &discovery.HTTPClientDispatcher{
			Client: &http.Client{
				Timeout:   cfg.Timeout,
				Transport: discovery.NewDeviceTransport(net.Dialer{}),
				// Relay 3xx verbatim rather than following it: an
				// auto-followed redirect to loopback or a link-local
				// metadata address would turn the bridge into an SSRF
				// probe against its own no-auth admin API. Mirrors
				// NewMediaServerDiscoveryClient and internal/upnpproxy.
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			},
		}
	}
	if cfg.Logger == nil {
		cfg.Logger = logger
	}
	if cfg.KnownUDNs == nil {
		cfg.KnownUDNs = func() map[string]struct{} { return nil }
	}
	return &ManualPoller{
		cache: cfg.Cache, dispatcher: cfg.Dispatcher,
		servers: cfg.Servers, knownUDNs: cfg.KnownUDNs,
		interval: cfg.Interval, timeout: cfg.Timeout, log: cfg.Logger,
		warned: make(map[string]struct{}), nowFunc: time.Now,
	}
}

// Run polls until ctx is done. One immediate pass, then on the interval.
func (p *ManualPoller) Run(ctx context.Context) {
	if p == nil {
		return
	}
	p.PollOnce(ctx)
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.PollOnce(ctx)
		}
	}
}

// PollOnce refreshes every configured manual server once. Failures are
// logged at Debug and leave the cache entry to age out — which is how an
// unreachable manual server comes to report offline.
func (p *ManualPoller) PollOnce(ctx context.Context) {
	if p == nil {
		return
	}
	known := p.knownUDNs()
	for _, srv := range p.servers() {
		if ctx.Err() != nil {
			return
		}
		p.pollServer(ctx, srv, known)
	}
}

// manualCloudMetadataWarning is what the poller logs, once per server, for
// a manual URL on a cloud metadata address, which it does not fetch.
const manualCloudMetadataWarning = "UPnP manual server: not fetching its description, which is on a cloud metadata " +
	"address no media server serves on; configure the server's own address"

// descriptionHostForLog is a manual URL's host, and port, for a log line:
// the rest of the URL is the operator's and can carry a credential (a user
// name and password, a token in the query) the journal must not (backlog
// B54). "" when the URL does not parse.
func descriptionHostForLog(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return u.Host
}

// manualURLNamesNoHost is what fetchErrorForLog logs in place of the fetch's
// error for a manual URL that names no host it can read.
const manualURLNamesNoHost = "the manual URL is not a URL with a host"

// fetchErrorForLog renders err, the description fetch's error for the manual
// URL raw, for a log line: each rendering of the URL in its text is replaced
// by the URL's host (descriptionHostForLog), so the line keeps the reason
// the fetch failed and loses the URL (backlog B66). The fetch names the URL
// in three renderings. Discovery's own wrapping writes it as the poller
// passed it (`GET <url>: …`, `parse description <url>: …`). net/http's
// *url.Error quotes the request URL as the client re-serialized it, masking
// a password and nothing else, so a token written as the user name, a query
// and a fragment reach it whole; and it quotes it with %q, so a URL holding
// a `"`, a `\` or a rune that is not printable appears escaped, which a
// search for the URL as written does not find. The *url.Error's own URL
// field is its rendering exactly, so no form is guessed, and each form is
// replaced longest first, so one that contains another is not left half
// replaced.
//
// A URL that names no host it can read (one that does not parse, or
// `user:password@host` written without a scheme) gets manualURLNamesNoHost
// in place of the error, never the error with the URL taken out: it could
// have reached nothing, and its error can quote any part of it, net/http's
// "unsupported protocol scheme" the scheme a hostless value parses with,
// which is its user name (the lesson under B54).
func fetchErrorForLog(err error, raw string) string {
	host := descriptionHostForLog(raw)
	if host == "" {
		return manualURLNamesNoHost
	}
	type form struct{ old, new string }
	forms := []form{{raw, host}}
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		forms = append(forms, form{strconv.Quote(ue.URL), strconv.Quote(host)}, form{ue.URL, host})
	}
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i].old) > len(forms[j].old) })
	msg := err.Error()
	for _, f := range forms {
		msg = strings.ReplaceAll(msg, f.old, f.new)
	}
	return msg
}

func (p *ManualPoller) pollServer(ctx context.Context, srv ManualServer, knownUDNs map[string]struct{}) {
	descURL := strings.TrimSpace(srv.DescriptionURL)
	if descURL == "" || srv.Key == "" {
		return
	}
	// A manual URL on a cloud metadata address is not fetched, whatever
	// the operator's approval covers (backlog B54, #1074's rule): no media
	// server serves on one, the fetch would send a cloud VM's metadata
	// service a GET every poll, and every later dial of what it found is
	// refused anyway. The literal is refused here, before any request; a
	// name that resolves to one is refused by the dial check below. Both
	// warn once per server, because a fetch that fails is a Debug line,
	// which the bridge never prints, and the server would just never
	// appear.
	if discovery.NamesCloudMetadataAddr(descURL) {
		p.warnCloudMetadata(srv)
		return
	}
	fetchCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	fetchCtx = discovery.WithDialApproval(fetchCtx, discovery.ManualDescriptionFetch())

	// SourceUserChosen, because the operator configured this URL: that
	// choice is the approval the SSDP path's same-host rule stands in for,
	// so a ContentDirectory on another host is kept (the escape hatch for a
	// real server that spans hosts). A control URL that is not http(s)
	// with a host is still refused (external audit 2026-09-23, M3).
	desc, err := discovery.FetchDeviceDescriptionWithSource(fetchCtx, p.dispatcher, descURL, discovery.SourceUserChosen)
	if errors.Is(err, discovery.ErrCloudMetadataAddr) {
		p.warnCloudMetadata(srv)
		return
	}
	// FetchDeviceDescription returns a "no AVTransport service" error for
	// any non-renderer device — which every MediaServer is — while still
	// populating desc.Services. Tolerate that specific shape and let the
	// ContentDirectory lookup below be the real verdict. Same allowance
	// the SSDP path makes.
	// Both Debug lines name the host alone, as the warnings do, and the
	// fetch's error goes through fetchErrorForLog, which takes the URL out
	// of it (backlog B66): they named the URL whole until then.
	if err != nil && len(desc.Services) == 0 {
		p.log.Debug("UPnP manual server: description fetch failed",
			slog.String("server", srv.Name), slog.String("host", descriptionHostForLog(descURL)),
			slog.String("err", fetchErrorForLog(err, descURL)))
		return
	}
	ctrlURL := lookupContentDirectoryControlURL(desc.Services)
	if ctrlURL == "" {
		p.log.Debug("UPnP manual server: description carries no ContentDirectory service",
			slog.String("server", srv.Name), slog.String("host", descriptionHostForLog(descURL)))
		return
	}

	// A device reachable BOTH by SSDP and by manual URL would otherwise
	// land in the cache twice — under its real UDN and under manual:<sha>
	// — and the ingest would walk it twice under two routing prefixes,
	// producing duplicate rows for one upstream. Refuse the manual entry
	// and say so once.
	//
	// UNLESS the UDN is this entry's OWN. A server configured with both a
	// UDN and a manual URL has StableServerKey == its lowercased UDN, so
	// the description it returns necessarily "matches a configured UDN" —
	// itself. Rejecting there would make the manual URL useless to the
	// operator who supplied both as a belt-and-braces, which is precisely
	// the case where SSDP is unreliable and the fallback is wanted. There
	// is no double-walk risk: the ingest walks per CONFIGURED SERVER,
	// keyed on StableServerKey, so one entry is walked once however many
	// ways its description was obtained.
	if realUDN := strings.ToLower(strings.TrimSpace(desc.UDN)); realUDN != "" && realUDN != strings.ToLower(srv.Key) {
		if _, dup := knownUDNs[realUDN]; dup {
			p.warnOnce(srv.Key, func() {
				// The host alone, never the URL (descriptionHostForLog).
				p.log.Warn("UPnP manual server: this device is already configured by UDN — "+
					"ignoring the manual URL so it is not walked twice",
					slog.String("server", srv.Name),
					slog.String("host", descriptionHostForLog(descURL)),
					slog.String("udn", realUDN))
			})
			return
		}
	}

	name := desc.FriendlyName
	if strings.TrimSpace(name) == "" {
		name = srv.Name
	}
	// UpsertConfigured, never the bounded Upsert: the operator named this
	// server, and a flood of SSDP fakes must not keep it out of the cache.
	p.cache.UpsertConfigured(ServerInfo{
		// The StableServerKey, deliberately — see the file docblock.
		UDN:                        srv.Key,
		FriendlyName:               name,
		Manufacturer:               desc.Manufacturer,
		ModelDescription:           desc.ModelDescription,
		ModelName:                  desc.ModelName,
		ContentDirectoryControlURL: ctrlURL,
		DescriptionURL:             descURL,
		DeviceUDN:                  strings.TrimSpace(desc.UDN),
		// The operator's URL approves a local control URL only when it is
		// of that kind itself, and a name approves none: the ingest and the
		// proxy dial the control URL under this (backlog B36).
		DialApproval: discovery.OperatorChose(descURL),
		LastSeenAt:   p.nowFunc(),
	})
}

// warnCloudMetadata logs manualCloudMetadataWarning for srv, once per
// server for the life of the poller: the configuration it is about cannot
// change without a restart.
func (p *ManualPoller) warnCloudMetadata(srv ManualServer) {
	p.warnOnce("cloud-metadata:"+srv.Key, func() {
		p.log.Warn(manualCloudMetadataWarning,
			slog.String("server", srv.Name),
			slog.String("host", descriptionHostForLog(srv.DescriptionURL)))
	})
}

// warnOnce emits fn the first time it is called for key. The
// duplicate-config warning describes a static misconfiguration, so
// repeating it on every poll tick would be noise.
func (p *ManualPoller) warnOnce(key string, fn func()) {
	p.mu.Lock()
	_, seen := p.warned[key]
	if !seen {
		p.warned[key] = struct{}{}
	}
	p.mu.Unlock()
	if !seen {
		fn()
	}
}
