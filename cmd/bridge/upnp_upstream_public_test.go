package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

func openPublicTestStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPublic_ConfiguredServers_PreservesYAMLOrder(t *testing.T) {
	// iOS expects a deterministic per-server order so the rendered
	// sub-source list doesn't churn between health probes. The adapter
	// MUST emit servers in YAML config order — the same invariant the
	// admin adapter holds.
	cfg := &config.Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		UPnPUpstream: config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{
				{Name: "Alpha", UDN: "uuid:alpha", PathPrefix: "alpha"},
				{Name: "Bravo", UDN: "uuid:bravo", PathPrefix: "bravo"},
				{Name: "Charlie", UDN: "uuid:charlie", PathPrefix: "charlie"},
			},
		},
	}
	a := &upnpPublicAdapter{
		cfgHolder: config.NewRuntimeConfig(cfg),
		cache:     upnp.NewServerCache(),
		store:     openPublicTestStore(t),
	}
	got := a.PublicServers()
	if len(got) != 3 {
		t.Fatalf("got %d rows; want 3", len(got))
	}
	want := []string{"Alpha", "Bravo", "Charlie"}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("row %d: name = %q; want %q", i, got[i].Name, w)
		}
	}
}

func TestPublic_FriendlyName_FromDiscoveryCache(t *testing.T) {
	// FriendlyName comes from the SSDP discovery cache — the operator
	// configures only the UDN, the bridge fills in the display label
	// from the upstream's `<friendlyName>` descriptor. The public
	// adapter MUST publish that label so iOS shows the device's own
	// name (e.g. "Chord 2Go:2go-ars") instead of just the operator's
	// terse YAML `Name` field.
	cfg := &config.Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		UPnPUpstream: config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{
				{Name: "Chord", UDN: "uuid:2go", PathPrefix: "chord"},
			},
		},
	}
	cache := upnp.NewServerCache()
	cache.Upsert(upnp.ServerInfo{
		UDN:                        "uuid:2go",
		FriendlyName:               "Chord 2Go:2go-ars",
		ContentDirectoryControlURL: "http://192.168.0.62:8200/ctl/ContentDir",
		LastSeenAt:                 time.Now(),
	})
	a := &upnpPublicAdapter{
		cfgHolder: config.NewRuntimeConfig(cfg),
		cache:     cache,
		store:     openPublicTestStore(t),
	}
	got := a.PublicServers()
	if len(got) != 1 {
		t.Fatalf("got %d rows; want 1", len(got))
	}
	if got[0].FriendlyName != "Chord 2Go:2go-ars" {
		t.Errorf("FriendlyName = %q; want %q", got[0].FriendlyName, "Chord 2Go:2go-ars")
	}
	if got[0].ConfiguredUDN != "uuid:2go" {
		t.Errorf("ConfiguredUDN = %q; want %q", got[0].ConfiguredUDN, "uuid:2go")
	}
	if got[0].PathPrefix != "chord" {
		t.Errorf("PathPrefix = %q; want %q", got[0].PathPrefix, "chord")
	}
}

func TestPublic_FriendlyName_EmptyWhenUndiscovered(t *testing.T) {
	// Pre-discovery (bridge just started, SSDP cache empty for this
	// UDN) the FriendlyName field MUST be empty on the wire — iOS
	// falls back to Name. NOT a stale placeholder like the UDN string.
	cfg := &config.Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		UPnPUpstream: config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{
				{Name: "Cold-start 2Go", UDN: "uuid:cold", PathPrefix: "cold"},
			},
		},
	}
	a := &upnpPublicAdapter{
		cfgHolder: config.NewRuntimeConfig(cfg),
		cache:     upnp.NewServerCache(), // empty
		store:     openPublicTestStore(t),
	}
	got := a.PublicServers()
	if got[0].FriendlyName != "" {
		t.Errorf("FriendlyName = %q; want \"\" (pre-discovery)", got[0].FriendlyName)
	}
}

func TestPublic_ManualURLServer_OmitsConfiguredUDN(t *testing.T) {
	// Manual-URL entries don't have a configured UDN — iOS would have
	// nothing useful to do with the bridge's internal hash-key form
	// (e.g. "manual:abc123..."). The wire shape MUST emit an empty
	// ConfiguredUDN (which omitempty drops) so iOS doesn't try to
	// persist it.
	cfg := &config.Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		UPnPUpstream: config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{
				{Name: "Manual", ManualDescriptionURL: "http://manual:8200/desc.xml", PathPrefix: "manual"},
			},
		},
	}
	a := &upnpPublicAdapter{
		cfgHolder: config.NewRuntimeConfig(cfg),
		cache:     upnp.NewServerCache(),
		store:     openPublicTestStore(t),
	}
	got := a.PublicServers()
	if got[0].ConfiguredUDN != "" {
		t.Errorf("ConfiguredUDN = %q; want \"\" for manual entry", got[0].ConfiguredUDN)
	}
	if got[0].Name != "Manual" {
		t.Errorf("Name = %q; want %q", got[0].Name, "Manual")
	}
}

