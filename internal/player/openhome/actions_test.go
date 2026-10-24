package openhome

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
)

// bg is the tests' context for the engine's commands.
var bg = context.Background()

// TestTheReadsAndTheWritesAreEveryAction pins which actions this release
// answers: every action of the four services is a read (reads) or a write
// (writes), never both and never neither; 25 reads and 17 writes, the 42 of
// the SCPDs; and every read names an action its service has.
func TestTheReadsAndTheWritesAreEveryAction(t *testing.T) {
	nReads, nWrites := 0, 0
	for _, s := range services {
		for _, a := range s.actions {
			_, isRead := reads[s.name][a.name]
			isWrite := slices.Contains(writes[s.name], a.name)
			switch {
			case isRead && isWrite:
				t.Errorf("%s#%s is a read and a write", s.name, a.name)
			case !isRead && !isWrite:
				t.Errorf("%s#%s is neither a read nor a write", s.name, a.name)
			case isRead:
				nReads++
			default:
				nWrites++
			}
		}
	}
	for svc, m := range reads {
		s := servicesByPath[strings.ToLower(svc)]
		for name := range m {
			if s == nil || s.action(name) == nil {
				t.Errorf("the read %s#%s names no action", svc, name)
			}
		}
	}
	if nReads != 25 || nWrites != 17 {
		t.Errorf("%d reads and %d writes, want 25 and 17", nReads, nWrites)
	}
}

// sourceList is a SourceXml document as a controller reads it.
type sourceList struct {
	XMLName xml.Name `xml:"SourceList"`
	Sources []struct {
		Name       string `xml:"Name"`
		Type       string `xml:"Type"`
		Visible    string `xml:"Visible"`
		SystemName string `xml:"SystemName"`
	} `xml:"Source"`
}

// strictly decodes doc into v strictly, failing the test when it is not
// well-formed XML with nothing after its root.
func strictly(t *testing.T, what, doc string, v any) {
	t.Helper()
	d := xml.NewDecoder(strings.NewReader(doc))
	d.Strict = true
	if err := d.Decode(v); err != nil {
		t.Fatalf("%s does not read as XML: %v\n%s", what, err, doc)
	}
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("%s has something after its root: %v", what, err)
		}
		if cd, ok := tok.(xml.CharData); !ok || len(bytes.TrimSpace(cd)) != 0 {
			t.Fatalf("%s has something after its root: %v", what, tok)
		}
	}
}

// TestTheProductReadsAreTheSourcesOwn pins Product:1's reads: the vendor,
// the model and the product, the room the configured one (as the friendly
// name holds it: what XML cannot carry dropped, the rest escaped and read
// back) or the default name; never in standby; one source, Playlist, at
// index 0, in OpenHome's SourceList form; Source(0) that source and any
// other index fault 800; the optional services Info and Time.
func TestTheProductReadsAreTheSourcesOwn(t *testing.T) {
	p := newTestPlayer(t)
	for _, tc := range []struct{ room, want string }{{"Study", "Study"}, {"", DefaultName}, {"Kitchen\x01 & <Bar> \"1\"", "Kitchen & <Bar> \"1\""}} {
		h := p.handler(tc.room)
		got := read(t, h, product, "Product")
		want := map[string]string{"Room": tc.want, "Name": "1-bit Bridge Player", "Info": "The 1-bit bridge's own player", "Url": "https://1-bit.app", "ImageUri": ""}
		if !mapsEqual(got, want) {
			t.Errorf("room %q: Product %v, want %v", tc.room, got, want)
		}
	}
	h := p.handler("Study")
	for _, tc := range []struct {
		action string
		want   map[string]string
	}{
		{"Manufacturer", map[string]string{"Name": "1-bit", "Info": "", "Url": "https://1-bit.app", "ImageUri": ""}},
		{"Model", map[string]string{"Name": "1-bit Bridge Player", "Info": "The 1-bit bridge's own player", "Url": "https://1-bit.app", "ImageUri": ""}},
		{"Standby", map[string]string{"Value": "0"}},
		{"SourceCount", map[string]string{"Value": "1"}},
		{"SourceIndex", map[string]string{"Value": "0"}},
		{"Attributes", map[string]string{"Value": "Info Time"}},
		{"SourceXmlChangeCount", map[string]string{"Value": "0"}},
	} {
		if got := read(t, h, product, tc.action); !mapsEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.action, got, tc.want)
		}
	}
	xmlDoc := read(t, h, product, "SourceXml")["Value"]
	var list sourceList
	strictly(t, "SourceXml", xmlDoc, &list)
	if len(list.Sources) != 1 || list.Sources[0].Name != "Playlist" || list.Sources[0].Type != "Playlist" ||
		list.Sources[0].Visible != "true" || list.Sources[0].SystemName != "Playlist" {
		t.Errorf("SourceXml: %+v, want the one Playlist source, visible", list.Sources)
	}
	if got := read(t, h, product, "Source", "Index", "0"); !mapsEqual(got, map[string]string{"SystemName": "Playlist", "Type": "Playlist", "Name": "Playlist", "Visible": "1"}) {
		t.Errorf("Source(0): %v", got)
	}
	for _, index := range []string{"1", "4294967295"} {
		if r := invoke(t, h, product, "Source", "Index", index); r.fault != 800 {
			t.Errorf("Source(%s): %d, fault %d, want fault 800", index, r.status, r.fault)
		}
	}
}

