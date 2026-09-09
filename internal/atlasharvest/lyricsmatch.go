package atlasharvest

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ReleaseTrack is one entry of Atlas's release listing.
//
// These six fields are the whole contract — verified against the live service
// on 2026-09-09, where `/v1/atlas/release/{mbid}/tracks` returned exactly
// `{medium_position, position, number, title, length_ms, recording_mbid}`.
// There is NO `isrc` and no per-track artist credit, so neither can be used to
// disambiguate; the matcher below has only position, number, title and length
// to work with.
type ReleaseTrack struct {
	MediumPosition int    `json:"medium_position"`
	Position       int    `json:"position"`
	Number         string `json:"number"`
	Title          string `json:"title"`
	LengthMS       int64  `json:"length_ms"`
	RecordingMBID  string `json:"recording_mbid"`
}

// MatchTier records HOW a local track was identified, because the confidence
// of the answer differs enormously between them and the caller uses it to
// decide whether the duration has to agree.
type MatchTier int

const (
	MatchNone MatchTier = iota
	// MatchTagged: the track's own tags carried a recording MBID. No matching
	// happened at all.
	MatchTagged
	// MatchCorroborated: position AND title independently name the same entry.
	MatchCorroborated
	// MatchTitle: exactly one entry on the release has this title.
	MatchTitle
	// MatchPosition: the position names an entry whose title disagrees.
	MatchPosition
)

func (t MatchTier) String() string {
	switch t {
	case MatchTagged:
		return "tagged"
	case MatchCorroborated:
		return "corroborated"
	case MatchTitle:
		return "title"
	case MatchPosition:
		return "position"
	}
	return "none"
}

// maxDurationDeltaMS is how far a local track's length may sit from
// MusicBrainz's before an UNCORROBORATED match is refused.
//
// It is a "this is different music" threshold, not a precision one, and it is
// deliberately loose. Measured over 447 position-matched pairs from this
// library: the median delta is 693 ms but the p90 is 21.2 SECONDS, so only
// 77.9% agree within 4 s — a tight global gate would throw away a fifth of
// CORRECT matches. The tail is not a keying artefact either; single-medium,
// disc-tagged pairs still show a p90 of 14.6 s.
//
// What the same measurement showed is that corroboration, not duration, is the
// discriminator. Over 752 tracks on 30 releases:
//
//	position AND title agree   37.0%   median 134 ms   97% ≤5 s   100% ≤30 s
//	unique title only          18.1%   median 1440 ms  68% ≤5 s    82% ≤30 s
//	position only              22.9%   median 1626 ms  65% ≤5 s    87% ≤30 s
//	no match                   22.1%
//
// So the veto is applied ONLY to the two uncorroborated tiers, where it
// discriminates, and never to a corroborated match, where at 100% within 30 s
// it could not fire without being wrong.
const maxDurationDeltaMS = 15000

// MatchRelease finds the recording MBID for one local track among a release's
// entries.
//
// The ladder is ordered by how much independent evidence each rung has, not by
// convenience. Corroboration first — two keys naming the same entry is a far
// stronger claim than either alone, and the measurement says so — then a title
// that is unique on the release, then position alone.
//
// `localDisc` of 0 means the file carries no disc number. That is not rare:
// 3,752 tracks in this library have none. Such a track can still be positioned
// on a SINGLE-medium release, where there is only one medium it could be on,
// but never on a multi-medium one, where assuming medium 1 silently matches
// disc 2's track N against disc 1's.
func MatchRelease(entries []ReleaseTrack, localTitle string, localDisc, localTrack int,
	localDurationMS int64) (ReleaseTrack, MatchTier) {
	if len(entries) == 0 {
		return ReleaseTrack{}, MatchNone
	}
	folded := foldTitle(localTitle)

	// The position key, when the release lets us form one.
	var byPos *ReleaseTrack
	if localTrack > 0 {
		disc := localDisc
		if disc == 0 {
			if m, single := soleMedium(entries); single {
				disc = m
			}
		}
		if disc > 0 {
			for i := range entries {
				if entries[i].MediumPosition == disc && entries[i].Position == localTrack {
					byPos = &entries[i]
					break
				}
			}
		}
	}

	// The title key, but only when it is UNAMBIGUOUS on this release. 1,275
	// albums in this library carry a duplicate title across 2,987 tracks —
	// "Intro", "Untitled", an acoustic version on a bonus disc — so a title
	// that appears twice names nothing.
	var byTitle *ReleaseTrack
	if folded != "" {
		seen := 0
		for i := range entries {
			if foldTitle(entries[i].Title) == folded {
				seen++
				byTitle = &entries[i]
			}
		}
		if seen != 1 {
			byTitle = nil
		}
	}

	switch {
	// ONE corroboration rule, not two. An earlier draft led with
	// `byPos == byTitle`, which this entirely subsumes: if the position and a
	// unique title name the same entry, that entry's title folds equal to the
	// local one by definition. Keeping both left the first arm unreachable —
	// and a NEGATIVE CONTROL is what proved it, by mutating that arm and
	// watching the suite stay green. This form is also strictly wider: it
	// corroborates when the title is AMBIGUOUS across the release but the entry
	// the position names is one of the ones carrying it, which is the "Intro"
	// case a duplicate-title release presents — and 1,275 albums here have one.
	case byPos != nil && folded != "" && foldTitle(byPos.Title) == folded:
		return *byPos, MatchCorroborated
	case byTitle != nil && durationAgrees(localDurationMS, byTitle.LengthMS):
		return *byTitle, MatchTitle
	case byPos != nil && durationAgrees(localDurationMS, byPos.LengthMS):
		return *byPos, MatchPosition
	}
	return ReleaseTrack{}, MatchNone
}

// soleMedium reports the medium every entry sits on, when there is only one.
func soleMedium(entries []ReleaseTrack) (int, bool) {
	m := entries[0].MediumPosition
	for _, e := range entries[1:] {
		if e.MediumPosition != m {
			return 0, false
		}
	}
	return m, true
}

// durationAgrees is the uncorroborated tiers' veto. An UNKNOWN length on either
// side abstains rather than refusing: MusicBrainz does not always carry one and
// neither does every local file, and turning those away would refuse a match on
// the absence of evidence rather than on evidence of a mismatch.
func durationAgrees(localMS, remoteMS int64) bool {
	if localMS <= 0 || remoteMS <= 0 {
		return true
	}
	d := localMS - remoteMS
	if d < 0 {
		d = -d
	}
	return d <= maxDurationDeltaMS
}

// foldTitle reduces a title to what two catalogues can be expected to agree on.
//
// Deliberately its own function and NOT unified with the four sibling folds in
// this tree. `enrich/matchfold` is comparison-time only, `unicodeLowerScalar`
// backs functional SQL indexes, `ArtistImagePathByName` hashes into a filename,
// and `dupes` mirrors the iOS partition byte for byte — changing any of them to
// suit this one would migrate an index, orphan a cache or split a library.
// This value is compared once, in memory, and never stored.
func foldTitle(s string) string {
	s = norm.NFKD.String(strings.ToLower(strings.TrimSpace(s)))
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
			// A combining mark left by the decomposition — dropping it is what
			// makes "Hasta mañana" and "Hasta Manana" the same title, which is
			// a real pair from this library.
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		default:
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
