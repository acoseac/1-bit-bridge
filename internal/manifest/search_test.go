package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestBuildFTSMatchExpr pins the query-sanitisation contract.
// Each token in user input becomes a quoted FTS5 term; tokens ≥ 3
// chars get prefix-expanded with `*`. Non-alphanumeric runs split
// the input into tokens; pure-punctuation/whitespace input yields
// an empty expression so the caller can short-circuit before
// hitting SQL.
func TestBuildFTSMatchExpr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"punctuation only", ".,!?-/", ""},
		{"short token no prefix", "ab", `"ab"`},
		{"long token with prefix", "beethoven", `"beethoven"*`},
		{"three-char threshold", "abc", `"abc"*`},
		{"two-char no threshold", "ab", `"ab"`},
		{"multi-token mixed prefix", "moonlight sonata", `"moonlight"* "sonata"*`},
		{"strip dash separator — 2-char token in middle skips prefix", "track-01-foo", `"track"* "01" "foo"*`},
		{"unicode letters preserved", "Dvořák", `"Dvořák"*`},
		{"three-char digits cross threshold", "192", `"192"*`},
		{"mixed alnum unicode all ≥3 chars", "Bach BWV 1006", `"Bach"* "BWV"* "1006"*`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := buildFTSMatchExpr(c.in)
			if got != c.want {
				t.Errorf("buildFTSMatchExpr(%q): got %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSearchTracksLimitHardCapped pins the manifest-layer slice
// allocation cap (CodeQL Sec/HIGH on PR #243, hard cap added in
// PR #246). A caller-supplied `limit` above 500 must be clamped
// to 500 so the result-slice make() can't blow up memory. Gemini
// medium on PR #246 asked for regression coverage.
//
// Seeds 510 tracks and queries with limit=10000. Result must be
// exactly 500 (not 510, not 10000).
func TestSearchTracksLimitHardCapped(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 510; i++ {
		tk := Track{
			Path: filepath.Join("Album", "Foo", "track-"+itoaN(i)+".flac"),
			Size: 100, ModTime: now,
			Title:  "Foo " + itoaN(i),
			Artist: "Foo Artist",
			Album:  "Foo Album",
		}
		if err := s.UpsertTrack(ctx, &tk); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.SearchTracks(ctx, "foo", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 500 {
		t.Errorf("limit cap: got %d hits, want exactly 500", len(hits))
	}
}

// itoaN is a tiny non-allocating integer→string for test fixture
// names. strconv.Itoa pulls in the strconv import just for one
// per-test loop, and the test only needs ASCII digits.
func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	out := make([]byte, 0, 4)
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// TestSplitFTSTokens covers the lower-level tokeniser independently
// of MATCH-string construction. Catches regressions where alnum
// classification changes (e.g. switching from `unicode.IsLetter` to
// something narrower).
func TestSplitFTSTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "hello", []string{"hello"}},
		{"two words", "hello world", []string{"hello", "world"}},
		{"colons split", "Bach:BWV", []string{"Bach", "BWV"}},
		{"unicode preserved", "Dvořák symphony", []string{"Dvořák", "symphony"}},
		{"digits preserved", "192kHz audio", []string{"192kHz", "audio"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitFTSTokens(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %d tokens %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("token[%d]: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// TestUtf8RuneLen ensures the prefix-threshold check counts Unicode
// code points, not UTF-8 bytes. A single Chinese character (3 bytes)
// is structurally one token and should NOT cross the 3-char prefix
// threshold.
func TestUtf8RuneLen(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"Dvořák", 6}, // 6 runes (one with combining diacritic? No — single codepoint)
		{"中", 1},      // single CJK ideograph
		{"中国", 2},
	}
	for _, c := range cases {
		if got := utf8RuneLen(c.in); got != c.want {
			t.Errorf("utf8RuneLen(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSearchAvailableMigratedDB exercises the happy path: after a
// fresh OpenStore the FTS5 migration has landed and `tracks_fts`
// exists. SearchAvailable returns true.
func TestSearchAvailableMigratedDB(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ok, err := s.SearchAvailable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("SearchAvailable: got false on freshly-migrated DB, want true")
	}
}

// TestSearchTracksRoundTripViaTriggers writes tracks through the
// standard UpsertTrack path and confirms they're searchable. The
// FTS migration's AFTER INSERT trigger should populate tracks_fts
// from each UpsertTrack, so the search end-to-end MUST land without
// a manual backfill.
func TestSearchTracksRoundTripViaTriggers(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	fixtures := []Track{
		{Path: filepath.Join("Diana Krall", "Look of Love", "01 Love Letters.flac"),
			Size: 100, ModTime: now,
			Title: "Love Letters", Artist: "Diana Krall", Album: "The Look of Love"},
		{Path: filepath.Join("Diana Krall", "Look of Love", "02 I Remember You.flac"),
			Size: 100, ModTime: now,
			Title: "I Remember You", Artist: "Diana Krall", Album: "The Look of Love"},
		{Path: filepath.Join("Beethoven", "Symphony No 9", "01 Allegro.flac"),
			Size: 100, ModTime: now,
			Title: "Allegro", Artist: "Berliner Philharmoniker", Album: "Symphony No. 9"},
	}
	for i := range fixtures {
		if err := s.UpsertTrack(ctx, &fixtures[i]); err != nil {
			t.Fatalf("UpsertTrack[%d]: %v", i, err)
		}
	}

	// Search for "krall" → matches Artist on first two.
	hits, err := s.SearchTracks(ctx, "krall", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("artist search 'krall': got %d hits, want 2", len(hits))
	}
	for _, h := range hits {
		if h.Artist != "Diana Krall" {
			t.Errorf("hit artist = %q, want Diana Krall", h.Artist)
		}
		if h.ParentPath == "" || h.ParentPath == "." {
			t.Errorf("ParentPath should be non-empty for track under nested folders, got %q", h.ParentPath)
		}
	}

	// Search for "beet" → prefix-expands to "beet"* → matches Beethoven.
	hits, err = s.SearchTracks(ctx, "beet", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("prefix search 'beet': got %d hits, want 1", len(hits))
	}
	if hits[0].Title != "Allegro" {
		t.Errorf("got title %q, want Allegro", hits[0].Title)
	}

	// Pure punctuation → no hits, no error.
	hits, err = s.SearchTracks(ctx, "!!!", 10)
	if err != nil {
		t.Fatal(err)
	}
	if hits != nil {
		t.Errorf("punctuation-only: got %v, want nil", hits)
	}
}

// TestSearchTracksUnicodeDiacritics pins the `remove_diacritics 2`
// tokenizer config: "dvorak" (ASCII) matches "Dvořák" stored on a
// Track. This is the audiophile-collection-friendly behaviour
// flagged in the v1.4 plan.
func TestSearchTracksUnicodeDiacritics(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	tk := Track{
		Path: filepath.Join("Dvořák", "Symphony 9", "01 Allegro.flac"),
		Size: 100, ModTime: now,
		Title: "Allegro", Artist: "Antonín Dvořák", Album: "Symphony No. 9",
	}
	if err := s.UpsertTrack(ctx, &tk); err != nil {
		t.Fatal(err)
	}
	hits, err := s.SearchTracks(ctx, "dvorak", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("ascii-vs-diacritic search 'dvorak': got %d hits, want 1", len(hits))
	}
}

// TestSearchFoldersRollup confirms the per-parent-path aggregation:
// three matching tracks under one folder yield one folder hit with
// hitCount == 3.
func TestSearchFoldersRollup(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 3; i++ {
		tk := Track{
			Path: filepath.Join("Diana Krall", "Look of Love", "0"+string(rune('1'+i))+" Love.flac"),
			Size: 100, ModTime: now,
			Title: "Love Song", Artist: "Diana Krall", Album: "The Look of Love",
		}
		if err := s.UpsertTrack(ctx, &tk); err != nil {
			t.Fatal(err)
		}
	}
	folders, err := s.SearchFolders(ctx, "krall", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 {
		t.Fatalf("got %d folder hits, want 1", len(folders))
	}
	if folders[0].HitCount != 3 {
		t.Errorf("HitCount = %d, want 3", folders[0].HitCount)
	}
}

// TestSearchTracksUpdateTriggerKeepsFTSCoherent confirms the AFTER
// UPDATE trigger replaces the FTS row when a track's metadata
// changes (e.g., enrichment overwrites the title).
func TestSearchTracksUpdateTriggerKeepsFTSCoherent(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	tk := Track{
		Path: filepath.Join("Music", "Foo.flac"),
		Size: 100, ModTime: now,
		Title: "OldTitle", Artist: "Foo", Album: "Bar",
	}
	if err := s.UpsertTrack(ctx, &tk); err != nil {
		t.Fatal(err)
	}
	// Initial search finds it.
	hits, _ := s.SearchTracks(ctx, "oldtitle", 10)
	if len(hits) != 1 {
		t.Fatalf("pre-update: want 1 hit, got %d", len(hits))
	}
	// UpsertTrack again with a different title (enrichment pattern).
	tk.Title = "NewTitle"
	if err := s.UpsertTrack(ctx, &tk); err != nil {
		t.Fatal(err)
	}
	// Old title no longer matches.
	hits, _ = s.SearchTracks(ctx, "oldtitle", 10)
	if len(hits) != 0 {
		t.Errorf("post-update old title: want 0 hits, got %d", len(hits))
	}
	// New title matches.
	hits, _ = s.SearchTracks(ctx, "newtitle", 10)
	if len(hits) != 1 {
		t.Errorf("post-update new title: want 1 hit, got %d", len(hits))
	}
}

// TestSortFolderHitsOrdering pins the deterministic sort: hit count
// desc, then path length asc, then path asc.
func TestSortFolderHitsOrdering(t *testing.T) {
	hits := []FolderHit{
		{Path: "longer/path/here", HitCount: 5},
		{Path: "short", HitCount: 5},
		{Path: "another", HitCount: 8},
	}
	sortFolderHits(hits)
	if hits[0].Path != "another" {
		t.Errorf("rank 1: got %q, want another (highest HitCount)", hits[0].Path)
	}
	if hits[1].Path != "short" {
		t.Errorf("rank 2: got %q, want short (tied HitCount, shorter path wins)", hits[1].Path)
	}
	if hits[2].Path != "longer/path/here" {
		t.Errorf("rank 3: got %q, want longer/path/here", hits[2].Path)
	}
}
