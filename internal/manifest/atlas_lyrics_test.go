package manifest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

func atlasDoc(body string, synced bool) lyrics.Doc {
	f := lyrics.FormatText
	if synced {
		f = lyrics.FormatLRC
	}
	return lyrics.Doc{Format: f, Synced: synced, Body: body}
}

// seedTrack inserts a track carrying the MBIDs and tags the candidate query
// reads, so the tests drive the REAL query rather than a hand-built row.
func seedAtlasTrack(t *testing.T, s *Store, path, albumMBID, recMBID, title string, disc, num int, dur float64) {
	t.Helper()
	tr := &Track{
		Path: path, Size: 1, ModTime: time.Unix(0, 0).UTC(),
		Title: title, MusicBrainzAlbumID: albumMBID, MusicBrainzTrackID: recMBID,
	}
	if disc > 0 {
		tr.DiscNumber = &disc
	}
	if num > 0 {
		tr.TrackNumber = &num
	}
	if dur > 0 {
		tr.Duration = &dur
	}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
}

func atlasCandidatePaths(t *testing.T, s *Store) []string {
	t.Helper()
	cands, err := s.AtlasLyricsCandidates(context.Background(), time.Now().UnixNano(), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Path)
	}
	return out
}

// A network row must survive a scan that finds no local document.
//
// This is the whole reason the tier needs a distinct source. Every branch of
// writeLyricsRowTx reasons from a LOCAL extraction, and "the file has no
// lyrics" is evidence about the file — which says nothing whatsoever about a
// document Atlas supplied. Before the guard, the unconditional DELETE that is
// right for a vanished sidecar reaped the Atlas row on the very next scan, and
// on every scan after it.
func TestAScanDoesNotReapANetworkLyricsRow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)

	ok, err := s.UpsertAtlasLyrics(ctx, "a/x.flac", atlasDoc("[00:01.00] la", true), lyrics.SourceAtlasLRC)
	if err != nil || !ok {
		t.Fatalf("seed the network row: ok=%v err=%v", ok, err)
	}

	// A rescan of the same file, which finds nothing locally.
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)

	got, err := s.GetLyrics(ctx, "a/x.flac")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("the scan reaped the Atlas row")
	}
	if got.Source != string(lyrics.SourceAtlasLRC) {
		t.Errorf("source = %q, want %q", got.Source, lyrics.SourceAtlasLRC)
	}
}

