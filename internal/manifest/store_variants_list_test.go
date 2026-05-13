package manifest

import (
	"context"
	"testing"
	"time"
)

// TestListVariantsByPathPrefix_exactPrefixOnly pins the "no partial
// matches" contract — `Diana Krall` must NOT match `Diana Krall/...`
// rows unless the prefix carries the trailing separator. SQL `LIKE`
// with `escape='\\'` would otherwise accept "Diana" as a prefix of
// "Diana Krall/Live/01.flac" silently.
func TestListVariantsByPathPrefix_exactPrefixOnly(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Diana Krall/Live/01.flac")
	upsertParent(t, s, "Diana Krall/Live/02.flac")
	upsertParent(t, s, "Dianasaurs/Greatest/01.flac")

	for _, p := range []string{
		"Diana Krall/Live/01.flac",
		"Diana Krall/Live/02.flac",
		"Dianasaurs/Greatest/01.flac",
	} {
		if err := s.UpsertVariant(context.Background(), VariantRow{
			SourcePath:    p,
			VariantID:     "upscaled-v2-176400-24",
			SidecarPath:   "/tmp/" + p + ".flac",
			Format:        "flac",
			SampleRate:    176400,
			BitsPerSample: 24,
			SizeBytes:     1024,
			SourceMTimeNS: time.Now().UnixNano(),
			SourceSize:    100,
			SoxSettings:   "{}",
			CreatedAt:     time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("UpsertVariant(%q): %v", p, err)
		}
	}

	rows, err := s.ListVariantsByPathPrefix(context.Background(), "Diana Krall/")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (Diana Krall/Live/01.flac, Diana Krall/Live/02.flac); rows=%+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.SourcePath == "Dianasaurs/Greatest/01.flac" {
			t.Errorf("prefix matched partial segment: %q", r.SourcePath)
		}
	}
}

// TestListVariantsByPathPrefix_likeEscape pins the SQL-wildcard
// defense — `%` and `_` in operator-supplied prefix strings must
// be escaped so an album folder named `20%_Hits` doesn't match
// every `20...` album.
func TestListVariantsByPathPrefix_likeEscape(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Albums/20%_Hits/01.flac")
	upsertParent(t, s, "Albums/2020 Best/01.flac")
	upsertParent(t, s, "Albums/2025 Best/01.flac")

	for _, p := range []string{
		"Albums/20%_Hits/01.flac",
		"Albums/2020 Best/01.flac",
		"Albums/2025 Best/01.flac",
	} {
		if err := s.UpsertVariant(context.Background(), VariantRow{
			SourcePath: p, VariantID: "v", SidecarPath: "/tmp/x", Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 1, SourceMTimeNS: 1, SourceSize: 1,
			SoxSettings: "{}", CreatedAt: 1,
		}); err != nil {
			t.Fatalf("UpsertVariant: %v", err)
		}
	}

	rows, err := s.ListVariantsByPathPrefix(context.Background(), "Albums/20%_Hits/")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (escaped %% and _); rows=%+v", len(rows), rows)
	}
	if rows[0].SourcePath != "Albums/20%_Hits/01.flac" {
		t.Errorf("matched wrong row: %q", rows[0].SourcePath)
	}
}

// TestListVariantsByPathPrefix_empty returns no rows when the prefix
// has no matches.
func TestListVariantsByPathPrefix_empty(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	rows, err := s.ListVariantsByPathPrefix(context.Background(), "Nothing/Here/")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}

// TestListVariantsForPath_exactMatchCaseInsensitive pins case-folding
// behaviour — iOS sends lowercase-normalized paths, the manifest
// stores filesystem-canonical case. Lookup must use unicode_lower
// like every other variant CRUD path.
func TestListVariantsForPath_exactMatchCaseInsensitive(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/Album/01.flac")
	if err := s.UpsertVariant(context.Background(), VariantRow{
		SourcePath: "Music/Album/01.flac", VariantID: "v",
		SidecarPath: "/tmp/x.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 1,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	// Lowercase shape iOS would send
	rows, err := s.ListVariantsForPath(context.Background(), "music/album/01.flac")
	if err != nil {
		t.Fatalf("ListVariantsForPath: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1; case-folding broken", len(rows))
	}
}

// TestListVariantsForPath_empty returns no rows when no variants
// exist for the requested source path.
func TestListVariantsForPath_empty(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	rows, err := s.ListVariantsForPath(context.Background(), "Music/Missing/track.flac")
	if err != nil {
		t.Fatalf("ListVariantsForPath: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(rows))
	}
}
