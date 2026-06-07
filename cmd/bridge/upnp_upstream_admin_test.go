package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// runtimeCfgFor returns a *config.RuntimeConfig backed by the supplied
// Config. The admin adapter only reads cfgHolder.Load() so a minimal
// holder is enough.
func runtimeCfgFor(t *testing.T, cfg *config.Config) *config.RuntimeConfig {
	t.Helper()
	return config.NewRuntimeConfig(cfg)
}

func openUPnPTestStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAdmin_ConfiguredServers_IncludesManualURLServers(t *testing.T) {
	// Manual-URL server has empty UDN, but upnpingest.StableServerKey hashes its
	// ManualDescriptionURL into a "manual:<sha>" key. Pre-fix the
	// adapter gated the routed-tracks + last-walk lookups on srv.UDN
	// != "" which silently excluded manual servers entirely (Gemini
	// HIGH on PR #353). The adapter now uses upnpingest.StableServerKey so manual
	// servers DO surface their state.
	cfg := newUPnPTestCfg(t,
		config.UPnPUpstreamServerConfig{Name: "Manual Server", ManualDescriptionURL: "http://manual:8200/desc.xml"},
		config.UPnPUpstreamServerConfig{Name: "UDN Server", UDN: "uuid:abc"},
	)
	rt := runtimeCfgFor(t, cfg)
	store := openUPnPTestStore(t)
	cache := upnp.NewServerCache()

	// Pre-seed manifest routing rows for BOTH servers — including the
	// manual one keyed on its upnpingest.StableServerKey.
	manualKey := upnpingest.StableServerKey(cfg.UPnPUpstream.Servers[0])
	udnKey := upnpingest.StableServerKey(cfg.UPnPUpstream.Servers[1])
	for _, p := range []struct{ key, path string }{
		{manualKey, "manual/track1.flac"},
		{manualKey, "manual/track2.flac"},
		{udnKey, "udn/track1.flac"},
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

	a := &upnpAdminAdapter{
		cfgHolder: rt, cache: cache, store: store,
		state: newUPnPAdminState(),
	}
	got := a.ConfiguredServers()
	if len(got) != 2 {
		t.Fatalf("got %d server rows; want 2", len(got))
	}
	// Verify both servers report their routed-track counts.
	rowsByName := map[string]admin.UPnPUpstreamServerState{}
	for _, r := range got {
		rowsByName[r.Name] = r
	}
	if rowsByName["Manual Server"].RoutedTracks != 2 {
		t.Errorf("manual server RoutedTracks = %d; want 2",
			rowsByName["Manual Server"].RoutedTracks)
	}
	if rowsByName["UDN Server"].RoutedTracks != 1 {
		t.Errorf("UDN server RoutedTracks = %d; want 1",
			rowsByName["UDN Server"].RoutedTracks)
	}
}

func TestAdmin_ForceRescan_AsyncDoesNotBlockHandler(t *testing.T) {
	// Pre-fix ForceRescan ran Ingester.Run synchronously, so a multi-
	// second walk would tie up the admin HTTP connection AND abort on
	// any client disconnect. Now the call returns 202 immediately and
	// the walk runs on a background goroutine using the lifecycle's
	// ctx (not the request ctx). Gemini HIGH on PR #353.
	cfg := newUPnPTestCfg(t,
		config.UPnPUpstreamServerConfig{Name: "S", UDN: "uuid:abc"},
	)
	rt := runtimeCfgFor(t, cfg)
	store := openUPnPTestStore(t)

	a := &upnpAdminAdapter{
		cfgHolder: rt, cache: upnp.NewServerCache(), store: store,
		state: newUPnPAdminState(), bgCtx: context.Background(),
	}

	// We can't inject a swappable Ingester today (the field is typed
	// *upnpingest.Ingester), so we test the async-spawn semantics
	// directly via the state.inFlight latch: a pre-held latch means a
	// second request returns ErrUPnPRescanInFlight WITHOUT touching
	// the ingester or spawning a goroutine. A future async-test-seam
	// refactor can exercise the goroutine-spawn path with a stub.

	// Start: take in_flight by hand to model "first call returns 202".
	a.state.mu.Lock()
	a.state.inFlight = true
	a.state.mu.Unlock()
	// Second call must return in-flight error WITHOUT touching the ingester.
	if err := a.ForceRescan(context.Background(), ""); !errors.Is(err, admin.ErrUPnPRescanInFlight) {
		t.Fatalf("err = %v; want ErrUPnPRescanInFlight", err)
	}
}

func TestAdmin_ForceRescan_RejectsUnknownUDN(t *testing.T) {
	cfg := newUPnPTestCfg(t,
		config.UPnPUpstreamServerConfig{Name: "S", UDN: "uuid:known"},
	)
	rt := runtimeCfgFor(t, cfg)
	a := &upnpAdminAdapter{
		cfgHolder: rt, cache: upnp.NewServerCache(),
		state: newUPnPAdminState(), bgCtx: context.Background(),
	}
	err := a.ForceRescan(context.Background(), "uuid:unknown")
	if !errors.Is(err, admin.ErrUPnPNoSuchServer) {
		t.Fatalf("err = %v; want ErrUPnPNoSuchServer", err)
	}
}

func TestAdmin_ForceRescan_AcceptsManualURLServer(t *testing.T) {
	// A server configured with ONLY a ManualDescriptionURL (empty UDN)
	// must be force-rescannable by that URL — RemoveServer/UpdateServer
	// already accept it via findConfiguredIdx; pre-fix ForceRescan's
	// srv.UDN-only loop 404'd it (residual miss of PR #353's sibling-gate
	// fix). Pre-hold the in-flight latch so a PASSING identity check
	// surfaces as ErrUPnPRescanInFlight (reached only AFTER the identity
	// gate) rather than ErrUPnPNoSuchServer — and without spawning the
	// background ingester goroutine (nil in this adapter).
	const manualURL = "http://manual:8200/desc.xml"
	cfg := newUPnPTestCfg(t,
		config.UPnPUpstreamServerConfig{Name: "Manual", ManualDescriptionURL: manualURL, PathPrefix: "manual"},
	)
	rt := runtimeCfgFor(t, cfg)
	a := &upnpAdminAdapter{
		cfgHolder: rt, cache: upnp.NewServerCache(),
		state: newUPnPAdminState(), bgCtx: context.Background(),
	}
	a.state.mu.Lock()
	a.state.inFlight = true
	a.state.mu.Unlock()
	if err := a.ForceRescan(context.Background(), manualURL); !errors.Is(err, admin.ErrUPnPRescanInFlight) {
		t.Fatalf("err = %v; want ErrUPnPRescanInFlight (identity gate must accept the manual URL)", err)
	}
}
