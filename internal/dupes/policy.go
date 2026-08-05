package dupes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
)

// FilterMode selects which duplicate tiers the bridge suppresses from
// serving. String values are the config vocabulary (duplicates.filter)
// — cmd/bridge maps config.EffectiveFilter's canonical string onto this
// type one-to-one (lockstep-tested).
type FilterMode string

const (
	// FilterOff suppresses nothing. Groups are still stamped and counted
	// (the stats surface works with the filter off).
	FilterOff FilterMode = "off"
	// FilterSameFormat suppresses self-nested twins and same-format
	// duplicates only; cross-format groups are served in full.
	FilterSameFormat FilterMode = "same-format"
	// FilterHighestQuality additionally collapses different-format groups
	// to the highest-quality member WITHIN each DSD/PCM domain (the
	// default). DSD and PCM editions are never cross-suppressed.
	FilterHighestQuality FilterMode = "highest-quality"
)

// Policy is the suppression policy snapshot a stamping pass runs under.
type Policy struct {
	Mode FilterMode
}

// lossyCodecs is the bridge's lossy-codec vocabulary for QUALITY RANKING
// only (lossless outranks lossy within a domain). It deliberately
// mirrors manifest.IsLossyCodec's set — internal/manifest imports this
// package, so calling it directly would be an import cycle; the lockstep
// test lives on the manifest side (TestDupesLossyCodecsMirrorIsLossyCodec),
// which is the only package that can see both.
var lossyCodecs = map[string]struct{}{
	"MP3": {}, "AAC": {}, "OGG": {}, "OPUS": {}, "WMA": {},
}

// LossyCodecNames returns the ranking vocabulary, sorted, for the
// manifest-side lockstep test.
func LossyCodecNames() []string {
	out := make([]string, 0, len(lossyCodecs))
	for c := range lossyCodecs {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func isLossy(codec string) bool {
	_, ok := lossyCodecs[strings.ToUpper(strings.TrimSpace(codec))]
	return ok
}

// outranks is the deterministic total order that elects a suppression
// unit's winner: lossless beats lossy, then bit depth, then sample rate,
// then size (a bigger file of the same geometry is the less-compressed /
// better-provenanced rip), then the shallower path, then the shorter
// path, then lexicographic path — the last three exist purely so the
// order is TOTAL and the winner is stable across scans (a flapping
// winner would churn indexed_at and iOS deltas).
func outranks(a, b Row) bool {
	if la, lb := isLossy(a.Codec), isLossy(b.Codec); la != lb {
		return !la
	}
	if a.BitsPerSample != b.BitsPerSample {
		return a.BitsPerSample > b.BitsPerSample
	}
	if a.SampleRate != b.SampleRate {
		return a.SampleRate > b.SampleRate
	}
	if a.Size != b.Size {
		return a.Size > b.Size
	}
	da, db := len(pathSegments(a.Path)), len(pathSegments(b.Path))
	if da != db {
		return da < db
	}
	if len(a.Path) != len(b.Path) {
		return len(a.Path) < len(b.Path)
	}
	return a.Path < b.Path
}

// PlanSuppression returns the member paths the policy suppresses from
// serving for one classified group. Invariants, all test-pinned:
//
//   - FilterOff and TierInconclusive suppress nothing — every
//     uncertainty already degraded INTO inconclusive, and inconclusive
//     is never a suppression candidate.
//   - DSD and PCM members are NEVER cross-suppressed: each domain is its
//     own suppression unit, and a domain with a single member keeps it.
//   - TierSelfNested suppresses only nest-twins: within each
//     collapsed-path class the shallowest copy survives; members outside
//     any twin class are untouched (deliberately conservative — a
//     self-nested group's non-twin members are left alone rather than
//     re-tiered).
//   - TierSameFormat suppresses everything but the elected winner per
//     domain; TierDifferentFormat does the same only under
//     FilterHighestQuality.
//   - Every suppression unit serves EXACTLY ONE member (its winner).
func PlanSuppression(g Group, p Policy) []string {
	if p.Mode == FilterOff || g.Tier == TierInconclusive {
		return nil
	}
	switch g.Tier {
	case TierSelfNested:
		return planNestTwinSuppression(g.Members)
	case TierSameFormat:
		return planDomainWinners(g.Members)
	case TierDifferentFormat:
		if p.Mode != FilterHighestQuality {
			return nil
		}
		return planDomainWinners(g.Members)
	}
	return nil
}

// planNestTwinSuppression keeps the shallowest copy of each
// collapsed-path class and suppresses its deeper twins.
func planNestTwinSuppression(members []Row) []string {
	classes := map[string][]Row{}
	for _, m := range members {
		c := collapseNestedSegments(m.Path)
		classes[c] = append(classes[c], m)
	}
	var out []string
	for _, rows := range classes {
		if len(rows) < 2 {
			continue
		}
		winner := 0
		for i := 1; i < len(rows); i++ {
			if nestShallower(rows[i], rows[winner]) {
				winner = i
			}
		}
		for i, m := range rows {
			if i != winner {
				out = append(out, m.Path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// nestShallower orders nest-twins: fewer path segments first, then the
// generic rank as the deterministic tail.
func nestShallower(a, b Row) bool {
	da, db := len(pathSegments(a.Path)), len(pathSegments(b.Path))
	if da != db {
		return da < db
	}
	return outranks(a, b)
}

// planDomainWinners splits members into DSD and PCM domains and, within
// each domain holding ≥2 members, suppresses everything but the
// rank-elected winner. A single-member domain is untouched — that is the
// "DSD and PCM are never cross-suppressed" rule expressed structurally:
// the sole DSD edition can never lose to a PCM one, or vice versa.
func planDomainWinners(members []Row) []string {
	var dsd, pcm []Row
	for _, m := range members {
		if m.IsDSD {
			dsd = append(dsd, m)
		} else {
			pcm = append(pcm, m)
		}
	}
	var out []string
	for _, domain := range [][]Row{dsd, pcm} {
		if len(domain) < 2 {
			continue
		}
		winner := 0
		for i := 1; i < len(domain); i++ {
			if outranks(domain[i], domain[winner]) {
				winner = i
			}
		}
		for i, m := range domain {
			if i != winner {
				out = append(out, m.Path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ID is the stable persistence identity for a key: hex SHA-256 over an
// injective serialization (length-prefixed strings so a field boundary
// can never be forged by tag content). Stamped into tracks.dupe_group_id;
// stable across scans because the key itself is.
func (k Key) ID() string {
	h := sha256.New()
	var lenBuf [8]byte
	writeStr := func(s string) {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	writeInt := func(v int) {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(int64(v)))
		h.Write(lenBuf[:])
	}
	writeStr(k.AlbumID)
	writeInt(k.Disc)
	writeInt(k.Track)
	writeStr(k.NormTitle)
	return hex.EncodeToString(h.Sum(nil))
}
