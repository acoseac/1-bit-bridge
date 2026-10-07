package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

func expectLibraryNote(t *testing.T, notes *syncNotes, what string) {
	t.Helper()
	if len(notes.lib) == 0 {
		t.Fatalf("%s published no library.changed", what)
	}
	notes.lib = nil
}

func TestASidecarLyricsBumpOnAnUnchangedFileReachesTheLibraryHook(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeMinimalFLACPairs(t, filepath.Join(root, "song.flac"), 44100, 16, [][2]string{{"TITLE", "Song"}, {"ARTIST", "A"}})
	s, _, notes := openNotifyingStore(t)
	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	before, _, err := s.LibraryWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	notes.lib = nil
	if err := os.WriteFile(filepath.Join(root, "song.lrc"), []byte(lrcBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanSubtree(ctx, root); err != nil {
		t.Fatal(err)
	}
	after, ok, err := s.LibraryWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || after == before {
		t.Fatal("the sidecar did not move the library watermark")
	}
	if sc.IsScanning() {
		t.Fatal("a subtree scan left the full-scan flag set")
	}
	if len(notes.lib) == 0 {
		t.Fatal("indexed_at moved and the library hook was not called")
	}
}

func TestADeletionOutsideAScanPublishesLibraryChanged(t *testing.T) {
	ctx := context.Background()
	s, _, notes := openNotifyingStore(t)
	upsertParent(t, s, "A/1.flac")
	notes.lib = nil
	n, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{"A/1.flac"}, 1)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	expectLibraryNote(t, notes, "threshold reap")
	wm, ok, err := s.LibraryWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || wm <= 0 {
		t.Fatalf("watermark ok %v ns %d", ok, wm)
	}
	gone, _, err := s.DeletedSince(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0] != "A/1.flac" {
		t.Fatalf("deleted %v", gone)
	}
}

func TestDeleteTrackPublishesLibraryChanged(t *testing.T) {
	ctx := context.Background()
	s, _, notes := openNotifyingStore(t)
	upsertParent(t, s, "A/1.flac")
	notes.lib = nil
	if err := s.DeleteTrack(ctx, "A/1.flac"); err != nil {
		t.Fatal(err)
	}
	expectLibraryNote(t, notes, "DeleteTrack")
	if _, ok, err := s.LibraryWatermark(ctx); err != nil || !ok {
		t.Fatal("a journaled deletion left no watermark")
	}
}

func TestDeleteTracksBatchPublishesLibraryChanged(t *testing.T) {
	ctx := context.Background()
	s, _, notes := openNotifyingStore(t)
	upsertParent(t, s, "A/1.flac")
	upsertParent(t, s, "A/2.flac")
	upsertParent(t, s, "A/3.flac")
	notes.lib = nil
	// Two of three is past the journal's library-fraction line, so this
	// is the mass coverage reset, which writes no per-path tombstone.
	if err := s.DeleteTracksBatch(ctx, []string{"A/1.flac", "A/2.flac"}); err != nil {
		t.Fatal(err)
	}
	expectLibraryNote(t, notes, "DeleteTracksBatch")
}

func TestDeleteTracksByPrefixPublishesLibraryChanged(t *testing.T) {
	ctx := context.Background()
	s, _, notes := openNotifyingStore(t)
	upsertParent(t, s, "Album/1.flac")
	notes.lib = nil
	n, err := s.DeleteTracksByPrefix(ctx, "Album")
	if err != nil || n != 1 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	expectLibraryNote(t, notes, "DeleteTracksByPrefix")
}

func TestClearMissingCountsPublishesLibraryChanged(t *testing.T) {
	ctx := context.Background()
	s, _, notes := openNotifyingStore(t)
	upsertParent(t, s, "A/1.flac")
	if _, err := s.db.ExecContext(ctx, `UPDATE tracks SET missing_count = 1 WHERE path = ?`, "A/1.flac"); err != nil {
		t.Fatal(err)
	}
	notes.lib = nil
	n, err := s.ClearMissingCounts(ctx)
	if err != nil || n < 1 {
		t.Fatalf("cleared %d err %v", n, err)
	}
	expectLibraryNote(t, notes, "ClearMissingCounts")
}

func TestASuppressionOutsideAScanPublishesLibraryChanged(t *testing.T) {
	ctx := context.Background()
	s, _, notes := openNotifyingStore(t)
	upsertParent(t, s, "A/1.flac")
	notes.lib = nil
	n, err := s.ApplyDupeStamps(ctx, []DupeStamp{{
		Path: "A/1.flac", GroupID: "g", Tier: "t", Suppressed: true, JournalDelete: true,
	}})
	if err != nil || n != 1 {
		t.Fatalf("stamped %d err %v", n, err)
	}
	expectLibraryNote(t, notes, "suppression")
	gone, _, err := s.DeletedSince(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0] != "A/1.flac" {
		t.Fatalf("deleted %v", gone)
	}
}

func TestIndexedAtWritersOutsideAScanReachTheLibraryHook(t *testing.T) {
	ctx := context.Background()
	s, _, notes := openNotifyingStore(t)
	const path = "A/1.flac"
	upsertParent(t, s, path)
	notes.lib = nil

	row := VariantRow{
		SourcePath: path, VariantID: "upscaled-v2-176400-24", SidecarPath: "/v/a.flac",
		Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 10,
		SourceMTimeNS: 1, SourceSize: 100, CreatedAt: 1,
	}
	if err := s.UpsertVariant(ctx, row); err != nil {
		t.Fatal(err)
	}
	expectLibraryNote(t, notes, "UpsertVariant")
	if err := s.DeleteVariant(ctx, path, row.VariantID); err != nil {
		t.Fatal(err)
	}
	expectLibraryNote(t, notes, "DeleteVariant")

	analysis := AnalysisRow{
		SourcePath: path, WaveformPath: "/w/a.bin", WaveformTag: "tag1",
		WaveformSize: 10, SourceMTimeNS: 1, SourceSize: 2,
		SchemaVersion: "wf1", CreatedAt: 100,
	}
	if err := s.UpsertAnalysis(ctx, analysis); err != nil {
		t.Fatal(err)
	}
	expectLibraryNote(t, notes, "UpsertAnalysis")
	analysis.WaveformTag = "tag2"
	if err := s.UpsertAnalysis(ctx, analysis); err != nil {
		t.Fatal(err)
	}
	expectLibraryNote(t, notes, "UpsertAnalysis change")
	if err := s.DeleteAnalysis(ctx, path); err != nil {
		t.Fatal(err)
	}
	expectLibraryNote(t, notes, "DeleteAnalysis")

	mbid := "12aae8a7-e814-4c46-94d7-5c9e053bda5b"
	if err := s.UpsertTrack(ctx, &Track{Path: path, Size: 100, ModTime: time.Now(), ArtworkMBID: mbid}); err != nil {
		t.Fatal(err)
	}
	notes.lib = nil
	bumped, err := s.SetArtworkVersionAndBumpIndex(ctx, mbid, "v1")
	if err != nil || bumped == 0 {
		t.Fatalf("artwork n %d err %v", bumped, err)
	}
	expectLibraryNote(t, notes, "SetArtworkVersionAndBumpIndex")

	album := "11111111-1111-4111-8111-111111111111"
	if err := s.UpsertTrack(ctx, &Track{Path: path, Size: 100, ModTime: time.Now(), MusicBrainzAlbumID: album}); err != nil {
		t.Fatal(err)
	}
	notes.lib = nil
	bumped, err = s.SetBookletTagAndBumpIndex(ctx, album, "tag")
	if err != nil || bumped == 0 {
		t.Fatalf("booklet n %d err %v", bumped, err)
	}
	expectLibraryNote(t, notes, "SetBookletTagAndBumpIndex")

	seedAtlasTrack(t, s, "B/1.flac", album, "22222222-2222-4222-8222-222222222222", "Song", 1, 1, 100)
	notes.lib = nil
	ok, err := s.UpsertAtlasLyrics(ctx, "B/1.flac", atlasDoc("[00:01.00] la", true), lyrics.SourceAtlasLRC)
	if err != nil || !ok {
		t.Fatalf("atlas ok %v err %v", ok, err)
	}
	expectLibraryNote(t, notes, "UpsertAtlasLyrics")

	tr, err := s.GetTrack(ctx, path)
	if err != nil || tr == nil {
		t.Fatal(err)
	}
	tr.Album = "Reconciled"
	notes.lib = nil
	reconciled, err := s.applyReconciledTracks(ctx, []Track{*tr})
	if err != nil || reconciled == 0 {
		t.Fatalf("reconcile n %d err %v", reconciled, err)
	}
	expectLibraryNote(t, notes, "applyReconciledTracks")

	notes.lib = nil
	stamped, err := s.ApplyDupeStamps(ctx, []DupeStamp{{
		Path: path, GroupID: "g", Tier: "t", BumpIndexed: true,
	}})
	if err != nil || stamped == 0 {
		t.Fatalf("lift n %d err %v", stamped, err)
	}
	expectLibraryNote(t, notes, "ApplyDupeStamps lift")
}

func TestPlaylistBasePutPublishesAfterTheNameCommits(t *testing.T) {
	s, path, notes := openNotifyingStore(t)
	ctx := context.Background()
	items := []PlaylistItemRow{{Position: 0, Path: "a/b.flac", Title: "T", Artist: "A"}}
	row := PlaylistRow{ID: "pl1", Name: "Mix", LastModifiedAt: 100}
	if _, err := s.PutPlaylist(ctx, "dev", row, items, nil); err != nil {
		t.Fatal(err)
	}
	var seen string
	s.SetSyncHooks(SyncHooks{
		Playlists: func(epoch string) {
			notes.pl = append(notes.pl, epoch)
			other, err := OpenStore(path)
			if err != nil {
				t.Errorf("second open: %v", err)
				return
			}
			defer other.Close()
			got, _, err := other.GetPlaylist(ctx, "pl1")
			if err != nil || got == nil {
				t.Errorf("get %v %v", got, err)
				return
			}
			seen = got.Name
		},
	})
	base := row.LastModifiedAt
	changed := row
	changed.Name = "Mix 2"
	changed.LastModifiedAt = 200
	if _, err := s.PutPlaylist(ctx, "dev", changed, items, &base); err != nil {
		t.Fatal(err)
	}
	if seen != "Mix 2" {
		t.Fatalf("the hook saw %q", seen)
	}
}

func TestAPostCommitNoteSurvivesTheRequestContext(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	upsertParent(t, s, "A/1.flac")
	notes.lib = nil
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	s.notePlaylists(canceled)
	s.noteLibrary(canceled)
	if len(notes.pl) == 0 || len(notes.lib) == 0 {
		t.Fatalf("a cancelled request dropped the note, playlists %d library %d", len(notes.pl), len(notes.lib))
	}
	if _, err := s.PutPlaylist(canceled, "dev", PlaylistRow{ID: "pl1", Name: "Mix", LastModifiedAt: 1}, nil, nil); err == nil {
		t.Fatal("a cancelled request stored the playlist")
	}
}

func TestASmartMixCoverDoesNotPublishPlaylistsChanged(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	cover := PlaylistCover{Scope: CoverScopeSmartMix, Key: "mix", ImageHash: "h1", Ext: "jpg", UpdatedAt: 1}
	if err := s.SetPlaylistCover(context.Background(), cover); err != nil {
		t.Fatal(err)
	}
	if len(notes.pl) != 0 {
		t.Fatalf("a smart-mix cover published %d playlist events", len(notes.pl))
	}
}

func TestDeletingAPlaylistWithACoverPublishesOnce(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	row := PlaylistRow{ID: "pl1", Name: "Mix", LastModifiedAt: 100}
	items := []PlaylistItemRow{{Position: 0, Path: "a/b.flac", Title: "T", Artist: "A"}}
	if _, err := s.PutPlaylist(ctx, "dev", row, items, nil); err != nil {
		t.Fatal(err)
	}
	cover := PlaylistCover{Scope: CoverScopePlaylist, Key: "pl1", ImageHash: "h1", Ext: "jpg", UpdatedAt: 1}
	if err := s.SetPlaylistCover(ctx, cover); err != nil {
		t.Fatal(err)
	}
	notes.pl = nil
	if _, err := s.TombstonePlaylist(ctx, "pl1", "dev"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.DeletePlaylistCover(ctx, CoverScopePlaylist, "pl1"); err != nil {
		t.Fatal(err)
	}
	if len(notes.pl) != 1 {
		t.Fatalf("notes %d, want the tombstone alone", len(notes.pl))
	}
}

func TestAMassDeletePublishesAWatermarkThatMovedForward(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	for _, p := range []string{"A/1.flac", "A/2.flac", "A/3.flac"} {
		upsertParent(t, s, p)
	}
	before, ok, err := s.LibraryWatermark(ctx)
	if err != nil || !ok {
		t.Fatalf("watermark before: %d ok %v err %v", before, ok, err)
	}
	notes.lib = nil
	if err := s.DeleteTracksBatch(ctx, []string{"A/2.flac", "A/3.flac"}); err != nil {
		t.Fatal(err)
	}
	if len(notes.lib) == 0 {
		t.Fatal("a mass delete published no library.changed")
	}
	if got := notes.lib[len(notes.lib)-1]; got <= before {
		t.Fatalf("watermark went backwards: before %d, event %d", before, got)
	}
}

func TestDeletingEveryTrackPublishesLibraryChanged(t *testing.T) {
	s, _, notes := openNotifyingStore(t)
	ctx := context.Background()
	time.Sleep(3 * time.Millisecond)
	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		upsertParent(t, s, p)
	}
	cursor := time.Now()
	notes.lib = nil
	if err := s.DeleteTracksBatch(ctx, []string{"A/1.flac", "A/2.flac"}); err != nil {
		t.Fatal(err)
	}
	m, err := BuildManifest(ctx, s, nil, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes.lib) == 0 {
		t.Fatalf("deleting every track published nothing (deltaIncomplete=%v deleted=%d tracks=%d)",
			m.DeltaIncomplete, len(m.Deleted), len(m.Tracks))
	}
}
