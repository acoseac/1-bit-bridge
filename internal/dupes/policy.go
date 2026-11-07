package dupes

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
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
// which is the only package that can see both. The compressed AIFF-C and
// WAV encodings joined both with ExtractorVersion 21 (backlog B124, B154),
// and a hybrid WavPack file with no correction file with ExtractorVersion 32
// (backlog B489).
var lossyCodecs = map[string]struct{}{
	"MP3": {}, "AAC": {}, "OGG": {}, "OPUS": {}, "WMA": {},
	"ULAW": {}, "ALAW": {}, "IMA4": {}, "ADPCM": {}, "GSM": {}, "MP2": {},
	"WAVPACK-LOSSY": {},
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
// unit's winner: a file the last scan found beats one it did not (Missing:
// a winner whose file is gone suppresses the one copy a client can play
// until the deletion pass reaps its row, three full scans later by
// default; backlog B756 round 2), then a copy every client is served beats
// one the client format gate holds back (formatgate.Gated: a WavPack or
// Monkey's Audio file, by its path), then lossless beats lossy, then bit
// depth, then sample rate, then size (a bigger file of the same geometry is
// the less-compressed / better-provenanced rip), then the shallower path,
// then the shorter path, then lexicographic path — the last three exist
// purely so the order is TOTAL and the winner is stable across scans (a
// flapping winner would churn indexed_at and iOS deltas). Missing moves
// only when a file goes or comes back, never on a scan that sees the
// library as it was, and a walk that could not read a folder spares its
// rows' counts (the deletion pass's errored subtrees), so a flapping mount
// does not move it either.
//
// The gate's term comes before every term but Missing because suppression
// is server-wide and the gate is per client (backlog B489): an app that
// declares no gated format is never served a .wv or an .ape, so a gated
// winner would leave it no copy of the track at all, where an ungated
// winner is in every client's view. It costs an app that plays both the
// gated copy's better geometry, which is the owner's choice: no client
// ever loses the only copy it can play. Missing comes before the gate
// because a copy whose file is gone is no copy any client can play (B489's
// merge of B756): a found .wv over a gone .flac costs an app that declares
// nothing a row it could not play, and keeps the copy the others can.
func outranks(a, b Row) bool {
	if a.Missing != b.Missing {
		return !a.Missing
	}
	if ga, gb := formatgate.Gated(a.Path), formatgate.Gated(b.Path); ga != gb {
		return !ga
	}
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
//   - Every suppression unit serves EXACTLY ONE member (its winner), or
//     none when every member's file is gone (Row.Missing) and a unit of its
//     domain holding a found copy serves: rows no client can play, beside a
//     copy a client can (round 4 of backlog B489; exactly one until then).
func PlanSuppression(g Group, p Policy) []string {
	if p.Mode == FilterOff || g.Tier == TierInconclusive {
		return nil
	}
	switch g.Tier {
	case TierSelfNested:
		return planNestTwinSuppression(g.Members)
	case TierSameFormat, TierIdenticalAudio:
		// identical-audio is same-format with proof — same treatment,
		// stronger claim.
		return planDomainWinners(g.Members)
	case TierDifferentAudio:
		// Proven remasters: NEVER suppressed, under any mode. This is
		// the MD5 evidence's safety payoff — it strips false redundancy
		// off same-geometry groups, and the stamping pass's normal
		// diff+bump un-suppresses any member a pre-evidence pass hid.
		return nil
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

// nestShallower orders nest-twins: a file the last scan found first, as
// outranks orders every unit (Missing), then fewer path segments, then the
// generic rank as the deterministic tail.
func nestShallower(a, b Row) bool {
	if a.Missing != b.Missing {
		return !a.Missing
	}
	da, db := len(pathSegments(a.Path)), len(pathSegments(b.Path))
	if da != db {
		return da < db
	}
	return outranks(a, b)
}

// planDomainWinners splits members into DSD and PCM domains and, within
// each suppression unit holding ≥2 members, suppresses everything but the
// rank-elected winner. A single-member domain is untouched — that is the
// "DSD and PCM are never cross-suppressed" rule expressed structurally:
// the sole DSD edition can never lose to a PCM one, or vice versa.
//
// A domain is one unit while it holds a copy every client is served whose
// file the last scan found, which outranks then makes the winner, or copies
// of one gated format at most (gateUnits). Otherwise it is one unit per
// format, so a .wv and an .ape of one track are never suppressed against
// each other: the gate is per format, and a client that declares one of them
// alone would lose its only copy.
//
// A unit whose every member's file is gone (Row.Missing) is suppressed whole
// while another unit of its domain holds a found copy (round 4 of backlog
// B489): no client can play those rows, so none is lost, and a client whose
// view holds the found copy is no longer served a dead row beside it until
// the deletion pass reaps them. A domain whose every copy is gone keeps each
// unit's winner, as before. The rule is one domain's: a gone DSD edition
// beside found PCM copies is its domain's one member, and is served.
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
		found := holdsFound(domain)
		for _, unit := range gateUnits(domain) {
			if found && !holdsFound(unit) {
				for _, m := range unit {
					out = append(out, m.Path)
				}
				continue
			}
			if len(unit) < 2 {
				continue
			}
			winner := 0
			for i := 1; i < len(unit); i++ {
				if outranks(unit[i], unit[winner]) {
					winner = i
				}
			}
			for i, m := range unit {
				if i != winner {
					out = append(out, m.Path)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// gateUnits splits one domain into its suppression units by the client
// format gate (formatgate.Of of each member's path): the whole domain when
// it holds a copy every client is served whose file the last scan found, or
// copies of one gated format at most, and otherwise one unit per format, in
// the order the domain first names each. A copy every client is served whose
// file is gone (Row.Missing) is then a unit of its own, which suppresses none
// of the gated copies: the found ones are the only copies any client can
// play, so a .wv and an .ape beside a gone .flac are no more suppressed
// against each other than beside no .flac at all (round 3 of backlog B489,
// the merge of B756). planDomainWinners then suppresses that unit whole, as
// it does any unit whose every copy is gone, while a gated unit of the domain
// holds a found copy (round 4): its rows are served to no client beside the
// copy that client can play.
func gateUnits(domain []Row) [][]Row {
	var order []formatgate.Format
	byFormat := map[formatgate.Format][]Row{}
	served, gated := false, 0
	for _, m := range domain {
		f := formatgate.Of(m.Path)
		if _, seen := byFormat[f]; !seen {
			order = append(order, f)
			if f != formatgate.None {
				gated++
			}
		}
		byFormat[f] = append(byFormat[f], m)
		served = served || (f == formatgate.None && !m.Missing)
	}
	if served || gated < 2 {
		return [][]Row{domain}
	}
	units := make([][]Row, 0, len(order))
	for _, f := range order {
		units = append(units, byFormat[f])
	}
	return units
}

// holdsFound reports whether any of rows is a file the last scan found.
func holdsFound(rows []Row) bool {
	for _, m := range rows {
		if !m.Missing {
			return true
		}
	}
	return false
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
