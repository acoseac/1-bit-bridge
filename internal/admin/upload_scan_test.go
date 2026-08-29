package admin

import (
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestPlanScanDirs(t *testing.T) {
	cases := []struct {
		name     string
		in       []string
		cap      int
		want     []string
		wantFull bool
	}{
		{
			name: "one album",
			in:   []string{"Pink Floyd/Dark Side"},
			cap:  8,
			want: []string{"Pink Floyd/Dark Side"},
		},
		{
			// The whole point: an ancestor-based trigger would resolve
			// these to the library root and silently do a full scan.
			name: "two unrelated top-level folders stay separate",
			in:   []string{"Aphex Twin/SAW", "Zappa/Hot Rats"},
			cap:  8,
			want: []string{"Aphex Twin/SAW", "Zappa/Hot Rats"},
		},
		{
			name: "descendants are dropped",
			in:   []string{"A/Album", "A/Album/Disc 1", "A/Album/Disc 2"},
			cap:  8,
			want: []string{"A/Album"},
		},
		{
			name: "sibling with a shared prefix is NOT swallowed",
			in:   []string{"A/Album", "A/Album-Bonus"},
			cap:  8,
			want: []string{"A/Album", "A/Album-Bonus"},
		},
		{
			// A discography: twelve album folders under one artist
			// collapse to the artist on the first iteration.
			name: "discography collapses to the artist folder",
			in: []string{
				"Bowie/Low", "Bowie/Heroes", "Bowie/Lodger", "Bowie/Hunky Dory",
				"Bowie/Station to Station", "Bowie/Blackstar", "Bowie/Aladdin Sane",
				"Bowie/Diamond Dogs", "Bowie/Pin Ups",
			},
			cap:  8,
			want: []string{"Bowie"},
		},
		{
			// Collapsing CREATES an ancestor relationship that did not
			// exist before: A/B/C → A/B and A/X → A, and A/B is now a
			// descendant of A.
			name: "collapse re-applies the descendant drop",
			in:   []string{"A/B/C", "A/X"},
			cap:  1,
			want: []string{"A"},
		},
		{
			// This one DISTINGUISHES re-pruning from merely re-deduping.
			// Pruning inside the loop frees cap headroom, so the collapse
			// stops sooner and the result stays NARROW; without it the set
			// keeps a redundant pair, spends its budget on it, and
			// over-collapses to whole top-level folders.
			//
			// Re-dedupe only would give [A, A/X, B, Y] — four scans, one of
			// them covered by another, and A instead of A/X.
			name: "re-pruning keeps the scan scope narrow",
			in:   []string{"A/X/Q", "B/X/C", "A/X/A/Y", "B/Y", "Y/Y/C"},
			cap:  4,
			want: []string{"A/X", "B", "Y/Y"},
		},
		{
			name: "a root-level file means the whole root",
			in:   []string{"."},
			cap:  8,
			want: nil, wantFull: true,
		},
		{
			name: "no directories at all",
			in:   nil,
			cap:  8,
			want: nil,
		},
		{
			name: "leading slash is tolerated",
			in:   []string{"/A/Album"},
			cap:  8,
			want: []string{"A/Album"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, full := planScanDirs(c.in, c.cap)
			if full != c.wantFull {
				t.Fatalf("fullScan = %v, want %v (dirs %v)", full, c.wantFull, got)
			}
			if !full && !reflect.DeepEqual(got, c.want) {
				t.Errorf("dirs = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNineTopLevelFoldersDoNotCollapseToRoot is the depth-1 floor.
//
// Without it, one collapse iteration turns every top-level artist folder into
// the root and escalates to a full scan — exactly when N targeted scans were
// the point, and after the collapse has thrown away the information needed to
// do them.
func TestNineTopLevelFoldersDoNotCollapseToRoot(t *testing.T) {
	var dirs []string
	for i := 0; i < 9; i++ {
		dirs = append(dirs, "Artist"+strconv.Itoa(i)+"/Album")
	}
	got, full := planScanDirs(dirs, 8)
	if !full {
		t.Fatalf("expected the full-scan fallback on genuine breadth, got %v", got)
	}
	// And the fallback must fire on BREADTH, not on the loop: raise the cap
	// and the same input must produce nine discrete subtree scans.
	got, full = planScanDirs(dirs, 9)
	if full {
		t.Fatal("nine folders under a cap of nine still escalated — the collapse walked above depth 1")
	}
	if len(got) != 9 {
		t.Errorf("got %d scan dirs, want 9: %v", len(got), got)
	}
}

func TestPruneDescendants(t *testing.T) {
	got := pruneDescendants([]string{"A/B/C", "A", "A/B", "Z", "A-x"})
	want := []string{"A", "A-x", "Z"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pruneDescendants = %v, want %v", got, want)
	}
}

// TestPlanScanDirsNeverReturnsARedundantSet is the property behind the
// re-prune: a returned directory must never be covered by another one in the
// same result. A redundant pair is not merely untidy — every subtree scan runs
// a WHOLE-LIBRARY duplicate restamp in its tail, so a covered entry is a full
// restamp bought for nothing.
func TestPlanScanDirsNeverReturnsARedundantSet(t *testing.T) {
	segs := []string{"A", "B", "C", "X", "Y", "Z", "Q"}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		var dirs []string
		for n := 1 + rng.Intn(6); n > 0; n-- {
			var parts []string
			for d := 1 + rng.Intn(4); d > 0; d-- {
				parts = append(parts, segs[rng.Intn(len(segs))])
			}
			dirs = append(dirs, strings.Join(parts, "/"))
		}
		got, full := planScanDirs(dirs, 1+rng.Intn(5))
		if full {
			continue
		}
		for a := range got {
			for b := range got {
				if a != b && strings.HasPrefix(got[a], got[b]+"/") {
					t.Fatalf("planScanDirs(%v) returned %v: %q is already covered by %q",
						dirs, got, got[a], got[b])
				}
			}
		}
	}
}
