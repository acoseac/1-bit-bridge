package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
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
	got := a.ConfiguredServers(context.Background())
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

func TestAdmin_ForceRescan_RefusesDuringShutdown(t *testing.T) {
	// bgCtx is the lifecycle run scope (the PARENT of the periodic ingest
	// loop's tickCtx). Once it's cancelled the periodic loop drops its
	// startup +1 on ingestWg toward zero, so ForceRescan must refuse
	// rather than call ingestWg.Add(1) into a possible Wait-at-zero race
	// (Gemini medium on PR #524). The refusal happens BEFORE the inFlight
	// latch AND before the Add, so the latch is left clean and no walk
	// goroutine is spawned. Regression guard: without the bgCtx.Err() gate
	// this call would return nil (and Add(1) + spawn against the nil
	// ingester).
	cfg := newUPnPTestCfg(t,
		config.UPnPUpstreamServerConfig{Name: "S", UDN: "uuid:abc"},
	)
	rt := runtimeCfgFor(t, cfg)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	var ingestWg sync.WaitGroup
	a := &upnpAdminAdapter{
		cfgHolder: rt, cache: upnp.NewServerCache(),
		state: newUPnPAdminState(), bgCtx: cancelled, ingestWg: &ingestWg,
	}
	if err := a.ForceRescan(context.Background(), ""); err == nil {
		t.Fatal("ForceRescan should refuse when bgCtx is cancelled (shutdown in progress)")
	}
	// The inFlight latch must be untouched — the refusal returns before it.
	a.state.mu.Lock()
	inFlight := a.state.inFlight
	a.state.mu.Unlock()
	if inFlight {
		t.Error("inFlight latch leaked on the shutdown-refusal path")
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

func TestSanitizeSkipList(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"trims and drops empties", []string{" A ", "", "B", "  "}, []string{"A", "B"}},
		{"dedups preserving order", []string{"System", "System", "Other"}, []string{"System", "Other"}},
		{"dedup is case-sensitive", []string{"System", "system"}, []string{"System", "system"}},
		{"all empty is nil", []string{"", "   "}, nil},
		{"nil input is nil", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sanitizeSkipList(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("sanitizeSkipList(%q) = %q, want %q", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("index %d: got %q, want %q (full: %q)", i, got[i], c.want[i], got)
				}
			}
		})
	}
}

// upnpAdapterForPersistTest wires the minimal adapter shape the CRUD
// paths exercise: holder + cfgPath (the persist target). cfg gets a
// DataDir so updateCfg's NormalizeAndValidate sees a fully-valid
// config. cfgPath's parent must exist (Config.Save writes its temp
// file there).
func upnpAdapterForPersistTest(t *testing.T, cfg *config.Config) (*upnpAdminAdapter, *config.RuntimeConfig, string) {
	t.Helper()
	cfg.DataDir = filepath.Join(t.TempDir(), "data")
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	rt := runtimeCfgFor(t, cfg)
	return &upnpAdminAdapter{cfgHolder: rt, cfgPath: cfgPath}, rt, cfgPath
}

// TestAdmin_UPnPCRUDPersistsThroughUpdate walks the real CRUD →
// updateCfg → RuntimeConfig.Update → Save path end-to-end (pre-fix
// this persist path had no direct coverage — the admin-layer tests
// stub the provider). Each step asserts BOTH the live snapshot and
// the on-disk YAML, pinning the Save→Store consistency the M13 fix
// relies on.
func TestAdmin_UPnPCRUDPersistsThroughUpdate(t *testing.T) {
	a, rt, cfgPath := upnpAdapterForPersistTest(t, newUPnPTestCfg(t,
		config.UPnPUpstreamServerConfig{Name: "Seed", UDN: "uuid:seed"},
	))

	assertPersisted := func(t *testing.T, wantUDNs ...string) {
		t.Helper()
		reloaded, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("reload bridge.yaml: %v", err)
		}
		for _, snapshot := range []struct {
			label string
			cfg   *config.Config
		}{{"live", rt.Load()}, {"disk", reloaded}} {
			got := make([]string, 0, len(snapshot.cfg.UPnPUpstream.Servers))
			for _, srv := range snapshot.cfg.UPnPUpstream.Servers {
				got = append(got, srv.UDN)
			}
			if !slices.Equal(got, wantUDNs) {
				t.Errorf("%s servers = %v, want %v", snapshot.label, got, wantUDNs)
			}
		}
	}

	t.Run("add persists to live + disk", func(t *testing.T) {
		err := a.AddServer(context.Background(), admin.UPnPServerAddRequest{
			Name: "Denon", UDN: "uuid:denon", PathPrefix: "/music",
		})
		if err != nil {
			t.Fatalf("AddServer: %v", err)
		}
		assertPersisted(t, "uuid:seed", "uuid:denon")
	})

	t.Run("add duplicate identity rejected", func(t *testing.T) {
		err := a.AddServer(context.Background(), admin.UPnPServerAddRequest{
			Name: "Denon clone", UDN: "uuid:denon",
		})
		if !errors.Is(err, admin.ErrUPnPDuplicateUDN) {
			t.Fatalf("err = %v, want ErrUPnPDuplicateUDN", err)
		}
		assertPersisted(t, "uuid:seed", "uuid:denon")
	})

	t.Run("update edits in place", func(t *testing.T) {
		name := "Denon AVR"
		err := a.UpdateServer(context.Background(), "uuid:denon", admin.UPnPServerUpdateRequest{Name: &name})
		if err != nil {
			t.Fatalf("UpdateServer: %v", err)
		}
		assertPersisted(t, "uuid:seed", "uuid:denon")
		if got := rt.Load().UPnPUpstream.Servers[1].Name; got != "Denon AVR" {
			t.Errorf("live Name = %q, want %q", got, "Denon AVR")
		}
	})

	t.Run("remove drops from live + disk", func(t *testing.T) {
		if err := a.RemoveServer(context.Background(), "uuid:denon"); err != nil {
			t.Fatalf("RemoveServer: %v", err)
		}
		assertPersisted(t, "uuid:seed")
	})

	t.Run("remove unknown identity", func(t *testing.T) {
		err := a.RemoveServer(context.Background(), "uuid:ghost")
		if !errors.Is(err, admin.ErrUPnPNoSuchServer) {
			t.Fatalf("err = %v, want ErrUPnPNoSuchServer", err)
		}
	})
}

