package manifest

import (
	"context"
	"testing"
	"time"

	tag "github.com/dhowden/tag"
)

// rawOnlyMetadata is a minimal dhowden/tag Metadata implementation
// whose ONLY populated surface is the Raw() map — enough to drive
// populateFromTagMetadata's raw-key paths (ReplayGain, MusicBrainz
// IDs, compilation flag) without a real audio file.
type rawOnlyMetadata struct{ raw map[string]any }

func (m rawOnlyMetadata) Format() tag.Format     { return "" }
func (m rawOnlyMetadata) FileType() tag.FileType { return "" }
func (m rawOnlyMetadata) Title() string          { return "" }
func (m rawOnlyMetadata) Album() string          { return "" }
func (m rawOnlyMetadata) Artist() string         { return "" }
func (m rawOnlyMetadata) AlbumArtist() string    { return "" }
func (m rawOnlyMetadata) Composer() string       { return "" }
func (m rawOnlyMetadata) Year() int              { return 0 }
func (m rawOnlyMetadata) Genre() string          { return "" }
func (m rawOnlyMetadata) Track() (int, int)      { return 0, 0 }
func (m rawOnlyMetadata) Disc() (int, int)       { return 0, 0 }
func (m rawOnlyMetadata) Picture() *tag.Picture  { return nil }
func (m rawOnlyMetadata) Lyrics() string         { return "" }
func (m rawOnlyMetadata) Comment() string        { return "" }
func (m rawOnlyMetadata) Raw() map[string]any    { return m.raw }

// TestPopulateFromTagMetadata_NonFiniteReplayGainOmitted pins the
// H3 fix at the tag-application seam: a "nan"/"inf" ReplayGain tag
// (real scanners emit these for digital-silence tracks) must land on
// the Track as ABSENT, not as a non-finite float — the latter would
// survive marshalForStorage (tag-derived ReplayGain is never scrubbed)
// and fail json.Marshal inside UpsertTrackBatch, silently dropping the
// whole 500-row scan batch on every rescan (2026-07-21 review H3).
func TestPopulateFromTagMetadata_NonFiniteReplayGainOmitted(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"track nan", map[string]any{"replaygain_track_gain": "nan"}},
		{"track +inf", map[string]any{"replaygain_track_gain": "+inf dB"}},
		{"track -inf", map[string]any{"REPLAYGAIN_TRACK_GAIN": "-inf"}},
		{"album nan db", map[string]any{"replaygain_album_gain": "NaN dB"}},
		{"both poisoned", map[string]any{
			"replaygain_track_gain": "inf",
			"replaygain_album_gain": "-infinity",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr Track
			populateFromTagMetadata(rawOnlyMetadata{raw: tc.raw}, &tr)
			if tr.ReplayGainTrackDB != nil {
				t.Errorf("ReplayGainTrackDB = %v, want nil (non-finite tag omitted)", *tr.ReplayGainTrackDB)
			}
			if tr.ReplayGainAlbumDB != nil {
				t.Errorf("ReplayGainAlbumDB = %v, want nil (non-finite tag omitted)", *tr.ReplayGainAlbumDB)
			}
		})
	}
}

// TestUpsertTrackBatch_CommitsAroundFormerlyPoisonedTrack is the
// batch-level regression for H3: a track whose source tags carried a
// non-finite ReplayGain value must upsert fine (gain omitted) AND must
// not sink the sibling rows sharing its batch. Pre-fix, the NaN gain
// failed json.Marshal and the scanner's flush dropped ALL rows in the
// batch — permanently, on every rescan.
func TestUpsertTrackBatch_CommitsAroundFormerlyPoisonedTrack(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	poisoned := &Track{Path: "A/silence.flac", Size: 100, ModTime: time.Now()}
	populateFromTagMetadata(rawOnlyMetadata{raw: map[string]any{
		"replaygain_track_gain": "nan",
	}}, poisoned)
	clean := &Track{Path: "A/normal.flac", Size: 200, ModTime: time.Now()}
	populateFromTagMetadata(rawOnlyMetadata{raw: map[string]any{
		"replaygain_track_gain": "-7.32 dB",
	}}, clean)

	if err := s.UpsertTrackBatch(ctx, []*Track{poisoned, clean}); err != nil {
		t.Fatalf("UpsertTrackBatch: %v (batch must not fail on a formerly-poisoned track)", err)
	}
	got, err := s.GetTrack(ctx, "A/silence.flac")
	if err != nil {
		t.Fatalf("GetTrack poisoned: %v", err)
	}
	if got == nil {
		t.Fatal("poisoned-source track missing after batch commit")
	}
	if got.ReplayGainTrackDB != nil {
		t.Errorf("poisoned-source ReplayGainTrackDB = %v, want nil", *got.ReplayGainTrackDB)
	}
	sibling, err := s.GetTrack(ctx, "A/normal.flac")
	if err != nil {
		t.Fatalf("GetTrack clean: %v", err)
	}
	if sibling == nil {
		t.Fatal("clean sibling row lost — batch poison regressed")
	}
	if sibling.ReplayGainTrackDB == nil || *sibling.ReplayGainTrackDB != -7.32 {
		t.Errorf("clean sibling ReplayGainTrackDB = %v, want -7.32", sibling.ReplayGainTrackDB)
	}
}
