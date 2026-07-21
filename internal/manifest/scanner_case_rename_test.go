package manifest

import (
	"sort"
	"testing"
)

// TestCaseOnlyRenames pins the deletion-pass fold filter (2026-07-21
// review Low): on a case-insensitive filesystem a case-only rename
// (Album→album) leaves the walker seeing the NEW case while the
// beforeSet still carries the old one — the old-case row must be
// reaped immediately rather than shadow the new row in /v1/manifest
// until missing_count hits the delete threshold. The fold matches the
// store's unicode_lower() SQL function (cases.Lower(language.Und))
// byte-for-byte. Unit-level on purpose: a full case-insensitive-FS
// integration test isn't portable (Linux CI filesystems are
// case-sensitive), and GetTrack stays exact-key (pinned by
// store_lookup_case_test.go).
func TestCaseOnlyRenames(t *testing.T) {
	cases := []struct {
		name   string
		before []string
		seen   []string
		want   []string
	}{
		{
			name:   "exact seen match spared (no rename)",
			before: []string{"Album/01.flac"},
			seen:   []string{"Album/01.flac"},
			want:   nil,
		},
		{
			name:   "case-only rename flagged",
			before: []string{"Album/01.flac"},
			seen:   []string{"album/01.flac"},
			want:   []string{"Album/01.flac"},
		},
		{
			name:   "genuinely missing path NOT flagged",
			before: []string{"Album/01.flac", "Gone/02.flac"},
			seen:   []string{"album/01.flac"},
			want:   []string{"Album/01.flac"},
		},
		{
			name:   "unicode fold matches unicode_lower semantics",
			before: []string{"Sigur Rós/Ágætis Byrjun/01.flac"},
			seen:   []string{"sigur rós/ágætis byrjun/01.flac"},
			want:   []string{"Sigur Rós/Ágætis Byrjun/01.flac"},
		},
		{
			name:   "both cases live on case-sensitive FS — nothing flagged",
			before: []string{"Album/01.flac", "album/01.flac"},
			seen:   []string{"Album/01.flac", "album/01.flac"},
			want:   nil,
		},
		{
			name:   "empty seen flags nothing",
			before: []string{"Album/01.flac"},
			seen:   nil,
			want:   nil,
		},
		{
			name:   "multi-root shape folds whole path",
			before: []string{"Music/Album/01.flac"},
			seen:   []string{"Music/album/01.flac"},
			want:   []string{"Music/Album/01.flac"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := make(map[string]struct{}, len(tc.before))
			for _, p := range tc.before {
				before[p] = struct{}{}
			}
			seen := make(map[string]struct{}, len(tc.seen))
			for _, p := range tc.seen {
				seen[p] = struct{}{}
			}
			gotSet := caseOnlyRenames(before, seen)
			got := make([]string, 0, len(gotSet))
			for p := range gotSet {
				got = append(got, p)
			}
			sort.Strings(got)
			if len(got) != len(tc.want) {
				t.Fatalf("caseOnlyRenames = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("caseOnlyRenames = %v, want %v", got, tc.want)
				}
			}
		})
	}
}
