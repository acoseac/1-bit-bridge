package admin

import (
	"os"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

type fakeInfo struct {
	os.FileInfo
	size int64
	mod  time.Time
}

func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) ModTime() time.Time { return f.mod }

func freshRow(id string) manifest.VariantRow {
	return manifest.VariantRow{
		VariantID: id, Format: "flac",
		SourceMTimeNS: 1_000_000_000, SourceSize: 4096,
	}
}

var pcmTestInfo = fakeInfo{size: 4096, mod: time.Unix(1, 0)}

// TestPickPlayableVariantFallsBackToTheFaithfulTier.
//
// A DSD source cannot be played by any browser, so its rendition IS the only
// playable copy — and with the compact tier off, the only rendition it has is
// `pcm-`. pickPlayableVariant switched on `optimized-` / `upscaled-` only, so
// the player reported the track unplayable with a perfectly playable FLAC
// sidecar sitting right beside it: the single case the faithful tier exists
// for.
//
// (`optimized-` already covers `optimized-dsd-` by prefix, which is why only
// this one family was missing.)
func TestPickPlayableVariantFallsBackToTheFaithfulTier(t *testing.T) {
	got := pickPlayableVariant([]manifest.VariantRow{freshRow("pcm-v1-176400-24")}, pcmTestInfo)
	if got == nil {
		t.Fatal("a fresh pcm- rendition was reported unplayable; it is the only sidecar a DSD source has when the compact tier is off")
	}
	if got.VariantID != "pcm-v1-176400-24" {
		t.Errorf("picked %q", got.VariantID)
	}
}

// TestPickPlayableVariantPrefersBandwidthOrder pins the ORDER, which is the
// half a bare "pcm is now pickable" test would not catch. The web player is
// explicitly not a bit-exact path: optimized is the small CarPlay-floor copy,
// upscaled can be 176.4/24, and pcm is always 24-bit at the source's own
// family rate — so the faithful tier must be LAST, never a fidelity upgrade.
func TestPickPlayableVariantPrefersBandwidthOrder(t *testing.T) {
	all := []manifest.VariantRow{
		freshRow("pcm-v1-176400-24"),
		freshRow("upscaled-v2-96000-24"),
		freshRow("optimized-v2-44100-16"),
	}
	if got := pickPlayableVariant(all, pcmTestInfo); got == nil || got.VariantID != "optimized-v2-44100-16" {
		t.Errorf("with all three present picked %v, want the optimized copy", got)
	}
	if got := pickPlayableVariant(all[:2], pcmTestInfo); got == nil || got.VariantID != "upscaled-v2-96000-24" {
		t.Errorf("with pcm+upscaled picked %v, want the upscaled copy", got)
	}
}

// TestPickPlayableVariantStillRejectsStaleAndNonFLAC is the negative control:
// the new arm must not become a way past the freshness and format gates.
func TestPickPlayableVariantStillRejectsStaleAndNonFLAC(t *testing.T) {
	stale := freshRow("pcm-v1-176400-24")
	stale.SourceSize = 999
	if got := pickPlayableVariant([]manifest.VariantRow{stale}, pcmTestInfo); got != nil {
		t.Errorf("a STALE pcm rendition was picked: %v", got.VariantID)
	}
	wav := freshRow("pcm-v1-176400-24")
	wav.Format = "wav"
	if got := pickPlayableVariant([]manifest.VariantRow{wav}, pcmTestInfo); got != nil {
		t.Errorf("a non-FLAC pcm rendition was picked: %v", got.VariantID)
	}
	if got := pickPlayableVariant(nil, pcmTestInfo); got != nil {
		t.Errorf("picked something from an empty set: %v", got.VariantID)
	}
}
