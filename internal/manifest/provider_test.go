package manifest

import "testing"

// TestUpscaleGateSourceSupersedesTheStoredFlag — the stored atomic is
// written once at startup, so after a settings PATCH it would keep
// stripping (or keep emitting) variants against a value the rest of the
// bridge had already moved past: a client's manifest disagreeing with
// /v1/health about whether the feature is on.
func TestUpscaleGateSourceSupersedesTheStoredFlag(t *testing.T) {
	p := &Provider{}
	p.SetUpscaleEnabled(false)
	if p.upscaleGate() {
		t.Fatal("stored false read as true")
	}

	live := true
	p.SetUpscaleEnabledSource(func() bool { return live })
	if !p.upscaleGate() {
		t.Error("the live source must supersede the stored flag")
	}
	live = false
	if p.upscaleGate() {
		t.Error("the live source must be re-read, not captured")
	}

	// Clearing it restores the stored value — what every caller that only
	// ever calls SetUpscaleEnabled (tests, the CLI) keeps getting.
	p.SetUpscaleEnabled(true)
	p.SetUpscaleEnabledSource(nil)
	if !p.upscaleGate() {
		t.Error("clearing the source must fall back to the stored flag")
	}
}
