package alsapcm_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// codecOf is the codec a golden's container is, by its file's name.
func codecOf(t *testing.T, file string) string {
	t.Helper()
	switch strings.ToLower(filepath.Ext(file)) {
	case ".flac":
		return "FLAC"
	case ".wav":
		return "WAV"
	case ".aiff", ".aifc":
		return "AIFF"
	case ".m4a":
		return "ALAC"
	}
	t.Fatalf("no codec for %s", file)
	return ""
}

// TestPlaybackReportsTheDecodersLengthAndFormatBeforeItIsHeard pins what the
// backend reports of an occurrence's content (B470 PR 5a3): for every PR 1
// golden that plays, FLAC, WAV, AIFF, AIFC and ALAC (an edited one among
// them), its Details are the decoder's own: the exact length in frames,
// which the reference tools counted and no tag rounded, the rate, the valid
// bits, the channels, the codec content inspection chose, and lossless;
// reported once, before the occurrence is reported Audible.
func TestPlaybackReportsTheDecodersLengthAndFormatBeforeItIsHeard(t *testing.T) {
	for _, g := range goldens(t) {
		if g.refusal != "" {
			continue
		}
		t.Run(filepath.Base(g.file), func(t *testing.T) {
			r := newRig(t)
			r.k.Free()
			r.files.add(g.ref, g.data)
			r.insert(g.ref)
			r.play()
			r.waitStopped(engine.ReasonEndOfQueue)
			evs, ds := r.rec.events(), r.rec.detailsList()
			heard := -1
			for i, e := range evs {
				if e.kind == "audible" {
					heard = i
					break
				}
			}
			if heard < 0 {
				t.Fatalf("nothing was reported audible: %s", kinds(evs))
			}
			want := engine.Details{Frames: g.frames, RateHz: g.rate, Bits: g.bits, Channels: 2, Codec: codecOf(t, g.file), Lossless: true}
			if len(ds) != 1 {
				t.Fatalf("details reported %d times, want once: %+v", len(ds), ds)
			}
			if d := ds[0]; d.d != want || d.o != evs[heard].o || d.at > heard {
				t.Errorf("reported %+v for occurrence %d at event %d; want %+v for the occurrence heard (%d), before its Audible (event %d)",
					d.d, d.o, d.at, want, evs[heard].o, heard)
			}
		})
	}
}

// TestTheEnginesTrackCarriesTheBackendsDetails pins the chain end to end:
// what the backend reports reaches the engine's track as the occurrence is
// heard, its length the decoder's frames, whatever the entry's metadata
// says (a controller's metadata is never read for it).
func TestTheEnginesTrackCarriesTheBackendsDetails(t *testing.T) {
	r := newRig(t)
	r.files.add("a", wav(44100, 16, pattern(1, 20000, 16)))
	ref, err := engine.NewReference("a")
	if err != nil {
		t.Fatal(err)
	}
	meta := []byte(`<DIDL-Lite><item><res duration="0:09:59.000" sampleFrequency="96000" bitsPerSample="24">x</res></item></DIDL-Lite>`)
	if _, err := r.eng.InsertAfter(bg, 0, ref, meta); err != nil {
		t.Fatal(err)
	}
	r.play()
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	r.k.Play(t, 50*time.Millisecond)
	r.tick()
	s := r.waitSnap("a heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
	tr, _ := s.Track()
	want := engine.Details{Frames: 20000, RateHz: 44100, Bits: 16, Channels: 2, Codec: "WAV", Lossless: true}
	if !tr.HasDetails || tr.Details != want || string(tr.Meta) != string(meta) {
		t.Errorf("the engine's track: %+v; want the decoder's %+v beside the entry's metadata", tr, want)
	}
	if s.TrackCount() != 1 || s.DetailsCount() != 1 {
		t.Errorf("%d tracks, %d changes of the details; want 1 and 1", s.TrackCount(), s.DetailsCount())
	}
}
