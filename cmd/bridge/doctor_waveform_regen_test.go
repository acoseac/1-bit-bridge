package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// doctorRegenInstall writes a bridge.yaml over a regenFixture's library and
// data directory, analysis on, and returns its path.
func doctorRegenInstall(t *testing.T, f *regenFixture) string {
	t.Helper()
	root := filepath.Dir(filepath.Dir(f.dbPath))
	cfgPath := filepath.Join(root, "bridge.yaml")
	body := "libraryRoots:\n  - " + f.lib + "\ndataDir: " + filepath.Dir(f.dbPath) +
		"\nadminAddress: 127.0.0.1:" + closedLoopbackPort(t) + "\nanalysis:\n  enabled: true\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func waveformRegenLine(t *testing.T, cfgPath string) doctor.Check {
	t.Helper()
	d := buildDoctorDeps(cfgPath)
	if d.WaveformRegeneration == nil {
		t.Fatal("the waveform-regeneration probe is not wired: the line would say 'run after the first scan' forever")
	}
	return findCheck(t, doctor.Run(context.Background(), d), "waveform-regeneration")
}

// TestTheDoctorReportsWhatTheRegenerationGuardJudges drives buildDoctorDeps
// and doctor.Run over a real store and waveform directory, the probe both
// `bridge doctor` and the console's doctor read: the curves proven missing,
// a refusal in the guard's own terms, and the curves whose regeneration
// stopped under this build, each apart from the relocated ones.
func TestTheDoctorReportsWhatTheRegenerationGuardJudges(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		f := newRegenFixture(t, 3)
		if c := waveformRegenLine(t, doctorRegenInstall(t, f)); c.Status != doctor.OK {
			t.Errorf("a healthy catalog: %+v, want ok", c)
		}
	})
	t.Run("a curve deleted, and one relocated", func(t *testing.T) {
		f := newRegenFixture(t, 3)
		f.deleteCurve(t, f.tracks[0])
		if err := f.store.UpdateAnalysisWaveformPath(f.ctx, f.tracks[1], filepath.Join(t.TempDir(), "old", "x.waveform.bin")); err != nil {
			t.Fatal(err)
		}
		c := waveformRegenLine(t, doctorRegenInstall(t, f))
		if c.Status != doctor.Warn || !strings.Contains(c.Summary, "1 curve(s) proven missing") ||
			!strings.Contains(c.Summary, "1 relocated") || !strings.Contains(c.Hint, "rendered again, waveform only") {
			t.Errorf("got %+v, want the missing curve and the relocated one apart", c)
		}
	})
	t.Run("the directory gone", func(t *testing.T) {
		f := newRegenFixture(t, 3)
		if err := os.RemoveAll(f.dir); err != nil {
			t.Fatal(err)
		}
		c := waveformRegenLine(t, doctorRegenInstall(t, f))
		// The whole line: a refusal at the probe looked at no row, and claims
		// no budget's scope (round 2).
		if c.Status != doctor.Warn || c.Summary != "regeneration refused: the waveform directory is gone" {
			t.Errorf("got %+v, want the refusal said, and nothing else", c)
		}
	})
	t.Run("emptied", func(t *testing.T) {
		f := newRegenFixture(t, 30)
		for _, rel := range f.tracks[:12] {
			f.deleteCurve(t, rel)
		}
		c := waveformRegenLine(t, doctorRegenInstall(t, f))
		if c.Status != doctor.Warn || !strings.Contains(c.Summary, "more than a fifth of the curves are gone") ||
			!strings.Contains(c.Hint, "12 of 30 rows") {
			t.Errorf("got %+v, want the emptied refusal with the guard's numbers", c)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		f := newRegenFixture(t, 3)
		rel := f.tracks[0]
		f.deleteCurve(t, rel)
		row := f.row(t, rel)
		for i := 0; i < 2; i++ {
			if _, err := f.store.RecordWaveformRegenFailure(f.ctx, rel, waveformRegenBuild(), row.SourceMTimeNS, row.SourceSize, "write: disk full"); err != nil {
				t.Fatal(err)
			}
		}
		c := waveformRegenLine(t, doctorRegenInstall(t, f))
		if c.Status != doctor.Warn || !strings.Contains(c.Summary, "1 with regeneration stopped") || !strings.Contains(c.Hint, rel) {
			t.Errorf("got %+v, want the stopped curve named", c)
		}
	})
}