func TestPublic_RoutedTracks_KeyedOnStableServerKey(t *testing.T) {
	// RoutedTracks MUST count via upnpingest.StableServerKey so manual
	// servers (hashed-URL key) AND UDN servers see their own counts.
	// Pre-fix the admin adapter gated this on `srv.UDN != ""` which
	// silently excluded manual servers — same hazard here. The public
	// adapter takes that lesson via the StableServerKey call.
	cfg := &config.Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		UPnPUpstream: config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{
				{Name: "UDN", UDN: "uuid:udn", PathPrefix: "udn"},
				{Name: "Manual", ManualDescriptionURL: "http://manual:8200/desc.xml", PathPrefix: "manual"},
			},
		},
	}
	store := openPublicTestStore(t)

	udnKey := upnpingest.StableServerKey(cfg.UPnPUpstream.Servers[0])
	manualKey := upnpingest.StableServerKey(cfg.UPnPUpstream.Servers[1])

	// Seed 3 udn-routed tracks + 5 manual-routed tracks. Tracks first
	// (FK), then routing rows.
	for _, p := range []struct{ key, path string }{
		{udnKey, "udn/a.flac"}, {udnKey, "udn/b.flac"}, {udnKey, "udn/c.flac"},
		{manualKey, "manual/a.flac"}, {manualKey, "manual/b.flac"}, {manualKey, "manual/c.flac"},
		{manualKey, "manual/d.flac"}, {manualKey, "manual/e.flac"},
	} {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: p.path, Size: 1, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertUPnPRouting(context.Background(), &manifest.UPnPRouting{
			SourcePath: p.path, ServerUDN: p.key, ObjectID: "x",
			ResURL: "http://h/x.flac", LastSeenAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	a := &upnpPublicAdapter{
		cfgHolder: config.NewRuntimeConfig(cfg),
		cache:     upnp.NewServerCache(),
		store:     store,
	}
	got := a.PublicServers()
	if got[0].RoutedTracks != 3 {
		t.Errorf("UDN row RoutedTracks = %d; want 3", got[0].RoutedTracks)
	}
	if got[1].RoutedTracks != 5 {
		t.Errorf("Manual row RoutedTracks = %d; want 5", got[1].RoutedTracks)
	}
}

func TestPublic_RoutedTracks_ZeroWhenNoIngest(t *testing.T) {
	// Warm-up window OR per-server walk error → no routing rows yet
	// → RoutedTracks emits 0 (NOT a stale carry-over). The field is
	// NOT omitempty on the wire, so iOS sees the literal 0 and can
	// render "(0 tracks via …)" or hide the chip.
	cfg := &config.Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		UPnPUpstream: config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{
				{Name: "Fresh", UDN: "uuid:fresh", PathPrefix: "fresh"},
			},
		},
	}
	a := &upnpPublicAdapter{
		cfgHolder: config.NewRuntimeConfig(cfg),
		cache:     upnp.NewServerCache(),
		store:     openPublicTestStore(t),
	}
	got := a.PublicServers()
	if got[0].RoutedTracks != 0 {
		t.Errorf("RoutedTracks = %d; want 0", got[0].RoutedTracks)
	}
}

func TestPublic_NilCfg_ReturnsNil(t *testing.T) {
	// Defensive: a torn-down RuntimeConfig must not panic the
	// /v1/health handler — return nil so the omitempty drops the
	// field cleanly.
	rt := config.NewRuntimeConfig(nil)
	a := &upnpPublicAdapter{
		cfgHolder: rt,
		cache:     upnp.NewServerCache(),
		store:     openPublicTestStore(t),
	}
	if got := a.PublicServers(); got != nil {
		t.Errorf("got %v; want nil for nil cfg", got)
	}
}

func TestPublic_NilStore_RoutedTracksZero(t *testing.T) {
	// If the store wiring is nil (lifecycle constructed pre-store —
	// shouldn't happen in production, but the adapter MUST stay
	// crash-free regardless), RoutedTracks defaults to 0 rather than
	// panicking.
	cfg := &config.Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		UPnPUpstream: config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{
				{Name: "No store", UDN: "uuid:n", PathPrefix: "n"},
			},
		},
	}
	a := &upnpPublicAdapter{
		cfgHolder: config.NewRuntimeConfig(cfg),
		cache:     upnp.NewServerCache(),
		store:     nil,
	}
	got := a.PublicServers()
	if got[0].RoutedTracks != 0 {
		t.Errorf("RoutedTracks = %d; want 0 with nil store", got[0].RoutedTracks)
	}
}

func TestPublic_InstallPublicProvider_NilLifecycle(t *testing.T) {
	// `installPublicProvider` MUST return nil for a nil lifecycle so
	// cmd/bridge can pass through `WithUPnPUpstreamPublicProvider(nil)`
	// and the wire shape stays pre-feature on disabled deploys.
	var l *upnpUpstreamLifecycle
	cfg := &config.Config{}
	got := l.installPublicProvider(config.NewRuntimeConfig(cfg), nil)
	if got != nil {
		t.Errorf("got %v; want nil for nil lifecycle", got)
	}
}

func TestPublic_InstallPublicProvider_NilCache(t *testing.T) {
	// A lifecycle whose cache wasn't initialised (feature disabled
	// path / no LAN interface bind) MUST also surface nil — the
	// PublicServers() implementation depends on cache for friendly-
	// name lookup, so a nil-cache adapter would crash on Get().
	l := &upnpUpstreamLifecycle{cache: nil}
	cfg := &config.Config{}
	got := l.installPublicProvider(config.NewRuntimeConfig(cfg), nil)
	if got != nil {
		t.Errorf("got %v; want nil for nil cache", got)
	}
}
