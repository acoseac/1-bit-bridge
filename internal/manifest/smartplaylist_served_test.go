package manifest

import (
	"context"
	"slices"
	"sort"
	"testing"
	"time"
)

func TestSmartPlaylistServed_RecordsAndPrunesPastSevenDays(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	day := int64(24 * time.Hour)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC).UnixNano()

	// Plant both ages with a cutoff that keeps them, then prune at 7 days.
	if err := s.RecordSmartPlaylistServed(ctx, []SmartPlaylistServedRow{
		{ServedAt: now - 8*day, Slug: "auto-mix", Path: "/old.flac"},
		{ServedAt: now - day, Slug: "daily-mix", Path: "/recent.flac"},
		{ServedAt: now - day, Slug: "lift-off", Path: "/recent.flac"},
	}, now-30*day); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := s.SmartPlaylistServedSince(ctx, now-30*day)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["/old.flac"]; !ok {
		t.Fatal("row inside the planted window was not stored")
	}
	if _, ok := got["/recent.flac"]; !ok {
		t.Fatal("recent row was not stored")
	}

	if err := s.RecordSmartPlaylistServed(ctx, nil, now-7*day); err != nil {
		t.Fatalf("prune: %v", err)
	}
	got, err = s.SmartPlaylistServedSince(ctx, now-30*day)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["/old.flac"]; ok {
		t.Fatal("a path served 8 days ago survived the 7-day prune")
	}
	if _, ok := got["/recent.flac"]; !ok {
		t.Fatal("a path served yesterday was pruned")
	}
	// The lookup the regenerator uses is the same 7-day window.
	got, err = s.SmartPlaylistServedSince(ctx, now-7*day)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["/recent.flac"]; !ok || len(got) != 1 {
		t.Fatalf("7-day lookup = %v, want only /recent.flac", got)
	}
}

// servedTestDay is one UTC day in nanoseconds; Unix time has no leap
// seconds, so every UTC day is this long.
const servedTestDay = int64(24 * time.Hour)

// servedBoundaryDay is the start of the UTC day the boundary tests run on,
// 2026-10-18 00:00:00 UTC.
var servedBoundaryDay = time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC).UnixNano()

// servedBoundaryRow is one planted row of smart_playlist_served.
type servedBoundaryRow struct {
	at   int64
	slug string
	path string
}

// servedBoundaryRows stamps a row at each edge the day math has: the
// seven-day window's first nanosecond and the one before it, the day's
// first nanosecond and the ones either side of it, the day's last and the
// next day's first, all on auto-mix, and one row that day on lift-off.
func servedBoundaryRows() []servedBoundaryRow {
	d := servedBoundaryDay
	window := d - 7*servedTestDay
	return []servedBoundaryRow{
		{window - 1, "auto-mix", "/pruned.flac"},
		{window, "auto-mix", "/oldest.flac"},
		{d - 1, "auto-mix", "/yesterday.flac"},
		{d, "auto-mix", "/midnight.flac"},
		{d + 1, "auto-mix", "/today.flac"},
		{d + servedTestDay - 1, "auto-mix", "/late.flac"},
		{d + servedTestDay, "auto-mix", "/tomorrow.flac"},
		{d + 1, "lift-off", "/other-shelf.flac"},
	}
}

// plantServedRows inserts rows as the runs at their instants wrote them.
func plantServedRows(t *testing.T, s *Store, rows []servedBoundaryRow) {
	t.Helper()
	for _, r := range rows {
		if _, err := s.db.ExecContext(context.Background(),
			`INSERT INTO smart_playlist_served (served_at, slug, path) VALUES (?, ?, ?)`, r.at, r.slug, r.path); err != nil {
			t.Fatalf("plant %s %s: %v", r.slug, r.path, err)
		}
	}
}

// servedTableRows reads the whole table, ordered by slug, served_at and path.
func servedTableRows(t *testing.T, s *Store) []servedBoundaryRow {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT served_at, slug, path FROM smart_playlist_served ORDER BY slug, served_at, path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []servedBoundaryRow
	for rows.Next() {
		var r servedBoundaryRow
		if err := rows.Scan(&r.at, &r.slug, &r.path); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The exclusion is whole UTC days before the run's own. A row stamped at a
// day's first nanosecond belongs to that day: a run that day does not
// exclude it, and a run from the next day's first nanosecond does. The
// window's first nanosecond is inside it, and the one before is not.
func TestSmartPlaylistServed_ExclusionStartsAtUTCMidnight(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	plantServedRows(t, s, servedBoundaryRows())
	d := servedBoundaryDay
	sameDay := []string{"/oldest.flac", "/yesterday.flac"}
	cases := []struct {
		name string
		now  int64
		want []string
	}{
		{"at the day's first nanosecond", d, sameDay},
		{"one nanosecond into the day", d + 1, sameDay},
		{"at the day's last nanosecond", d + servedTestDay - 1, sameDay},
		{"one nanosecond before the day", d - 1, []string{"/oldest.flac", "/pruned.flac"}},
		{"at the next day's first nanosecond", d + servedTestDay,
			[]string{"/late.flac", "/midnight.flac", "/other-shelf.flac", "/today.flac", "/yesterday.flac"}},
	}
	for _, c := range cases {
		got, err := s.SmartPlaylistServedOnEarlierDays(ctx, c.now, 7)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		have := make([]string, 0, len(got))
		for p := range got {
			have = append(have, p)
		}
		sort.Strings(have)
		want := slices.Clone(c.want)
		sort.Strings(want)
		if !slices.Equal(have, want) {
			t.Errorf("%s: excluded %v, want %v", c.name, have, want)
		}
	}
}

// Writing a shelf replaces that shelf's rows from the run's own UTC day and
// no others: not the day before's, not the next day's, not another shelf's
// from the same day. The prune then drops what the run's window has passed.
func TestSmartPlaylistServed_ReplaceTouchesOnlyTheRunsUTCDay(t *testing.T) {
	d := servedBoundaryDay
	today := []string{"/midnight.flac", "/today.flac", "/late.flac"}
	cases := []struct {
		name string
		now  int64
		gone []string // the planted rows the call removes
	}{
		{"a run at the day's first nanosecond", d, append([]string{"/pruned.flac"}, today...)},
		{"a run one nanosecond into the day", d + 1, append([]string{"/pruned.flac"}, today...)},
		{"a run one nanosecond before the day", d - 1, []string{"/yesterday.flac"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSPStore(t)
			planted := servedBoundaryRows()
			plantServedRows(t, s, planted)
			if err := s.ReplaceSmartPlaylistServedToday(context.Background(), c.now, 7,
				map[string][]string{"auto-mix": {"/new.flac"}}); err != nil {
				t.Fatal(err)
			}
			want := []servedBoundaryRow{{c.now, "auto-mix", "/new.flac"}}
			for _, r := range planted {
				if r.slug != "auto-mix" || !slices.Contains(c.gone, r.path) {
					want = append(want, r)
				}
			}
			sort.Slice(want, func(i, j int) bool {
				if want[i].slug != want[j].slug {
					return want[i].slug < want[j].slug
				}
				if want[i].at != want[j].at {
					return want[i].at < want[j].at
				}
				return want[i].path < want[j].path
			})
			if got := servedTableRows(t, s); !slices.Equal(got, want) {
				t.Errorf("rows after the run:\n  got  %v\n  want %v", got, want)
			}
		})
	}
}
