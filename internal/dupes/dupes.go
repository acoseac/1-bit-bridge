// Package dupes groups library tracks by the iOS client's duplicate-
// collapse identity and classifies each group by the evidence available.
//
// It is a VERBATIM MIRROR of the iOS client's grouping rule — a different
// contract from every other normaliser in this repo, and the fourth entry
// on the "DO NOT UNIFY" list in internal/enrich/matchfold.go. Its output
// must equal the iOS `MetadataNormalizer` / `CrossSourceTrackDedup`
// pipeline's partition of the library, so it may never be "improved":
// a fix that makes it better than the client makes it WRONG, because the
// whole point is to predict which rows the client already collapses
// (iOS PR #1288). Each helper in clientkey.go is named after and annotated
// with its Swift twin; behaviour changes land ONLY as mirrors of an iOS
// change, with the test literals lifted from MetadataNormalizerTests.swift.
//
// Two knowingly-accepted divergences from Swift, both bounded and
// documented at the site: Go's strings.ToLower applies simple (not full)
// Unicode case mappings, and Swift regex character classes are
// Unicode-aware where Go's are ASCII — everywhere it matters (whitespace)
// the mirror hand-rolls the scan with unicode.IsSpace instead of regexp.
//
// The package is manifest-free: plain value types, testable with literals
// (the reconcile.go house style). internal/manifest adapts its rows into
// Row via StreamTrackDupeRefsUnderPrefix.
package dupes

// Row is one track's grouping-relevant projection. DiscTagged /
// TrackTagged carry the tag-present-vs-absent distinction (the client
// falls back to folder/filename inference ONLY when the tag is absent —
// an explicit 0 is a real value). Year ≤ 0 means absent (the client's
// albumID treats Some(0) and nil identically). SampleRate /
// BitsPerSample 0 and Codec "" mean unknown geometry; Duration ≤ 0 means
// unknown length — both degrade the group to TierInconclusive.
//
// AudioMD5 is always "" until the FLAC STREAMINFO capture lands (PR D of
// the duplicates program); it is declared now so adding the evidence
// requires no plumbing change.
type Row struct {
	Path          string
	Title         string
	Album         string
	AlbumArtist   string
	Artist        string
	Year          int
	Disc          int
	DiscTagged    bool
	Track         int
	TrackTagged   bool
	Size          int64
	Duration      float64 // seconds
	SampleRate    int     // Hz
	BitsPerSample int
	IsDSD         bool
	Codec         string
	AudioMD5      string
}

// Key is the mirror of the iOS ContentKey (CrossSourceTrackDedup.swift:264
// `ContentKey{albumID, disc, track, normTitle}`). Comparable, so it is
// used directly as a map key.
type Key struct {
	AlbumID   string
	Disc      int
	Track     int
	NormTitle string
}

// Tier names are EVIDENCE CLAIMS, not verdicts — the report and the admin
// page render them as such. TierDifferentFormat in particular asserts the
// members are different masters, NOT redundant copies.
type Tier string

const (
	// TierSelfNested — ≥2 members are the same file at different
	// self-nesting depths (an upload accident: "CD 01/CD 01/CD 01/…").
	// The one tier that is a pure filesystem fact.
	TierSelfNested Tier = "self-nested"
	// TierSameFormat — identical geometry (codec, rate, bits, DSD-ness)
	// and agreeing durations: likely true duplicates. Once audio-MD5
	// evidence covers EVERY member, the group refines into one of the
	// two tiers below; partial coverage keeps it here.
	TierSameFormat Tier = "same-format"
	// TierIdenticalAudio — every member FLAC with a known STREAMINFO
	// MD5, all equal: the ONLY tier where redundancy is a fact rather
	// than an inference.
	TierIdenticalAudio Tier = "identical-audio"
	// TierDifferentAudio — every member's MD5 known, and they differ:
	// same-geometry REMASTERS. A demotion out of same-format, and the
	// evidence's real payoff — these are never suppressed.
	TierDifferentAudio Tier = "different-audio"
	// TierDifferentFormat — differing geometry: different masters of the
	// same release (96/24 vs 48/24). NOT redundant.
	TierDifferentFormat Tier = "different-format"
	// TierInconclusive absorbs every uncertainty: unknown geometry or
	// duration, duration disagreement, version-token asymmetry. Always
	// degrade DOWN into this tier, never up out of it.
	TierInconclusive Tier = "inconclusive"
)

// Group is a set of ≥2 rows sharing the client key, classified.
// Members are sorted by Path for deterministic output.
type Group struct {
	Key     Key
	Tier    Tier
	Members []Row
}

// RedundantBytes is the total size of every member except the largest —
// the report's per-tier "bytes in the non-largest copies" figure. It is
// deliberately NOT called waste or savings: for TierDifferentFormat the
// non-largest copies are different masters, not redundancy.
func (g *Group) RedundantBytes() int64 {
	var sum, max int64
	for _, m := range g.Members {
		sum += m.Size
		if m.Size > max {
			max = m.Size
		}
	}
	return sum - max
}

// MD5Coverage reports how much audio-MD5 evidence this group carries:
// known = members with a non-empty AudioMD5, total = all members. The
// report surfaces the aggregate so a partially-backfilled library reads
// as "evidence still arriving", not as an absence of remasters.
func (g *Group) MD5Coverage() (known, total int) {
	for _, m := range g.Members {
		if m.AudioMD5 != "" {
			known++
		}
	}
	return known, len(g.Members)
}
