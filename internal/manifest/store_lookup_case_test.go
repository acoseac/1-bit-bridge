package manifest

import (
	"path/filepath"
	"testing"
	"time"
)

// Pins the iOS↔bridge path-shape contract that
// `Store.LookupTrack` and `Store.LookupVariant` honour:
//
//   - iOS sends the path through `share.normalize(path:)` before
//     storing it in SwiftData and re-emitting on every endpoint
//     that takes a path. The normalisation lowercases the string
//     AND prepends "/" for bridge-source tracks.
//   - The bridge manifest stores the original FS-canonical case
//     (no leading slash, mixed case) as the PRIMARY KEY.
//
// Pre-fix the upscale eligibility gate called `GetTrack` directly
// and exact byte-compare missed every iOS-shaped request — POST
// /v1/upscale returned `enqueued=0` because `track == nil` made
// the eligibility gate short-circuit to `ErrUpscaleIneligible`.
//
// Note: `GetTrack` and `GetVariant` deliberately stay
// case-SENSITIVE so the scanner's unchanged-file fast-path can't
// alias two distinct case-colliding files on case-sensitive
// filesystems — see the function docs and Qodo's bug 1 on PR #126.
// External callers handing in iOS-shaped paths must use the
// Lookup* variants tested here.
func TestLookupTrack_caseInsensitiveAndLeadingSlash(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Abdullah Ibrahim/The Balance/09 - Devotion.flac"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
		Artist: "Abdullah Ibrahim", Album: "The Balance",
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"exact match (back-compat)", canonical},
		{"leading slash only", "/" + canonical},
		{"lowercase only (the iOS shape on a case-sensitive FS)",
			"abdullah ibrahim/the balance/09 - devotion.flac"},
		{"leading slash + lowercase (real iOS shape)",
			"/abdullah ibrahim/the balance/09 - devotion.flac"},
		{"uppercase",
			"ABDULLAH IBRAHIM/THE BALANCE/09 - DEVOTION.FLAC"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := s.LookupTrack(tc.path)
			if err != nil {
				t.Fatalf("LookupTrack(%q): %v", tc.path, err)
			}
			if tr == nil {
				t.Fatalf("LookupTrack(%q) = nil; expected the track to be found", tc.path)
			}
			if tr.Path != canonical {
				t.Errorf("LookupTrack(%q).Path = %q, want canonical %q", tc.path, tr.Path, canonical)
			}
		})
	}
}

