package acoustid

import (
	"math"
	"sort"
	"strings"
)

// The acceptance gate.
//
// This is the layer where a wrong answer becomes a wrong MBID in someone's
// library, so two properties are deliberate and worth preserving:
//
//   - It is PURE. No context, no I/O, no clock. Every clause is exercised by a
//     table test in CI with no fpcalc, no network and no audio fixture.
//   - Its input type carries ONLY what the gate may use. Release groups reach
//     it (they back the unambiguous-album case) but there is nowhere to put a
//     release MBID in [Decision], so no future edit can start writing one by
//     accident. See the package doc for why that restriction exists.
//
// The numbers below are defaults chosen to be conservative: this path runs
// only after text matching has already failed, so a refused track is exactly
// as unresolved as it is today, while a wrong write is permanent. Precision
// beats recall at every clause.
const (
	// MinTrackSeconds refuses audio too short to identify. Below ~30s a
	// fingerprint carries few sub-fingerprints, so collisions are common and
	// the score saturates on a short overlap. 35 rather than the folklore 30
	// buys margin over a 30.4s interlude sitting on the boundary. The cost is
	// refusing some legitimately short songs, which is the right trade.
	MinTrackSeconds = 35.0

	// MaxTrackSeconds refuses single-file DJ mixes, full-CD rips and live
	// sets. fpcalc reads only the leading window, so a 74-minute mix would
	// confidently take its first track's identity — a wrong answer that every
	// later clause would happily confirm. Refusal is the only defence.
	MaxTrackSeconds = 1800.0

	// minDistinctB64Chars is the entropy floor, and it is the clause that
	// stops silence, tones, applause-only tracks and hidden-track gap files.
	//
	// Those inputs cannot be caught after the fact: silence converges to one
	// degenerate fingerprint cluster linked to hundreds of unrelated
	// recordings, and it scores 1.00. There is nothing for a later clause to
	// object to. Refusing to look them up at all is the defence.
	//
	// Calibrated by measurement against fpcalc 1.6.1, counting distinct
	// characters in the compressed base64 fingerprint:
	//
	//	45s digital silence      12–13
	//	45s pure sine tone       13
	//	45s stationary 4-note chord  14
	//	35s stepwise melody      64      <- the FLOOR case for real audio
	//	45s pink noise           63–64
	//
	// The separation is ~5x with nothing in between, and it holds at the
	// 35s eligibility boundary (silence 12, melody 64), so the threshold is
	// duration-independent. 32 is the midpoint of the alphabet and of the gap.
	//
	// Note the third row: a rich but STATIONARY chord is as degenerate as
	// silence, because Chromaprint keys on spectral change over time. Real
	// music is never stationary; a synthetic fixture easily is.
	minDistinctB64Chars = 32

	// decodeAgreementSec bounds the difference between what fpcalc reported and
	// the duration the bridge read from the container. A mismatch means fpcalc
	// saw something other than the file we think it did.
	//
	// This is a BACKSTOP, not the primary truncation guard, and the
	// distinction is measured rather than assumed: a truncated source makes
	// fpcalc report a read error and exit non-zero, which Compute already
	// turns into ErrUnreadable before any of this runs. And for FLAC
	// specifically the comparison could not catch it anyway — fpcalc reports
	// the STREAMINFO duration, so a truncated FLAC still claims its full
	// length. What this clause covers is the residue: a container whose
	// reported length is derived from the decode, or a file whose header
	// disagrees with its payload, where fpcalc exits clean regardless.
	decodeAgreementSec = 2.0

	// minScore gates fingerprint similarity. A genuine same-audio match is
	// essentially always >0.95; the 0.5–0.9 band is partial overlaps —
	// crossfades, shared intros, truncations. Picard's 0.5 default assumes a
	// human reviewing the result; there is none here. Not 0.98, because
	// legitimate transcodes and alternate masters at the same length live in
	// 0.92–0.97.
	minScore = 0.92

	// minScoreMargin refuses ambiguity: two results both above the floor means
	// the audio matches two clusters and the ordering between them is decided
	// by nothing meaningful. The clause almost never fires (AcoustID's tail
	// sits far below the floor), which makes it cheap to keep strict.
	//
	// No waiver when the tied results agree on artist. A conditional exemption
	// inside an acceptance predicate is where bugs hide.
	minScoreMargin = 0.05

	// minSources is the only signal in the payload that grades the LINK rather
	// than the audio. One person running Picard over mis-tagged files produces
	// a 1-source link that scores 1.00 and passes every other clause. Not 2:
	// one person re-ripping from two machines can reach it.
	minSources = 3

	// minSourcesNoLocalArtist raises the bar when the track has no usable
	// artist tag to contradict the answer. There the gate is entirely AcoustID
	// grading its own homework, so the evidence requirement rises. Not 5 flat,
	// because this population is by construction the obscure material and a
	// blanket 5 would reject most of it.
	minSourcesNoLocalArtist = 5

	// durationToleranceSec / durationToleranceFrac bound the difference
	// between the track's real length and the matched recording's. This is the
	// highest-value clause after the entropy floor: it catches long mixes,
	// radio-edit-vs-album-version pairs, truncated decodes (it compares
	// against the container, not against the decode) and a good share of
	// mislinks. 7s because MusicBrainz durations swing by a few seconds with
	// pre-gap conventions and mastering; 3s false-rejects heavily and 15s
	// admits radio edits.
	durationToleranceSec  = 7.0
	durationToleranceFrac = 0.02
)