// mapsEqual reports whether a and b hold the same keys and values (the
// package's listener.go declares a function named maps, so the standard
// library's is not imported here).
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// TestTransportStateIsTheEnginesTransport pins Playlist's TransportState:
// for every state, starting or not, and every pending intent, the one
// word the table gives; and through the engine, each that it reaches:
// Stopped; Buffering while the session starts; Playing once it is heard;
// Paused; a release to stop Stopped, to play Buffering, to pause Paused;
// maintenance Stopped.
func TestTransportStateIsTheEnginesTransport(t *testing.T) {
	for _, st := range []engine.State{engine.Stopped, engine.Playing, engine.Paused, engine.Releasing, engine.Maintenance, engine.State(99)} {
		for _, starting := range []bool{false, true} {
			for _, pending := range []engine.Intent{engine.IntentStop, engine.IntentPause, engine.IntentPlay} {
				want := "Stopped"
				switch {
				case st == engine.Playing && starting:
					want = "Buffering"
				case st == engine.Playing:
					want = "Playing"
				case st == engine.Paused:
					want = "Paused"
				case st == engine.Releasing && pending == engine.IntentPause:
					want = "Paused"
				case st == engine.Releasing && pending == engine.IntentPlay:
					want = "Buffering"
				}
				if got := transportState(st, starting, pending); got != want {
					t.Errorf("%s, starting %v, pending %s: %s, want %s", st, starting, pending, got, want)
				}
			}
		}
	}

	p := newTestPlayer(t)
	h := p.handler("")
	state := func(want, what string) {
		t.Helper()
		if got := read(t, h, playlist, "TransportState")["Value"]; got != want {
			t.Errorf("%s: TransportState %q, want %q (the engine: %s, starting %v, pending %s)", what, got, want,
				p.eng.Snapshot().State(), p.eng.Snapshot().Starting(), p.eng.Snapshot().Pending())
		}
	}
	state("Stopped", "an empty queue")
	ids := []uint32{p.add("a", nil), p.add("b", nil), p.add("c", nil)}
	must(t, "play", p.eng.Play(bg))
	s1 := p.be.session(t, 1)
	state("Buffering", "a session starting")
	s1.hear(s1.first.ID, nil)
	p.heardNow(s1.first.ID)
	state("Playing", "a heard")
	must(t, "pause", p.eng.Pause(bg))
	state("Paused", "paused")
	must(t, "resume", p.eng.Play(bg))
	state("Playing", "resumed")

	letGo := p.be.holdReleases()
	next := asyncErr(func() error { return p.eng.Next(bg) })
	p.waitSnap("a release to play b", func(s *engine.Snapshot) bool {
		return s.State() == engine.Releasing && s.Pending() == engine.IntentPlay
	})
	state("Buffering", "a release to play")
	letGo()
	s2 := p.be.session(t, 2)
	must(t, "next", <-next)
	state("Buffering", "b's session starting")
	s2.hear(s2.first.ID, nil)
	p.heardNow(s2.first.ID)
	must(t, "pause", p.eng.Pause(bg))

	letGo = p.be.holdReleases()
	must(t, "delete the paused b", p.eng.Delete(bg, ids[1]))
	p.waitSnap("a release to pause at c", func(s *engine.Snapshot) bool {
		return s.State() == engine.Releasing && s.Pending() == engine.IntentPause
	})
	state("Paused", "a release to pause")
	letGo()
	p.waitSnap("paused at c with no session", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	state("Paused", "paused with no session")
	must(t, "play c", p.eng.Play(bg))
	s3 := p.be.session(t, 3)
	s3.hear(s3.first.ID, nil)
	p.heardNow(s3.first.ID)

	letGo = p.be.holdReleases()
	stop := asyncErr(func() error { return p.eng.Stop(bg) })
	p.waitSnap("a release to stop", func(s *engine.Snapshot) bool {
		return s.State() == engine.Releasing && s.Pending() == engine.IntentStop
	})
	state("Stopped", "a release to stop")
	letGo()
	must(t, "stop", <-stop)
	state("Stopped", "stopped")

	hold, err := p.eng.EnterMaintenance(bg, p.eng.Snapshot().Generation(), "the test")
	must(t, "enter maintenance", err)
	state("Stopped", "maintenance")
	must(t, "exit maintenance", p.eng.ExitMaintenance(bg, hold.Lease))
}

// asyncErr runs f on a goroutine and returns where its error goes.
func asyncErr(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

// trackList is a ReadList TrackList document as a controller reads it.
type trackList struct {
	XMLName xml.Name `xml:"TrackList"`
	Entries []struct {
		ID       string `xml:"Id"`
		URI      string `xml:"Uri"`
		Metadata string `xml:"Metadata"`
	} `xml:"Entry"`
}

// entries is a ReadList's entries: their IDs, references and metadata.
func entries(t *testing.T, h *handler, idList string) trackList {
	t.Helper()
	var list trackList
	strictly(t, "TrackList", read(t, h, playlist, "ReadList", "IdList", idList)["TrackList"], &list)
	return list
}

// idsOf is a TrackList's IDs, in order.
func idsOf(list trackList) []string {
	var out []string
	for _, e := range list.Entries {
		out = append(out, e.ID)
	}
	return out
}

// u is id in decimal.
func u(id uint32) string { return strconv.FormatUint(uint64(id), 10) }

// TestIdReadAndReadListFollowTheQueue pins Id, Read and ReadList on the
// engine's queue: an empty queue selects nothing (Id 0), reads nothing
// (Read fault 800) and lists nothing; an insert into it is selected; Read
// is the entry's reference and metadata; ReadList answers the entries asked
// for that the queue holds, in the order asked, as often as asked, and
// leaves out an ID it does not hold; the selection moves with a command;
// a deleted entry reads as one the queue does not hold.
func TestIdReadAndReadListFollowTheQueue(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	if got := read(t, h, playlist, "Id")["Value"]; got != "0" {
		t.Errorf("an empty queue: Id %s, want 0", got)
	}
	if r := invoke(t, h, playlist, "Read", "Id", "1"); r.fault != 800 {
		t.Errorf("an empty queue: Read: %d, fault %d, want 800", r.status, r.fault)
	}
	if list := entries(t, h, "1 2 3"); len(list.Entries) != 0 {
		t.Errorf("an empty queue: ReadList %v, want no entry", idsOf(list))
	}
	a := p.add("a", []byte("<DIDL-Lite>a</DIDL-Lite>"))
	if got := read(t, h, playlist, "Id")["Value"]; got != u(a) {
		t.Errorf("one entry: Id %s, want the entry inserted into the empty queue, %d", got, a)
	}
	if got := read(t, h, playlist, "Read", "Id", u(a)); got["Uri"] != "a" || got["Metadata"] != "<DIDL-Lite>a</DIDL-Lite>" {
		t.Errorf("Read(%d): %v", a, got)
	}
	b, c := p.add("b", []byte("meta b")), p.add("c", nil)
	list := entries(t, h, u(c)+" "+u(a)+" 999999 "+u(b)+" "+u(c))
	if got, want := idsOf(list), []string{u(c), u(a), u(b), u(c)}; !slices.Equal(got, want) {
		t.Errorf("ReadList: %v, want %v: the order asked, as often as asked, 999999 left out", got, want)
	}
	if list.Entries[2].URI != "b" || list.Entries[2].Metadata != "meta b" || list.Entries[0].Metadata != "" {
		t.Errorf("ReadList's entries: %+v", list.Entries)
	}
	must(t, "play b", p.eng.PlayID(bg, b))
	if got := read(t, h, playlist, "Id")["Value"]; got != u(b) {
		t.Errorf("after PlayID(b): Id %s, want %d", got, b)
	}
	must(t, "delete c", p.eng.Delete(bg, c))
	if r := invoke(t, h, playlist, "Read", "Id", u(c)); r.fault != 800 {
		t.Errorf("Read of a deleted entry: %d, fault %d, want 800", r.status, r.fault)
	}
	if got := idsOf(entries(t, h, u(c)+" "+u(a))); !slices.Equal(got, []string{u(a)}) {
		t.Errorf("ReadList with a deleted entry: %v, want only %d", got, a)
	}
}

// TestReadListTakesWhiteSpaceAndRefusesWhatIsNoID pins the IdList's
// grammar: decimal ui4s separated by XML's white space (space, tab, line
// feed, carriage return), leading, trailing and repeated white space taken,
// leading zeros taken, none at all an empty list; a token that is not a ui4
// (a sign, hexadecimal, a fraction, a comma, a word, past 4294967295, a
// space that is not XML's) is fault 402.
func TestReadListTakesWhiteSpaceAndRefusesWhatIsNoID(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	a, b := p.add("a", nil), p.add("b", nil)
	for _, list := range []string{u(a) + " " + u(b), "  " + u(a) + "\t\t" + u(b) + "  ", "\n" + u(a) + "\r\n" + u(b) + "\r", "00" + u(a) + " " + u(b)} {
		if got := idsOf(entries(t, h, list)); !slices.Equal(got, []string{u(a), u(b)}) {
			t.Errorf("IdList %q: %v, want %d and %d", list, got, a, b)
		}
	}
	for _, list := range []string{"", "   ", "\t\r\n"} {
		if got := entries(t, h, list); len(got.Entries) != 0 {
			t.Errorf("IdList %q: %v, want an empty TrackList", list, idsOf(got))
		}
	}
	for _, list := range []string{"-1", "+" + u(a), "0x10", "1.0", u(a) + "," + u(b), "a", "4294967296", u(a) + " x", u(a) + " " + u(b)} {
		if r := invoke(t, h, playlist, "ReadList", "IdList", list); r.fault != 402 {
			t.Errorf("IdList %q: %d, fault %d, want 402", list, r.status, r.fault)
		}
	}
}

// TestReadListIsBounded pins ReadList's two bounds, each fault 605 (String
// Argument Too Long): more IDs than the engine reads at once
// (engine.MaxReadList, 1000; exactly that many is answered), and a
// TrackList past maxTrackList, which entries of the most metadata the
// engine takes, each character of it escaped to five, reach in 26 copies
// of one ID.
func TestReadListIsBounded(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	a := p.add("a", nil)
	ids := strings.TrimSpace(strings.Repeat(u(a)+" ", engine.MaxReadList))
	if got := entries(t, h, ids); len(got.Entries) != engine.MaxReadList {
		t.Errorf("%d IDs: %d entries, want every one", engine.MaxReadList, len(got.Entries))
	}
	if r := invoke(t, h, playlist, "ReadList", "IdList", ids+" "+u(a)); r.fault != 605 {
		t.Errorf("%d IDs: %d, fault %d, want 605", engine.MaxReadList+1, r.status, r.fault)
	}
	big := p.add("big", bytes.Repeat([]byte("&"), engine.MaxMetadataBytes))
	if r := invoke(t, h, playlist, "ReadList", "IdList", strings.TrimSpace(strings.Repeat(u(big)+" ", 26))); r.fault != 605 {
		t.Errorf("a TrackList past %d bytes: %d, fault %d, want 605", maxTrackList, r.status, r.fault)
	}
	if got := entries(t, h, u(big)+" "+u(a)); len(got.Entries) != 2 || len(got.Entries[0].Metadata) != engine.MaxMetadataBytes {
		t.Errorf("an entry of the most metadata the engine takes: %d entries", len(got.Entries))
	}
}

// decodeIDArray is an IdArray's Array as a controller reads it.
func decodeIDArray(t *testing.T, array string) []uint32 {
	t.Helper()
	raw, err := base64.StdEncoding.Strict().DecodeString(array)
	if err != nil || len(raw)%4 != 0 {
		t.Fatalf("the Array %q is no big-endian ui4s in base64: %v", array, err)
	}
	var out []uint32
	for i := 0; i < len(raw); i += 4 {
		out = append(out, binary.BigEndian.Uint32(raw[i:]))
	}
	return out
}

// TestIdArrayIsTheTokenAndTheIDsInQueueOrder pins IdArray and
// IdArrayChanged: the token and the IDs, as big-endian ui4s in base64, in
// queue order, from one snapshot (an empty queue is an empty Array);
// IdArrayChanged is false for the token now and true for any other, the one
// before an edit included; after a restart the token is the new run's, the
// queue's IDs the same, and the old token changed.
func TestIdArrayIsTheTokenAndTheIDsInQueueOrder(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	got := read(t, h, playlist, "IdArray")
	if got["Array"] != "" || got["Token"] != u(p.eng.Snapshot().Token()) {
		t.Errorf("an empty queue: IdArray %v, want the engine's token %d and an empty Array", got, p.eng.Snapshot().Token())
	}
	a, c := p.add("a", nil), p.add("c", nil)
	r, err := engine.NewReference("b")
	must(t, "reference", err)
	b, err := p.eng.InsertAfter(bg, a, r, nil)
	must(t, "insert b after a", err)
	got = read(t, h, playlist, "IdArray")
	token := p.eng.Snapshot().Token()
	if ids := decodeIDArray(t, got["Array"]); !slices.Equal(ids, []uint32{a, b, c}) || got["Token"] != u(token) {
		t.Errorf("IdArray: token %s, IDs %v; want %d and %v, queue order", got["Token"], ids, token, []uint32{a, b, c})
	}
	if v := read(t, h, playlist, "IdArrayChanged", "Token", u(token))["Value"]; v != "0" {
		t.Errorf("IdArrayChanged(the token now): %s, want 0", v)
	}
	must(t, "delete b", p.eng.Delete(bg, b))
	if v := read(t, h, playlist, "IdArrayChanged", "Token", u(token))["Value"]; v != "1" {
		t.Errorf("IdArrayChanged(the token before an edit): %s, want 1", v)
	}
	before := p.eng.Snapshot().Token()
	p.restart()
	h = p.handler("")
	got = read(t, h, playlist, "IdArray")
	if ids := decodeIDArray(t, got["Array"]); !slices.Equal(ids, []uint32{a, c}) || got["Token"] == u(before) || got["Token"] != u(p.eng.Snapshot().Token()) {
		t.Errorf("after a restart: token %s (was %d), IDs %v; want the new run's token and %v", got["Token"], before, ids, []uint32{a, c})
	}
	if v := read(t, h, playlist, "IdArrayChanged", "Token", u(before))["Value"]; v != "1" {
		t.Errorf("after a restart: IdArrayChanged(the last run's token): %s, want 1", v)
	}
}

// TestTracksMaxRepeatShuffleAndProtocolInfo pins the remaining Playlist
// reads: TracksMax the engine's MaxEntries; Repeat the engine's, as it
// changes; Shuffle false; and ProtocolInfo an http-get entry for each
// container the backend plays, FLAC, WAV, AIFF and ALAC in M4A, each type
// the one the library's MediaServer gives that file, for every renderer
// profile it knows.
func TestTracksMaxRepeatShuffleAndProtocolInfo(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	if got := read(t, h, playlist, "TracksMax")["Value"]; got != "1000" {
		t.Errorf("TracksMax %s, want 1000", got)
	}
	if got := read(t, h, playlist, "Repeat")["Value"]; got != "0" {
		t.Errorf("Repeat %s, want 0", got)
	}
	must(t, "repeat on", p.eng.SetRepeat(bg, true))
	if got := read(t, h, playlist, "Repeat")["Value"]; got != "1" {
		t.Errorf("Repeat after SetRepeat(true) %s, want 1", got)
	}
	if got := read(t, h, playlist, "Shuffle")["Value"]; got != "0" {
		t.Errorf("Shuffle %s, want 0", got)
	}
	const want = "http-get:*:audio/x-flac:*,http-get:*:audio/wav:*,http-get:*:audio/aiff:*,http-get:*:audio/mp4:*"
	got := read(t, h, playlist, "ProtocolInfo")["Value"]
	if got != want {
		t.Errorf("ProtocolInfo %q, want %q", got, want)
	}
	entries := strings.Split(got, ",")
	for _, profile := range dlna.Profiles {
		for _, ua := range profile.UserAgentMatchers {
			for _, ext := range []string{".flac", ".wav", ".aiff", ".aif", ".aifc", ".m4a"} {
				if e := "http-get:*:" + dlna.ResolveMIMEType(ua, ext) + ":*"; !slices.Contains(entries, e) {
					t.Errorf("the MediaServer gives %s %s to %q, which ProtocolInfo does not list", ext, dlna.ResolveMIMEType(ua, ext), ua)
				}
			}
		}
	}
}

// details is a FLAC's report: 180.49997 s at 44.1 kHz, 16 bits.
var details = engine.Details{Frames: 44_100*180 + 22_049, RateHz: 44_100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}

// TestTimeAndInfoReadTheTrackHeard pins Time and Info on the engine's track:
// nothing heard reads zero and empty; a track heard is its reference and
// metadata, its Duration the backend's exact length rounded to the nearest
// second (never the metadata's, which says something else here), its
// Seconds the whole seconds of its position, its Details the decoder's
// format; each track heard counts one, each change of the details one; a
// pause keeps the track; a stop reads zero again, one more change of the
// details.
func TestTimeAndInfoReadTheTrackHeard(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	zero := func(what string, tracks, changes string) {
		t.Helper()
		if got := read(t, h, timeSvc, "Time"); !mapsEqual(got, map[string]string{"TrackCount": tracks, "Duration": "0", "Seconds": "0"}) {
			t.Errorf("%s: Time %v", what, got)
		}
		if got := read(t, h, info, "Counters"); !mapsEqual(got, map[string]string{"TrackCount": tracks, "DetailsCount": changes, "MetatextCount": "0"}) {
			t.Errorf("%s: Counters %v", what, got)
		}
		if got := read(t, h, info, "Track"); !mapsEqual(got, map[string]string{"Uri": "", "Metadata": ""}) {
			t.Errorf("%s: Track %v", what, got)
		}
		if got := read(t, h, info, "Details"); !mapsEqual(got, map[string]string{"Duration": "0", "BitRate": "0", "BitDepth": "0", "SampleRate": "0", "Lossless": "0", "CodecName": ""}) {
			t.Errorf("%s: Details %v", what, got)
		}
	}
	zero("nothing heard", "0", "0")
	if got := read(t, h, info, "Metatext"); got["Value"] != "" {
		t.Errorf("Metatext %v, want empty", got)
	}
	meta := `<DIDL-Lite><item><res duration="0:09:59.000" sampleFrequency="96000" bitsPerSample="24">a</res></item></DIDL-Lite>`
	p.add("a", []byte(meta))
	p.add("b", nil)
	must(t, "play", p.eng.Play(bg))
	s := p.be.session(t, 1)
	zero("a starting", "0", "0")
	s.hear(s.first.ID, &details)
	p.heardNow(s.first.ID)
	s.position(s.first.ID, 44_100*61+44_099, 44_100)
	p.waitSnap("a's position", func(sn *engine.Snapshot) bool { return sn.Position().Frames == 44_100*61+44_099 })
	if got := read(t, h, timeSvc, "Time"); !mapsEqual(got, map[string]string{"TrackCount": "1", "Duration": "180", "Seconds": "61"}) {
		t.Errorf("a heard: Time %v, want 1, the decoder's 180 s (not the metadata's 599), 61", got)
	}
	if got := read(t, h, info, "Counters"); !mapsEqual(got, map[string]string{"TrackCount": "1", "DetailsCount": "1", "MetatextCount": "0"}) {
		t.Errorf("a heard: Counters %v", got)
	}
	if got := read(t, h, info, "Track"); !mapsEqual(got, map[string]string{"Uri": "a", "Metadata": meta}) {
		t.Errorf("a heard: Track %v", got)
	}
	if got := read(t, h, info, "Details"); !mapsEqual(got, map[string]string{"Duration": "180", "BitRate": "1411200", "BitDepth": "16", "SampleRate": "44100", "Lossless": "1", "CodecName": "FLAC"}) {
		t.Errorf("a heard: Details %v", got)
	}
	b := s.successor(t, 1)
	s.ev.Submitted(s.id, b.ID)
	db := engine.Details{Frames: 96_000*200 + 48_000, RateHz: 96_000, Bits: 24, Channels: 2, Codec: "WAV", Lossless: true}
	s.hear(b.ID, &db)
	p.heardNow(b.ID)
	if got := read(t, h, timeSvc, "Time"); !mapsEqual(got, map[string]string{"TrackCount": "2", "Duration": "201", "Seconds": "0"}) {
		t.Errorf("b heard: Time %v, want 2, 200.5 s rounded to 201, nothing of b measured yet", got)
	}
	if got := read(t, h, info, "Details"); !mapsEqual(got, map[string]string{"Duration": "201", "BitRate": "4608000", "BitDepth": "24", "SampleRate": "96000", "Lossless": "1", "CodecName": "WAV"}) {
		t.Errorf("b heard: Details %v", got)
	}
	must(t, "pause", p.eng.Pause(bg))
	if got := read(t, h, info, "Counters"); !mapsEqual(got, map[string]string{"TrackCount": "2", "DetailsCount": "2", "MetatextCount": "0"}) {
		t.Errorf("paused: Counters %v", got)
	}
	if got := read(t, h, info, "Track")["Uri"]; got != "b" {
		t.Errorf("paused: Track's Uri %q, want b", got)
	}
	must(t, "stop", p.eng.Stop(bg))
	zero("stopped", "2", "3")
	p.restart()
	h = p.handler("")
	zero("after a restart", "0", "0")
}

// TestADeletedAudibleEntryIsNoLongerHeard pins Time and Info across the
// deletion of the entry heard: the engine releases its session to play the
// entry that followed, and in that step nothing is heard, so Track is empty
// and Time and Details are zero (the count of tracks kept), the transport
// Buffering, a release to play; never the next entry's values under the
// deleted one's occurrence; then the next entry heard is the track.
func TestADeletedAudibleEntryIsNoLongerHeard(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	a := p.add("a", []byte("meta a"))
	p.add("b", []byte("meta b"))
	must(t, "play", p.eng.Play(bg))
	s := p.be.session(t, 1)
	s.hear(s.first.ID, &details)
	p.heardNow(s.first.ID)
	letGo := p.be.holdReleases()
	defer letGo()
	must(t, "delete a", p.eng.Delete(bg, a))
	p.waitSnap("the release to play b", func(sn *engine.Snapshot) bool {
		return sn.State() == engine.Releasing && sn.Pending() == engine.IntentPlay
	})
	if got := read(t, h, info, "Track"); !mapsEqual(got, map[string]string{"Uri": "", "Metadata": ""}) {
		t.Errorf("a deleted while heard: Track %v, want nothing heard", got)
	}
	if got := read(t, h, timeSvc, "Time"); !mapsEqual(got, map[string]string{"TrackCount": "1", "Duration": "0", "Seconds": "0"}) {
		t.Errorf("a deleted while heard: Time %v", got)
	}
	if got := read(t, h, info, "Details")["Duration"]; got != "0" {
		t.Errorf("a deleted while heard: Details' Duration %s", got)
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Buffering" {
		t.Errorf("a deleted while heard: TransportState %s, want Buffering", got)
	}
	letGo()
	s2 := p.be.session(t, 2)
	s2.hear(s2.first.ID, nil)
	p.heardNow(s2.first.ID)
	if got := read(t, h, info, "Track"); !mapsEqual(got, map[string]string{"Uri": "b", "Metadata": "meta b"}) {
		t.Errorf("b heard: Track %v", got)
	}
	if got := read(t, h, info, "Counters"); !mapsEqual(got, map[string]string{"TrackCount": "2", "DetailsCount": "2", "MetatextCount": "0"}) {
		t.Errorf("b heard with nothing reported: Counters %v, want 2 tracks and 2 changes (a's details, then none)", got)
	}
}

// TestTheTimeIsHeldForOneBit pins Time's bounds and Duration's rounding:
// every value held to 10,000,000, which 1-bit reads at most; the length
// rounded to the nearest second, a half up, an odd rate's half included,
// with no overflow at int64's top; nothing known is 0. Through the engine,
// a length and a position past the bound read the bound.
func TestTheTimeIsHeldForOneBit(t *testing.T) {
	track := func(frames int64, rate int) played {
		return played{ok: true, track: engine.Track{HasDetails: true, Details: engine.Details{Frames: frames, RateHz: rate}}}
	}
	for _, tc := range []struct {
		frames int64
		rate   int
		want   int64
	}{
		{0, 44_100, 0}, {-5, 44_100, 0}, {44_100, 0, 0}, {44_100 * 3, 44_100, 3}, {44_100*3 + 22_049, 44_100, 3},
		{44_100*3 + 22_050, 44_100, 4}, {11_025*2 + 5_512, 11_025, 2}, {11_025*2 + 5_513, 11_025, 3},
		{math.MaxInt64, 1, math.MaxInt64}, {math.MaxInt64, 2, math.MaxInt64/2 + 1},
	} {
		if got := track(tc.frames, tc.rate).duration(); got != tc.want {
			t.Errorf("%d frames at %d Hz: %d s, want %d", tc.frames, tc.rate, got, tc.want)
		}
	}
	if got := (played{ok: true, track: engine.Track{Details: engine.Details{Frames: 44_100, RateHz: 44_100}}}).duration(); got != 0 {
		t.Errorf("a track with no details reported: %d s, want 0", got)
	}
	big := track(math.MaxInt64, 1)
	big.seconds = math.MaxInt64
	if tc, d, s := timeOf(math.MaxUint64, big); tc != timeMax || d != timeMax || s != timeMax {
		t.Errorf("past the bound: %d, %d, %d; want %d each", tc, d, s, timeMax)
	}
	if tc, d, s := timeOf(timeMax, track(timeMax*44_100, 44_100)); tc != timeMax || d != timeMax || s != 0 {
		t.Errorf("at the bound: %d, %d, %d", tc, d, s)
	}

	p := newTestPlayer(t)
	h := p.handler("")
	p.add("a", nil)
	must(t, "play", p.eng.Play(bg))
	s := p.be.session(t, 1)
	long := engine.Details{Frames: 44_100 * 20_000_000, RateHz: 44_100, Bits: 16, Channels: 2, Codec: "FLAC", Lossless: true}
	s.hear(s.first.ID, &long)
	p.heardNow(s.first.ID)
	s.position(s.first.ID, 44_100*30_000_000, 44_100)
	p.waitSnap("the position", func(sn *engine.Snapshot) bool { return sn.Position().Frames > 0 })
	if got := read(t, h, timeSvc, "Time"); !mapsEqual(got, map[string]string{"TrackCount": "1", "Duration": "10000000", "Seconds": "10000000"}) {
		t.Errorf("past the bound: Time %v", got)
	}
	if got := read(t, h, info, "Details")["Duration"]; got != "10000000" {
		t.Errorf("past the bound: Details' Duration %s", got)
	}
}

// readCalls are a valid request of each read action, its arguments named.
var readCalls = func() [][]string {
	var out [][]string
	for _, s := range services {
		for _, a := range s.actions {
			if reads[s.name][a.name] == nil {
				continue
			}
			c := []string{s.path, a.name}
			for _, g := range a.ins() {
				v := "0"
				if s.variable(g.related).dataType == typeString {
					v = ""
				}
				c = append(c, g.name, v)
			}
			out = append(out, c)
		}
	}
	return out
}()

// endedEngine is an engine whose Run a panic ended: its Done closed, its
// last snapshot one published before (not Closed), as the engine leaves
// them when a panic on its owner abandons it.
type endedEngine struct{ *engine.Engine }

func (endedEngine) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestNoEngineIsOneFaultForEveryRead pins the read actions while no engine
// runs: with none given, with one whose Run has ended (its last snapshot
// Closed), and with one whose Run a panic ended (Done closed, its last
// snapshot not), every read is fault 501 (Action Failed), never an answer
// from an engine that is gone; the writes stay 602.
func TestNoEngineIsOneFaultForEveryRead(t *testing.T) {
	everyRead := func(t *testing.T, h *handler, want int) {
		t.Helper()
		for _, c := range readCalls {
			s := servicesByPath[c[0]]
			if r := invoke(t, h, s, c[1], c[2:]...); r.fault != want {
				t.Errorf("%s#%s: %d, fault %d, want %d", s.name, c[1], r.status, r.fault, want)
			}
		}
	}
	t.Run("none given", func(t *testing.T) { everyRead(t, testHandler(), 501) })
	t.Run("one whose Run has ended", func(t *testing.T) {
		p := newTestPlayer(t)
		p.add("a", nil)
		h := p.handler("")
		if r := invoke(t, h, playlist, "TracksMax"); r.status != http.StatusOK {
			t.Fatalf("a running engine's TracksMax: %d, fault %d", r.status, r.fault)
		}
		p.stop()
		everyRead(t, h, 501)
		if r := invoke(t, h, playlist, "Insert", "AfterId", "0", "Uri", "a", "Metadata", ""); r.fault != 602 {
			t.Errorf("a write with the engine ended: %d, fault %d, want 602", r.status, r.fault)
		}
	})
	t.Run("one shutting down", func(t *testing.T) {
		p := newTestPlayer(t)
		p.add("a", nil)
		must(t, "play", p.eng.Play(bg))
		s := p.be.session(t, 1)
		s.hear(s.first.ID, nil)
		p.heardNow(s.first.ID)
		letGo := p.be.holdReleases()
		defer letGo()
		h := p.handler("")
		p.shut()
		p.waitSnap("the shutdown begun", func(sn *engine.Snapshot) bool { return sn.Closed() })
		select {
		case <-p.eng.Done():
			t.Fatal("the engine's Run returned with its release held")
		default:
		}
		everyRead(t, h, 501)
	})
	t.Run("one whose Run a panic ended", func(t *testing.T) {
		p := newTestPlayer(t)
		p.add("a", nil)
		if p.eng.Snapshot().Closed() {
			t.Fatal("the running engine's snapshot is Closed")
		}
		everyRead(t, newHandler(Device{UUID: testUUID}, endedEngine{p.eng}, DefaultLimits.orDefaults(), nil), 501)
	})
}

// TestEveryValueRoundTripsThroughItsEscaping pins the escaping: metadata
// that is XML 1.0 text (markup, references, quotes, an apostrophe, tab,
// line feed and carriage return, a CDATA end, non-ASCII) reads back byte
// for byte from Read, from Info's Track, and from ReadList's TrackList,
// escaped once more there; metadata holding what XML 1.0 cannot carry (a
// control, a byte that is not UTF-8, U+FFFE) still makes a well-formed
// reply, each such character U+FFFD, at either level.
func TestEveryValueRoundTripsThroughItsEscaping(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	text := "<DIDL-Lite xmlns:dc=\"http://purl.org/dc/elements/1.1/\"><item id='1'><dc:title>Q&amp;A \"it's\" &lt;b&gt;</dc:title></item></DIDL-Lite>" +
		"\t\n\r\r\n é 漢 🎵 ]]> &#x41;"
	a := p.add("a", []byte(text))
	if got := read(t, h, playlist, "Read", "Id", u(a))["Metadata"]; got != text {
		t.Errorf("Read's Metadata:\n%q\nwant\n%q", got, text)
	}
	if list := entries(t, h, u(a)); len(list.Entries) != 1 || list.Entries[0].Metadata != text || list.Entries[0].URI != "a" {
		t.Errorf("ReadList's entry: %+v\nwant the metadata %q", list.Entries, text)
	}
	must(t, "play", p.eng.Play(bg))
	s := p.be.session(t, 1)
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	if got := read(t, h, info, "Track")["Metadata"]; got != text {
		t.Errorf("Info's Track's Metadata:\n%q\nwant\n%q", got, text)
	}
	bad := p.add("bad", []byte("a\x01b\xffc￾d\x00"))
	const want = "a�b�c�d�"
	if got := read(t, h, playlist, "Read", "Id", u(bad))["Metadata"]; got != want {
		t.Errorf("Read of what XML cannot carry: %q, want %q", got, want)
	}
	if list := entries(t, h, u(bad)); len(list.Entries) != 1 || list.Entries[0].Metadata != want {
		t.Errorf("ReadList of what XML cannot carry: %+v, want %q", list.Entries, want)
	}
}

// TestOneBitsCastStartAndPoll pins 1-bit's own sequence through the SOAP
// layer: at a cast start, Standby (awake), SourceXml and SourceIndex (the
// first visible Playlist source is the active one), TracksMax (a window
// 1-bit takes: above 0, at most 16,384), and after its inserts IdArray once,
// the token and exactly the IDs inserted; then the poll, Id, TransportState
// (one of the four words) and Time (each value at most 10,000,000), and
// Read of the ID heard, its Uri the reference inserted exactly, through the
// queue to its end: Stopped on the last ID.
func TestOneBitsCastStartAndPoll(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("Living Room")
	if got := read(t, h, product, "Standby")["Value"]; got != "0" {
		t.Fatalf("Standby %s: 1-bit would wake the player first", got)
	}
	var list sourceList
	strictly(t, "SourceXml", read(t, h, product, "SourceXml")["Value"], &list)
	first := -1
	for i, src := range list.Sources {
		if src.Name == "" || src.Type == "" || src.Visible == "" {
			t.Fatalf("source %d lacks its Type, Name or Visible, which fails 1-bit's whole list: %+v", i, src)
		}
		if first < 0 && src.Type == "Playlist" && src.Visible == "true" {
			first = i
		}
	}
	if index := read(t, h, product, "SourceIndex")["Value"]; first < 0 || index != strconv.Itoa(first) {
		t.Fatalf("SourceIndex %s, the first visible Playlist source %d: 1-bit would switch", index, first)
	}
	if max, err := strconv.Atoi(read(t, h, playlist, "TracksMax")["Value"]); err != nil || max <= 0 || max > 16_384 {
		t.Fatalf("TracksMax %d (%v): 1-bit would keep its own window", max, err)
	}
	uris := []string{"http://192.168.1.5:7790/dlna/file/0123456789abcdef", "http://192.168.1.5:7790/dlna/file/fedcba9876543210",
		"http://192.168.1.5:7790/dlna/file/00112233445566ff"}
	var ids []uint32
	for _, uri := range uris {
		ids = append(ids, p.add(uri, []byte(`<DIDL-Lite/>`)))
	}
	got := read(t, h, playlist, "IdArray")
	if arr := decodeIDArray(t, got["Array"]); !slices.Equal(arr, ids) || got["Token"] != u(p.eng.Snapshot().Token()) {
		t.Fatalf("IdArray: token %s, IDs %v; 1-bit wants the token and exactly %v", got["Token"], arr, ids)
	}

	poll := func(what string, wantID uint32, wantState string, wantTracks string) map[string]string {
		t.Helper()
		if id := read(t, h, playlist, "Id")["Value"]; id != u(wantID) {
			t.Errorf("%s: Id %s, want %d", what, id, wantID)
		}
		state := read(t, h, playlist, "TransportState")["Value"]
		if !slices.Contains([]string{"Playing", "Paused", "Stopped", "Buffering"}, state) || state != wantState {
			t.Errorf("%s: TransportState %q, want %q", what, state, wantState)
		}
		tm := read(t, h, timeSvc, "Time")
		for _, k := range []string{"TrackCount", "Duration", "Seconds"} {
			v, err := strconv.ParseUint(tm[k], 10, 32)
			if err != nil || v > timeMax {
				t.Errorf("%s: Time's %s %q, which 1-bit refuses", what, k, tm[k])
			}
		}
		if tm["TrackCount"] != wantTracks {
			t.Errorf("%s: Time's TrackCount %s, want %s", what, tm["TrackCount"], wantTracks)
		}
		return tm
	}
	must(t, "play", p.eng.Play(bg))
	s := p.be.session(t, 1)
	poll("starting", ids[0], "Buffering", "0")
	s.hear(s.first.ID, &details)
	p.heardNow(s.first.ID)
	s.position(s.first.ID, 44_100*5, 44_100)
	p.waitSnap("the position", func(sn *engine.Snapshot) bool { return sn.Position().Frames > 0 })
	if tm := poll("the first heard", ids[0], "Playing", "1"); tm["Duration"] != "180" || tm["Seconds"] != "5" {
		t.Errorf("the first heard: Time %v, want Duration 180 and Seconds 5", tm)
	}
	if uri := read(t, h, playlist, "Read", "Id", u(ids[0]))["Uri"]; uri != uris[0] {
		t.Errorf("Read(%d)'s Uri %q, want exactly the reference inserted, %q", ids[0], uri, uris[0])
	}
	for i := 1; i < len(ids); i++ {
		next := s.successor(t, i)
		s.ev.Submitted(s.id, next.ID)
		s.hear(next.ID, &details)
		p.heardNow(next.ID)
		poll("the next heard", ids[i], "Playing", strconv.Itoa(i+1))
	}
	if end := s.successor(t, len(ids)); end != nil {
		t.Fatalf("after the last entry the engine named %+v, want nothing", end)
	}
	s.ev.Finished(s.id)
	p.waitSnap("stopped at the end of the queue", func(sn *engine.Snapshot) bool { return sn.State() == engine.Stopped })
	poll("the end of the queue", ids[2], "Stopped", "3")
}

// TestTheSurfaceAnswersReadsFromItsEngine pins the wiring: a surface
// started with an engine (Config.Engine) answers a read over its own
// listener, admitted from the LAN, from that engine's state.
func TestTheSurfaceAnswersReadsFromItsEngine(t *testing.T) {
	p := newTestPlayer(t)
	a := p.add("a", nil)
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s, err := Start(Config{Device: Device{UUID: testUUID, ModelNumber: "test"}, Engine: p.eng, Addresses: box.Addresses, Listen: box.Listen})
	must(t, "start", err)
	t.Cleanup(func() { s.Stop() })
	at := listening(t, s, eth0)[eth0]
	req, err := http.NewRequest(http.MethodPost, "http://"+at.String()+"/openhome/playlist/control",
		strings.NewReader(envelope(call(playlist, "IdArray", ""))))
	must(t, "request", err)
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", header(playlist, "IdArray"))
	res, err := box.Client(netip.MustParseAddr("192.168.1.50")).Do(req)
	must(t, "IdArray", err)
	defer res.Body.Close()
	r := readResult(t, res, playlist, "IdArray")
	if r.status != http.StatusOK || !slices.Equal(decodeIDArray(t, r.values["Array"]), []uint32{a}) {
		t.Errorf("IdArray over the surface: %d, %v; want the engine's queue, %d", r.status, r.values, a)
	}
}
