package manifest

import (
	"context"
	"testing"
)

func TestListDupeGroupsPage(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedDupeTrack(t, s, &Track{
		Path: "A/x/01.flac", Size: 900, Title: "Song", Album: "X", AlbumArtist: "AA",
		Duration: f64ptr(200), SampleRate: f64ptr(44100), BitsPerSample: intptr(16), Codec: "FLAC",
	})
	seedDupeTrack(t, s, &Track{
		Path: "B/x/01.flac", Size: 1000, Title: "Song", Album: "X", AlbumArtist: "AA",
		Duration: f64ptr(200), SampleRate: f64ptr(44100), BitsPerSample: intptr(16), Codec: "FLAC",
	})
	seedDupeTrack(t, s, &Track{Path: "C/y/01.flac", Size: 5, Title: "Other"})
	seedDupeTrack(t, s, &Track{Path: "D/y/01.flac", Size: 6, Title: "Other"})
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{
		{Path: "A/x/01.flac", GroupID: "g-aaa", Tier: "same-format", Suppressed: true},
		{Path: "B/x/01.flac", GroupID: "g-aaa", Tier: "same-format"},
		{Path: "C/y/01.flac", GroupID: "g-bbb", Tier: "inconclusive"},
		{Path: "D/y/01.flac", GroupID: "g-bbb", Tier: "inconclusive"},
	}); err != nil {
		t.Fatal(err)
	}

	groups, next, err := s.ListDupeGroupsPage(ctx, "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || next != "" {
		t.Fatalf("groups=%d next=%q", len(groups), next)
	}
	g := groups[0] // ordered by group id: g-aaa first
	if g.GroupID != "g-aaa" || g.Tier != "same-format" || len(g.Members) != 2 {
		t.Fatalf("group 0: %+v", g)
	}
	// Members sorted by path; projection carries geometry + tags + state.
	m := g.Members[0]
	if m.Path != "A/x/01.flac" || !m.Suppressed || m.Codec != "FLAC" ||
		m.SampleRate != 44100 || m.BitsPerSample != 16 || m.SizeBytes != 900 ||
		m.Title != "Song" || m.Album != "X" || m.AlbumArtist != "AA" {
		t.Fatalf("member projection: %+v", m)
	}
	if g.Members[1].Suppressed {
		t.Fatal("winner must read served")
	}

	// Tier filter.
	inc, _, err := s.ListDupeGroupsPage(ctx, "inconclusive", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 1 || inc[0].GroupID != "g-bbb" {
		t.Fatalf("tier filter: %+v", inc)
	}

	// Cursor paging: limit 1 → next cursor is the last group id.
	p1, next, err := s.ListDupeGroupsPage(ctx, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 1 || next != "g-aaa" {
		t.Fatalf("page1: %d groups, next=%q", len(p1), next)
	}
	p2, next, err := s.ListDupeGroupsPage(ctx, "", next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2) != 1 || p2[0].GroupID != "g-bbb" || next != "" {
		t.Fatalf("page2: %+v next=%q", p2, next)
	}
}
