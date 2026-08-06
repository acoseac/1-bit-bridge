package enrich

import (
	"context"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/metrics"
)

// AcousticMatch is what the fingerprint pipeline concluded about one track.
//
// Note what is absent: no release MBID and no artwork MBID. A fingerprint
// identifies audio, AcoustID maps audio to a recording, and one recording sits
// under many releases precisely because they contain the same audio — so
// choosing one would be a uniform draw dressed as a match. An album MBID is
// still reachable from here, but only by running the EXISTING text ladder with
// the recovered artist name and letting pickBestRelease decide.
type AcousticMatch struct {
	// ArtistMBID is the head credited artist, agreed across every recording
	// the gate accepted.
	ArtistMBID string
	// ArtistName is that artist's canonical MusicBrainz name — used as a
	// QUERY term for the release ladder, and as the value the local-artist
	// veto below is checked against.
	ArtistName string
	// RecordingMBID is set only when the gate resolved exactly one recording.
	// Often empty on a perfectly good match; that is the conservative outcome,
	// not a defect.
	RecordingMBID string
	// AlbumHint is a release-group TITLE, set only when the accepted
	// recordings share exactly one release group — i.e. when there is no
	// ambiguity to launder. A query term, never an identifier to store.
	AlbumHint string
	// AcoustID is the cluster that produced the match, recorded as provenance
	// so the writes are attributable and reversible.
	AcoustID string
}

// AcousticLookup reads a fingerprint verdict for a track.
//
// Deliberately a pure read: no context, no I/O, no error. The implementation
// is an in-memory cache that a separate sweeper populates, so consulting it
// from the enricher's single goroutine costs a map lookup. That is what lets
// the fallback sit on the enricher's hot path without giving the enricher a
// filesystem dependency — an os.Stat on a hung network mount would otherwise
// block the one goroutine that drives all enrichment.
//
// A nil AcousticLookup means fingerprinting is off, which is the default.
type AcousticLookup interface {
	// LookupPath takes the client-relative path the enricher already holds.
	LookupPath(clientPath string) (AcousticMatch, bool)
}

// WithAcousticFallback attaches the fingerprint verdict source. Returns the
// receiver for fluent setup, matching WithITunes / WithPremiumCovers.
func (e *Enricher) WithAcousticFallback(l AcousticLookup) *Enricher {
	e.acoustic = l
	return e
}

// junkArtistTags are the tag values that carry no artist information, so a
// track bearing one has no witness with which to contradict a fingerprint.
//
// CLOSED, TINY, AND MATCHED FOLD-EXACT. An over-eager junk detector is worse
// than none: it disables the local-artist veto precisely on the tracks where
// that veto is the only independent evidence available. Anything not on this
// list is a real artist and DOES get to veto.
//
// Bracketed and parenthesised forms need no entries of their own —
// foldForMatch maps every non-alphanumeric except apostrophes to a space and
// collapses runs, so "[Unknown Artist]" and "(no artist)" already fold onto
// entries here.
var junkArtistTags = map[string]struct{}{
	"an unknown artist": {},
	"unknown artist":    {},
	"unknown":           {},
	"various artists":   {},
	"various":           {},
	"va":                {},
	"no artist":         {},
	"artist":            {},
	"untitled":          {},
	"none":              {},
	"n a":               {}, // "N/A" — the slash folds to a space
}

// isDiscOrTrackLabel reports whether an ALREADY-FOLDED tag is a disc or track
// label that leaked out of a folder name — "CD 01", "Disc 2", "Track 7".
//
// Shared by both artist-tag predicates on purpose: of everything they do, this
// is the one shape they genuinely agree on. A folder label is neither a usable
// witness nor a searchable name, so both want it, and naming it once keeps the
// two from drifting on the one rule they should share. Everything else about
// them differs deliberately — see isUnsearchableArtistTag.
//
// Takes the folded form because both callers have already folded.
func isDiscOrTrackLabel(folded string) bool {
	for _, prefix := range []string{"track ", "cd ", "disc ", "disk "} {
		if rest, ok := strings.CutPrefix(folded, prefix); ok && isAllDigits(rest) {
			return true
		}
	}
	return false
}

