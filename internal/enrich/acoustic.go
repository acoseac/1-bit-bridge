package enrich

import (
	"context"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
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
	for _, prefix := range []string{"track ", "cd ", "disc ", "disk "} {
		if rest, ok := strings.CutPrefix(folded, prefix); ok && isAllDigits(rest) {
			return true
		}
	}
	return false
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

// applyAcousticFallback consults the fingerprint verdict for a track and, if
// it survives the local-artist veto, writes the artist onto the track.
//
// Returns true when it recovered something. Writes ONLY the artist MBID, its
// name, and the recording MBID — never a release or artwork MBID; see
// AcousticMatch for why that restriction is structural rather than a policy.
func (e *Enricher) applyAcousticFallback(t *manifest.Track) (AcousticMatch, bool) {
	if e.acoustic == nil {
		return AcousticMatch{}, false
	}
	m, ok := e.acoustic.LookupPath(t.Path)
	if !ok || m.ArtistMBID == "" {
		return AcousticMatch{}, false
	}
	// Validate before the value can reach a URL or a cache path: the same
	// F30 rationale the MusicBrainz results are held to. AcoustID is a
	// third-party JSON source and ArtistMBID lands in ArtistImagePath's
	// filepath.Join as a leading component.
	if !isValidMBID(m.ArtistMBID) {
		logger.Warn("ignoring non-UUID fingerprint artist MBID",
			"path", t.Path, "value", truncateForLog(m.ArtistMBID))
		return AcousticMatch{}, false
	}
	// The veto — the only check using information the fingerprint pipeline
	// did not produce.
	if acousticMatchContradictsTag(t.Artist, m) {
		logger.Info("fingerprint match contradicts the local artist tag; ignoring",
			"path", t.Path, "tagged", t.Artist, "fingerprinted", m.ArtistName)
		return AcousticMatch{}, false
	}

	t.ArtistMBID = m.ArtistMBID
	if m.RecordingMBID != "" && isValidMBID(m.RecordingMBID) {
		t.MusicBrainzTrackID = m.RecordingMBID
	}
	logger.Info("artist recovered by acoustic fingerprint",
		"path", t.Path, "artist", m.ArtistName, "mbid", m.ArtistMBID, "acoustid", m.AcoustID)
	return m, true
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
	// The album hop: try to turn the recovered artist into a release through
	// the existing text acceptance. A miss is normal and costs nothing; a
	// TRANSIENT failure returns without stamping so the worker retries, which
	// is the same contract the tag-driven paths honour.
	releaseMBID, rgMBID, err := e.resolveAlbumFromAcoustic(ctx, t, m)
	if err != nil {
		if ctx.Err() == nil && !IsTransient(err) {
			logger.Error("MB search (fingerprint artist)", "path", t.Path, "err", err)
		} else {
			return
		}
	}
	if releaseMBID != "" {
		t.MusicBrainzAlbumID = releaseMBID
		// Artwork rides the same chain every other release does — nothing
		// fingerprint-specific about it once a real release MBID exists.
		if !strings.HasPrefix(t.ArtworkMBID, "local-") {
			if cached, aerr := e.ensureArtworkCached(ctx, releaseMBID, rgMBID, m.ArtistName, t.Album, 500); aerr != nil {
				if ctx.Err() == nil {
					logger.Error("artwork", "mbid", releaseMBID, "err", aerr)
				}
			} else if cached {
				t.ArtworkMBID = releaseMBID
			}
		}
	}

	// The portrait lookup needs an artist NAME, and on this path the track's
	// own tag is exactly the thing that could not be trusted — for a
	// junk-tagged file it is "An Unknown Artist" or a folder name. Use the
	// canonical name the fingerprint resolved.
	if t.ArtistMBID != "" && e.deezer != nil && !e.deezerNegCache.Has(t.ArtistMBID) {
		found, err := e.ensureArtistImageCached(ctx, t.ArtistMBID, m.ArtistName)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("artist image", "mbid", t.ArtistMBID, "err", err)
			}
		} else if !found {
			e.deezerNegCache.Set(t.ArtistMBID, struct{}{})
		}
	}
	if err := e.store.MarkEnriched(ctx, t); err != nil {
		logger.Error("mark enriched", "path", t.Path, "err", err)
		return
	}
	e.done.Add(1)
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
	album := strings.TrimSpace(t.Album)
	if isJunkAlbumTag(album) {
		album = strings.TrimSpace(m.AlbumHint)
	}
	if album == "" || m.ArtistName == "" {
		return "", "", nil
	}

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
		return "", "", err
	}
	if res == nil || !isValidMBID(res.MBID) {
		return "", "", nil
	}
	rg := ""
	if isValidMBID(res.ReleaseGroupMBID) {
		rg = res.ReleaseGroupMBID
	}
	logger.Info("album recovered via fingerprint artist",
		"path", t.Path, "searchedArtist", m.ArtistName, "searchedAlbum", album, "release", res.MBID)
	return res.MBID, rg, nil
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
