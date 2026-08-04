package manifest

import (
	"context"
	"testing"
)

// seedPrefixScopeFixture lays out two sibling folders whose names share
// a prefix — the shape that makes an unanchored `LIKE 'Album%'`
// over-match. `Album 2` sorts adjacent to `Album` and is a completely
// unrelated release.
func seedPrefixScopeFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	rate, bits, isDSD := float64(96000), 24, false
	for _, p := range []string{
		"Album/01.flac",
		"Album/02.flac",
		"Album 2/01.flac",
		"Albums/01.flac",
	} {
		if err := s.UpsertTrack(ctx, &Track{
			Path: p, Size: 1_000_000, SampleRate: &rate,
			BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", p, err)
		}
		if err := s.UpsertVariant(ctx, VariantRow{
			SourcePath: p, VariantID: "upscaled-v2-192000-24",
			SidecarPath: "/tmp/" + p + ".variant.flac", Format: "flac",
			SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1_500_000,
			SourceMTimeNS: 1, SourceSize: 1_000_000,
			SoxSettings: "{}", CreatedAt: 1,
		}); err != nil {
			t.Fatalf("UpsertVariant %q: %v", p, err)
		}
	}
}

// ListVariantsByPathPrefix must match DESCENDANTS of the named folder,
// never siblings that merely share a name prefix.
//
// Pre-fix it built a bare `likeEscape(prefix) + "%"`, so a delete
// scoped to `Album` also reaped every variant under `Album 2/` and
// `Albums/`. The API layer could not compensate: validateRelativePath
// rejects any prefix carrying a trailing slash (`cleaned != p`), so
// there was NO input that produced a correctly-scoped delete — and the
// deletion is silent and unrecoverable in place.
//
// The store's own TestListVariantsByPathPrefix_exactPrefixOnly already
// declared this contract ("must NOT match `Diana Krall/...` rows unless
// the prefix carries the trailing separator"); it just passed the
// separator itself, which the API can't.
func TestListVariantsByPathPrefix_DoesNotMatchSiblingFolders(t *testing.T) {
	s := openTestStore(t)
	seedPrefixScopeFixture(t, s)

	rows, err := s.ListVariantsByPathPrefix(context.Background(), "Album")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix: %v", err)
	}
	if len(rows) != 2 {
		var got []string
		for _, r := range rows {
			got = append(got, r.SourcePath)
		}
		t.Fatalf("matched %d rows %v, want exactly the 2 under Album/ — "+
			"a sibling folder sharing the name prefix was swept in", len(rows), got)
	}
	for _, r := range rows {
		if r.SourcePath != "Album/01.flac" && r.SourcePath != "Album/02.flac" {
			t.Fatalf("matched %q, which is not under Album/", r.SourcePath)
		}
	}
}

// A trailing slash is equivalent — the helper appends its own
// separator, so it must trim first rather than build `Album//%`.
func TestListVariantsByPathPrefix_TrailingSlashEquivalent(t *testing.T) {
	s := openTestStore(t)
	seedPrefixScopeFixture(t, s)
	ctx := context.Background()

	for _, prefix := range []string{"Album", "Album/", "Album//"} {
		rows, err := s.ListVariantsByPathPrefix(ctx, prefix)
		if err != nil {
			t.Fatalf("ListVariantsByPathPrefix(%q): %v", prefix, err)
		}
		if len(rows) != 2 {
			t.Errorf("prefix %q matched %d rows, want 2", prefix, len(rows))
		}
	}
}

// The empty scope stays "every row" — the delete-all path depends on
// it (the handler gates that behind ?confirm=true).
func TestListVariantsByPathPrefix_EmptyMatchesAll(t *testing.T) {
	s := openTestStore(t)
	seedPrefixScopeFixture(t, s)

	rows, err := s.ListVariantsByPathPrefix(context.Background(), "")
	if err != nil {
		t.Fatalf("ListVariantsByPathPrefix: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("empty prefix matched %d rows, want all 4", len(rows))
	}
}

// All four tracks-side prefix helpers must agree on what a slash-only
// prefix means. They did not: ListTrackProjectionsUnderPrefix and
// EligibleRollupByPrefix decided AFTER trimming (→ whole library),
// while RollupByPrefix and CountTracksByPrefix decided BEFORE (→ the
// range `path >= '/' AND path < '0'`, which matches nothing).
//
// The admin Inspector renders a rollup and a projection from the SAME
// submit, side by side, so `{"path": "//"}` showed 0 tracks in the card
// while the batch it enqueued covered the entire library.
func TestPrefixHelpers_AgreeOnSlashOnlyPrefix(t *testing.T) {
	s := openTestStore(t)
	seedPrefixScopeFixture(t, s) // 4 tracks total
	ctx := context.Background()

	for _, prefix := range []string{"", "/", "//", "///"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			n, err := s.CountTracksByPrefix(ctx, prefix)
			if err != nil {
				t.Fatalf("CountTracksByPrefix: %v", err)
			}
			if n != 4 {
				t.Errorf("CountTracksByPrefix = %d, want 4 (whole library)", n)
			}

			roll, err := s.RollupByPrefix(ctx, prefix)
			if err != nil {
				t.Fatalf("RollupByPrefix: %v", err)
			}
			if roll.TrackCount != 4 {
				t.Errorf("RollupByPrefix.TrackCount = %d, want 4 (whole library)",
					roll.TrackCount)
			}

			proj, err := s.ListTrackProjectionsUnderPrefix(ctx, prefix, "upscaled")
			if err != nil {
				t.Fatalf("ListTrackProjectionsUnderPrefix: %v", err)
			}
			if len(proj) != 4 {
				t.Errorf("ListTrackProjectionsUnderPrefix = %d rows, want 4 (whole library)",
					len(proj))
			}

			if roll.TrackCount != n || len(proj) != n {
				t.Errorf("helpers disagree for %q: count=%d rollup=%d projection=%d",
					prefix, n, roll.TrackCount, len(proj))
			}
		})
	}
}
