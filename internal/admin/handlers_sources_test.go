package admin

import "testing"

type sourcesCase struct {
	name        string
	total       int
	routed      int
	provider    UPnPUpstreamProvider
	wantFS      int
	wantRouted  int
	wantEnabled bool
	wantServers []sourceServerRow
}

// TestGetSourcesSnapshot pins the dashboard filesystem-vs-UPnP breakdown
// assembler: filesystem = total - routedTotal (clamped >= 0), the clamped
// routed distributed as a running budget across per-server rows so the
// rendered breakdown always reconciles to Total, manual-URL servers marked
// unmonitored, and a nil provider yielding a non-nil empty server slice.
//
// Uses the warm stats cache directly (s.statsDB / s.statsDBValid) so no
// real *manifest.Store is needed — trackSourceCounts short-circuits before
// any DB read.
func TestGetSourcesSnapshot(t *testing.T) {
	udnSrv := func(name, udn string, routed int, discovered bool) UPnPUpstreamServerState {
		return UPnPUpstreamServerState{Name: name, ConfiguredUDN: udn, RoutedTracks: routed, Discovered: discovered}
	}

	tests := []sourcesCase{
		{
			name: "normal split", total: 100, routed: 40,
			provider:    &stubUPnPProvider{servers: []UPnPUpstreamServerState{udnSrv("2Go", "uuid:a", 40, false)}},
			wantFS:      60,
			wantRouted:  40,
			wantEnabled: true,
			wantServers: []sourceServerRow{{Name: "2Go", RoutedTracks: 40, Online: false, Monitored: true}},
		},
		{
			// Sub-ms cross-read race: cached routed briefly exceeds total.
			// Clamp routed to total so filesystem stays >= 0 and the row is
			// budget-capped, keeping fs + rows == total.
			name: "over-routed clamps to total", total: 100, routed: 120,
			provider:    &stubUPnPProvider{servers: []UPnPUpstreamServerState{udnSrv("2Go", "uuid:a", 120, true)}},
			wantFS:      0,
			wantRouted:  100,
			wantEnabled: true,
			wantServers: []sourceServerRow{{Name: "2Go", RoutedTracks: 100, Online: true, Monitored: true}},
		},
		{
			// Per-server counts (separate read) transiently sum above the
			// cached routed total; the running budget caps the later row so
			// the rows never over-sum. Steady state never hits this path.
			name: "budget distributes across servers", total: 100, routed: 100,
			provider: &stubUPnPProvider{servers: []UPnPUpstreamServerState{
				udnSrv("A", "uuid:a", 80, true),
				udnSrv("B", "uuid:b", 40, false),
			}},
			wantFS:      0,
			wantRouted:  100,
			wantEnabled: true,
			wantServers: []sourceServerRow{
				{Name: "A", RoutedTracks: 80, Online: true, Monitored: true},
				{Name: "B", RoutedTracks: 20, Online: false, Monitored: true}, // 40 capped to remaining budget 20
			},
		},
		{
			name: "manual server not monitored", total: 100, routed: 30,
			provider: &stubUPnPProvider{servers: []UPnPUpstreamServerState{
				{Name: "Manual", ManualURL: "http://host/rootDesc.xml", RoutedTracks: 30, Discovered: false},
			}},
			wantFS:      70,
			wantRouted:  30,
			wantEnabled: true,
			wantServers: []sourceServerRow{{Name: "Manual", RoutedTracks: 30, Online: false, Monitored: false}},
		},
		{
			name: "upnp disabled", total: 50, routed: 0, provider: nil,
			wantFS:      50,
			wantRouted:  0,
			wantEnabled: false,
			wantServers: []sourceServerRow{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{deps: Deps{UPnPUpstream: tc.provider}}
			s.statsDB = statsDBPart{tracks: tc.total, upnpRouted: tc.routed}
			s.statsDBValid = true
			assertSourcesSnapshot(t, tc, s.getSourcesSnapshot())
		})
	}
}

// assertSourcesSnapshot checks the scalar fields, then delegates the
// per-server rows and the reconciliation invariant to focused helpers
// (kept split so each stays under the cognitive-complexity budget).
func assertSourcesSnapshot(t *testing.T, tc sourcesCase, got sourcesResponse) {
	t.Helper()
	if got.Filesystem != tc.wantFS {
		t.Errorf("Filesystem: got %d want %d", got.Filesystem, tc.wantFS)
	}
	if got.RoutedTotal != tc.wantRouted {
		t.Errorf("RoutedTotal: got %d want %d", got.RoutedTotal, tc.wantRouted)
	}
	if got.Total != tc.total {
		t.Errorf("Total: got %d want %d", got.Total, tc.total)
	}
	if got.UPnPEnabled != tc.wantEnabled {
		t.Errorf("UPnPEnabled: got %v want %v", got.UPnPEnabled, tc.wantEnabled)
	}
	assertSourceServers(t, tc.wantServers, got.Servers)
	assertSourcesReconcile(t, got)
}

// assertSourceServers checks the per-server rows exactly and that the slice
// is non-nil (so it marshals to [] not null).
func assertSourceServers(t *testing.T, want, got []sourceServerRow) {
	t.Helper()
	if got == nil {
		t.Fatalf("Servers must be non-nil so it marshals to [] not null")
	}
	if len(got) != len(want) {
		t.Fatalf("Servers len: got %d want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Servers[%d]: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// assertSourcesReconcile pins the invariant that the rendered rows sum back
// to Total — filesystem + sum(server rows) + orphan remainder == total — and
// that the remainder is never negative (the budget cap keeps
// sum(rows) <= routedTotal).
func assertSourcesReconcile(t *testing.T, got sourcesResponse) {
	t.Helper()
	sum := 0
	for _, r := range got.Servers {
		sum += r.RoutedTracks
	}
	remainder := got.RoutedTotal - sum
	if remainder < 0 {
		t.Errorf("orphan remainder negative: routedTotal=%d sumRows=%d", got.RoutedTotal, sum)
	}
	if got.Filesystem+sum+remainder != got.Total {
		t.Errorf("does not reconcile: fs=%d + rows=%d + remainder=%d != total=%d",
			got.Filesystem, sum, remainder, got.Total)
	}
}