// isJunkArtistTag reports whether an artist tag is too generic to contradict
// anything.
//
// Fold-exact plus two shapes that are structurally generic: a purely numeric
// tag, and "track N" / "cd N" / "disc N". Deliberately NOT substring matching:
// "Artist Name - Unknown" folds to "artist name unknown", which is a real
// enough artist to be worth vetoing with, and a contains-check would classify
// it as junk and silently drop the veto.
func isJunkArtistTag(artist string) bool {
	folded := foldName(artist)
	if folded == "" {
		// Nothing survived folding — blank, whitespace, or a name made
		// entirely of punctuation ("!!!", "()"). The last case is a REAL
		// artist, but it is still not a usable witness: foldedTokenContains
		// refuses an empty needle, so there is nothing here to compare a
		// fingerprint answer against either way. Treating it as "no witness"
		// costs that track a higher submission-count bar at the gate, which
		// is the correct conservative outcome rather than a misclassification.
		return true
	}
	if _, ok := junkArtistTags[folded]; ok {
		return true
	}
	if isAllDigits(folded) {
		return true
	}
	return isDiscOrTrackLabel(folded)
}

// isUnsearchableArtistTag reports whether an artist tag is one MusicBrainz
// cannot possibly answer, so the query should never be sent.
//
// Consulted by BOTH ladders, per rung — buildArtistLadder and
// buildReleaseLadder each drop a rung whose artist is unanswerable. Per rung
// rather than per track, because a track carrying such a tag usually still has
// an albumArtist worth asking about, and gating on the tag would discard that
// rung along with the pointless one.
//
// # This is NOT isJunkArtistTag, and the two must not be merged
//
// They look like duplicates and are not. The sets differ because a false
// positive costs something different in each.
//
// For the veto, calling a real artist junk removes the local witness, and the
// gate answers by demanding more submissions — stricter, still safe. So that
// set can afford to be broad, and is: it includes all-digit names, "various
// artists", and names that fold to nothing.
//
// Here, calling a real artist junk means the query is NEVER SENT, and the track
// loses a text match it would have got. That is a permanent correctness loss,
// not a stricter check, so the broad entries are exactly wrong:
//
//   - all-digits — 112 and 311 are real bands
//   - "various artists" / "various" / "va" — a real MusicBrainz
//     special-purpose artist that resolves today
//   - folds-to-empty — "!!!" is a real band
//
// What remains is only what cannot name an artist under any reading: a disc or
// track label that leaked out of a folder name, and the explicit placeholder
// phrases taggers write when they have nothing. Bare "unknown", "none",
// "untitled" and "artist" are deliberately absent — each is a plausible band
// name, and one wasted query is the cheaper mistake.
//
// Skipping also avoids a bad write: "An Unknown Artist" put through the fuzzy
// A4 pass can clear its threshold against a placeholder-shaped entity, which
// would stamp a meaningless MBID on the track.
//
// The asymmetry argument above only gets STRONGER now that the release ladder
// consults this too: a false positive costs the track its album match as well
// as its artist. Do not widen this set.
func isUnsearchableArtistTag(artist string) bool {
	return isUnsearchableArtistFolded(foldName(artist))
}