// RejectReason names the clause that refused a track. It is a closed set:
// callers use it for logging and metrics, and its cardinality is bounded so it
// can safely key a counter map.
//
// It is NOT an enricher skip reason. The enricher's skip-reason map is
// separately bounded and must never be keyed on one of these.
type RejectReason string

const (
	// ReasonNone means accepted.
	ReasonNone RejectReason = ""

	ReasonUnknownDuration RejectReason = "unknown_duration"
	ReasonTooShort        RejectReason = "too_short"
	ReasonTooLong         RejectReason = "too_long"
	ReasonIsDSD           RejectReason = "is_dsd"

	ReasonLowEntropy     RejectReason = "low_entropy"
	ReasonDecodeMismatch RejectReason = "decode_mismatch"

	ReasonNoResults        RejectReason = "no_results"
	ReasonLowScore         RejectReason = "low_score"
	ReasonAmbiguousResults RejectReason = "ambiguous_results"
	ReasonFewSources       RejectReason = "few_sources"

	ReasonNoRecordings       RejectReason = "no_recordings"
	ReasonDurationMismatch   RejectReason = "duration_mismatch"
	ReasonArtistDisagreement RejectReason = "artist_disagreement"
	ReasonNoArtistMBID       RejectReason = "no_artist_mbid"
	// ReasonOnlyPlaceholderArtist — every surviving recording is credited to
	// MusicBrainz's [unknown]. Distinct from no_artist_mbid on purpose: the
	// data is well formed and the audio was identified, there is simply no
	// performer named to write.
	ReasonOnlyPlaceholderArtist RejectReason = "only_placeholder_artist"
)

// unknownArtistMBID is MusicBrainz's [unknown] special-purpose artist, whose
// own disambiguation reads "Special Purpose Artist – Do not add releases here,
// if possible."
//
// Verified against a LIVE AcoustID response rather than assumed: placeholder
// recordings in the payload are credited to exactly this ID. Keyed on the MBID
// and never the display name, which is localisable and third-party supplied.
const unknownArtistMBID = "125ec42a-7229-4250-afc5-e057484327fe"

// Input is everything the gate may consider. Deliberately not
// *manifest.Track: passing the whole row would put every future field within
// reach of a clause, and the point of this type is that it cannot happen.
type Input struct {
	// DurationSec is the track's length as the bridge read it from the
	// CONTAINER (FLAC STREAMINFO), not as fpcalc decoded it. That
	// independence is what makes the truncated-file and long-mix clauses
	// work. Zero means unknown, which is disqualifying.
	DurationSec float64

	// IsDSD excludes DSD sources: fpcalc's handling of them is unreliable, and
	// a mis-decoded DSD stream is a degenerate input wearing a high-entropy
	// disguise.
	IsDSD bool

	// Fingerprint is what fpcalc produced.
	Fingerprint Fingerprint

	// Results is what AcoustID returned, in the order it returned them.
	Results []Result

	// HasLocalArtistWitness reports whether the track carries a usable
	// (non-blank, non-junk) artist tag that could contradict the answer.
	//
	// The caller computes it because classifying a tag as junk uses the
	// enricher's match-folding vocabulary, which lives in internal/enrich —
	// and internal/enrich consumes this package, so importing it back would
	// be a cycle. It reaches the gate as a bool for one more reason: keeping
	// it a parameter is what lets the table test drive both sides of the
	// clause without constructing tag fixtures.
	//
	// NOTE the gate does NOT itself compare the local artist against the
	// answer. That veto — the single most valuable clause, because it is the
	// only one using information the fingerprint pipeline did not produce —
	// belongs on the write path in internal/enrich, where the fold lives.
	// This flag only raises the sources bar in its absence.
	HasLocalArtistWitness bool
}