// Rank arbitrates, in both directions, and the ladder is the only judge.
func TestALocalDocumentTakesTheRowOnlyWhenItOutranksTheNetworkOne(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		network    lyrics.Source
		local      string
		wantSource string
	}{
		// The app's DD3 rule: an UNSYNCED embedded tag does not displace a
		// SYNCED network document, because timing outranks source.
		{"plain text does not displace synced atlas", lyrics.SourceAtlasLRC,
			string(lyrics.SourceTextPlain), string(lyrics.SourceAtlasLRC)},
		{"a txt sidecar does not displace synced atlas", lyrics.SourceAtlasLRC,
			string(lyrics.SourceSidecarText), string(lyrics.SourceAtlasLRC)},
		// ...but a TIMED local document does, because between two timed
		// documents the operator's own file is the better authority.
		{"an lrc sidecar displaces synced atlas", lyrics.SourceAtlasLRC,
			string(lyrics.SourceSidecarLRC), string(lyrics.SourceSidecarLRC)},
		{"embedded LRC-shaped text displaces synced atlas", lyrics.SourceAtlasLRC,
			string(lyrics.SourceTextLRC), string(lyrics.SourceTextLRC)},
		// Plain atlas is last outright: any local document beats it.
		{"plain text displaces plain atlas", lyrics.SourceAtlas,
			string(lyrics.SourceTextPlain), string(lyrics.SourceTextPlain)},
		{"a txt sidecar displaces plain atlas", lyrics.SourceAtlas,
			string(lyrics.SourceSidecarText), string(lyrics.SourceSidecarText)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
			if _, err := s.UpsertAtlasLyrics(ctx, "a/x.flac",
				atlasDoc("[00:01.00] net", tc.network == lyrics.SourceAtlasLRC), tc.network); err != nil {
				t.Fatal(err)
			}
			doc := &extractedLyrics{
				Format: "text", Body: "local words", Source: tc.local,
				Tag: lyrics.Tag(lyrics.Doc{Format: "text", Body: "local words"}),
			}
			if err := s.UpsertTrack(ctx, &Track{
				Path: "a/x.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), lyrics: doc,
			}); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetLyrics(ctx, "a/x.flac")
			if err != nil || got == nil {
				t.Fatalf("row vanished: %v", err)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

// A sweep must not demote a row it did not write.
//
// The endpoint serves ONE row, so a write that loses the ladder is a write that
// makes the library worse. It also strict-advances indexed_at, which is how a
// losing write becomes a delta pushed to every paired device.
func TestUpsertAtlasLyricsRefusesToDemote(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
	doc := &extractedLyrics{
		Format: "lrc", Synced: true, Body: "[00:02.00] mine", Source: string(lyrics.SourceSidecarLRC),
		SidecarName: "x.lrc",
		Tag:         lyrics.Tag(lyrics.Doc{Format: "lrc", Synced: true, Body: "[00:02.00] mine"}),
	}
	if err := s.UpsertTrack(ctx, &Track{
		Path: "a/x.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), lyrics: doc,
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetLyrics(ctx, "a/x.flac")
	if err != nil || before == nil {
		t.Fatal("seed the sidecar row")
	}

	took, err := s.UpsertAtlasLyrics(ctx, "a/x.flac",
		atlasDoc("[00:01.00] theirs", true), lyrics.SourceAtlasLRC)
	if err != nil {
		t.Fatal(err)
	}
	if took {
		t.Error("the sweep displaced a better-ranked local document")
	}
	after, _ := s.GetLyrics(ctx, "a/x.flac")
	if after.Source != string(lyrics.SourceSidecarLRC) || after.Body != before.Body {
		t.Errorf("the sidecar row was modified: %q / %q", after.Source, after.Body)
	}
}

// The provenance columns stay ZERO, and that is load-bearing rather than
// tidiness: /v1/lyrics stats the lyrics source and answers 410 when it drifted.
// A network document has no such file, so anything else stored here — the audio
// file's stat included — makes an unrelated tag edit answer 410 for a document
// that never came from it.
func TestANetworkRowCarriesNoLocalProvenance(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
	if _, err := s.UpsertAtlasLyrics(ctx, "a/x.flac", atlasDoc("words", false), lyrics.SourceAtlas); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetLyrics(ctx, "a/x.flac")
	if got.SourceMTimeNS != 0 || got.SourceSize != 0 || got.SidecarName != "" {
		t.Errorf("network row carries local provenance: mtime=%d size=%d sidecar=%q",
			got.SourceMTimeNS, got.SourceSize, got.SidecarName)
	}
}

// Writing the network row advances the delta cursor, because a client that
// already synced this track has to be told the lyrics appeared.
func TestUpsertAtlasLyricsAdvancesTheDeltaCursor(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
	seedAtlasTrack(t, s, "a/y.flac", "album-1", "", "Other", 1, 2, 100)

	before := indexedAtOf(t, s, "a/x.flac")
	if _, err := s.UpsertAtlasLyrics(ctx, "a/x.flac", atlasDoc("words", false), lyrics.SourceAtlas); err != nil {
		t.Fatal(err)
	}
	after := indexedAtOf(t, s, "a/x.flac")
	if after <= before {
		t.Errorf("indexed_at did not advance: %d -> %d", before, after)
	}
	// Strict advance means past the LIBRARY-wide max, not merely past its own
	// prior value — a cursor a client already holds would drop the row.
	if after <= indexedAtOf(t, s, "a/y.flac") {
		t.Error("indexed_at did not clear the library-wide max")
	}
}

// The candidate query's gate is the ABSENCE of a row, not the verdict — which
// is what makes the tier self-healing when a local document is later deleted.
func TestAtlasLyricsCandidatesGateOnTheRowNotTheVerdict(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)

	if !contains(atlasCandidatePaths(t, s), "a/x.flac") {
		t.Fatal("a bare track with an album MBID must be a candidate")
	}

	// A successful fetch: the row exists, so it stops being a candidate even
	// though its attempt has no future due time.
	if _, err := s.UpsertAtlasLyrics(ctx, "a/x.flac", atlasDoc("words", false), lyrics.SourceAtlas); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAtlasLyricsAttempt(ctx, "a/x.flac", "album-1", "", "rec-1",
		AtlasLyricsAvailable, 0); err != nil {
		t.Fatal(err)
	}
	if contains(atlasCandidatePaths(t, s), "a/x.flac") {
		t.Error("a track that HAS lyrics is still being offered")
	}

	// The operator deletes the document. This goes through the REAL scanner
	// path — a local row that a rescan no longer finds — because that is how
	// it happens in production, and a hand-rolled DELETE here would prove
	// nothing about writeLyricsRowTx.
	local := &extractedLyrics{
		Format: "text", Body: "mine", Source: string(lyrics.SourceTextPlain),
		Tag: lyrics.Tag(lyrics.Doc{Format: "text", Body: "mine"}),
	}
	if err := s.UpsertTrack(ctx, &Track{
		Path: "a/x.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), lyrics: local,
	}); err != nil {
		t.Fatal(err)
	}
	// ...and now the rescan finds none: the LOCAL row is reaped as always.
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
	if got, _ := s.GetLyrics(ctx, "a/x.flac"); got != nil {
		t.Fatal("a local row must still be reaped when the file loses its lyrics")
	}
	cands, err := s.AtlasLyricsCandidates(ctx, time.Now().UnixNano(), 100)
	if err != nil {
		t.Fatal(err)
	}
	var found *AtlasLyricsCandidate
	for i := range cands {
		if cands[i].Path == "a/x.flac" {
			found = &cands[i]
		}
	}
	if found == nil {
		t.Fatal("a track whose lyrics were deleted never returns as a candidate")
	}
	if found.CachedMBID != "rec-1" {
		t.Errorf("the resolved recording MBID was not carried back: %q", found.CachedMBID)
	}
}

// `instrumental` is a SUCCESS that leaves no row, so it is the one verdict the
// row-absence gate cannot see. Without the status arm the sweeper re-asks about
// every instrumental track on every sweep, forever.
func TestInstrumentalIsTerminalWithoutARow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
	if err := s.MarkAtlasLyricsAttempt(ctx, "a/x.flac", "album-1", "", "rec-1",
		AtlasLyricsInstrumental, 0); err != nil {
		t.Fatal(err)
	}
	if contains(atlasCandidatePaths(t, s), "a/x.flac") {
		t.Error("an instrumental track is being re-asked about")
	}
	if got, _ := s.GetLyrics(ctx, "a/x.flac"); got != nil {
		t.Error("instrumental wrote a lyrics row")
	}
}

// A retag invalidates the verdict it was made under, including a terminal one.
// The attempt stores the identity it ASKED about, so a corrected MBID makes it
// stale by construction — no scanner hook, nothing to remember to call.
func TestARetagInvalidatesTheStoredVerdict(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, status string }{
		{"instrumental", AtlasLyricsInstrumental},
		{"unavailable", AtlasLyricsUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			seedAtlasTrack(t, s, "a/x.flac", "wrong-album", "", "Song", 1, 1, 100)
			// Parked far in the future, so only the MBID change can revive it.
			future := time.Now().Add(400 * 24 * time.Hour).UnixNano()
			if err := s.MarkAtlasLyricsAttempt(ctx, "a/x.flac", "wrong-album", "", "rec-1",
				tc.status, future); err != nil {
				t.Fatal(err)
			}
			if contains(atlasCandidatePaths(t, s), "a/x.flac") {
				t.Fatal("the parked verdict is not being honoured at all")
			}
			// The operator fixes the album MBID.
			seedAtlasTrack(t, s, "a/x.flac", "right-album", "", "Song", 1, 1, 100)
			if !contains(atlasCandidatePaths(t, s), "a/x.flac") {
				t.Error("a corrected MBID did not invalidate the stale verdict")
			}
		})
	}
}

// Routed and suppressed rows are not candidates. A routed path does not resolve
// on this filesystem, so /v1/lyrics could never serve what was fetched for it;
// a suppressed duplicate is not served either, and its winner is a candidate in
// its own right.
func TestAtlasLyricsCandidatesExcludeRoutedAndSuppressed(t *testing.T) {
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/keep.flac", "album-1", "", "Keep", 1, 1, 100)
	seedAtlasTrack(t, s, "a/routed.flac", "album-1", "", "Routed", 1, 2, 100)
	seedAtlasTrack(t, s, "a/dupe.flac", "album-1", "", "Dupe", 1, 3, 100)

	if _, err := s.db.Exec(`INSERT INTO upnp_track_routing(source_path, server_udn, object_id, res_url, last_seen_at)
		VALUES ('a/routed.flac', 'udn', 'oid', 'http://x/1', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET dupe_suppressed = 1 WHERE path = 'a/dupe.flac'`); err != nil {
		t.Fatal(err)
	}
	got := atlasCandidatePaths(t, s)
	if !contains(got, "a/keep.flac") {
		t.Error("the ordinary track stopped being a candidate")
	}
	if contains(got, "a/routed.flac") {
		t.Error("a UPnP-routed track is a candidate")
	}
	if contains(got, "a/dupe.flac") {
		t.Error("a dupe-suppressed track is a candidate")
	}
}

// A track with no MBID of any kind cannot be resolved, so it must not occupy a
// slot in the budget.
func TestAtlasLyricsCandidatesSkipTracksWithNoMBID(t *testing.T) {
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/none.flac", "", "", "No IDs", 1, 1, 100)
	seedAtlasTrack(t, s, "a/rec.flac", "", "rec-9", "Has recording", 1, 2, 100)
	got := atlasCandidatePaths(t, s)
	if contains(got, "a/none.flac") {
		t.Error("a track with no MBID at all is a candidate")
	}
	if !contains(got, "a/rec.flac") {
		t.Error("a track with only a recording MBID must still be a candidate")
	}
}

// The `attempts` counter drives the caller's backoff, so it must count attempts
// against ONE identity and reset when the question changes.
func TestAtlasLyricsAttemptsResetOnANewIdentity(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
	for i := 0; i < 3; i++ {
		if err := s.MarkAtlasLyricsAttempt(ctx, "a/x.flac", "album-1", "", "",
			AtlasLyricsPending, 0); err != nil {
			t.Fatal(err)
		}
	}
	if n := attemptCount(t, s, "a/x.flac"); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
	if err := s.MarkAtlasLyricsAttempt(ctx, "a/x.flac", "album-2", "", "",
		AtlasLyricsPending, 0); err != nil {
		t.Fatal(err)
	}
	if n := attemptCount(t, s, "a/x.flac"); n != 1 {
		t.Errorf("a new album MBID must restart the count, got %d", n)
	}
}

func attemptCount(t *testing.T, s *Store, path string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT attempts FROM atlas_lyrics_attempt WHERE source_path = ?`,
		path).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The candidate list is album-ordered, which is what lets one release listing
// serve a whole album: 9,454 candidates spanned 1,088 releases when measured,
// so a caller that had to fetch per track would issue nine times the requests.
func TestAtlasLyricsCandidatesAreGroupedByAlbum(t *testing.T) {
	s := openTestStore(t)
	seedAtlasTrack(t, s, "z/1.flac", "album-b", "", "B1", 1, 1, 100)
	seedAtlasTrack(t, s, "a/1.flac", "album-a", "", "A1", 1, 1, 100)
	seedAtlasTrack(t, s, "m/1.flac", "album-b", "", "B2", 1, 2, 100)
	seedAtlasTrack(t, s, "b/1.flac", "album-a", "", "A2", 1, 2, 100)

	cands, err := s.AtlasLyricsCandidates(context.Background(), time.Now().UnixNano(), 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	last := ""
	for _, c := range cands {
		if c.AlbumMBID != last {
			if seen[c.AlbumMBID] {
				t.Fatalf("album %q is interleaved with another", c.AlbumMBID)
			}
			seen[c.AlbumMBID] = true
			last = c.AlbumMBID
		}
	}
	if len(seen) != 2 {
		t.Errorf("saw %d albums, want 2", len(seen))
	}
}

// The candidate query hands the matcher what it needs. A test that only checked
// paths would pass against a query returning zeroes for every key.
func TestAtlasLyricsCandidateCarriesTheMatchKeys(t *testing.T) {
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "rec-7", "Love Me Do", 2, 5, 143.5)
	cands, err := s.AtlasLyricsCandidates(context.Background(), time.Now().UnixNano(), 10)
	if err != nil || len(cands) != 1 {
		t.Fatalf("want 1 candidate, got %d (%v)", len(cands), err)
	}
	c := cands[0]
	if c.AlbumMBID != "album-1" || c.TrackMBID != "rec-7" || c.Title != "Love Me Do" ||
		c.DiscNumber != 2 || c.TrackNumber != 5 || c.DurationMS != 143500 {
		t.Errorf("match keys did not survive the query: %+v", c)
	}
}

// UpsertAtlasLyrics is the only writer of these rows, so its refusals are the
// only guard against a local source string being laundered onto a network row.
func TestUpsertAtlasLyricsRefusesNonNetworkSourcesAndEmptyBodies(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/x.flac", "album-1", "", "Song", 1, 1, 100)
	if _, err := s.UpsertAtlasLyrics(ctx, "a/x.flac", atlasDoc("w", false), lyrics.SourceTextPlain); err == nil {
		t.Error("a local source was accepted as a network write")
	}
	if _, err := s.UpsertAtlasLyrics(ctx, "a/x.flac", atlasDoc("", false), lyrics.SourceAtlas); err == nil {
		t.Error("an empty body was accepted")
	}
	if _, err := s.UpsertAtlasLyrics(ctx, "", atlasDoc("w", false), lyrics.SourceAtlas); err == nil {
		t.Error("an empty path was accepted")
	}
}

// The rollup separates what was SERVED from what was merely ANSWERED — the two
// diverge by exactly the instrumental and unavailable verdicts, which is the
// number an operator asks about first.
func TestAtlasLyricsStats(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAtlasTrack(t, s, "a/1.flac", "album-1", "", "One", 1, 1, 100)
	seedAtlasTrack(t, s, "a/2.flac", "album-1", "", "Two", 1, 2, 100)
	seedAtlasTrack(t, s, "a/3.flac", "album-1", "", "Three", 1, 3, 100)
	seedAtlasTrack(t, s, "a/4.flac", "", "", "No MBID", 1, 4, 100)
	// The witness for `addressable`: an MBID, no lyrics row, no verdict. It is
	// what stops the assertion below from passing just because everything got
	// excluded.
	seedAtlasTrack(t, s, "a/5.flac", "album-1", "", "Five", 1, 5, 100)

	if _, err := s.UpsertAtlasLyrics(ctx, "a/1.flac", atlasDoc("[00:01.00] x", true), lyrics.SourceAtlasLRC); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertAtlasLyrics(ctx, "a/2.flac", atlasDoc("plain", false), lyrics.SourceAtlas); err != nil {
		t.Fatal(err)
	}
	for p, st := range map[string]string{
		"a/1.flac": AtlasLyricsAvailable,
		"a/2.flac": AtlasLyricsAvailable,
		"a/3.flac": AtlasLyricsInstrumental,
	} {
		if err := s.MarkAtlasLyricsAttempt(ctx, p, "album-1", "", "rec", st, 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.AtlasLyricsStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.SyncedRows != 1 || got.PlainRows != 1 {
		t.Errorf("rows: synced=%d plain=%d, want 1/1", got.SyncedRows, got.PlainRows)
	}
	if got.ByStatus[AtlasLyricsAvailable] != 2 || got.ByStatus[AtlasLyricsInstrumental] != 1 {
		t.Errorf("statuses: %v", got.ByStatus)
	}
	// a/5 is the only one left to act on. a/4 carries no MBID at all — and
	// a/3 is INSTRUMENTAL, which is a success that correctly leaves no
	// `track_lyrics` row: the candidate query will never offer it again, so
	// counting it as outstanding floors the operator's remaining-work number
	// at the instrumental population and makes a finished tier read as a
	// stalled one. (This test asserted the opposite, and said so in a comment.)
	if got.Addressable != 1 {
		t.Errorf("addressable = %d, want 1 — a/5 only", got.Addressable)
	}

	// A RETAG invalidates the verdict, here as in the candidate query. An
	// instrumental answer was about one recording; moved to another album,
	// the track is addressable again. Without this the exclusion would be a
	// permanent one keyed on a path.
	seedAtlasTrack(t, s, "a/3.flac", "album-2", "", "Three", 1, 3, 100)
	got, err = s.AtlasLyricsStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Addressable != 2 {
		t.Errorf("addressable after a retag = %d, want 2 — the stale verdict must stop excluding", got.Addressable)
	}
}

// The wire document is unchanged by any of this — the source never reaches it,
// so no ProtocolVersion bump and no iOS mirror are owed.
func TestNetworkSourceNeverReachesTheWireDocument(t *testing.T) {
	b, err := json.Marshal(lyrics.Doc{Format: "lrc", Synced: true, Body: "[00:01.00] x"})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"source", "sidecarName", "tag", "sourceMtimeNs", "sourceSize"} {
		if _, present := back[k]; present {
			t.Errorf("the wire document gained a provenance field %q", k)
		}
	}
}
