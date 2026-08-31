package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
	"github.com/acoseac/1-bit-bridge/internal/upnpproxy"
)

// Wiring for the admin web player's runtime dependencies. These live
// here rather than in internal/admin because the admin package must
// stay free of internal/upnpproxy and internal/updater — the same
// closure/adapter boundary every other foreign dependency crosses.

// playerUPnPAudioAdapter adapts upnpproxy.Proxy to admin's
// ProxyUPnPAudio closure shape.
//
// The proxy instance is the SAME one the /v1 download fast-path and the
// DLNA file handler use, so all three surfaces observe one SSDP cache
// and a DHCP-floated upstream host heals for all of them at once.
// It builds its OWN Proxy rather than borrowing the DLNA server's:
// the player must serve routed tracks whether or not the DLNA
// MediaServer is enabled, and dlna_wiring's instance only exists when
// it is. What matters is the shared HOST RESOLVER underneath — that is
// the SSDP cache, so a DHCP-floated upstream heals for every surface at
// once. A second http.Client per surface is the cost dlna_wiring
// already documents as acceptable at operator scale.
func playerUPnPAudioAdapter(lc *upnpUpstreamLifecycle, log *slog.Logger) func(http.ResponseWriter, *http.Request, *manifest.UPnPRouting) *admin.RoutedAudioError {
	hr := lc.HostResolver()
	if hr == nil {
		return nil
	}
	proxy := upnpproxy.New(hr, log)
	return func(w http.ResponseWriter, r *http.Request, rt *manifest.UPnPRouting) *admin.RoutedAudioError {
		if err := proxy.Serve(r.Context(), w, r.Method, r.Header, rt); err != nil {
			return &admin.RoutedAudioError{
				Status: err.Status, Code: err.Code, Message: err.Message,
			}
		}
		return nil
	}
}

// playerHostOnlineAdapter answers "is this upstream reachable right
// now" from the SSDP cache — a map lookup, no network. Nil when
// discovery isn't running, which admin reports as UNKNOWN rather than
// offline.
func playerHostOnlineAdapter(lc *upnpUpstreamLifecycle) func(string) bool {
	hr := lc.HostResolver()
	if hr == nil {
		return nil
	}
	return func(udn string) bool {
		_, ok := hr.LiveHost(udn)
		return ok
	}
}

// playerSessionAdapter hands admin the updater's session tracker so a
// self-update can't swap the binary out from under an active stream.
func playerSessionAdapter(t *updater.Tracker) func() func() {
	if t == nil {
		return nil
	}
	return func() func() {
		t.Begin()
		return t.End
	}
}

// startCatalogInvalidator drains the post-scan nudge and marks the
// player's library catalog stale.
//
// Invalidation is deliberately LAZY — it bumps an epoch and the next
// reader rebuilds — so this goroutine does no work beyond the wakeup.
// That laziness is also the debounce: postScanNudges fires after every
// successful scan INCLUDING watcher-driven ScanSubtree, so a bulk
// import or a noisy inotify burst coalesces into one rebuild, and into
// none at all if nobody has the player open.
//
// Not joined to bgWriters: it writes nothing, so it has no ordering
// relationship with Store.Close. scanCtx cancellation is its whole
// lifecycle.
func startCatalogInvalidator(ctx context.Context, nudge <-chan struct{}, srv *admin.Server) {
	if nudge == nil || srv == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-nudge:
				// A CLOSED channel makes this case succeed immediately
				// and forever — a 100%-CPU spin, not a stall, which is
				// the kind of failure that looks like a hardware
				// problem. Nothing closes this channel today; the guard
				// costs one line and removes the possibility.
				if !ok {
					return
				}
				srv.InvalidateLibraryCatalog()
			}
		}
	}()
}

// playerUPnPSourcesAdapter lists the configured upstream MediaServers
// for the player's source facet.
//
// Config + SSDP cache only — no DB, unlike the admin adapter's
// ConfiguredServers, which issues a COUNT(*) per upstream. The facet
// gets its counts from the library catalog instead, so this only has
// to supply identity and liveness.
//
// The pairing of the two keys is the whole point (see admin.UPnPSource):
// StableServerKey is what the manifest's routing rows carry, while the
// SSDP cache is keyed on the raw UDN, and only the config row knows
// both. Reads the live holder per call so an upstream added or removed
// through the admin console shows up without a restart.
func playerUPnPSourcesAdapter(lc *upnpUpstreamLifecycle, holder *config.RuntimeConfig) func() []admin.UPnPSource {
	if lc == nil || lc.cache == nil || holder == nil {
		return nil
	}
	return func() []admin.UPnPSource {
		cfg := holder.Load()
		if cfg == nil {
			return nil
		}
		out := make([]admin.UPnPSource, 0, len(cfg.UPnPUpstream.Servers))
		for _, srv := range cfg.UPnPUpstream.Servers {
			name := strings.TrimSpace(srv.Name)
			online := false
			// Trimmed before the lookup for the same reason every other
			// cache read here trims: a hand-edited bridge.yaml UDN with
			// stray whitespace would otherwise miss the SSDP-clean key
			// and false-report an upstream that is up as offline.
			if udn := strings.TrimSpace(srv.UDN); udn != "" {
				if info, ok := lc.cache.Get(udn); ok {
					online = true
					if name == "" {
						name = info.FriendlyName
					}
				}
			}
			// A manual-URL entry has no UDN to look up and is not
			// discoverable at all yet (see discoveryServerResolver's
			// TODO), so false is the accurate answer rather than a
			// pessimistic one. It also cannot be ingested today, so it
			// contributes no tracks and the facet skips it anyway.
			if name == "" {
				name = "Upstream server"
			}
			out = append(out, admin.UPnPSource{
				Key:    upnpingest.StableServerKey(srv),
				Name:   name,
				Online: online,
			})
		}
		return out
	}
}
