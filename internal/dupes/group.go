package dupes

import (
	"sort"
	"strings"
)

// durationToleranceSec is the maximum spread of member durations for a
// group to still count as evidence-agreeing. Anything wider (a radio edit
// that slipped past the version tokens, a live cut) demotes to
// TierInconclusive — degrading DOWN is the safe direction, because
// inconclusive groups are never suppression candidates.
const durationToleranceSec = 1.5

// pathSegments splits a library path on both separators —
// separator-agnostic like the reconciliation passes' trackDir, because a
// Windows-hosted store can carry backslash paths.
func pathSegments(p string) []string {
	return strings.FieldsFunc(p, func(c rune) bool { return c == '/' || c == '\\' })
}

// SelfNestDepth is the longest run of consecutive IDENTICAL segments in
// the path — "Chicago/CD 01/CD 01/CD 01/x.flac" → 3, any normal path → 1.
// Display/diagnostic helper (exported for the CLI report's per-member
// depth badge); group MEMBERSHIP in the self-nested tier is decided by
// collapsed-path equality (see collapseNestedSegments), which is what
// keeps an eponymous album ("Metallica/Metallica/One.flac", a run of 2)
// out of the tier.
func SelfNestDepth(p string) int {
	segs := pathSegments(p)
	best, run := 1, 1
	for i := 1; i < len(segs); i++ {
		if segs[i] == segs[i-1] {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 1
		}
	}
	if len(segs) == 0 {
		return 0
	}
	return best
}

// collapseNestedSegments removes CONSECUTIVE duplicate path segments:
// "A/CD 01/CD 01/CD 01/x.flac" → "A/CD 01/x.flac". Identity for normal
// paths. Two members of a group are nest-twins — the same file uploaded
// at different self-nesting depths — exactly when their collapsed paths
// are equal while their raw paths differ. This is deliberately NARROW:
// "A/CD 01/CD 02/x.flac" and "A/Live/Live at the Apollo/x.flac" collapse
// to themselves, and an eponymous-album pair collapses to two DIFFERENT
// paths, so none of those ever reads as an upload accident.
func collapseNestedSegments(p string) string {
	segs := pathSegments(p)
	out := segs[:0]
	for i, s := range segs {
		if i > 0 && s == segs[i-1] {
			continue
		}
		out = append(out, s)
	}
	return strings.Join(out, "/")
}

// hasNestTwins reports whether ≥2 members share a collapsed path — the
// self-nested tier membership test.
func hasNestTwins(members []Row) bool {
	seen := make(map[string]struct{}, len(members))
	for _, m := range members {
		c := collapseNestedSegments(m.Path)
		if _, dup := seen[c]; dup {
			return true
		}
		seen[c] = struct{}{}
	}
	return false
}

// classify assigns the evidence tier for one group's members. Order is
// load-bearing: the filesystem-fact tier first, then every uncertainty
// demotes to inconclusive, and only fully-known groups reach the
// same-/different-format verdicts.
func classify(members []Row) Tier {
	// Defensive floor: every real caller passes ≥2 members (Groups()
	// drops smaller sets), but a hypothetical empty/singleton call must
	// degrade DOWN like every other uncertainty, not fall through to a
	// geometry verdict computed over nothing.
	if len(members) < 2 {
		return TierInconclusive
	}
	if hasNestTwins(members) {
		return TierSelfNested
	}
	if !versionTokensSymmetric(members) {
		return TierInconclusive
	}
	var minDur, maxDur float64
	for i, m := range members {
		if m.SampleRate <= 0 || m.BitsPerSample <= 0 || m.Codec == "" {
			return TierInconclusive // unknown geometry
		}
		if m.Duration <= 0 {
			return TierInconclusive // unknown length
		}
		if i == 0 || m.Duration < minDur {
			minDur = m.Duration
		}
		if i == 0 || m.Duration > maxDur {
			maxDur = m.Duration
		}
	}
	if maxDur-minDur > durationToleranceSec {
		return TierInconclusive
	}
	first := members[0]
	for _, m := range members[1:] {
		if !strings.EqualFold(m.Codec, first.Codec) ||
			m.SampleRate != first.SampleRate ||
			m.BitsPerSample != first.BitsPerSample ||
			m.IsDSD != first.IsDSD {
			return TierDifferentFormat
		}
	}
	return TierSameFormat
}

// Collector is the two-pass grouping accumulator. Pass 1 (Note) retains
// only keys and counts; Seal drops the singletons; pass 2 (Collect)
// retains member rows only for keys seen ≥ 2 — the StreamTracks →
// []ReconcileTarget OOM discipline one level further: on a 20k-track
// library the collector holds ~20k keys after pass 1 and only the ~2×
// group-member rows after pass 2, never the whole library.
//
// The two passes run over two invocations of the same stream; rows that
// appear or vanish between them are handled (an unknown key in Collect is
// ignored; a group left with < 2 members is dropped by Groups).
type Collector struct {
	counts   map[Key]int
	members  map[Key][]Row
	sealed   bool
	observed int
}

// NewCollector returns an empty collector.
func NewCollector() *Collector {
	return &Collector{counts: make(map[Key]int, 1024)}
}

// Note records one row's key (pass 1). The row itself is NOT retained.
func (c *Collector) Note(r Row) {
	c.counts[KeyFor(r)]++
}

// Seal transitions to pass 2: keys with fewer than two sightings are
// dropped so Collect retains nothing for them.
func (c *Collector) Seal() {
	c.members = make(map[Key][]Row)
	for k, n := range c.counts {
		if n >= 2 {
			c.members[k] = nil
		}
	}
	c.counts = nil
	c.sealed = true
}

// Collect retains the row iff its key survived Seal (pass 2). Row is a
// value copy, so the caller may reuse its struct across invocations (the
// StreamTracks contract).
func (c *Collector) Collect(r Row) {
	c.observed++
	k := KeyFor(r)
	if rows, ok := c.members[k]; ok {
		c.members[k] = append(rows, r)
	}
}

// Observed is the pass-2 row count — the "scanned" figure to report
// (never the pass-1 count; the store can change between passes).
func (c *Collector) Observed() int {
	return c.observed
}

// Groups classifies and returns every group with ≥ 2 collected members,
// members sorted by path, groups sorted by key for determinism.
func (c *Collector) Groups() []Group {
	out := make([]Group, 0, len(c.members))
	for k, rows := range c.members {
		if len(rows) < 2 {
			continue
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
		out = append(out, Group{Key: k, Tier: classify(rows), Members: rows})
	}
	sort.Slice(out, func(i, j int) bool { return groupKeyLess(out[i].Key, out[j].Key) })
	return out
}

func groupKeyLess(a, b Key) bool {
	if a.AlbumID != b.AlbumID {
		return a.AlbumID < b.AlbumID
	}
	if a.Disc != b.Disc {
		return a.Disc < b.Disc
	}
	if a.Track != b.Track {
		return a.Track < b.Track
	}
	return a.NormTitle < b.NormTitle
}
