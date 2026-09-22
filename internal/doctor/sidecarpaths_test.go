package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCheckSidecarPaths pins the four answers: no probe wired (skip),
// a probe that fails (warn — "don't know" is not "fine"), everything
// under its directory (ok), and rows outside it (warn, with a hint that
// names each table's directory and its remedy).
func TestCheckSidecarPaths(t *testing.T) {
	ctx := context.Background()

	t.Run("no probe skips", func(t *testing.T) {
		c := checkSidecarPaths(ctx, Deps{})
		if c.Status != OK || !strings.Contains(c.Summary, "no manifest") {
			t.Fatalf("got %+v, want ok/no manifest", c)
		}
	})
	t.Run("probe error warns rather than answering ok", func(t *testing.T) {
		c := checkSidecarPaths(ctx, Deps{RelocatedSidecars: func(context.Context) (RelocatedSidecars, error) {
			return RelocatedSidecars{}, errors.New("database is locked")
		}})
		if c.Status != Warn || !strings.Contains(c.Hint, "database is locked") {
			t.Fatalf("got %+v, want warn carrying the error", c)
		}
	})
	t.Run("everything under its directory is ok", func(t *testing.T) {
		c := checkSidecarPaths(ctx, Deps{RelocatedSidecars: func(context.Context) (RelocatedSidecars, error) {
			return RelocatedSidecars{VariantsDir: "/srv/variants", WaveformDir: "/var/lib/bridge/waveforms"}, nil
		}})
		if c.Status != OK || c.Hint != "" {
			t.Fatalf("got %+v, want ok with no hint", c)
		}
	})
	t.Run("the 2026-09-20 shape warns and names both remedies", func(t *testing.T) {
		c := checkSidecarPaths(ctx, Deps{RelocatedSidecars: func(context.Context) (RelocatedSidecars, error) {
			return RelocatedSidecars{
				Variants: 10248, VariantBytes: 278_836_000_000, Waveforms: 9_000,
				VariantsDir: "/srv/bridge-variants", WaveformDir: "/var/lib/bridge/waveforms",
			}, nil
		}})
		if c.Status != Warn {
			t.Fatalf("got %+v, want warn", c)
		}
		for _, want := range []string{
			"10248 variant row(s)", "9000 waveform row(s)",
			"/srv/bridge-variants", "/var/lib/bridge/waveforms",
			"bridge variants move --to /srv/bridge-variants", "bridge upscale --gc",
			// The waveform half names adoption FIRST and keeps
			// `--force` for the case where it is genuinely the
			// remedy — a curve at neither location.
			"adopted on the first analysis lookup", "bridge analyze --force",
		} {
			if !strings.Contains(c.Summary+" "+c.Hint, want) {
				t.Errorf("report lacks %q:\nsummary: %s\nhint: %s", want, c.Summary, c.Hint)
			}
		}
	})
	t.Run("variants only leaves the waveform advice out", func(t *testing.T) {
		c := checkSidecarPaths(ctx, Deps{RelocatedSidecars: func(context.Context) (RelocatedSidecars, error) {
			return RelocatedSidecars{Variants: 3, VariantsDir: "/srv/v", WaveformDir: "/d/waveforms"}, nil
		}})
		if c.Status != Warn || strings.Contains(c.Hint, "analyze") {
			t.Fatalf("got %+v, want a warn with no waveform advice", c)
		}
	})
	t.Run("managed skips and carries no advice", func(t *testing.T) {
		probed := false
		c := checkSidecarPaths(ctx, Deps{Managed: true, RelocatedSidecars: func(context.Context) (RelocatedSidecars, error) {
			probed = true
			return RelocatedSidecars{Variants: 5}, nil
		}})
		if c.Status != OK || !strings.Contains(c.Summary, "skipped") || c.Hint != "" {
			t.Fatalf("got %+v, want ok/skipped with no hint", c)
		}
		if probed {
			t.Error("a managed bridge should not open the database for a check it will not report")
		}
	})
}

// TestWaveformHintDoesNotPrescribeAFullReDecodeForARelocation.
//
// #954 wired integrity.LocateWaveform into analysisStoreAdapter, so a
// relocated curve rebinds to its row on the first analysis lookup and
// nothing is re-decoded. The hint went on telling the operator that "no
// relocation exists for waveforms yet" and to run `bridge analyze
// --force` — hours of sox/ffmpeg across the library to recover files
// that are already on disk, for a state that heals on the next play.
//
// The prior version of TestCheckSidecarPaths asserted that exact
// sentence, so the suite PINNED the stale advice: this guard exists
// because a want-list can encode the bug as easily as the fix.
//
// Anchored on the CLAIM, not on the command — `--force` is still the
// right answer for a curve at neither location, and a test that banned
// the word would forbid saying so.
func TestWaveformHintDoesNotPrescribeAFullReDecodeForARelocation(t *testing.T) {
	c := checkSidecarPaths(context.Background(), Deps{
		RelocatedSidecars: func(context.Context) (RelocatedSidecars, error) {
			return RelocatedSidecars{Waveforms: 9000, WaveformDir: "/var/lib/bridge/waveforms"}, nil
		}})
	if c.Status != Warn {
		t.Fatalf("got %+v, want warn", c)
	}
	for _, stale := range []string{
		"No relocation exists for waveforms yet",
		"no relocation exists",
		"answers 410 on /v1/waveform and the analysis sweeper will not regenerate",
	} {
		if strings.Contains(c.Hint, stale) {
			t.Errorf("the hint still claims %q — adoption has shipped (#954):\n%s", stale, c.Hint)
		}
	}
	if !strings.Contains(c.Hint, "adopted on the first analysis lookup") {
		t.Errorf("the hint does not tell the operator the rows heal by themselves:\n%s", c.Hint)
	}
}
