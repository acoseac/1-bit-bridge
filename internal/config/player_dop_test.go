package config

import (
	"strings"
	"testing"
)

// TestADoPBlockTakesNoSampleTransform: a player block set for DoP (dsdMode
// dop, allowDoP true) that also asks for a setting that would change a
// sample, software or mixer volume, ReplayGain, resampling or a forced rate,
// normalization, dither, crossfade or DSD to PCM, is refused by that
// setting's name and turns the player off, so no DoP sample ever passes
// through such a transform. The same block without it is not refused for
// one.
func TestADoPBlockTakesNoSampleTransform(t *testing.T) {
	const why = "never resamples, converts, normalizes or mixes in software"
	dop := "  player:\n    enabled: true\n    dsdMode: dop\n    allowDoP: true\n"
	for _, key := range []string{"softwareVolume", "volume", "replayGain", "resample", "forceRate", "normalization", "dither", "crossfade", "dsdToPCM"} {
		t.Run(key, func(t *testing.T) {
			cfg, err := Load(writePlayerConfig(t, dop+"    "+key+": true\n"))
			if err != nil {
				t.Fatalf("a refused player block stopped the load: %v", err)
			}
			if on, reason := cfg.PlayerEnabled("linux"); on || reason == nil || !strings.Contains(reason.Error(), why) {
				t.Fatalf("PlayerEnabled = %v, %v; want off, refusing %s", on, reason, key)
			}
		})
	}
	cfg, err := Load(writePlayerConfig(t, dop))
	if err != nil {
		t.Fatal(err)
	}
	if _, reason := cfg.PlayerEnabled("linux"); reason != nil && strings.Contains(reason.Error(), why) {
		t.Fatalf("a DoP block with no transform was refused for one: %v", reason)
	}
}
