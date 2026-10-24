package engine

import (
	"reflect"
	"testing"
)

// details reports what the backend knows of o's content, as a backend whose
// session's Events takes Details does (DetailsEvents).
func (s *fakeSession) details(o OccurrenceID, d Details) {
	de, ok := s.ev.(DetailsEvents)
	if !ok {
		s.b.t.Fatalf("the engine's Events take no Details")
	}
	de.Details(s.id, o, d)
}

// trackOf is sn's track, failing the test when nothing is heard.
func trackOf(t *testing.T, sn *Snapshot) Track {
	t.Helper()
	tr, ok := sn.Track()
	if !ok {
		t.Fatal("no track is heard")
	}
	return tr
}

// TestTheTrackIsTheOccurrenceHeardWithWhatTheBackendReported pins the track
// the snapshot keeps (B470 PR 5a3): nothing before anything is heard; the
// occurrence heard, its entry's reference and metadata as it was handed
// off and the Details the backend reported before it was heard; one more
// track counted for each occurrence heard, one more change of the details
// each time they differ from the last (an occurrence heard with the same
// details as the one before counts none), and nothing heard after a stop,
// which is one more change; a session's report after its release changes
// nothing; and none of it moves the generation but the selection's own
// change.
func TestTheTrackIsTheOccurrenceHeardWithWhatTheBackendReported(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b", "c")
	h.available()
	h.must("play", h.eng.Play(bg))
	s := h.be.nth(1)
	sn := h.snap()
	if _, ok := sn.Track(); ok || sn.TrackCount() != 0 || sn.DetailsCount() != 0 {
		t.Fatalf("before anything is heard: a track %v, %d tracks, %d changes of the details; want none", ok, sn.TrackCount(), sn.DetailsCount())
	}
	da := Details{Frames: 441_000, RateHz: 44_100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}
	gen := sn.Generation()
	s.details(s.first.ID, da)
	s.audible(s.first.ID)
	sn = h.waitSnap("a heard", func(sn *Snapshot) bool { _, ok := sn.Track(); return ok })
	want := Track{Occurrence: s.first.ID, Entry: ids[0], Ref: ref("a"), Meta: []byte("meta:a"), Details: da, HasDetails: true}
	if got := trackOf(t, sn); !reflect.DeepEqual(got, want) {
		t.Errorf("a heard: the track is %+v, want %+v", got, want)
	}
	if sn.TrackCount() != 1 || sn.DetailsCount() != 1 {
		t.Errorf("a heard: %d tracks, %d changes of the details; want 1 and 1", sn.TrackCount(), sn.DetailsCount())
	}
	if sn.Generation() != gen {
		t.Errorf("the first sound moved the generation from %s to %s", gen, sn.Generation())
	}

	db := Details{Frames: 288_000, RateHz: 96_000, Bits: 24, Channels: 2, Codec: "WAV", Lossless: true}
	b := s.prepared(s.first.ID, 1)
	s.submitted(b.ID)
	s.details(b.ID, db)
	s.audible(b.ID)
	sn = h.waitSnap("b heard", func(sn *Snapshot) bool { tr, ok := sn.Track(); return ok && tr.Occurrence == b.ID })
	want = Track{Occurrence: b.ID, Entry: ids[1], Ref: ref("b"), Meta: []byte("meta:b"), Details: db, HasDetails: true}
	if got := trackOf(t, sn); !reflect.DeepEqual(got, want) {
		t.Errorf("b heard: the track is %+v, want %+v", got, want)
	}
	if sn.TrackCount() != 2 || sn.DetailsCount() != 2 {
		t.Errorf("b heard: %d tracks, %d changes of the details; want 2 and 2", sn.TrackCount(), sn.DetailsCount())
	}

	c := s.prepared(b.ID, 1)
	s.submitted(c.ID)
	s.details(c.ID, db)
	s.audible(c.ID)
	sn = h.waitSnap("c heard", func(sn *Snapshot) bool { tr, ok := sn.Track(); return ok && tr.Occurrence == c.ID })
	if got := trackOf(t, sn); got.Entry != ids[2] || got.Details != db {
		t.Errorf("c heard: the track is %+v, want entry %d with b's details", got, ids[2])
	}
	if sn.TrackCount() != 3 || sn.DetailsCount() != 2 {
		t.Errorf("c heard with b's details: %d tracks, %d changes of the details; want 3 and 2", sn.TrackCount(), sn.DetailsCount())
	}

	h.must("stop", h.eng.Stop(bg))
	sn = h.snap()
	if tr, ok := sn.Track(); ok {
		t.Errorf("stopped: the track is %+v, want none", tr)
	}
	if sn.TrackCount() != 3 || sn.DetailsCount() != 3 {
		t.Errorf("stopped: %d tracks, %d changes of the details; want 3 and 3", sn.TrackCount(), sn.DetailsCount())
	}
	mark := h.appliedMark()
	s.details(c.ID, da)
	s.audible(c.ID)
	h.waitApplied("handled audible", mark)
	if after := h.snap(); after.DetailsCount() != 3 || after.TrackCount() != 3 {
		t.Errorf("a released session's report counted: %d tracks, %d changes of the details", after.TrackCount(), after.DetailsCount())
	}
}