// TestAdmin_UPnPCRUDConcurrentWithSettingsUpdate is the 2026-07-21
// review M13 regression test at the adapter level. Pre-fix, UPnP
// server CRUD serialized its clone→Save→Store on the adapter's own
// crudMu while the admin settings PATCH serialized on the admin
// server's s.mu — two DIFFERENT mutexes — so a concurrent pair cloned
// the same base and the last Save silently dropped the loser's field
// from bridge.yaml AND the live snapshot while both callers returned
// success. Both writer families now funnel through
// RuntimeConfig.Update's single write lock, so every mutation must
// survive on disk and in the live snapshot.
func TestAdmin_UPnPCRUDConcurrentWithSettingsUpdate(t *testing.T) {
	a, rt, cfgPath := upnpAdapterForPersistTest(t, newUPnPTestCfg(t))

	const n = 8
	var wg sync.WaitGroup
	wg.Add(2)
	// UPnP-style writer: adapter CRUD appending to a slice field.
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			udn := fmt.Sprintf("uuid:race-%d", i)
			if err := a.AddServer(context.Background(), admin.UPnPServerAddRequest{
				Name: "Race " + udn, UDN: udn,
			}); err != nil {
				t.Errorf("AddServer(%s): %v", udn, err)
				return
			}
		}
	}()
	// Settings-style writer: scalar field mutation via the same
	// holder the admin server's settings PATCH writes through.
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("Library %d", i)
			if err := rt.Update(cfgPath, func(next *config.Config) error {
				next.LibraryName = name
				return nil
			}); err != nil {
				t.Errorf("settings-style Update: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// Only the settings writer touches LibraryName and every clone
	// preserves it, so the final value is deterministically its last
	// write; every adapter append must have landed exactly once.
	live := rt.Load()
	if want := fmt.Sprintf("Library %d", n-1); live.LibraryName != want {
		t.Errorf("live LibraryName = %q, want %q — settings write lost to a stale clone",
			live.LibraryName, want)
	}
	if len(live.UPnPUpstream.Servers) != n {
		t.Errorf("live servers = %d, want %d — adapter writes lost to a stale clone",
			len(live.UPnPUpstream.Servers), n)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload bridge.yaml: %v", err)
	}
	if reloaded.LibraryName != live.LibraryName {
		t.Errorf("disk LibraryName = %q, live = %q — last Save dropped a concurrent write",
			reloaded.LibraryName, live.LibraryName)
	}
	if len(reloaded.UPnPUpstream.Servers) != len(live.UPnPUpstream.Servers) {
		t.Errorf("disk servers = %d, live = %d — last Save dropped a concurrent write",
			len(reloaded.UPnPUpstream.Servers), len(live.UPnPUpstream.Servers))
	}
}

// TestAdmin_EditRoundTripPreservesFieldsTheModalDidNotChange replays the
// exact sequence the admin edit modal performs — READ a row, PATCH it
// back with one field changed — and asserts the other three survive.
//
// This is the regression pin for a silent config wipe. The modal builds
// its PATCH from the row it was given and sends all four editable fields
// unconditionally; JSON.stringify preserves "" and [] (only `undefined`
// is dropped), and on the server every non-nil pointer is an assignment.
// So while `ConfiguredServers` omitted PathPrefix / RootObjectID /
// SkipTopLevelContainers, the modal had nothing to send but blanks and a
// plain rename cleared all three. Next ingest: EffectiveRootObjectID()
// falls back to "64" (the wrong subtree for any non-MiniDLNA upstream)
// and normalizePrefix falls back to the raw Name, moving every routed
// track's manifest path into a new namespace — the reconcile sweep reaps
// the old rows and the re-inserted ones return with enriched_at = 0, so
// the whole upstream re-crawls MB/CAA/Deezer and re-syncs to every
// paired device.
//
// The existing handler test asserted only that UpdateServer was CALLED.
// That gap is why this shipped, so this one asserts the PAYLOAD and the
// persisted result.
func TestAdmin_EditRoundTripPreservesFieldsTheModalDidNotChange(t *testing.T) {
	const (
		udn      = "uuid:2go"
		prefix   = "chord-2go"
		rootObj  = "0"
		skipJunk = "System Volume Information"
	)
	cfg := newUPnPTestCfg(t, config.UPnPUpstreamServerConfig{
		Name:                   "2Go",
		UDN:                    udn,
		PathPrefix:             prefix,
		RootObjectID:           rootObj,
		SkipTopLevelContainers: []string{skipJunk, "$RECYCLE.BIN"},
	})
	a, rt, _ := upnpAdapterForPersistTest(t, cfg)
	// The persist helper wires only holder + cfgPath; ConfiguredServers
	// also reads the last-walk state.
	a.state = newUPnPAdminState()

	// --- READ: what the modal can see. ---
	rows := a.ConfiguredServers(context.Background())
	if len(rows) != 1 {
		t.Fatalf("ConfiguredServers returned %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.PathPrefix != prefix {
		t.Errorf("row.PathPrefix = %q, want %q — the modal cannot round-trip "+
			"a field the read surface hides", row.PathPrefix, prefix)
	}
	if row.RootObjectID != rootObj {
		t.Errorf("row.RootObjectID = %q, want %q", row.RootObjectID, rootObj)
	}
	if !slices.Equal(row.SkipTopLevelContainers, []string{skipJunk, "$RECYCLE.BIN"}) {
		t.Errorf("row.SkipTopLevelContainers = %v, want the configured pair",
			row.SkipTopLevelContainers)
	}

	// --- PATCH: exactly what wireUpnpEditModal submits. Every editable
	// field is sent, sourced from the row above; only Name was typed. ---
	renamed := "Chord 2Go"
	patch := admin.UPnPServerUpdateRequest{
		Name:                   &renamed,
		PathPrefix:             &row.PathPrefix,
		RootObjectID:           &row.RootObjectID,
		SkipTopLevelContainers: &row.SkipTopLevelContainers,
	}
	if err := a.UpdateServer(context.Background(), udn, patch); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}

	// --- ASSERT: the rename landed and nothing else moved. ---
	saved := rt.Load().UPnPUpstream.Servers[0]
	if saved.Name != renamed {
		t.Errorf("Name = %q, want %q", saved.Name, renamed)
	}
	if saved.PathPrefix != prefix {
		t.Errorf("PathPrefix = %q after a rename, want %q — every routed "+
			"track's manifest path just moved namespace", saved.PathPrefix, prefix)
	}
	if saved.RootObjectID != rootObj {
		t.Errorf("RootObjectID = %q after a rename, want %q — the next walk "+
			"browses the wrong subtree", saved.RootObjectID, rootObj)
	}
	if !slices.Contains(saved.SkipTopLevelContainers, skipJunk) {
		t.Errorf("SkipTopLevelContainers = %v after a rename, want %q retained",
			saved.SkipTopLevelContainers, skipJunk)
	}
}

// TestAdmin_ConfiguredServers_ClonesSkipList guards the DTO against
// aliasing the live config's slice — the row escapes to handlers, and a
// mutation there would reach into the running config.
func TestAdmin_ConfiguredServers_ClonesSkipList(t *testing.T) {
	cfg := newUPnPTestCfg(t, config.UPnPUpstreamServerConfig{
		Name: "2Go", UDN: "uuid:2go", SkipTopLevelContainers: []string{"junk"},
	})
	a := &upnpAdminAdapter{cfgHolder: runtimeCfgFor(t, cfg), state: newUPnPAdminState()}

	rows := a.ConfiguredServers(context.Background())
	rows[0].SkipTopLevelContainers[0] = "clobbered"

	if got := cfg.UPnPUpstream.Servers[0].SkipTopLevelContainers[0]; got != "junk" {
		t.Errorf("live config skip list = %q after mutating the DTO copy; "+
			"the row aliases the running config", got)
	}
}