// Decision is what the gate concluded. Note what is absent: there is no
// release MBID and no artwork MBID, and that is structural. A fingerprint
// identifies audio, AcoustID maps audio to a recording, and one recording sits
// under many releases precisely because they contain the same audio — so
// choosing one would be a uniform draw dressed as a match.
type Decision struct {
	// ArtistMBID is the head credited artist, agreed across every surviving
	// recording. Always set on an accept.
	ArtistMBID string

	// ArtistName is that artist's canonical MusicBrainz name. The caller uses
	// it as a QUERY term for the existing text ladder, and as the value the
	// local-artist veto is checked against.
	ArtistName string

	// RecordingMBID is set only when the survivors name exactly one recording.
	// Often empty on a perfectly good match — MusicBrainz merge redirects mean
	// two distinct-looking MBIDs can be one entity — and that is the intended
	// conservative outcome, not a defect.
	RecordingMBID string

	// AlbumHint is a release-group TITLE, set only when every surviving
	// recording shares exactly one release group, i.e. when there is no
	// ambiguity to launder. It is a query term for the existing text
	// acceptance, never an identifier to store.
	AlbumHint string

	// AcoustID is the cluster ID that produced this decision. Recorded as
	// provenance so the writes are attributable and reversible.
	AcoustID string

	// Score and Sources are carried for logging and for the control harness's
	// histograms. Nothing downstream should branch on them: the gate has
	// already applied every threshold they feed.
	Score   float64
	Sources int
}

// CheckEligible is the pre-fingerprint screen. Callers run it BEFORE spending a
// decode, which on a network-backed library is also before spending egress.
func CheckEligible(durationSec float64, isDSD bool) RejectReason {
	if durationSec <= 0 {
		return ReasonUnknownDuration
	}
	if isDSD {
		return ReasonIsDSD
	}
	if durationSec < MinTrackSeconds {
		return ReasonTooShort
	}
	if durationSec > MaxTrackSeconds {
		return ReasonTooLong
	}
	return ReasonNone
}

// CheckFingerprint is the post-decode, pre-lookup screen. Running it before the
// network call is what keeps a silent track from ever reaching AcoustID.
func CheckFingerprint(in Input) RejectReason {
	if r := CheckEligible(in.DurationSec, in.IsDSD); r != ReasonNone {
		return r
	}
	if in.Fingerprint.DistinctB64 < minDistinctB64Chars {
		return ReasonLowEntropy
	}
	if in.Fingerprint.Duration > 0 &&
		math.Abs(in.Fingerprint.Duration-in.DurationSec) > decodeAgreementSec {
		return ReasonDecodeMismatch
	}
	return ReasonNone
}

// Accept is the complete predicate: it re-runs every earlier stage so a single
// call is the whole gate, which is what lets the table test drive all of it
// through one entry point. Callers that short-circuit early with CheckEligible
// and CheckFingerprint lose nothing by doing so.
//
// On refusal the returned Decision is zero and the reason names the clause.
// The stages run in a fixed order and each may only ever REFUSE. That
// ordering is the safety property, not an implementation detail: a later,
// looser-looking clause can never lift a candidate an earlier one rejected.
// Keep them as separate named steps — collapsing them into one pass would be
// behaviour-preserving and property-hiding.
func Accept(in Input) (Decision, RejectReason) {
	if r := CheckFingerprint(in); r != ReasonNone {
		return Decision{}, r
	}
	top, tied, reason := selectResult(in)
	if reason != ReasonNone {
		return Decision{}, reason
	}
	return acceptRecordings(in, top, tied)
}