// TestDetailsReportedOnceTheOccurrenceIsHeardAreItsTrack: an occurrence
// heard with nothing reported is a track with no details, which counts no
// change; the details the backend reports for it afterwards are the
// track's, one change, without moving the generation; the same details
// again count none, and others one more.
func TestDetailsReportedOnceTheOccurrenceIsHeardAreItsTrack(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	sn := h.snap()
	tr := trackOf(t, sn)
	if tr.HasDetails || tr.Details != (Details{}) || sn.DetailsCount() != 0 || sn.TrackCount() != 1 {
		t.Fatalf("heard with nothing reported: %+v, %d changes of the details, %d tracks; want no details, 0 and 1", tr, sn.DetailsCount(), sn.TrackCount())
	}
	gen := sn.Generation()
	d := Details{Frames: 1000, RateHz: 48_000, Bits: 24, Channels: 2, Codec: "AIFF", Lossless: true}
	s.details(s.first.ID, d)
	sn = h.waitSnap("the details the track's", func(sn *Snapshot) bool { tr, _ := sn.Track(); return tr.HasDetails })
	if tr := trackOf(t, sn); tr.Details != d || sn.DetailsCount() != 1 || sn.Generation() != gen {
		t.Errorf("details after the occurrence was heard: %+v, %d changes, generation %s (was %s); want them, 1, unmoved", tr, sn.DetailsCount(), sn.Generation(), gen)
	}
	mark := h.appliedMark()
	s.details(s.first.ID, d)
	h.waitApplied("handled details", mark)
	if n := h.snap().DetailsCount(); n != 1 {
		t.Errorf("the same details again: %d changes, want 1", n)
	}
	d.Frames++
	s.details(s.first.ID, d)
	sn = h.waitSnap("other details", func(sn *Snapshot) bool { return sn.DetailsCount() == 2 })
	if tr := trackOf(t, sn); tr.Details != d {
		t.Errorf("other details: the track's are %+v, want %+v", tr.Details, d)
	}
}

// TestTheTrackIsTheOccurrencesOwnCopy: a reader's track is a copy, so
// changing its metadata changes nothing the engine holds; and the entry
// heard deleted, its occurrence is no longer heard in the same step: no
// track is read, never the next entry's under the deleted one's
// occurrence, until the next is heard, and the count of tracks moves only
// then.
func TestTheTrackIsTheOccurrencesOwnCopy(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b")
	tr := trackOf(t, h.snap())
	tr.Meta[0] = 'X'
	if again := trackOf(t, h.snap()); string(again.Meta) != "meta:a" {
		t.Fatalf("a reader's change reached the engine's track: %q", again.Meta)
	}
	h.must("delete a", h.eng.Delete(bg, ids[0]))
	sn := h.snap()
	if tr, ok := sn.Track(); ok {
		t.Errorf("a deleted while heard: the track is %+v, want none until the next is heard", tr)
	}
	if sn.TrackCount() != 1 {
		t.Errorf("a deleted: %d tracks, want 1", sn.TrackCount())
	}
	s2 := h.be.nth(2)
	if s2.first.Entry != ids[1] {
		t.Fatalf("the session after the deletion starts entry %d, want b's %d", s2.first.Entry, ids[1])
	}
	_ = s
	s2.audible(s2.first.ID)
	sn = h.waitSnap("b heard", func(sn *Snapshot) bool { tr, ok := sn.Track(); return ok && tr.Entry == ids[1] })
	if tr := trackOf(t, sn); tr.Ref != ref("b") || string(tr.Meta) != "meta:b" || sn.TrackCount() != 2 {
		t.Errorf("b heard: %+v, %d tracks; want b's reference and metadata, 2", tr, sn.TrackCount())
	}
}

// TestTheCountsStartAtZeroWithEachRun: a restarted engine has heard
// nothing: no track, no track counted, no change of the details.
func TestTheCountsStartAtZeroWithEachRun(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	s.details(s.first.ID, Details{Frames: 1, RateHz: 44_100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true})
	h.waitSnap("the details", func(sn *Snapshot) bool { return sn.DetailsCount() == 1 })
	h.restart()
	sn := h.snap()
	if _, ok := sn.Track(); ok || sn.TrackCount() != 0 || sn.DetailsCount() != 0 {
		t.Errorf("after a restart: a track %v, %d tracks, %d changes; want none", ok, sn.TrackCount(), sn.DetailsCount())
	}
}
