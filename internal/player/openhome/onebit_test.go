package openhome

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
)

// 1-bit's own sequences through the write actions (B470 PR 5a4), as its
// OpenHome client sends them at iOS 32ffba18, by the brief's account of
// OpenHomeHandoff.swift and BridgeDLNAURLResolver.swift: the cast start, the
// 500-track window and its re-handoff, what it sends while casting, and its
// read-only re-attach after a relaunch. Each runs against the real engine over
// the fake backend, through the SOAP layer.

// didl is the single-line DIDL-Lite, with no prolog, 1-bit sends as an
// Insert's Metadata.
func didl(title string) string {
	return `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
		`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"><item id="1" parentID="0" restricted="1"><dc:title>` + title +
		`</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class></item></DIDL-Lite>`
}

// trackID is the n-th of a test's track ids, from 1.
func trackID(n int) string { return fmt.Sprintf("%016x", n) }

// boolText is a boolean in-argument as 1-bit writes it.
func boolText(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// handOff is 1-bit's cast start through h for uris, Play aside: DeleteAll (a
// fault fails the start); an Insert of each after the last NewId, from 0,
// each NewId above 0; IdArray once, which must be exactly the IDs inserted;
// SetShuffle 0; SetRepeat repeat. It returns the IDs.
func handOff(t *testing.T, h *handler, uris []string, repeat bool) []uint32 {
	t.Helper()
	read(t, h, playlist, "DeleteAll")
	var ids []uint32
	after := uint32(0)
	for i, uri := range uris {
		id := newID(t, h, after, uri, didl(fmt.Sprintf("Track %d", i+1)))
		if id == 0 {
			t.Fatalf("the Insert of %s answered NewId 0", uri)
		}
		ids = append(ids, id)
		after = id
	}
	if got := queueOf(t, h); !slices.Equal(got, ids) {
		t.Fatalf("IdArray after the inserts: %v, want exactly %v", got, ids)
	}
	read(t, h, playlist, "SetShuffle", "Value", "0")
	read(t, h, playlist, "SetRepeat", "Value", boolText(repeat))
	return ids
}

// state is TransportState through h.
func state(t *testing.T, h *handler) string {
	t.Helper()
	return read(t, h, playlist, "TransportState")["Value"]
}

// selected is Id through h.
func selected(t *testing.T, h *handler) string {
	t.Helper()
	return read(t, h, playlist, "Id")["Value"]
}

// TestOneBitsCastStartThroughTheWrites pins 1-bit's cast start: Standby reads
// 0 (so it sends no SetStandby), the one source is the active one (no
// SetSourceIndex), TracksMax is the window it takes; DeleteAll, the Inserts
// chained by NewId, IdArray once and exactly the IDs inserted, SetShuffle 0,
// SetRepeat 1 (the whole queue on the player), then Play: Id is the first
// entry as Play answers, TransportState Buffering until it is heard, then
// Playing, Repeat 1 and Shuffle 0; SeekSecondAbsolute, sent when resuming at
// a position, is 803, which 1-bit only logs, and playback goes on; Read
// gives each Uri back byte for byte.
func TestOneBitsCastStartThroughTheWrites(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("Living Room")
	if got := read(t, h, product, "Standby")["Value"]; got != "0" {
		t.Fatalf("Standby %s: 1-bit would send SetStandby 0", got)
	}
	if got := read(t, h, product, "SourceIndex")["Value"]; got != "0" {
		t.Fatalf("SourceIndex %s: 1-bit would send SetSourceIndex", got)
	}
	if got := read(t, h, playlist, "TracksMax")["Value"]; got != "1000" {
		t.Fatalf("TracksMax %s", got)
	}
	uris := []string{uriOf(goldenID), uriOf(trackB), uriOf(trackC)}
	ids := handOff(t, h, uris, true)
	read(t, h, playlist, "Play")
	if got := selected(t, h); got != u(ids[0]) {
		t.Fatalf("Id %s as Play answers, want the first entry, %d", got, ids[0])
	}
	if got := state(t, h); got != "Buffering" {
		t.Errorf("TransportState %s before anything is heard, want Buffering", got)
	}
	if r := soapFault(t, h, playlist, "SeekSecondAbsolute", "Value", "42"); r.fault != 803 {
		t.Errorf("SeekSecondAbsolute: fault %d, want 803", r.fault)
	}
	s := p.be.session(t, 1)
	if s.first.Entry != ids[0] {
		t.Fatalf("the session started %d, want %d", s.first.Entry, ids[0])
	}
	s.hear(s.first.ID, &details)
	p.heardNow(s.first.ID)
	if got := state(t, h); got != "Playing" {
		t.Errorf("TransportState %s once heard, want Playing", got)
	}
	if got := read(t, h, playlist, "Repeat")["Value"]; got != "1" {
		t.Errorf("Repeat %s, want 1", got)
	}
	if got := read(t, h, playlist, "Shuffle")["Value"]; got != "0" {
		t.Errorf("Shuffle %s, want 0", got)
	}
	for i, id := range ids {
		if got := read(t, h, playlist, "Read", "Id", u(id)); got["Uri"] != uris[i] || got["Metadata"] != didl(fmt.Sprintf("Track %d", i+1)) {
			t.Errorf("Read(%d): %v, want the Uri and Metadata inserted", id, got)
		}
	}
}

// TestARefusedURIFailsTheCastStart pins what 1-bit meets when a row of its
// queue is no track of the bridge's mid-start (a UPnP server's res, which 5b
// and 5c filter out, or a bridge track the library no longer serves): the
// Insert is 600, never 801, which 1-bit would take for a full player and play
// what is in; 1-bit then sends DeleteAll, which empties the queue, and the
// start fails, nothing playing.
func TestARefusedURIFailsTheCastStart(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	p.lib.refuse(trackC, &engine.Unplayable{Reason: library.ReasonRouted})
	read(t, h, playlist, "DeleteAll")
	a := newID(t, h, 0, uriOf(goldenID), didl("a"))
	b := newID(t, h, a, uriOf(trackB), didl("b"))
	for _, uri := range []string{"http://192.168.1.77:8200/MediaItems/22.flac", uriOf(trackC)} {
		if r := soapFault(t, h, playlist, "Insert", "AfterId", u(b), "Uri", uri, "Metadata", didl("x")); r.fault != errArgumentValueInvalid {
			t.Errorf("the Insert of %s: fault %d, want 600", uri, r.fault)
		}
	}
	read(t, h, playlist, "DeleteAll")
	if got := queueOf(t, h); len(got) != 0 {
		t.Errorf("IdArray after DeleteAll: %v", got)
	}
	if got := selected(t, h); got != "0" {
		t.Errorf("Id %s after DeleteAll, want 0", got)
	}
	if got := state(t, h); got != "Stopped" {
		t.Errorf("TransportState %s, want Stopped", got)
	}
}

// TestTheFiveHundredTrackWindowAndItsReHandoff pins 1-bit's window: a handoff
// holds the lower of 500 and TracksMax, and repeat stays off while only part
// of the app's queue is on the player; once playback reaches the window's
// last row, 1-bit hands off again from its current row, a partial window
// (the 121 rows left of 620), and plays it: the IDs are new, none reused, and
// the first of them plays.
func TestTheFiveHundredTrackWindowAndItsReHandoff(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	app := make([]string, 620)
	for i := range app {
		app[i] = uriOf(trackID(i + 1))
	}
	window := int(min(500, ui4Value(t, "TracksMax", read(t, h, playlist, "TracksMax")["Value"])))
	first := handOff(t, h, app[:window], false)
	if got := read(t, h, playlist, "Repeat")["Value"]; got != "0" {
		t.Errorf("Repeat %s with part of the queue held, want 0", got)
	}
	read(t, h, playlist, "Play")
	s := p.be.session(t, 1)
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	read(t, h, playlist, "SeekId", "Value", u(first[window-1]))
	s = p.be.session(t, 2)
	if s.first.Entry != first[window-1] {
		t.Fatalf("the jump started %d, want the window's last row", s.first.Entry)
	}
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	rest := handOff(t, h, app[window-1:], false)
	if len(rest) != len(app)-window+1 {
		t.Fatalf("the re-handoff inserted %d rows, want %d", len(rest), len(app)-window+1)
	}
	for _, id := range rest {
		if slices.Contains(first, id) {
			t.Fatalf("the re-handoff reused ID %d", id)
		}
	}
	read(t, h, playlist, "Play")
	s = p.be.session(t, 3)
	if s.first.Entry != rest[0] || selected(t, h) != u(rest[0]) {
		t.Fatalf("after the re-handoff the session started %d, Id %s; want %d", s.first.Entry, selected(t, h), rest[0])
	}
}

// TestWhileCastingSeekIdStartsPlaybackFromEveryState pins 1-bit's jumps,
// which no Play follows: SeekId starts playback of its entry from Stopped,
// Paused, Playing, Buffering (a start under way, which the SeekId's
// replaces) and a release under way (a Stop's), answering success with Id
// its entry at once; from maintenance, an unconfirmed release or an output
// the engine has not been told of, it answers 501, changing nothing.
func TestWhileCastingSeekIdStartsPlaybackFromEveryState(t *testing.T) {
	plays := func(t *testing.T, p *testPlayer, h *handler, n int, want uint32) {
		t.Helper()
		if got := selected(t, h); got != u(want) {
			t.Errorf("Id %s as SeekId answers, want %d", got, want)
		}
		if s := p.be.session(t, n); s.first.Entry != want {
			t.Errorf("session %d started %d, want %d", n, s.first.Entry, want)
		}
		if got := state(t, h); got != "Buffering" && got != "Playing" {
			t.Errorf("TransportState %s, want the entry starting", got)
		}
	}
	setUp := func(t *testing.T) (*testPlayer, *handler, []uint32) {
		p := newTestPlayer(t)
		h := p.handler("")
		return p, h, []uint32{p.add(uriOf(goldenID), nil), p.add(uriOf(trackB), nil), p.add(uriOf(trackC), nil)}
	}
	t.Run("stopped", func(t *testing.T) {
		p, h, ids := setUp(t)
		read(t, h, playlist, "SeekId", "Value", u(ids[1]))
		plays(t, p, h, 1, ids[1])
	})
	t.Run("paused", func(t *testing.T) {
		p, h, ids := setUp(t)
		playing(t, p, h)
		read(t, h, playlist, "Pause")
		read(t, h, playlist, "SeekId", "Value", u(ids[2]))
		plays(t, p, h, 2, ids[2])
	})
	t.Run("playing", func(t *testing.T) {
		p, h, ids := setUp(t)
		playing(t, p, h)
		read(t, h, playlist, "SeekId", "Value", u(ids[1]))
		plays(t, p, h, 2, ids[1])
	})
	t.Run("buffering", func(t *testing.T) {
		p, h, ids := setUp(t)
		letGo := p.be.holdStarts(t)
		play := send(h, playlist, "Play")
		p.be.callsBegun(t, "Start", 1)
		if got := state(t, h); got != "Buffering" {
			t.Fatalf("TransportState %s while the start is held", got)
		}
		seek := send(h, playlist, "SeekId", "Value", u(ids[2]))
		p.be.callsBegun(t, "Start", 2)
		letGo()
		for _, c := range []sent{play, seek} {
			if r := c.answer(t); r.status != http.StatusOK {
				t.Errorf("%s: %d, fault %d", c.action, r.status, r.fault)
			}
		}
		plays(t, p, h, 2, ids[2])
	})
	t.Run("a release under way", func(t *testing.T) {
		p, h, ids := setUp(t)
		playing(t, p, h)
		letGo := p.be.holdReleases()
		defer letGo()
		stop := send(h, playlist, "Stop")
		p.be.callsBegun(t, "Release", 1)
		seek := send(h, playlist, "SeekId", "Value", u(ids[1]))
		p.waitSnap("the SeekId's selection", func(s *engine.Snapshot) bool { return s.Selected() == ids[1] })
		letGo()
		for _, c := range []sent{stop, seek} {
			if r := c.answer(t); r.status != http.StatusOK {
				t.Errorf("%s: %d, fault %d", c.action, r.status, r.fault)
			}
		}
		plays(t, p, h, 2, ids[1])
	})
	refused := func(t *testing.T, p *testPlayer, h *handler, id uint32, sessions int) {
		t.Helper()
		before := selected(t, h)
		if r := soapFault(t, h, playlist, "SeekId", "Value", u(id)); r.fault != errActionFailed {
			t.Errorf("SeekId: fault %d, want 501", r.fault)
		}
		if got := selected(t, h); got != before {
			t.Errorf("Id %s after the refused SeekId, want %s", got, before)
		}
		if n := p.be.sessionCount(); n != sessions {
			t.Errorf("%d sessions after the refused SeekId, want %d", n, sessions)
		}
	}
	t.Run("maintenance", func(t *testing.T) {
		p, h, ids := setUp(t)
		hold, err := p.eng.EnterMaintenance(bg, p.eng.Snapshot().Generation(), "a test's maintenance")
		must(t, "enter maintenance", err)
		refused(t, p, h, ids[1], 0)
		must(t, "exit maintenance", p.eng.ExitMaintenance(bg, hold.Lease))
		read(t, h, playlist, "SeekId", "Value", u(ids[1]))
		plays(t, p, h, 1, ids[1])
	})
	t.Run("an unconfirmed release", func(t *testing.T) {
		p, h, ids := setUp(t)
		playing(t, p, h)
		letGo := p.be.holdReleases()
		defer letGo()
		read(t, h, playlist, "Stop")
		if _, ok := p.eng.Snapshot().ReleaseUnconfirmed(); !ok {
			t.Fatal("the Stop answered with its release neither confirmed nor past its deadline")
		}
		refused(t, p, h, ids[1], 1)
		letGo()
		p.waitSnap("the late acknowledgement", func(s *engine.Snapshot) bool { _, ok := s.ReleaseUnconfirmed(); return !ok })
		read(t, h, playlist, "SeekId", "Value", u(ids[1]))
		plays(t, p, h, 2, ids[1])
	})
	t.Run("an output not reported", func(t *testing.T) {
		p := newTestPlayerWith(t, func(p *testPlayer) { p.unreported = true })
		h := p.handler("")
		ids := []uint32{p.add(uriOf(goldenID), nil), p.add(uriOf(trackB), nil)}
		refused(t, p, h, ids[1], 0)
		p.available()
		read(t, h, playlist, "SeekId", "Value", u(ids[1]))
		plays(t, p, h, 1, ids[1])
	})
}

// TestWhileCastingTheQueueEdits pins the rest of what 1-bit sends while
// casting: Pause and Play; an Insert mid-queue, which IdArray shows at once
// (1-bit reads it back); a DeleteId of a row ahead, playback going on; a
// DeleteId of the playing row, the row after it then playing and Id on it as
// DeleteId answers; a SeekId off the playing row, Id its entry at the first
// read (1-bit's Id wait); and Stop, Id staying on the row.
func TestWhileCastingTheQueueEdits(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	ids := handOff(t, h, []string{uriOf(trackID(1)), uriOf(trackID(2)), uriOf(trackID(3)), uriOf(trackID(4))}, false)
	a, b, c, d := ids[0], ids[1], ids[2], ids[3]
	playing(t, p, h)
	read(t, h, playlist, "Pause")
	if got := state(t, h); got != "Paused" {
		t.Errorf("TransportState %s after Pause", got)
	}
	read(t, h, playlist, "Play")
	if got := state(t, h); got != "Playing" {
		t.Errorf("TransportState %s after Play resumed", got)
	}
	x := newID(t, h, b, uriOf(trackID(9)), didl("x"))
	if got := queueOf(t, h); !slices.Equal(got, []uint32{a, b, x, c, d}) {
		t.Errorf("IdArray read back after an Insert after b: %v", got)
	}
	read(t, h, playlist, "DeleteId", "Value", u(d))
	if got := queueOf(t, h); !slices.Equal(got, []uint32{a, b, x, c}) || selected(t, h) != u(a) || state(t, h) != "Playing" {
		t.Errorf("after a DeleteId of a row ahead: %v, Id %s, %s", got, selected(t, h), state(t, h))
	}
	read(t, h, playlist, "DeleteId", "Value", u(a))
	if got := selected(t, h); got != u(b) {
		t.Errorf("Id %s as the DeleteId of the playing row answers, want b", got)
	}
	s := p.be.session(t, 2)
	if s.first.Entry != b {
		t.Fatalf("after the playing row's deletion the session started %d, want b", s.first.Entry)
	}
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	read(t, h, playlist, "SeekId", "Value", u(c))
	if got := selected(t, h); got != u(c) {
		t.Errorf("Id %s at the first read after SeekId, want c", got)
	}
	read(t, h, playlist, "Stop")
	if got := state(t, h); got != "Stopped" || selected(t, h) != u(c) {
		t.Errorf("after Stop: %s, Id %s; want Stopped on c", got, selected(t, h))
	}
}

// TestAnInsertAfterThePlayingRowOnceTheNextIsHandedOff pins an Insert after
// the playing row while the next row is already in the output (submitted,
// gapless): the engine cuts that successor at its boundary and hands off the
// inserted entry instead, which plays next, the row it displaced after it.
func TestAnInsertAfterThePlayingRowOnceTheNextIsHandedOff(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	ids := handOff(t, h, []string{uriOf(trackID(1)), uriOf(trackID(2))}, false)
	s := playing(t, p, h)
	next := s.successor(t, 1)
	if next == nil || next.Entry != ids[1] {
		t.Fatalf("the successor %+v, want the second row", next)
	}
	s.ev.Submitted(s.id, next.ID)
	if end := s.successor(t, 2); end != nil {
		t.Fatalf("after the second row the engine named %+v, want nothing", end)
	}
	x := newID(t, h, ids[0], uriOf(trackID(9)), didl("x"))
	p.be.callsBegun(t, "Cut", 1)
	again := s.successor(t, 3)
	if again == nil || again.Entry != x {
		t.Fatalf("the successor after the Insert %+v, want the inserted entry, %d", again, x)
	}
	s.ev.Submitted(s.id, again.ID)
	s.hear(again.ID, nil)
	p.heardNow(again.ID)
	if got := selected(t, h); got != u(x) {
		t.Errorf("Id %s, want the inserted entry", got)
	}
	if got := queueOf(t, h); !slices.Equal(got, []uint32{ids[0], x, ids[1]}) {
		t.Errorf("IdArray %v", got)
	}
}

// TestADeleteIdOfTheRowAheadAtATrackBoundary pins a DeleteId of the row
// ahead once it is in the output, at the boundary where it becomes current:
// cut in time, the row after it plays next; cut too late (its frames the
// kernel's), the engine stops playback when it is heard (removed_entry_audible),
// TransportState Stopped, which 1-bit reads as the end of the cast.
func TestADeleteIdOfTheRowAheadAtATrackBoundary(t *testing.T) {
	for _, tooLate := range []bool{false, true} {
		t.Run(fmt.Sprintf("too late %v", tooLate), func(t *testing.T) {
			p := newTestPlayer(t)
			h := p.handler("")
			ids := handOff(t, h, []string{uriOf(trackID(1)), uriOf(trackID(2)), uriOf(trackID(3))}, false)
			s := playing(t, p, h)
			next := s.successor(t, 1)
			s.ev.Submitted(s.id, next.ID)
			if c := s.successor(t, 2); c == nil || c.Entry != ids[2] {
				t.Fatalf("after the second row the engine named %+v, want the third", c)
			}
			p.be.mu.Lock()
			p.be.cutTooLate = tooLate
			p.be.mu.Unlock()
			read(t, h, playlist, "DeleteId", "Value", u(ids[1]))
			p.be.callsBegun(t, "Cut", 1)
			if !tooLate {
				after := s.successor(t, 3)
				if after == nil || after.Entry != ids[2] {
					t.Fatalf("the successor after the cut %+v, want the third row", after)
				}
				s.ev.Submitted(s.id, after.ID)
				s.hear(after.ID, nil)
				p.heardNow(after.ID)
				if got := selected(t, h); got != u(ids[2]) || state(t, h) != "Playing" {
					t.Errorf("Id %s, %s; want the third row playing", got, state(t, h))
				}
				return
			}
			s.hear(next.ID, nil)
			p.waitSnap("stopped", func(sn *engine.Snapshot) bool { return sn.State() == engine.Stopped })
			if got := p.eng.Snapshot().LastStop().Reason; got != engine.ReasonRemovedAudible {
				t.Errorf("the stop's reason %s, want removed_entry_audible", got)
			}
			if got := state(t, h); got != "Stopped" {
				t.Errorf("TransportState %s, want Stopped", got)
			}
			if got := queueOf(t, h); !slices.Equal(got, []uint32{ids[0], ids[2]}) {
				t.Errorf("IdArray %v, want the deleted row gone", got)
			}
		})
	}
}

// TestATrackGoneAfterItsInsertIsSkipped pins a track the library stops
// serving after its Insert, through the player's library resolver at the
// occurrence's open: one removed from the index (not_indexed) and one whose
// file was deleted with its row kept (missing_file) are skipped with those
// reasons, Id moving on to the entry that plays, and Read still gives each
// Uri back as inserted.
func TestATrackGoneAfterItsInsertIsSkipped(t *testing.T) {
	p := newTestPlayer(t)
	lib := newRealLibrary(t)
	for _, rel := range []string{"A/1 removed.flac", "A/2 deleted.flac", "A/3 kept.flac"} {
		lib.add(rel, []byte(rel))
	}
	h := p.handler("")
	h.library = lib.r
	var uris []string
	for _, rel := range []string{"A/1 removed.flac", "A/2 deleted.flac", "A/3 kept.flac"} {
		uris = append(uris, uriOf(library.Reference(rel).String()))
	}
	ids := handOff(t, h, uris, false)
	must(t, "delete the row", lib.store.DeleteTrack(bg, "A/1 removed.flac"))
	must(t, "delete the file", os.Remove(filepath.Join(lib.root, "A", "2 deleted.flac")))
	p.be.mu.Lock()
	p.be.refuseStart = func(req engine.StartRequest) error { return lib.opens(req.First.Ref) }
	p.be.mu.Unlock()
	read(t, h, playlist, "Play")
	if got := selected(t, h); got != u(ids[2]) {
		t.Errorf("Id %s once Play answered, want the entry that plays, %d", got, ids[2])
	}
	for i, why := range []engine.Reason{library.ReasonNotIndexed, engine.ReasonMissing} {
		if sk, ok := p.eng.Snapshot().Skipped(ids[i]); !ok || sk.Reason != why {
			t.Errorf("entry %d's skip: %+v %v, want %s", ids[i], sk, ok, why)
		}
	}
	for i, id := range ids {
		if got := read(t, h, playlist, "Read", "Id", u(id))["Uri"]; got != uris[i] {
			t.Errorf("Read(%d)'s Uri %q, want %q", id, got, uris[i])
		}
	}
}

// TestOneBitRelaunchedReattachesReadOnly pins 1-bit's re-attach after it is
// killed and relaunched while the bridge keeps running: it only reads, and
// IdArray (the token and the IDs), Read of each ID (byte for byte), Id and
// TransportState are what they were. An engine restart is another matter:
// the queue and its IDs are kept, Read still byte for byte, the transport
// stopped, and the token moves on, so a controller's cached view is stale.
func TestOneBitRelaunchedReattachesReadOnly(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	uris := []string{uriOf(goldenID), uriOf(trackB), uriOf(trackC)}
	ids := handOff(t, h, uris, true)
	playing(t, p, h)
	before := read(t, h, playlist, "IdArray")
	gen := p.eng.Snapshot().Generation()
	reattach := func(what string, wantState string) map[string]string {
		t.Helper()
		got := read(t, h, playlist, "IdArray")
		if arr := decodeIDArray(t, got["Array"]); !slices.Equal(arr, ids) {
			t.Errorf("%s: IdArray %v, want %v", what, arr, ids)
		}
		for i, id := range ids {
			if r := read(t, h, playlist, "Read", "Id", u(id)); r["Uri"] != uris[i] || r["Metadata"] != didl(fmt.Sprintf("Track %d", i+1)) {
				t.Errorf("%s: Read(%d) %v", what, id, r)
			}
		}
		if got := state(t, h); got != wantState {
			t.Errorf("%s: TransportState %s, want %s", what, got, wantState)
		}
		return got
	}
	if got := reattach("relaunched", "Playing"); got["Token"] != before["Token"] {
		t.Errorf("the token moved from %s to %s with nothing written", before["Token"], got["Token"])
	}
	if selected(t, h) != u(ids[0]) || p.eng.Snapshot().Generation() != gen {
		t.Errorf("the re-attach changed the engine")
	}
	p.restart()
	h = p.handler("")
	if got := reattach("after an engine restart", "Stopped"); got["Token"] == before["Token"] {
		t.Errorf("the token %s survived an engine restart", got["Token"])
	}
}

// TestAnUncleanStopInsideTheWriteCoalescingLosesOnlyTheTail pins an engine
// that stops uncleanly while its queue file holds an older revision (the
// newest still in the writer's hands: here the queue file's directory, which
// holds nothing else, refuses the write, so the window is held open without a
// timer, and the disk as it stands is copied, as a crash leaves it): the
// restart loads the older revision, the entries inserted since lost; no ID is
// handed out again, an Insert after the restart answering an ID past every
// one handed out before; and the token is new.
func TestAnUncleanStopInsideTheWriteCoalescingLosesOnlyTheTail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a directory's mode: the queue's write cannot be held back by it")
	}
	files := func(p *testPlayer) { p.queue, p.reserve = filepath.Join("q", "queue.json"), "player-ids.json" }
	p := newTestPlayerWith(t, files)
	h := p.handler("")
	head := handOff(t, h, []string{uriOf(trackID(1)), uriOf(trackID(2))}, false)
	p.restart()
	h = p.handler("")
	if got := queueOf(t, h); !slices.Equal(got, head) {
		t.Fatalf("the clean restart's queue %v, want %v", got, head)
	}
	q := filepath.Join(p.dir, "q")
	must(t, "hold the queue's writes", os.Chmod(q, 0o500))
	t.Cleanup(func() { _ = os.Chmod(q, 0o700) })
	tail := []uint32{newID(t, h, head[1], uriOf(trackID(3)), ""), newID(t, h, 0, uriOf(trackID(4)), "")}
	token := read(t, h, playlist, "IdArray")["Token"]

	crashed := newTestPlayerWith(t, files)
	crashed.stop()
	copyTree(t, p.dir, crashed.dir)
	must(t, "let the copy's queue be written", os.Chmod(filepath.Join(crashed.dir, "q"), 0o700))
	if err := crashed.start(); err != nil {
		t.Fatalf("the engine does not start on the crashed copy: %v", err)
	}
	hc := crashed.handler("")
	if got := queueOf(t, hc); !slices.Equal(got, head) {
		t.Fatalf("after the unclean stop the queue is %v, want the revision on disk, %v (the tail %v lost)", got, head, tail)
	}
	if got := read(t, hc, playlist, "IdArray")["Token"]; got == token {
		t.Errorf("the token %s after the unclean stop is the one before it", got)
	}
	next := newID(t, hc, 0, uriOf(trackID(5)), "")
	for _, id := range append(head, tail...) {
		if next <= id {
			t.Errorf("the first Insert after the unclean stop answered %d, not past %d, handed out before", next, id)
		}
	}
}

// copyTree copies the regular files and directories below from to to, as the
// disk holds them at the copy: what an unclean stop leaves.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	must(t, "clear the copy", os.RemoveAll(to))
	err := filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
	must(t, "copy the data directory", err)
}