// selectResult is stage 2: pick the cluster, and refuse if the audio points at
// two clusters that would answer DIFFERENTLY.
//
// `tied` reports that a competing cluster cleared the floor within the margin
// and agreed on the artist. The caller writes the artist but suppresses the
// recording MBID and the album cue, because which of the tied clusters to draw
// those from is genuinely undetermined.
func selectResult(in Input) (top Result, tied bool, reason RejectReason) {
	if len(in.Results) == 0 {
		return Result{}, false, ReasonNoResults
	}
	// AcoustID orders by score, but re-derive the top rather than trust it.
	top = in.Results[0]
	for i := range in.Results {
		if in.Results[i].Score > top.Score {
			top = in.Results[i]
		}
	}
	if top.Score < minScore {
		return Result{}, false, ReasonLowScore
	}
	return resolveTiedClusters(in, top)
}

// resolveTiedClusters decides what a near-tie between clusters means.
//
// The clause this replaced refused any track whose runner-up cleared the score
// floor within minScoreMargin, on the reasoning that the ordering between two
// matching clusters is decided by nothing meaningful. Measured against a real
// library that turned out to reject a quarter of the eligible population while
// preventing nothing: AcoustID's database carries UNMERGED DUPLICATE clusters
// for the same audio, so the overwhelming majority of "ties" are one answer
// stored twice — ABBA at 0.978 against ABBA at 0.974. Of 16 ties sampled, 14
// resolved to the same artist and the other 2 were already refused by the
// duration and consensus clauses; cross-cluster disagreement, the thing the
// clause was written for, did not occur once.
//
// So the predicate now measures what it was always about: ambiguity is the
// tied clusters yielding a DIFFERENT ANSWER, not merely existing. That is the
// same correction as head-artist-over-credit-tuple — the granularity of the
// check has to match the field being written.
//
// It is not a relaxation of the strictness that protects the library. Every
// competing cluster is put through the SAME duration and sources filtering as
// the winner before its opinion counts, disagreement still refuses outright,
// and a tie still costs the recording MBID and the album cue.
func resolveTiedClusters(in Input, top Result) (Result, bool, RejectReason) {
	topHead, ok := clusterHeadArtist(in, top)
	if !ok {
		// The winner has no usable answer of its own — no surviving
		// recordings, or survivors that disagree. Defer: stage 3 refuses it
		// with the specific reason (duration, sources, or disagreement),
		// which is more useful than calling it ambiguous.
		return top, false, ReasonNone
	}

	tied := false
	for i := range in.Results {
		other := in.Results[i]
		if other.ID == top.ID || other.Score < minScore ||
			top.Score-other.Score >= minScoreMargin {
			continue
		}
		otherHead, ok := clusterHeadArtist(in, other)
		if !ok {
			// A competitor that survives nothing is not a competing ANSWER.
			// Refusing on its account would be refusing on the strength of a
			// cluster that could not have answered at all.
			continue
		}
		if otherHead != topHead {
			return Result{}, false, ReasonAmbiguousResults
		}
		tied = true
	}
	return top, tied, ReasonNone
}

// clusterHeadArtist reports the single head artist a cluster would resolve to,
// after the SAME duration and sources filtering the winner is held to. ok is
// false when the cluster has no surviving recordings or its survivors name
// more than one head artist — in both cases it has no single answer to offer.
func clusterHeadArtist(in Input, r Result) (string, bool) {
	survivors := recordingsWithEnoughSources(
		recordingsMatchingDuration(r.Recordings, in.DurationSec),
		requiredSources(in),
	)
	// MUST apply the same placeholder filter acceptRecordings does. If the two
	// disagree about what a cluster says, a cluster carrying a placeholder
	// looks answerless here, resolveTiedClusters defers instead of comparing,
	// and the ambiguity check never runs — while acceptRecordings goes on to
	// accept it. That is a relaxation, not a deferral.
	survivors = recordingsWithNamedArtist(survivors)
	if len(survivors) == 0 {
		return "", false
	}
	id, _, reason := headArtistConsensus(survivors)
	if reason != ReasonNone || id == "" {
		return "", false
	}
	return id, true
}

