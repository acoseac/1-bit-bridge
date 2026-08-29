package admin

import (
	"net/http"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestLibrarySpaceConfiguredOnlyWithAFloorOrUploads drives the progressive
// disclosure. A self-hoster with a 40 TB NAS and no quota should never see a
// meter for a number that will never bind, so the server reports whether the
// question is even live rather than leaving the client to guess.
func TestLibrarySpaceConfiguredOnlyWithAFloorOrUploads(t *testing.T) {
	srv, _, _ := newTestServer(t)

	var sp map[string]any
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/space", nil, &sp); code != http.StatusOK {
		t.Fatalf("space = %d", code)
	}
	if on, _ := sp["configured"].(bool); on {
		t.Error("configured=true on a bridge with neither uploads nor a floor")
	}
	if probed, _ := sp["probed"].(bool); !probed {
		t.Error("probed=false — the free-space probe failed on a temp dir")
	}

	cfg := config.Clone(srv.deps.CfgHolder.Load())
	cfg.Upload.Enabled = true
	srv.deps.CfgHolder.Store(cfg)
	doJSON(t, srv.Handler(), "GET", "/api/library/space", nil, &sp)
	if on, _ := sp["configured"].(bool); !on {
		t.Error("configured=false with uploads enabled")
	}

	cfg = config.Clone(srv.deps.CfgHolder.Load())
	cfg.Upload.Enabled = false
	cfg.Upload.MinFreeBytes = 1 << 30
	srv.deps.CfgHolder.Store(cfg)
	doJSON(t, srv.Handler(), "GET", "/api/library/space", nil, &sp)
	if on, _ := sp["configured"].(bool); !on {
		t.Error("configured=false with a free-space floor set")
	}
}

// TestLibrarySpaceReportsLibraryBytes — every row counts, suppressed ones
// included: a duplicate-suppressed copy still occupies the volume, which is
// exactly the number an operator deciding what to delete needs.
func TestLibrarySpaceReportsLibraryBytes(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for i, sz := range []int64{1000, 2500} {
		if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{
			Path: string(rune('a'+i)) + ".flac", Size: sz,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var sp map[string]any
	doJSON(t, srv.Handler(), "GET", "/api/library/space", nil, &sp)
	if got, _ := sp["libraryBytes"].(float64); int64(got) != 3500 {
		t.Errorf("libraryBytes = %v, want 3500", sp["libraryBytes"])
	}
}
