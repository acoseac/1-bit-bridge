package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/auth"
)

// TestHealthEndpointsIsTheSnapshotHealthServes pins the advertised half of
// the player's address inventory (B470 PR 5a4): HealthEndpoints is exactly
// the endpoints /v1/health carries, through the same cache, so the hosts the
// player's Insert takes are the hosts a client of /v1/health could have
// read. With eth0 and wlan0 up, both addresses are in it; once the walk lists
// eth0 alone (wlan0's address gone, or a walk that could not read wlan0's
// addresses and gives the part it listed), neither /v1/health nor
// HealthEndpoints changes until the snapshot is refreshed, and then both lose
// the address together.
func TestHealthEndpointsIsTheSnapshotHealthServes(t *testing.T) {
	flags := net.FlagUp | net.FlagBroadcast | net.FlagMulticast | net.FlagRunning
	eth0 := advertise.InterfaceForTest{Name: "eth0", Flags: flags, Addrs: []string{"192.168.1.5"}}
	wlan0 := advertise.InterfaceForTest{Name: "wlan0", Flags: flags, Addrs: []string{"192.168.2.5"}}
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{eth0, wlan0}))
	cfg := healthTestConfig(t)
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "AB:CD")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	health := func() []string {
		t.Helper()
		res, err := http.Get(hs.URL + "/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var h HealthResponse
		if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
			t.Fatal(err)
		}
		return h.Endpoints
	}
	const lan, wifi = "https://192.168.1.5:7788", "https://192.168.2.5:7788"
	first := health()
	if got := srv.HealthEndpoints(); !slices.Equal(got, first) {
		t.Fatalf("HealthEndpoints %v, /v1/health %v", got, first)
	}
	if !slices.Contains(first, lan) || !slices.Contains(first, wifi) {
		t.Fatalf("the snapshot %v lacks an address of the walk", first)
	}
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{eth0}))
	if got := health(); !slices.Equal(got, first) {
		t.Errorf("/v1/health %v inside the snapshot's TTL, want it unchanged, %v", got, first)
	}
	if got := srv.HealthEndpoints(); !slices.Equal(got, first) {
		t.Errorf("HealthEndpoints %v inside the snapshot's TTL, want it unchanged, %v", got, first)
	}
	srv.endpointsCache.mu.Lock()
	srv.endpointsCache.hasSnap = false
	srv.endpointsCache.mu.Unlock()
	now := srv.HealthEndpoints()
	if got := health(); !slices.Equal(got, now) {
		t.Errorf("after the refresh HealthEndpoints %v, /v1/health %v", now, got)
	}
	if !slices.Contains(now, lan) || slices.Contains(now, wifi) {
		t.Errorf("after the refresh %v, want the LAN address and not wlan0's", now)
	}
}
