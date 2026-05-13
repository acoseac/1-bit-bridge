package manifest

import (
	"path/filepath"
	"testing"
)

// TestComposerConductor_ID3v2_SurfaceFromTCOMTPE3 pins the
// classical-metadata pickup from ID3v2 frames. dhowden's
// `m.Composer()` is mapped to TCOM directly; TPE3 (conductor) and
// TIT1 (work) require raw-map lookup via `stringOf` since dhowden
// doesn't surface them as first-class methods. The PR-D wiring
// runs both reads inside populateFromTagMetadata.
func TestComposerConductor_ID3v2_SurfaceFromTCOMTPE3(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "classical.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title":     "Allegro con brio",
		"artist":    "Vienna Philharmonic",
		"composer":  "Ludwig van Beethoven",
		"conductor": "Herbert von Karajan",
		"work":      "Symphony No. 5 in C minor, Op. 67",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Composer != "Ludwig van Beethoven" {
		t.Errorf("Composer = %q, want %q", track.Composer, "Ludwig van Beethoven")
	}
	if track.Conductor != "Herbert von Karajan" {
		t.Errorf("Conductor = %q, want %q", track.Conductor, "Herbert von Karajan")
	}
	if track.Work != "Symphony No. 5 in C minor, Op. 67" {
		t.Errorf("Work = %q, want %q", track.Work, "Symphony No. 5 in C minor, Op. 67")
	}
	// Title (TIT2) stays mapped to t.Title — Work and Title coexist.
	if track.Title != "Allegro con brio" {
		t.Errorf("Title = %q, want %q (TIT2 must remain mapped to Title)", track.Title, "Allegro con brio")
	}
}

// TestComposerConductor_Vorbis_SurfaceFromFLACTags: classical
// FLAC libraries commonly tag COMPOSER / CONDUCTOR / WORK in
// Vorbis Comments. The shared stringOf-based pickup must work
// for these too.
func TestComposerConductor_Vorbis_SurfaceFromFLACTags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "classical.flac")
	writeMinimalFLAC(t, path, 44100, 16, map[string]string{
		"TITLE":     "Movement I",
		"ARTIST":    "Berlin Philharmonic",
		"COMPOSER":  "Wolfgang Amadeus Mozart",
		"CONDUCTOR": "Karl Böhm",
		"WORK":      "Requiem in D minor, K. 626",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Composer != "Wolfgang Amadeus Mozart" {
		t.Errorf("Composer = %q, want %q", track.Composer, "Wolfgang Amadeus Mozart")
	}
	if track.Conductor != "Karl Böhm" {
		t.Errorf("Conductor = %q, want %q", track.Conductor, "Karl Böhm")
	}
	if track.Work != "Requiem in D minor, K. 626" {
		t.Errorf("Work = %q, want %q", track.Work, "Requiem in D minor, K. 626")
	}
}

// TestOriginalYear_ID3v2_ParsesFromTORY pins the OriginalYear
// pickup from ID3v2.3 TORY (4-digit year) and verifies it's
// distinct from Year (which holds the pressing / remaster year
// for reissues).
func TestOriginalYear_ID3v2_ParsesFromTORY(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "reissue.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title":        "Reissued Track",
		"year":         "2020", // remaster year
		"originalYear": "1974", // original release year
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Year == nil || *track.Year != 2020 {
		t.Errorf("Year = %v, want pointer to 2020 (remaster)", track.Year)
	}
	if track.OriginalYear == nil || *track.OriginalYear != 1974 {
		t.Errorf("OriginalYear = %v, want pointer to 1974 (TORY)", track.OriginalYear)
	}
}

// TestOriginalYear_AbsentReturnsNil: when no TORY/TDOR/ORIGINAL*
// tag is present, OriginalYear stays nil (the presence-gate from
// PR-B contract).
func TestOriginalYear_AbsentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no_original.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title": "Modern Track",
		"year":  "2024",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.OriginalYear != nil {
		t.Errorf("OriginalYear = %v, want nil (no TORY frame)", *track.OriginalYear)
	}
}

// TestBPM_ID3v2_ParsesFromTBPM locks the BPM field pickup.
func TestBPM_ID3v2_ParsesFromTBPM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "danceable.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title": "Tempo Test",
		"bpm":   "128",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.BPM == nil || *track.BPM != 128 {
		t.Errorf("BPM = %v, want pointer to 128", track.BPM)
	}
}

// TestBPM_AbsentReturnsNil: no TBPM frame → BPM stays nil.
func TestBPM_AbsentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no_bpm.mp3")
	writeMinimalMP3(t, path, map[string]string{"title": "Slow Track"})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.BPM != nil {
		t.Errorf("BPM = %v, want nil", *track.BPM)
	}
}

// TestParseYearPrefix_TruthTable pins the helper used to extract
// a 4-digit year from both bare ("1985") and ISO-8601 ("1985-06-22")
// originals.
func TestParseYearPrefix_TruthTable(t *testing.T) {
	cases := []struct {
		in       string
		want     int
		wantErr  bool
		whyValid string
	}{
		{"1985", 1985, false, "bare YYYY"},
		{"1985-06-22", 1985, false, "ISO-8601 date"},
		{"1985-06", 1985, false, "ISO-8601 year-month"},
		{"0001", 1, false, "year 1 boundary"},
		{"9999", 9999, false, "year 9999 boundary"},
		{"abc", 0, true, "non-numeric prefix"},
		{"19", 0, true, "too short"},
		{"", 0, true, "empty"},
		{"0000", 0, true, "year 0 out of range"},
		{"10000-01-01", 1000, false, "prefix-only parse returns first-four-chars 1000 — documents the by-design behavior"},
	}
	for _, c := range cases {
		got, err := parseYearPrefix(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseYearPrefix(%q) = (%d, nil), want error (%s)", c.in, got, c.whyValid)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseYearPrefix(%q) error = %v, want nil (%s)", c.in, err, c.whyValid)
			continue
		}
		if got != c.want {
			t.Errorf("parseYearPrefix(%q) = %d, want %d (%s)", c.in, got, c.want, c.whyValid)
		}
	}
}

// TestComposerConductor_AllAbsent: a file with no classical
// metadata frames produces empty Composer / Conductor / Work
// strings AND nil OriginalYear / BPM pointers — the absent-vs-
// zero discipline PR-B + PR-D maintain.
func TestComposerConductor_AllAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.mp3")
	writeMinimalMP3(t, path, map[string]string{
		"title": "Plain Track",
		"year":  "2024",
	})
	track := &Track{}
	if err := Extract(path, track); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if track.Composer != "" {
		t.Errorf("Composer = %q, want empty", track.Composer)
	}
	if track.Conductor != "" {
		t.Errorf("Conductor = %q, want empty", track.Conductor)
	}
	if track.Work != "" {
		t.Errorf("Work = %q, want empty", track.Work)
	}
	if track.OriginalYear != nil {
		t.Errorf("OriginalYear = %v, want nil", *track.OriginalYear)
	}
	if track.BPM != nil {
		t.Errorf("BPM = %v, want nil", *track.BPM)
	}
}
