package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUpscaleDelete_pathFreesRealSidecarBytes is the B15 positive control:
// when the sidecar actually exists on disk, its bytes ARE counted as freed
// (the sibling happy-path test covers the already-absent case → 0 freed).
func TestUpscaleDelete_pathFreesRealSidecarBytes(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)

	sidecar := filepath.Join(t.TempDir(), "abc-v1.flac")
	if err := os.WriteFile(sidecar, make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{{
		SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: sidecar, SizeBytes: 100,
	}}

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	dr := decodeDeleteResponse(t, resp)
	if dr.FreedBytes != 100 {
		t.Errorf("freedBytes: got %d, want 100 (a real sidecar was unlinked)", dr.FreedBytes)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("sidecar not unlinked: stat err = %v", err)
	}
}

// TestUpscaleDelete_emptySidecarPathStillDeletesRow guards the Gemini PR #518
// fix: a row with an empty SidecarPath (a legacy row whose sidecar path was
// never recorded) must still have its DB row deleted. os.Remove("") returns a
// non-ErrNotExist error on Windows, which the pre-fix code treated as an unlink
// failure and `continue`d past — stranding the row forever. It frees 0 bytes.
func TestUpscaleDelete_emptySidecarPathStillDeletesRow(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)

	deleter.byPath["Music/Album/01.flac"] = []VariantSummary{{
		SourcePath: "Music/Album/01.flac", VariantID: "v1", SidecarPath: "", SizeBytes: 100,
	}}

	resp := authDelete(t, hs, "/v1/upscale/variants?path=Music/Album/01.flac", raw)
	defer resp.Body.Close()
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 1 {
		t.Errorf("deletedCount: got %d, want 1 (empty-sidecar row must still be deleted, not stranded)", dr.DeletedCount)
	}
	if dr.FreedBytes != 0 {
		t.Errorf("freedBytes: got %d, want 0 (no sidecar on disk)", dr.FreedBytes)
	}
	if got := deleter.deletedKeys(); len(got) != 1 || got[0] != "Music/Album/01.flac|v1" {
		t.Errorf("DeleteVariant calls: got %v, want [Music/Album/01.flac|v1]", got)
	}
}
