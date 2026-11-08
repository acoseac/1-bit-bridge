package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func waveformRegenerationCheck(t *testing.T, d Deps) Check {
	t.Helper()
	c := checkWaveformRegeneration(t.Context(), d)
	if c.Name != checkNameWaveformRegeneration {
		t.Fatalf("check name = %q, want %q", c.Name, checkNameWaveformRegeneration)
	}
	return c
}

func regenProbe(r WaveformRegeneration, err error) func(context.Context) (WaveformRegeneration, error) {
	return func(context.Context) (WaveformRegeneration, error) { return r, err }
}

// TestTheWaveformRegenerationLineSaysEachStateApart: proven-missing curves,
// relocated ones, refused regenerations by their kind and stopped ones are
// each worded apart, and a relocated curve is never reported missing.
func TestTheWaveformRegenerationLineSaysEachStateApart(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		c := waveformRegenerationCheck(t, Deps{AnalysisEnabled: true,
			WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 12, Looked: 12, WaveformDir: "/d/waveforms"}, nil)})
		if c.Status != OK || c.Hint != "" {
			t.Fatalf("got %+v, want ok", c)
		}
	})
	t.Run("relocated only", func(t *testing.T) {
		c := waveformRegenerationCheck(t, Deps{AnalysisEnabled: true,
			WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 12, Looked: 12, Relocated: 3}, nil)})
		if c.Status != OK || !strings.Contains(c.Summary, "3 relocated") || strings.Contains(c.Summary, "missing") {
			t.Fatalf("got %+v, want ok naming the relocated curves and no missing one", c)
		}
	})
	t.Run("proven missing", func(t *testing.T) {
		c := waveformRegenerationCheck(t, Deps{AnalysisEnabled: true,
			WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 12, Looked: 12, Missing: 2, Mismatched: 1, Relocated: 1}, nil)})
		for _, want := range []string{"3 curve(s) proven missing", "2 at neither location", "1 a partial copy", "1 relocated"} {
			if !strings.Contains(c.Summary, want) {
				t.Errorf("summary %q lacks %q", c.Summary, want)
			}
		}
		if c.Status != Warn || !strings.Contains(c.Hint, "rendered again, waveform only") {
			t.Errorf("got %+v, want a warn saying each is rendered again on its next lookup", c)
		}
	})
	t.Run("proven missing with analysis off", func(t *testing.T) {
		c := waveformRegenerationCheck(t, Deps{
			WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 12, Looked: 12, Missing: 2}, nil)})
		if c.Status != Warn || !strings.Contains(c.Hint, "Analysis is off") {
			t.Errorf("got %+v, want a warn saying nothing renders them while analysis is off", c)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		c := waveformRegenerationCheck(t, Deps{AnalysisEnabled: true,
			WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 12, Looked: 12, Missing: 7, Stopped: 7,
				StoppedSample: []string{"A/1.flac", "A/2.flac", "A/3.flac", "A/4.flac", "A/5.flac"}}, nil)})
		for _, want := range []string{"7 with regeneration stopped"} {
			if !strings.Contains(c.Summary, want) {
				t.Errorf("summary %q lacks %q", c.Summary, want)
			}
		}
		for _, want := range []string{"Regeneration stopped for 7", "new build or a changed source", "A/1.flac", "(+2 more)"} {
			if !strings.Contains(c.Hint, want) {
				t.Errorf("hint %q lacks %q", c.Hint, want)
			}
		}
	})
	t.Run("undecided", func(t *testing.T) {
		c := waveformRegenerationCheck(t, Deps{AnalysisEnabled: true,
			WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 90000, Looked: 20000, Missing: 4, Undecided: true}, nil)})
		want := "4 curve(s) proven missing (4 at neither location, 0 a partial copy) (the first 20000 of 90000 row(s))"
		if c.Status != Warn || c.Summary != want || !strings.Contains(c.Hint, "cannot be told") {
			t.Errorf("got %+v, want a warn scoped to the rows looked at, %q, that claims no verdict", c, want)
		}
	})
	t.Run("a refusal before any row was looked at claims no scope", func(t *testing.T) {
		// The probe's refusals classify no row: Looked 0 is no budget.
		c := waveformRegenerationCheck(t, Deps{AnalysisEnabled: true,
			WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 3, Looked: 0,
				Refusal: waveformRefusalDirMissing, Reason: "waveform directory is missing"}, nil)})
		if c.Status != Warn || c.Summary != "regeneration refused: the waveform directory is gone" {
			t.Errorf("summary %q, want %q alone", c.Summary, "regeneration refused: the waveform directory is gone")
		}
	})
	for _, kind := range WaveformRefusalKinds() {
		t.Run("refused: "+kind, func(t *testing.T) {
			c := waveformRegenerationCheck(t, Deps{AnalysisEnabled: true,
				WaveformRegeneration: regenProbe(WaveformRegeneration{Rows: 12, Looked: 12, Missing: 12,
					Refusal: kind, Reason: "the guard's own words"}, nil)})
			if c.Status != Warn || !strings.HasPrefix(c.Summary, "regeneration refused: ") ||
				strings.Contains(c.Summary, kind) || !strings.Contains(c.Hint, "the guard's own words") ||
				!strings.Contains(c.Hint, "No curve is rendered again") {
				t.Errorf("got %+v, want a warn worded for %s, carrying the guard's reason", c, kind)
			}
		})
	}
	t.Run("probe failure is not an all clear", func(t *testing.T) {
		c := waveformRegenerationCheck(t, Deps{WaveformRegeneration: regenProbe(WaveformRegeneration{}, errors.New("database is locked"))})
		if c.Status != Warn || !strings.Contains(c.Hint, "database is locked") {
			t.Errorf("got %+v, want a warn carrying the error", c)
		}
	})
	t.Run("no probe, or a managed bridge", func(t *testing.T) {
		if c := waveformRegenerationCheck(t, Deps{}); c.Status != OK || !strings.Contains(c.Summary, "no manifest") {
			t.Errorf("no probe: %+v", c)
		}
		probed := false
		c := waveformRegenerationCheck(t, Deps{Managed: true, WaveformRegeneration: func(context.Context) (WaveformRegeneration, error) {
			probed = true
			return WaveformRegeneration{Rows: 1, Missing: 1}, nil
		}})
		if c.Status != OK || !strings.Contains(c.Summary, "skipped") || probed {
			t.Errorf("managed: %+v (probed %t), want skipped without a probe", c, probed)
		}
	})
}