// requiredSources is the submission-count bar a recording must clear.
//
// It rises when the track has no usable artist tag to contradict the answer:
// there the gate is entirely AcoustID grading its own homework, so the
// evidence requirement goes up to compensate for the missing witness.
func requiredSources(in Input) int {
	if !in.HasLocalArtistWitness {
		return minSourcesNoLocalArtist
	}
	return minSources
}

// acceptRecordings is stage 3: narrow the cluster's recordings to those whose
// length agrees with the file, then require them to agree on one artist.
func acceptRecordings(in Input, top Result, tied bool) (Decision, RejectReason) {
	if len(top.Recordings) == 0 {
		return Decision{}, ReasonNoRecordings
	}
	survivors := recordingsMatchingDuration(top.Recordings, in.DurationSec)
	if len(survivors) == 0 {
		return Decision{}, ReasonDurationMismatch
	}
	// Sources is graded PER RECORDING, so it acts as a second survivor filter
	// rather than a cluster-level gate. That is more precise than it sounds: a
	// cluster where one recording is well attested and another carries a
	// single mis-tagged submission keeps the former and drops the latter,
	// which also removes a common cause of spurious artist disagreement.
	survivors = recordingsWithEnoughSources(survivors, requiredSources(in))
	if len(survivors) == 0 {
		return Decision{}, ReasonFewSources
	}
	// MusicBrainz's [unknown] asserts NO KNOWLEDGE of the performer, not a
	// different performer, so it must not get a vote in the consensus below.
	// Counting it as dissent vetoes matches with overwhelming support: ABBA's
	// "Waterloo" carries 6,259 submissions on one recording and was rejected
	// artist_disagreement because a placeholder-credited sibling with 8
	// cleared the sources filter. Both of that track's top two clusters
	// carried one, so this is common rather than a curiosity.
	//
	// Dropping it from the VOTE is a correction, not a relaxation. It can
	// never become the answer — if placeholders are all that survive there is
	// no performer to write and the track is refused below — and every real
	// recording still has to agree, so a genuinely dirty cluster is rejected
	// exactly as before.
	survivors = recordingsWithNamedArtist(survivors)
	if len(survivors) == 0 {
		return Decision{}, ReasonOnlyPlaceholderArtist
	}
	headID, headName, reason := headArtistConsensus(survivors)
	if reason != ReasonNone {
		return Decision{}, reason
	}
	d := Decision{
		ArtistMBID:    headID,
		ArtistName:    headName,
		RecordingMBID: soleRecordingMBID(survivors),
		AlbumHint:     soleReleaseGroupTitle(survivors),
		AcoustID:      top.ID,
		Score:         top.Score,
		Sources:       weakestSources(survivors),
	}
	if tied {
		// A competing cluster agreed on the artist, so the artist is earned —
		// either cluster writes the same MBID. Which cluster to take a
		// RECORDING or an album title from is undetermined, though, so those
		// are dropped rather than guessed. Suppressing them is what makes
		// accepting the tie conservative rather than merely permissive.
		d.RecordingMBID = ""
		d.AlbumHint = ""
	}
	return d, ReasonNone
}

// recordingsWithEnoughSources keeps the recordings whose fingerprint→recording
// link carries at least `required` independent submissions.
// recordingsWithNamedArtist drops recordings credited to MusicBrainz's
// [unknown] placeholder.
//
// Applied AFTER the sources filter and BEFORE the consensus vote. Order
// matters: a placeholder with few submissions is already gone, and what
// remains is the case this exists for — a well-attested placeholder sitting
// alongside the real credit for the same audio.
//
// A recording with NO artist at all is deliberately left in place, so that
// headArtistConsensus still refuses it with ReasonNoArtistMBID. Silently
// dropping those here would let a cluster be accepted on the strength of
// whatever else remained.
func recordingsWithNamedArtist(recordings []Recording) []Recording {
	var out []Recording
	for _, rec := range recordings {
		if len(rec.Artists) > 0 && rec.Artists[0].ID == unknownArtistMBID {
			continue
		}
		out = append(out, rec)
	}
	return out
}

func recordingsWithEnoughSources(recordings []Recording, required int) []Recording {
	var out []Recording
	for _, rec := range recordings {
		if rec.Sources >= required {
			out = append(out, rec)
		}
	}
	return out
}

