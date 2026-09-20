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
// names each table's directory and its remedy — the waveform half being
// the one with no automatic relocation yet).
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
			"410", "bridge analyze --force",
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
