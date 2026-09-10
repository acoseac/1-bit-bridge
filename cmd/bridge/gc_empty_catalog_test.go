package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The forward sweep is the one that deletes FILES, and it runs first — so by
// the time gcCheckOutputDirBeforeReverseSweep fires on the row side, the
// sidecars are already gone. These pin the direction that was missing.

func TestGCRefusesAnEmptyCatalogOverAPopulatedVariantsDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.upscaled-v1-96000-24.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := gcRefuseEmptyKnownSetOverPopulatedDir(&stderr, dir, 0, false); code == 0 {
		t.Fatal("an empty catalog over a populated variants dir was allowed to sweep")
	}
	// The message has to name the way OUT, or the operator whose library
	// really is empty has a refusal and nothing to do about it.
	if !strings.Contains(stderr.String(), "--allow-empty") {
		t.Errorf("refusal does not name the override:\n%s", stderr.String())
	}
	// ...and the likely CAUSE, which is a --config naming another install.
	if !strings.Contains(stderr.String(), "--config") {
		t.Errorf("refusal does not name the likely cause:\n%s", stderr.String())
	}
}

func TestGCEmptyCatalogGuardLetsTheDeliberateCaseThrough(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.upscaled-v1-96000-24.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if code := gcRefuseEmptyKnownSetOverPopulatedDir(&stderr, dir, 0, true); code != 0 {
		t.Errorf("--allow-empty was refused anyway (exit %d): %s", code, stderr.String())
	}
	// An empty catalog over an EMPTY dir is a bridge that never transcoded
	// anything, not the hazard — it must stay a silent pass.
	stderr.Reset()
	if code := gcRefuseEmptyKnownSetOverPopulatedDir(&stderr, t.TempDir(), 0, false); code != 0 {
		t.Errorf("an empty catalog over an empty dir was refused (exit %d)", code)
	}
	// And a populated catalog is never the guard's business.
	stderr.Reset()
	if code := gcRefuseEmptyKnownSetOverPopulatedDir(&stderr, dir, 7, false); code != 0 {
		t.Errorf("a populated catalog was refused (exit %d)", code)
	}
}

// TestEveryGCCommandOffersTheEmptyCatalogOverride is the sweep, not the
// symptom. Three commands reach runGC and a fourth reaches runAnalyzeGC; the
// refusal is only usable if each of them offers the flag that lifts it, and a
// fifth `--gc` added later must not quietly ship a refusal with no way out.
//
// Anchored on the flag NAME in an fs.Bool call, and gated on a floor: a scan
// that stops matching reports no problems, which is the outcome that hides the
// drift. (stripGoComments would blank the string literals this looks for, so
// the scan is deliberately raw — a `--gc` mentioned in prose cannot satisfy
// `fs.Bool("gc"`.)
func TestEveryGCCommandOffersTheEmptyCatalogOverride(t *testing.T) {
	gcRe := regexp.MustCompile(`fs\.Bool\("gc"`)
	allowRe := regexp.MustCompile(`fs\.Bool\("allow-empty"`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		// CRLF-normalised: nothing pins eol, so a Windows checkout would
		// otherwise make every literal scan in this tree find nothing.
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if !gcRe.MatchString(src) {
			continue
		}
		checked++
		if !allowRe.MatchString(src) {
			t.Errorf("%s declares --gc but not --allow-empty: an operator whose library "+
				"really is empty gets a refusal with no way past it", name)
		}
	}
	if checked < 4 {
		t.Fatalf("only %d --gc commands found — the scan is broken, so this test "+
			"is not checking anything", checked)
	}
}
