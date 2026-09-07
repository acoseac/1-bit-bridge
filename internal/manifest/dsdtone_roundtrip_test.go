package manifest

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// The dsdtone fixtures exist so the transcode render can be tested end to
// end; this pins that the bridge's OWN extractors read what the minter
// writes, so a fixture that renders is also a fixture the scanner types the
// way a real rip is typed (rate, DSD flag, channels, duration).

func TestDSDToneFixturesRoundTripThroughTheExtractors(t *testing.T) {
	dir := t.TempDir()
	tone := dsdtone.Tone{RateHz: 2822400, Seconds: 0.25, AmplitudeDBFS: -20}

	dsfPath := filepath.Join(dir, "tone.dsf")
	if _, err := dsdtone.MintDSF(dsfPath, tone); err != nil {
		t.Fatal(err)
	}
	var dsf Track
	if err := extractDSF(dsfPath, &dsf); err != nil {
		t.Fatalf("extractDSF: %v", err)
	}
	if dsf.Codec != "DSF" || dsf.SampleRate == nil || *dsf.SampleRate != 2822400 || dsf.IsDSD == nil || !*dsf.IsDSD {
		t.Errorf("dsf typed wrong: codec=%q rate=%v isDSD=%v", dsf.Codec, dsf.SampleRate, dsf.IsDSD)
	}
	if dsf.BitsPerSample == nil || *dsf.BitsPerSample != 1 {
		t.Errorf("dsf bits = %v, want 1", dsf.BitsPerSample)
	}
	if dsf.Duration == nil || math.Abs(*dsf.Duration-0.25) > 1e-6 {
		t.Errorf("dsf duration = %v, want 0.25", dsf.Duration)
	}

	dffPath := filepath.Join(dir, "tone.dff")
	if _, err := dsdtone.MintDFF(dffPath, tone); err != nil {
		t.Fatal(err)
	}
	var dff Track
	if err := extractDFFWithContext(dffPath, &dff, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if dff.Codec != "DFF" || dff.SampleRate == nil || *dff.SampleRate != 2822400 || dff.IsDSD == nil || !*dff.IsDSD {
		t.Errorf("dff typed wrong: codec=%q rate=%v isDSD=%v", dff.Codec, dff.SampleRate, dff.IsDSD)
	}
	if dff.Channels == nil || *dff.Channels != 2 {
		t.Errorf("dff channels = %v, want 2", dff.Channels)
	}
	if dff.Compression != "" {
		t.Errorf("an uncompressed DFF must carry no compression tag, got %q", dff.Compression)
	}
	if dff.Duration == nil || math.Abs(*dff.Duration-0.25) > 1e-6 {
		t.Errorf("dff duration = %v, want 0.25", dff.Duration)
	}
}
