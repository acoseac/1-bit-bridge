package admin

import "testing"

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

	tests := []struct {
		name        string
		total       int
		routed      int
		provider    UPnPUpstreamProvider
		wantFS      int
		wantRouted  int
		wantEnabled bool
		wantServers []sourceServerRow
	}{
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

			got := s.getSourcesSnapshot()

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
			if got.Servers == nil {
				t.Fatalf("Servers must be non-nil so it marshals to [] not null")
			}
			if len(got.Servers) != len(tc.wantServers) {
				t.Fatalf("Servers len: got %d want %d (%+v)", len(got.Servers), len(tc.wantServers), got.Servers)
			}
			for i, want := range tc.wantServers {
				if got.Servers[i] != want {
					t.Errorf("Servers[%d]: got %+v want %+v", i, got.Servers[i], want)
				}
			}

			// Reconciliation invariant: filesystem + sum(rows) + orphan
			// remainder == total, and the remainder is never negative (the
			// budget cap guarantees sum(rows) <= routedTotal).
			sumRows := 0
			for _, r := range got.Servers {
				sumRows += r.RoutedTracks
			}
			remainder := got.RoutedTotal - sumRows
			if remainder < 0 {
				t.Errorf("orphan remainder negative: routedTotal=%d sumRows=%d", got.RoutedTotal, sumRows)
			}
			if got.Filesystem+sumRows+remainder != got.Total {
				t.Errorf("does not reconcile: fs=%d + rows=%d + remainder=%d != total=%d",
					got.Filesystem, sumRows, remainder, got.Total)
			}
		})
	}
}
