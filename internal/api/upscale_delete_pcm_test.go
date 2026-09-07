package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestUpscaleDelete_kindNarrowsToPCM: `kind=pcm` deletes exactly the
// `pcm-` rows, and — the half that matters for a mixed store — the
// optimize kind still leaves them alone while sweeping BOTH optimized
// families (the DSD compact tier rides `optimized-dsd-`).
func TestUpscaleDelete_kindNarrowsToPCM(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	seedMixedKindFixture(deleter)
	deleter.all = append(deleter.all,
		VariantSummary{SourcePath: "Music/DSD/01.dsf", VariantID: "pcm-v1-176400-24",
			SidecarPath: "/tmp/p1", SizeBytes: 5000},
		VariantSummary{SourcePath: "Music/DSD/01.dsf", VariantID: "optimized-dsd-v1-44100-16",
			SidecarPath: "/tmp/od1", SizeBytes: 400},
	)

	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true&kind=pcm", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 1 {
		t.Errorf("deletedCount: got %d, want 1 (only the pcm- row)", dr.DeletedCount)
	}
	got := deleter.deletedKeys()
	if len(got) != 1 || got[0] != "Music/DSD/01.dsf|pcm-v1-176400-24" {
		t.Errorf("DeleteVariant calls: got %v, want [Music/DSD/01.dsf|pcm-v1-176400-24]", got)
	}
}

func TestUpscaleDelete_kindOptimizeSweepsBothOptimizedFamiliesNotPCM(t *testing.T) {
	hs, raw, deleter, _ := deleteFixture(t, true)
	seedMixedKindFixture(deleter)
	deleter.all = append(deleter.all,
		VariantSummary{SourcePath: "Music/DSD/01.dsf", VariantID: "pcm-v1-176400-24",
			SidecarPath: "/tmp/p1", SizeBytes: 5000},
		VariantSummary{SourcePath: "Music/DSD/01.dsf", VariantID: "optimized-dsd-v1-44100-16",
			SidecarPath: "/tmp/od1", SizeBytes: 400},
	)
	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true&kind=optimize", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	dr := decodeDeleteResponse(t, resp)
	if dr.DeletedCount != 3 {
		t.Errorf("deletedCount: got %d, want 3 (two optimized-v2 rows + the optimized-dsd one)", dr.DeletedCount)
	}
	for _, key := range deleter.deletedKeys() {
		if !strings.Contains(key, "optimized-") {
			t.Errorf("unexpected deletion of a non-optimize variant: %s", key)
		}
	}
}

func TestUpscaleDelete_kindPCMUnknownVariantSpellingRejected(t *testing.T) {
	hs, raw, _, _ := deleteFixture(t, true)
	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=true&kind=pcm-render", raw)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 for an unknown kind spelling", resp.StatusCode)
	}
}
