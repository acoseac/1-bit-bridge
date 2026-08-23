package librarycat

// Quality classification — the iOS AlbumQualityFilter mirror
// (com.acoseac.dsdplayer/LibraryView.swift), with ONE deliberate,
// measured divergence documented below.

import "strings"

// QualityBucket is the curated format tier a track or album falls in.
// Seven values mirror iOS's filter; QualityDSDUnknownRate is the
// eighth, bridge-only.
type QualityBucket uint8

const (
	QualityUnknown QualityBucket = iota
	QualityLossy
	QualityCD
	QualityHiRes
	QualityDSD64
	QualityDSD128
	QualityDSD256Plus
	// QualityDSDUnknownRate is bridge-only and exists because of a
	// measured fact, not a taste: a routed UPnP row's tags_json is
	// DIDL-derived and carries no sample rate, so 1,730 of 1,743 DSD
	// tracks on the reference library classify into none of iOS's
	// three DSD tiers. Without this bucket they are reachable only
	// under "All".
	QualityDSDUnknownRate
)

// String is the wire/UI token for a bucket. Values match the iOS
// AlbumQualityFilter raw values where a twin exists.
func (q QualityBucket) String() string {
	switch q {
	case QualityLossy:
		return "lossy"
	case QualityCD:
		return "cdQuality"
	case QualityHiRes:
		return "hiresPCM"
	case QualityDSD64:
		return "dsd64"
	case QualityDSD128:
		return "dsd128"
	case QualityDSD256Plus:
		return "dsd256Plus"
	case QualityDSDUnknownRate:
		return "dsdUnknownRate"
	default:
		return "unknown"
	}
}

// IsDSD reports whether the bucket is any DSD tier — backs the "any
// DSD" filter value, which is the only way the rate-less DSD rows are
// selectable alongside the tiered ones.
func (q QualityBucket) IsDSD() bool {
	return q == QualityDSD64 || q == QualityDSD128 ||
		q == QualityDSD256Plus || q == QualityDSDUnknownRate
}

// lossyCodecs mirrors iOS's lossy set. "M4A" is included for the
// legacy reason the Swift side documents: the scanner historically
// stamped M4A for AAC content, and ALAC is stamped ALAC.
var lossyCodecs = map[string]struct{}{
	"MP3": {}, "AAC": {}, "M4A": {}, "OGG": {}, "OPUS": {}, "WMA": {}, "VORBIS": {},
}

// losslessPCMCodecs is the allowlist for the CD / hi-res tiers. An
// unknown codec deliberately reaches NEITHER — it lands in
// QualityUnknown, matching iOS's rule that a nil/empty codec matches
// only "All".
var losslessPCMCodecs = map[string]struct{}{
	"FLAC": {}, "ALAC": {}, "WAV": {}, "AIFF": {}, "AIF": {}, "PCM": {}, "APE": {}, "WV": {},
}

// DSD sample rates. DSD64 is 2.8224 MHz, DSD128 double that; anything
// at or above ~11.0 MHz is DSD256 or higher.
const (
	dsd64Rate       = 2822400
	dsd128Rate      = 5644800
	dsd256FloorRate = 11000000
)

// Classify buckets one track from its numbers rather than from a
// rendered string.
//
// iOS's AlbumQualityFilter.matches(codec:) parses a DISPLAY string
// ("FLAC 96/24", "DSD64"); mirroring that round-trip here would mean
// formatting a string only to re-parse it. The buckets are identical;
// only the input representation differs.
//
// THE ONE DELIBERATE DIVERGENCE — CD Quality accepts an ABSENT bit
// depth. iOS requires bits == 16 exactly. On the reference library
// bitsPerSample is present on 70 of 15,370 rows, so the iOS rule
// selects 23 tracks while 9,807 are genuinely at CD rate; three of the
// seven buckets ship empty. The sparsity is structural, not a scanner
// bug — a routed row's tags_json comes from DIDL and carries no bit
// depth — which means the bucket is equally dead on the phone. Absent
// is treated as unknown-not-disqualifying; an explicit non-16 depth
// still disqualifies. TestCDQualityTreatsAbsentBitDepthAsUnknown pins
// it, and its negative control is the iOS rule itself.
func Classify(codec string, rateHz, bits int, isDSD bool) QualityBucket {
	if isDSD {
		switch {
		case rateHz >= dsd256FloorRate:
			return QualityDSD256Plus
		case rateHz == dsd128Rate:
			return QualityDSD128
		case rateHz == dsd64Rate:
			return QualityDSD64
		case rateHz <= 0:
			return QualityDSDUnknownRate
		default:
			// A DSD rate we don't recognise: still DSD, still not a
			// tier we can name.
			return QualityDSDUnknownRate
		}
	}
	c := strings.ToUpper(strings.TrimSpace(codec))
	if c == "" {
		return QualityUnknown
	}
	if _, lossy := lossyCodecs[c]; lossy {
		return QualityLossy
	}
	if _, lossless := losslessPCMCodecs[c]; !lossless {
		return QualityUnknown
	}
	if rateHz > 48000 || bits > 16 {
		return QualityHiRes
	}
	// Within 100 Hz of 44.1 kHz, and a bit depth that is 16 or absent.
	if rateHz > 0 && abs(rateHz-44100) < 100 && (bits == 16 || bits == 0) {
		return QualityCD
	}
	// Lossless 48/16 (DAT / broadcast rate) reaches no bucket but
	// "All" — iOS's documented intentional gap, preserved deliberately.
	return QualityUnknown
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// QualityMask is the set of buckets present in an album.
type QualityMask uint16

func (m *QualityMask) add(q QualityBucket) { *m |= 1 << q }

// Has reports membership.
func (m QualityMask) Has(q QualityBucket) bool { return m&(1<<q) != 0 }

// Buckets lists the members in tier order — deterministic, so an
// album's "mixed" badge doesn't reshuffle between rebuilds.
func (m QualityMask) Buckets() []QualityBucket {
	var out []QualityBucket
	for q := QualityUnknown; q <= QualityDSDUnknownRate; q++ {
		if m.Has(q) {
			out = append(out, q)
		}
	}
	return out
}

// qualityRank orders buckets by fidelity for the album-level tie-break:
// when two buckets are equally common, the higher-fidelity one wins, so
// a 9-track FLAC album with one MP3 bonus reads as FLAC rather than
// flipping on an arbitrary map order.
func qualityRank(q QualityBucket) int {
	switch q {
	case QualityDSD256Plus:
		return 7
	case QualityDSD128:
		return 6
	case QualityDSD64:
		return 5
	case QualityDSDUnknownRate:
		return 4
	case QualityHiRes:
		return 3
	case QualityCD:
		return 2
	case QualityLossy:
		return 1
	default:
		return 0
	}
}