// isUnsearchableArtistFolded is isUnsearchableArtistTag over an ALREADY-FOLDED
// tag, for the ladder builders — both of them fold the same string again a
// line later to build their dedup key, and foldForMatch is not free (NFKD, a
// rune pass, a case-fold and a re-join).
//
// Same convention as isDiscOrTrackLabel above, and the same hazard: passing a
// raw tag here compares it against folded constants and silently answers
// false. Call isUnsearchableArtistTag unless the fold is already in hand.
func isUnsearchableArtistFolded(folded string) bool {
	if folded == "" {
		return false // may be a real name that folds away; let it search
	}
	switch folded {
	case "an unknown artist", "unknown artist", "no artist":
		return true
	}
	return isDiscOrTrackLabel(folded)
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// acousticMatchContradictsTag is the local-artist veto: the single most
// valuable check in the whole fingerprint path, because it is the ONLY one
// using information the fingerprint pipeline did not produce. Every clause
// inside the gate is AcoustID grading its own homework.
//
// It only ever VETOES. Agreement grants nothing that the gate had not already
// established, so this can subtract confidence but never add it — the same
// property that makes pickBestArtist's ordered passes safe.
//
// Returns false when there is nothing to check against: a blank or junk tag
// means no witness, which is exactly the population fingerprinting exists for.
// The gate compensates there by requiring more independent submissions.
func acousticMatchContradictsTag(localArtist string, m AcousticMatch) bool {
	if isJunkArtistTag(localArtist) {
		return false
	}
	if m.ArtistName == "" {
		return false
	}
	// Both sides folded, then token-containment in either direction — the
	// same comparison the release path uses, so "Bill Withers" agrees with
	// "Bill Withers & Friends" and a transliteration or punctuation
	// difference does not read as a contradiction.
	return !foldedTokenContains(foldName(m.ArtistName), foldName(localArtist))
}

// HasUsableArtistWitness reports whether a tag can contradict a fingerprint.
//
// Exported because the SWEEPER needs it: the gate raises its submission-count
// bar when no witness exists, and that decision is made where the fingerprint
// is taken, not here. The junk classification lives in this package because it
// uses the enricher's match-folding vocabulary.
func HasUsableArtistWitness(localArtist string) bool {
	return !isJunkArtistTag(localArtist)
}

// acousticOutcome distinguishes the ways the fallback can decline, so the
// skip-reason counters stay meaningful.
//
// Collapsing these would make the metric lie: a library with full fingerprint
// coverage but many vetoes would read identically to one with no fingerprints
// at all, and the veto rate is precisely the number worth watching — a spike in
// it means the pipeline is contradicting the library's own tags.
type acousticOutcome int

const (
	// acousticNoVerdict — nothing has been fingerprinted for this track yet,
	// or the gate refused it. A coverage signal.
	acousticNoVerdict acousticOutcome = iota
	// acousticRefused — a verdict existed and THIS layer rejected it: the
	// local artist tag contradicted it, or an MBID failed validation. A
	// disagreement signal.
	acousticRefused
	// acousticApplied — the artist was recovered.
	acousticApplied
)

// applyAcousticFallback consults the fingerprint verdict for a track and, if
// it survives the local-artist veto, writes the artist onto the track.
//
// Writes ONLY the artist MBID, its name, and the recording MBID — never a
// release or artwork MBID; see AcousticMatch for why that restriction is
// structural rather than a policy.
func (e *Enricher) applyAcousticFallback(t *manifest.Track) (AcousticMatch, acousticOutcome) {
	if e.acoustic == nil {
		return AcousticMatch{}, acousticNoVerdict
	}
	m, ok := e.acoustic.LookupPath(t.Path)
	if !ok || m.ArtistMBID == "" {
		return AcousticMatch{}, acousticNoVerdict
	}
	// Validate before the value can reach a URL or a cache path: the same
	// F30 rationale the MusicBrainz results are held to. AcoustID is a
	// third-party JSON source and ArtistMBID lands in ArtistImagePath's
	// filepath.Join as a leading component.
	if !isValidMBID(m.ArtistMBID) {
		logger.Warn("ignoring non-UUID fingerprint artist MBID",
			"path", t.Path, "value", truncateForLog(m.ArtistMBID))
		return AcousticMatch{}, acousticRefused
	}
	// The veto — the only check using information the fingerprint pipeline
	// did not produce.
	if acousticMatchContradictsTag(t.Artist, m) {
		logger.Info("fingerprint match contradicts the local artist tag; ignoring",
			"path", t.Path, "tagged", t.Artist, "fingerprinted", m.ArtistName)
		return AcousticMatch{}, acousticRefused
	}

	// Do NOT overwrite an artist the text path already resolved. Reaching
	// here with one set means pickBestArtist accepted a real tag, which is at
	// least as trustworthy as audio — the fingerprint is being consulted for
	// the ALBUM in that case, not the artist.
	if t.ArtistMBID == "" {
		t.ArtistMBID = m.ArtistMBID
	}
	if m.RecordingMBID != "" && isValidMBID(m.RecordingMBID) {
		t.MusicBrainzTrackID = m.RecordingMBID
	}
	logger.Info("artist recovered by acoustic fingerprint",
		"path", t.Path, "artist", m.ArtistName, "mbid", m.ArtistMBID, "acoustid", m.AcoustID)
	return m, acousticApplied
}

// enrichWithRecoveredArtist finishes a track whose artist came from the audio
// rather than its tags.
//
// It runs the Deezer portrait fetch and stamps the track done. The ALBUM is
// deliberately not pursued here: reaching one means running the existing text
// ladder with the recovered artist name, which is the album hop — see
// resolveAlbumFromAcoustic. Splitting the two keeps this function honest about
// what it knows.
func (e *Enricher) enrichWithRecoveredArtist(ctx context.Context, t *manifest.Track, m AcousticMatch) {
	if !e.applyAlbumHop(ctx, t, m) {
		return // transient failure: leave enriched_at alone so the worker retries
	}
	e.fetchRecoveredArtistImage(ctx, t, m)
	e.stampEnriched(ctx, t)
}

// applyAlbumHop resolves and applies the album, returning false only when the
// caller must NOT stamp the track — i.e. on a transient upstream failure,
// where leaving enriched_at alone is what lets the worker retry.
//
// A miss is normal and returns true: most of this population has no album to
// find, and stamping them is correct.
func (e *Enricher) applyAlbumHop(ctx context.Context, t *manifest.Track, m AcousticMatch) bool {
	releaseMBID, rgMBID, err := e.resolveAlbumFromAcoustic(ctx, t, m)
	if err != nil {
		if ctx.Err() != nil || IsTransient(err) {
			return false
		}
		logger.Error("MB search (fingerprint artist)", "path", t.Path, "err", err)
		return true
	}
	if releaseMBID == "" {
		return true
	}
	t.MusicBrainzAlbumID = releaseMBID
	// Artwork rides the same chain every other release does — nothing
	// fingerprint-specific about it once a real release MBID exists.
	if strings.HasPrefix(t.ArtworkMBID, "local-") {
		return true
	}
	// The SAME album term the release search used: the iTunes fallback inside
	// ensureArtworkCached searches by (artist, album), so a junk local title
	// here would quietly lose that fallback.
	cached, aerr := e.ensureArtworkCached(ctx, releaseMBID, rgMBID, m.ArtistName, albumSearchTerm(t, m), 500)
	if aerr != nil {
		if ctx.Err() == nil {
			logger.Error("artwork", "mbid", releaseMBID, "err", aerr)
		}
		return true
	}
	if cached {
		t.ArtworkMBID = releaseMBID
	}
	return true
}

// fetchRecoveredArtistImage fetches the Deezer portrait for a
// fingerprint-recovered artist.
//
// It uses the CANONICAL name from the match rather than the track's own tag,
// because on this path that tag is exactly the thing that could not be trusted
// — for a junk-tagged file it is "An Unknown Artist" or a folder name.
//
// Tier-2 throughout: any failure is logged and absorbed, so a portrait miss
// never blocks the already-resolved MBIDs from committing.
func (e *Enricher) fetchRecoveredArtistImage(ctx context.Context, t *manifest.Track, m AcousticMatch) {
	if t.ArtistMBID == "" || e.deezer == nil || e.deezerNegCache.Has(t.ArtistMBID) {
		return
	}
	// The name and the MBID must describe the SAME entity — the portrait is
	// fetched by name and hardlinked under the MBID, so a mismatch caches
	// one artist's face against another's id permanently.
	//
	// applyAcousticFallback deliberately does NOT overwrite an artist the
	// text path already resolved (a real tag pickBestArtist accepted is at
	// least as trustworthy as audio; the fingerprint is being consulted for
	// the ALBUM in that case). So reaching here with the two differing means
	// the MBID is the text path's and m.ArtistName is not its name. The text
	// path already owns that MBID's portrait — it fetched it, neg-cached it,
	// or hit a transient error worth retrying with the RIGHT name — and this
	// function is for a RECOVERED artist. There is nothing here to do.
	if t.ArtistMBID != m.ArtistMBID {
		return
	}
	found, err := e.ensureArtistImageCached(ctx, t.ArtistMBID, m.ArtistName)
	if err != nil {
		if ctx.Err() == nil {
			logger.Error("artist image", "mbid", t.ArtistMBID, "err", err)
		}
		return
	}
	if !found {
		e.deezerNegCache.Set(t.ArtistMBID, struct{}{})
	}
}

// resolveAlbumFromAcoustic is the album hop: it turns a fingerprint-recovered
// artist into a release MBID by running the EXISTING text ladder and letting
// pickBestRelease decide, unchanged.
//
// This is the whole design in one function. The fingerprint supplies the tag
// that was missing or wrong — an artist NAME — and the acceptance layer that
// already guards every other release write makes the call. The fingerprint
// never picks a release itself, because it cannot: one recording sits under
// many release groups precisely because they contain the same audio.
//
// The album TERM comes from the track's own tag by preference, and from the
// fingerprint's AlbumHint only when the local tag is unusable AND the hint is
// unambiguous — the gate sets it only when the accepted recordings share
// exactly one release group. That ordering matters: preferring the local tag
// keeps a real album title in charge, and falling back to the hint covers the
// junk-tagged case ("CD 03") that has no title to search by at all.
//
// Returns "" when nothing acceptable was found, which is the normal outcome
// and not an error. A transient upstream failure returns an error so the
// caller can leave enriched_at alone and retry — the same contract every other
// search path here honours.
func (e *Enricher) resolveAlbumFromAcoustic(ctx context.Context, t *manifest.Track, m AcousticMatch) (string, string, error) {
	album := albumSearchTerm(t, m)
	if album == "" || m.ArtistName == "" {
		return "", "", nil
	}

	// Share the text path's album cache, under a SEPARATE key space.
	//
	// Sharing the cache is what keeps sibling tracks under one junk-tagged
	// folder from each paying their own SearchRelease plus a full
	// MBMinInterval sleep — 1.1s each against public MusicBrainz, on exactly
	// the population this feature targets.
	//
	// Sharing the KEY was a bug. This comment used to claim "the key
	// semantics are the same as the text path's, so a hit from either side is
	// the answer to the same question"; PR #614 moved the text path to
	// releaseCacheKey and that stopped being true. See acousticCacheKey for
	// what the collision cost.
	key := acousticCacheKey(m.ArtistName, album)
	if hit, ok := e.albumCache.Get(key); ok {
		metrics.RecordMBCache("album", true)
		return hit.ReleaseMBID, hit.ReleaseGroupMBID, nil
	}
	metrics.RecordMBCache("album", false)

	// A single rung, deliberately: the ladder's own fallbacks exist to repair
	// tags, and both terms here are already canonical — the artist name comes
	// from MusicBrainz via AcoustID, and the album is either the operator's own
	// title or a MusicBrainz release-group title. Retrying variations of an
	// already-canonical query would only widen the candidate space.
	if !sleepCtx(ctx, e.MBMinInterval) {
		return "", "", ctx.Err()
	}
	res, err := e.mb.SearchRelease(ctx, m.ArtistName, album)
	if err != nil {
		// Deliberately NOT cached. A transient failure must not poison the key
		// for every sibling track — the same rule the text path follows.
		return "", "", err
	}
	resolution := albumResolution{}
	if res != nil {
		if isValidMBID(res.MBID) {
			resolution.ReleaseMBID = res.MBID
		}
		if isValidMBID(res.ReleaseGroupMBID) {
			resolution.ReleaseGroupMBID = res.ReleaseGroupMBID
		}
	}
	// Cached on a clean search whether or not it matched. Correct now that
	// the key is namespaced: this path's ladder IS the single rung above, so
	// a no-match here is a COMPLETE answer for anything that reaches it with
	// the same (artistName, album) — unlike a no-match written under the text
	// path's key, which would pre-empt rungs that path had yet to try.
	// Siblings are the reason this cache exists.
	e.albumCache.Set(key, resolution)
	if resolution.ReleaseMBID == "" {
		return "", "", nil
	}
	rg := resolution.ReleaseGroupMBID
	logger.Info("album recovered via fingerprint artist",
		"path", t.Path, "searchedArtist", m.ArtistName, "searchedAlbum", album,
		"release", resolution.ReleaseMBID)
	return resolution.ReleaseMBID, rg, nil
}

// albumSearchTerm picks the album string to search MusicBrainz by.
//
// The track's OWN tag wins whenever it is usable — a real title the operator
// curated outranks anything the fingerprint suggests. The gate's hint fills in
// only when the local tag has nothing to search by ("CD 03"), and the gate
// sets that hint only when the accepted recordings share exactly one release
// group, so it is never a guess between several.
//
// One function because two callers need the same answer: the release search
// and the artwork chain's iTunes fallback, which also searches by
// (artist, album). Computing it twice would let the two drift, and a junk
// album name reaching iTunes silently loses the fallback.
func albumSearchTerm(t *manifest.Track, m AcousticMatch) string {
	if album := strings.TrimSpace(t.Album); !isJunkAlbumTag(album) {
		return album
	}
	return strings.TrimSpace(m.AlbumHint)
}

// junkAlbumTags mirrors junkArtistTags for album titles: values that cannot
// usefully be searched for. Same closed-list discipline and the same reason —
// over-matching here would discard a real album title in favour of a
// fingerprint hint, which is the wrong way round.
var junkAlbumTags = map[string]struct{}{
	"unknown album": {},
	"unknown":       {},
	"untitled":      {},
	"various":       {},
	"none":          {},
	"album":         {},
	"n a":           {},
}

// isJunkAlbumTag reports whether an album tag is too generic to search by.
// The "cd N" / "disc N" shape is the common one in practice: a ripper that
// wrote the disc folder name into the album field.
//
// NOTE the asymmetry with isJunkArtistTag: an all-digits ALBUM title is NOT
// junk, though an all-digits artist is. Numeric album titles are genuinely
// common — "1" (The Beatles), "4", "21", "1989", "90125" — and the two
// misclassifications cost very different things. Calling a real artist junk
// only removes a witness, which raises the gate's evidence bar; harmless.
// Calling a real ALBUM junk SUBSTITUTES the fingerprint's release-group title
// for the operator's own, which risks resolving a different release. A failed
// search for a numeric title costs nothing, so the safe direction is to leave
// it alone.
func isJunkAlbumTag(album string) bool {
	folded := foldTitle(album)
	if folded == "" {
		return true
	}
	if _, ok := junkAlbumTags[folded]; ok {
		return true
	}
	for _, prefix := range []string{"cd ", "disc ", "disk ", "track "} {
		if rest, ok := strings.CutPrefix(folded, prefix); ok && isAllDigits(rest) {
			return true
		}
	}
	return false
}