// weakestSources reports the LOWEST submission count among the survivors —
// the weakest evidence the decision actually rests on. Every survivor has
// already cleared the bar, and the artist consensus spans all of them, so the
// minimum is the honest number to record rather than the flattering maximum.
func weakestSources(survivors []Recording) int {
	if len(survivors) == 0 {
		return 0
	}
	lowest := survivors[0].Sources
	for _, rec := range survivors[1:] {
		lowest = min(lowest, rec.Sources)
	}
	return lowest
}

// recordingsMatchingDuration keeps the recordings whose length agrees with the
// file's, within max(durationToleranceSec, durationToleranceFrac).
//
// A recording with no duration is REJECTED rather than waived. This is the
// deliberate inverse of analyze.decodedShortOfDuration, which fails OPEN on an
// unknown duration: there, an unknown length must not block a user's own file
// from being analysed; here, it must not let an unverifiable MBID into the
// library. Same arithmetic, opposite default, because the blast radius points
// the other way.
func recordingsMatchingDuration(recordings []Recording, durationSec float64) []Recording {
	tol := math.Max(durationToleranceSec, durationToleranceFrac*durationSec)
	var survivors []Recording
	for _, rec := range recordings {
		if rec.Duration <= 0 {
			continue
		}
		if math.Abs(rec.Duration-durationSec) <= tol {
			survivors = append(survivors, rec)
		}
	}
	return survivors
}

// headArtistConsensus requires every survivor to name the same PRIMARY
// credited artist.
//
// Keyed on the head rather than the full ordered credit: MusicBrainz routinely
// models one piece of audio as both "[Tony Bennett]" (original album) and
// "[Tony Bennett, Bill Evans]" (compilation), and a full-tuple rule would veto
// a match whose answer is identical either way. ArtistMBID holds one MBID, so
// the head is the right granularity for the field being written.
//
// Disagreement is a VETO; agreement is not evidence. Every recording on one
// AcoustID descends from a single submission lineage, so "four recordings all
// say Miles Davis" is one witness repeated, not four. Vetoes may only subtract.
func headArtistConsensus(survivors []Recording) (id, name string, reason RejectReason) {
	for _, rec := range survivors {
		if len(rec.Artists) == 0 || rec.Artists[0].ID == "" {
			return "", "", ReasonNoArtistMBID
		}
		if id == "" {
			id, name = rec.Artists[0].ID, rec.Artists[0].Name
			continue
		}
		if rec.Artists[0].ID != id {
			return "", "", ReasonArtistDisagreement
		}
	}
	return id, name, ReasonNone
}

// soleRecordingMBID returns the recording MBID when the survivors name exactly
// one, after deduplicating exact-equal IDs — AcoustID can list the same
// recording twice, and that is not ambiguity.
//
// It does NOT resolve MusicBrainz merge redirects, so two distinct-looking
// MBIDs that are really one entity still read as ambiguous and yield "".
// Resolving them would cost a MusicBrainz lookup per track to fill a field
// nothing currently consumes; leaving it empty is the conservative outcome.
func soleRecordingMBID(survivors []Recording) string {
	ids := distinctSorted(func(yield func(string)) {
		for _, rec := range survivors {
			yield(rec.ID)
		}
	})
	if len(ids) == 1 {
		return ids[0]
	}
	return ""
}

// soleReleaseGroupTitle returns a release-group title only when every
// surviving recording shares exactly one release group across all of them.
//
// That condition is the whole point: with one release group there is no
// ambiguity to launder, so the title is a fact the fingerprint supplied. With
// several, choosing one would be the uniform-draw error this package exists to
// avoid — so it returns "" and the caller falls back to the track's own album
// tag as the query term.
func soleReleaseGroupTitle(survivors []Recording) string {
	var title string
	ids := distinctSorted(func(yield func(string)) {
		for _, rec := range survivors {
			for _, rg := range rec.ReleaseGroups {
				if rg.ID == "" {
					continue
				}
				if title == "" {
					title = rg.Title
				}
				yield(rg.ID)
			}
		}
	})
	if len(ids) == 1 {
		return title
	}
	return ""
}

// distinctSorted collects the non-empty values produced by seq and returns
// them deduplicated and sorted, so callers get a deterministic answer.
func distinctSorted(seq func(yield func(string))) []string {
	seen := map[string]struct{}{}
	var out []string
	seq(func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, dup := seen[v]; dup {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	})
	sort.Strings(out)
	return out
}