// A genuinely missing track must still return (nil, nil) — the
// case-insensitive fallback must not loosen the not-found contract.
func TestLookupTrack_stillReturnsNilOnMiss(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	if err := s.UpsertTrack(&Track{
		Path: "Real/Track.flac", Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	tr, err := s.LookupTrack("/some/other/track.flac")
	if err != nil {
		t.Fatalf("LookupTrack: %v", err)
	}
	if tr != nil {
		t.Errorf("LookupTrack on a missing path returned %v; want nil", tr)
	}
}

// Pins the fail-closed-on-ambiguity contract for the case-folded
// fallback. On case-sensitive filesystems (most Linux deployments)
// two distinct files can legitimately coexist whose paths differ
// only by case; the prior `LIMIT 1` fallback would have returned
// whichever row SQLite happened to visit first, silently re-
// introducing the aliasing problem we kept `GetTrack` exact to
// avoid. (CodeRabbit on PR #126.)
//
// Conservative answer when the case-folded probe matches multiple
// distinct rows: nil. Better to surface "not found" than to guess
// at random — the upscale eligibility gate then returns
// ErrUpscaleIneligible, which is the correct response when the
// bridge can't unambiguously identify the track.
func TestLookupTrack_ambiguousCaseFoldReturnsNil(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	// Two distinct rows that fold to the same lowercase path.
	if err := s.UpsertTrack(&Track{
		Path: "Artist/Album/Track.flac", Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack 1: %v", err)
	}
	if err := s.UpsertTrack(&Track{
		Path: "ARTIST/ALBUM/TRACK.flac", Size: 2, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack 2: %v", err)
	}

	// iOS-shape that matches BOTH after lowercasing.
	tr, err := s.LookupTrack("/artist/album/track.flac")
	if err != nil {
		t.Fatalf("LookupTrack: %v", err)
	}
	if tr != nil {
		t.Errorf("LookupTrack on ambiguous case-fold returned %v; want nil — fail-closed contract broken",
			tr.Path)
	}

	// An exact case match must still win — only the fallback path
	// is ambiguous.
	exact, err := s.LookupTrack("Artist/Album/Track.flac")
	if err != nil {
		t.Fatalf("LookupTrack exact: %v", err)
	}
	if exact == nil || exact.Path != "Artist/Album/Track.flac" {
		t.Errorf("exact LookupTrack lost its row to the ambiguity guard; got %v", exact)
	}
}

// Pins the case-sensitive contract that internal callers (the
// scanner's unchanged-file fast-path in particular) rely on:
// `GetTrack` does an EXACT match. Two distinct case-colliding
// rows must each look up to themselves, never to each other.
// (Qodo bug 1 on PR #126 — case-folding the lookup made the
// scanner's optimization skip a real file that happened to have
// the same size/mtime as a case-colliding sibling.)
func TestGetTrack_remainsExactMatch(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Artist/Album/Track.flac"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	// Exact case → hit.
	if tr, err := s.GetTrack(canonical); err != nil || tr == nil {
		t.Fatalf("GetTrack(canonical) = (%v, %v); want non-nil", tr, err)
	}
	// Lowercase variant → MISS. The scanner relies on this.
	if tr, err := s.GetTrack("artist/album/track.flac"); err != nil {
		t.Fatalf("GetTrack(lowercase): %v", err)
	} else if tr != nil {
		t.Errorf("GetTrack(lowercase) = %v; want nil — exact-match contract broken", tr)
	}
	// Leading slash → MISS. Manifest never stores leading slash.
	if tr, err := s.GetTrack("/" + canonical); err != nil {
		t.Fatalf("GetTrack(leading-slash): %v", err)
	} else if tr != nil {
		t.Errorf("GetTrack(leading-slash) = %v; want nil — exact-match contract broken", tr)
	}
}

// Mirror of TestLookupTrack_caseInsensitiveAndLeadingSlash for the
// variant lookup that the upscale eligibility gate makes immediately
// after LookupTrack — same iOS-shaped path, same case-fold contract.
// Without this, the freshness check ("variant exists, refuse to
// re-convert") missed on case mismatch and the bridge would silently
// re-enqueue jobs whose output already lived on disk.
func TestLookupVariant_caseInsensitiveAndLeadingSlash(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Abdullah Ibrahim/The Balance/09 - Devotion.flac"
	const variantID = "v176400-24"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := s.UpsertVariant(VariantRow{
		SourcePath:    canonical,
		VariantID:     variantID,
		SidecarPath:   "/cache/variant.flac",
		Format:        "flac",
		SampleRate:    176_400,
		BitsPerSample: 24,
		SizeBytes:     1024,
		SourceMTimeNS: 0,
		SourceSize:    1,
		SoxSettings:   "{}",
		CreatedAt:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"exact", canonical},
		{"leading slash + lowercase", "/abdullah ibrahim/the balance/09 - devotion.flac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := s.LookupVariant(tc.path, variantID)
			if err != nil {
				t.Fatalf("LookupVariant: %v", err)
			}
			if v == nil {
				t.Fatalf("LookupVariant returned nil for %q + %q", tc.path, variantID)
			}
			if v.SourcePath != canonical {
				t.Errorf("v.SourcePath = %q, want %q", v.SourcePath, canonical)
			}
		})
	}
}

// TestNormalizePathForLookup_CleansRedundantSeparators is the unit-
// level pin for the path.Clean step added to normalizePathForLookup
// (Gemini on PR #147). Pre-fix the helper only stripped a leading
// slash; client-shape inputs containing redundant `//`, `.` or `..`
// segments would silently miss every lookup that walked through this
// helper (LookupTrack from /v1/upscale, LookupVariant from
// /v1/download?variant=). Post-fix all three forms canonicalize to
// the manifest's PRIMARY KEY shape.
//
// Tests the helper directly so a future refactor that splits the
// cleaning out into a separate function or moves it to a different
// layer can't bypass these cases.
func TestNormalizePathForLookup_CleansRedundantSeparators(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already canonical", "Artist/Album/01.flac", "Artist/Album/01.flac"},
		{"empty stays empty", "", ""},
		{"leading slash only (iOS bridge shape)", "/Artist/Album/01.flac", "Artist/Album/01.flac"},
		{"redundant separators", "Artist//Album/01.flac", "Artist/Album/01.flac"},
		{"multiple redundant separators", "Artist///Album//01.flac", "Artist/Album/01.flac"},
		{"dot-segment", "Artist/./Album/01.flac", "Artist/Album/01.flac"},
		{"trailing slash", "Artist/Album/", "Artist/Album"},
		{"leading slash + redundant separators", "/Artist//Album/01.flac", "Artist/Album/01.flac"},
		// path.Clean collapses `..` segments. Up-level escapes are
		// already rejected at the resolver layer (Resolve refuses any
		// `..` segment before path.Clean runs); this helper is a DB
		// lookup key and not a security boundary, so the cleanup
		// here is purely about matching the scanner's stored form.
		{"dot-dot collapse", "Artist/Other/../Album/01.flac", "Artist/Album/01.flac"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizePathForLookup(c.in); got != c.want {
				t.Errorf("normalizePathForLookup(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestLookupTrack_redundantSeparators_endToEnd asserts the
// centralized cleaning fixes the same class of bug for LookupTrack
// that PR #147 originally fixed for LookupVariant in the API layer.
// A request shaped like the upscale endpoint's libraryRelativePath
// (with `//` or `.` segments) MUST find the canonical row instead
// of silent-falling-through to ErrUpscaleIneligible.
func TestLookupTrack_redundantSeparators_endToEnd(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Artist/Album/01.flac"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
		Artist: "Artist", Album: "Album",
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	cases := []string{
		"Artist//Album/01.flac",
		"Artist/./Album/01.flac",
		"/Artist//Album/01.flac",
		"Artist/Other/../Album/01.flac",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			tr, err := s.LookupTrack(p)
			if err != nil {
				t.Fatalf("LookupTrack(%q): %v", p, err)
			}
			if tr == nil {
				t.Fatalf("LookupTrack(%q) = nil; redundant-separator path should canonicalize and hit", p)
			}
			if tr.Path != canonical {
				t.Errorf("LookupTrack(%q).Path = %q, want %q", p, tr.Path, canonical)
			}
		})
	}
}

// TestLookupTrack_unicodeFolding pins the v4 unicode_lower contract
// against the iOS↔bridge path-shape boundary. Pre-v4 the LOWER()
// fallback was ASCII-only: a path like `Sigur Rós/Ágætis byrjun/01.flac`
// stored by the bridge could not be matched by iOS's NFC + lowercased
// request shape `sigur rós/ágætis byrjun/01.flac` because SQLite's
// built-in LOWER doesn't fold `Ó` → `ó`. v4 swaps the indexes + queries
// to a Go-registered `unicode_lower(...)` function backed by
// `golang.org/x/text/cases.Lower(language.Und)` — same byte-for-byte
// fold iOS's `String.lowercased()` produces.
//
// Three regions cover the headline use-cases for non-English libraries:
// Latin Extended (Icelandic), Slavic (Polish), and German sharp-s.
func TestLookupTrack_unicodeFolding(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	cases := []struct {
		name      string
		canonical string // shape the bridge scanner records
		ioshape   string // shape iOS sends after NFC + lowercased + leading-slash
	}{
		{
			"sigur ros (Icelandic / Latin Extended)",
			"Sigur Rós/Ágætis byrjun/01 Svefn-g-englar.flac",
			"/sigur rós/ágætis byrjun/01 svefn-g-englar.flac",
		},
		{
			"hania rani (Polish)",
			"Hania Rani/Esja/01 Eden.flac",
			"/hania rani/esja/01 eden.flac",
		},
		{
			"german sharp-s",
			"Straße/Album/01.flac",
			"/straße/album/01.flac",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.UpsertTrack(&Track{
				Path: tc.canonical, Size: 1, ModTime: time.Now(),
				Artist: "Artist", Album: "Album",
			}); err != nil {
				t.Fatalf("UpsertTrack(%q): %v", tc.canonical, err)
			}
			tr, err := s.LookupTrack(tc.ioshape)
			if err != nil {
				t.Fatalf("LookupTrack(%q): %v", tc.ioshape, err)
			}
			if tr == nil {
				t.Fatalf("LookupTrack(%q) returned nil — pre-v4 ASCII-only LOWER would fail here; v4 unicode_lower must match", tc.ioshape)
			}
			if tr.Path != tc.canonical {
				t.Errorf("LookupTrack(%q).Path = %q, want canonical %q", tc.ioshape, tr.Path, tc.canonical)
			}
		})
	}
}
