package openhome

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// The actions (B470 PRs 5a3 and 5a4). A request that passes the SOAP table
// is answered here: a read from one snapshot of the engine, taken without
// waiting on its owner and never a command; a write by one engine command,
// within its budget (writes.go). While no engine runs, every action is fault
// 501 (Action Failed: UDA 1.1, §3.2.2, "the current state of the service
// prevents invoking that action"), never an answer from an engine that has
// ended, and that is asked first, before anything else a write does. Every
// string of a reply goes through escapeTo, once at each level of a value that
// is XML itself (SourceXml, ReadList's TrackList and the Metadata inside it),
// as OpenHome's own writes them.

// Engine is what the surface reads the player from and commands it with:
// the running engine (*engine.Engine), whose Snapshot never waits on its
// owner, and whose Done is closed once its Run has returned or a panic ended
// it; and the commands the write actions are, each answered as the engine's
// own callers are answered.
type Engine interface {
	Snapshot() *engine.Snapshot
	Done() <-chan struct{}
	Play(ctx context.Context, conds ...engine.Cond) error
	Pause(ctx context.Context, conds ...engine.Cond) error
	Stop(ctx context.Context, conds ...engine.Cond) error
	Next(ctx context.Context, conds ...engine.Cond) error
	Previous(ctx context.Context, conds ...engine.Cond) error
	PlayID(ctx context.Context, id uint32, conds ...engine.Cond) error
	PlayIndex(ctx context.Context, index int, conds ...engine.Cond) error
	InsertAfter(ctx context.Context, afterID uint32, ref engine.Reference, meta []byte, conds ...engine.Cond) (uint32, error)
	Delete(ctx context.Context, id uint32, conds ...engine.Cond) error
	DeleteAll(ctx context.Context, conds ...engine.Cond) error
	SetRepeat(ctx context.Context, on bool, conds ...engine.Cond) error
	SetShuffle(ctx context.Context, on bool, conds ...engine.Cond) error
}

var _ Engine = (*engine.Engine)(nil)

// The Source's fixed values.
const (
	// sourceName is the one source's name, type and system name: the
	// player's queue, OpenHome's Playlist source.
	sourceName = "Playlist"
	// attributes are the optional services the Source carries, as Product's
	// Attributes lists them, separated by spaces (ohPipeline's Product.cpp,
	// lines 209-215).
	attributes = "Info Time"
	// timeMax bounds what Time reports (TrackCount, Duration, Seconds) and
	// Info's Duration: 1-bit refuses a Time value past it.
	timeMax = 10_000_000
	// maxTrackList bounds ReadList's TrackList document, its closing tag
	// (trackListEnd) counted, before the reply escapes it once more: every
	// entry the queue holds (MaxEntries) answers within it with metadata of a
	// few KiB an entry; past it, a controller asks for fewer IDs (fault 605).
	maxTrackList = 8 << 20
	// trackListEnd closes ReadList's TrackList.
	trackListEnd = "</TrackList>"
)

// OpenHome's codes the reads answer, one service's each, as OpenHome's
// reference player, ohPipeline, answers them (read at master c1047a8dc801,
// 2026-10-21; the files cited by line here and in the rules file).
var (
	// faultIDNotFound is Playlist's Read of an ID the queue does not hold:
	// the engine's ReasonIDNotFound, OpenHome's 800, "Id not found"
	// (ProviderPlaylist.cpp, lines 23-24, answered by Read at 336-337).
	faultIDNotFound = faultReply{code: 800, description: "Id not found"}
	// faultSourceNotFound is Product's Source of an index past the one
	// source: FaultCode::kSourceNotFound, 801, "Source not found"
	// (FaultCode.cpp, line 21), which ProviderProduct.cpp answers at line 284
	// when Product's GetSourceDetails throws AvSourceNotFound for an index
	// past its last source (Product.cpp, lines 505-510).
	faultSourceNotFound = faultReply{code: 801, description: "Source not found"}
)

// sourceXML is Product's SourceXml: the one source, in the form OpenHome's
// reference player writes its SourceList (ohPipeline's Product.cpp, lines
// 299-317: each source's Name, Type, Visible as true or false, and
// SystemName, in that order, each value escaped at 319-329), the whole
// escaped once more as the argument's value.
var sourceXML = "<SourceList><Source><Name>" + escape(sourceName) + "</Name><Type>" + escape(sourceName) +
	"</Type><Visible>true</Visible><SystemName>" + escape(sourceName) + "</SystemName></Source></SourceList>"

// protocolInfo is Playlist's ProtocolInfo: an entry for each container the
// backend plays (FLAC, WAV, AIFF, ALAC in M4A; no DSD, nothing lossy), in
// http-get:*:<type>:* form, each type the one the library's MediaServer
// serves that file as (internal/dlna), so a controller that filters a
// track's <res> by it agrees.
var protocolInfo = func() string {
	var out []string
	for _, ext := range []string{".flac", ".wav", ".aiff", ".m4a"} {
		if e := "http-get:*:" + dlna.ResolveMIMEType("", ext) + ":*"; !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return strings.Join(out, ",")
}()

// reader answers one read action from one snapshot: its out-arguments'
// values, in the SCPD's order, each text the reply escapes, or a fault.
type reader func(r readRequest) ([][]byte, *faultReply)

// readRequest is what a read is answered from: the request's one snapshot,
// the device the surface describes, and the action's in-arguments' text,
// in the SCPD's order, as row 9 passed it.
type readRequest struct {
	snap   *engine.Snapshot
	device Device
	args   []string
}

// reads are the read actions, by service and action: every action of the
// four services that reads state and changes nothing. Every other is a write
// (writes.go's writes, TestTheReadsAndTheWritesAreEveryAction).
var reads = map[string]map[string]reader{
	"Product": {
		"Manufacturer": func(readRequest) ([][]byte, *faultReply) {
			return texts(manufacturer, "", manufacturerURL, ""), nil
		},
		"Model": func(readRequest) ([][]byte, *faultReply) {
			return texts(modelName, modelDescription, manufacturerURL, ""), nil
		},
		"Product": func(r readRequest) ([][]byte, *faultReply) {
			return texts(FriendlyName(r.device.Room), modelName, modelDescription, manufacturerURL, ""), nil
		},
		"Standby":              func(readRequest) ([][]byte, *faultReply) { return texts(boolean(false)), nil },
		"SourceCount":          func(readRequest) ([][]byte, *faultReply) { return texts("1"), nil },
		"SourceXml":            func(readRequest) ([][]byte, *faultReply) { return texts(sourceXML), nil },
		"SourceIndex":          func(readRequest) ([][]byte, *faultReply) { return texts("0"), nil },
		"Source":               productSource,
		"Attributes":           func(readRequest) ([][]byte, *faultReply) { return texts(attributes), nil },
		"SourceXmlChangeCount": func(readRequest) ([][]byte, *faultReply) { return texts("0"), nil },
	},
	"Playlist": {
		"Repeat":         func(r readRequest) ([][]byte, *faultReply) { return texts(boolean(r.snap.Repeat())), nil },
		"Shuffle":        func(r readRequest) ([][]byte, *faultReply) { return texts(boolean(r.snap.Shuffle())), nil },
		"TransportState": playlistTransportState,
		"Id":             func(r readRequest) ([][]byte, *faultReply) { return texts(ui4(int64(r.snap.Selected()))), nil },
		"Read":           playlistRead,
		"ReadList":       playlistReadList,
		"TracksMax":      func(readRequest) ([][]byte, *faultReply) { return texts(ui4(engine.MaxEntries)), nil },
		"IdArray":        playlistIDArray,
		"IdArrayChanged": playlistIDArrayChanged,
		"ProtocolInfo":   func(readRequest) ([][]byte, *faultReply) { return texts(protocolInfo), nil },
	},
	"Info": {
		"Counters": func(r readRequest) ([][]byte, *faultReply) {
			return texts(ui4(sat(r.snap.TrackCount())), ui4(sat(r.snap.DetailsCount())), "0"), nil
		},
		"Track":    infoTrack,
		"Details":  infoDetails,
		"Metatext": func(readRequest) ([][]byte, *faultReply) { return texts(""), nil },
	},
	"Time": {
		"Time": timeTime,
	},
}

// act answers an action that passed every row of the table, 501 while no
// engine runs, asked first: a read from one snapshot of the engine; a write
// by its writer (writes.go), within its budget, which runs from arrived.
func (h *handler) act(w http.ResponseWriter, r *http.Request, s *service, a *action, call *xmlNode, arrived time.Time, slot func()) {
	snap := h.snapshot()
	if snap == nil {
		writeFault(w, faultReply{code: errActionFailed}, false)
		return
	}
	args := make([]string, len(call.children))
	for i, c := range call.children {
		args[i] = string(c.text)
	}
	var values [][]byte
	var f *faultReply
	if read := reads[s.name][a.name]; read != nil {
		values, f = read(readRequest{snap: snap, device: h.device, args: args})
	} else if write := writes[s.name][a.name]; write != nil {
		ctx, cancel := context.WithDeadline(r.Context(), arrived.Add(h.lim.writeBudget()))
		values, f = write(writeRequest{ctx: ctx, h: h, r: r, action: a, args: args, slot: slot})
		cancel()
	} else {
		// An action with no answer is a defect, never a malformed reply.
		f = &faultReply{code: errActionFailed}
	}
	switch {
	case f != nil:
		writeFault(w, *f, false)
	case len(values) != len(a.outs()):
		// A reader that does not answer its action's arguments is a defect,
		// never a malformed reply.
		writeFault(w, faultReply{code: errActionFailed}, false)
	default:
		writeResponse(w, s, a, values)
	}
}

// snapshot is the engine's snapshot now, or nil while no engine runs: none
// was given, its Run has returned or a panic ended it (Done), or its
// shutdown has begun (Closed). Done is asked once the snapshot is taken, so
// an engine that ended before the ask is never read.
func (h *handler) snapshot() *engine.Snapshot {
	if h.engine == nil {
		return nil
	}
	snap := h.engine.Snapshot()
	select {
	case <-h.engine.Done():
		return nil
	default:
	}
	if snap == nil || snap.Closed() {
		return nil
	}
	return snap
}

// writeResponse answers a's out-arguments, values in the SCPD's order, in a
// SOAP response (UDA 1.1, §3.2.2): HTTP 200, text/xml; charset="utf-8", the
// envelope as a fault's, <u:actionResponse> in the service's namespace, and
// each value escaped once (escapeTo). Its length is counted first, so the
// reply carries a Content-Length and is escaped as it is written, with no
// second copy of its largest value.
func writeResponse(w http.ResponseWriter, s *service, a *action, values [][]byte) {
	head := `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<s:Envelope xmlns:s="` + soapEnvelopeNS + `" s:encodingStyle="` + soapEncodingNS + `"><s:Body>` +
		`<u:` + a.name + `Response xmlns:u="` + s.serviceType() + `">`
	tail := `</u:` + a.name + `Response></s:Body></s:Envelope>` + "\n"
	outs := a.outs()
	n := len(head) + len(tail)
	for i, g := range outs {
		n += 2*len(g.name) + len("<></>") + escapedLen(values[i])
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.Header().Set("Content-Length", strconv.Itoa(n))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, head)
	for i, g := range outs {
		_, _ = io.WriteString(w, "<"+g.name+">")
		escapeTo(w, values[i])
		_, _ = io.WriteString(w, "</"+g.name+">")
	}
	_, _ = io.WriteString(w, tail)
}

// byteCount counts what is written to it.
type byteCount int

func (c *byteCount) Write(p []byte) (int, error) {
	*c += byteCount(len(p))
	return len(p), nil
}

// escapedLen is b's length once escapeTo has escaped it.
func escapedLen(b []byte) int {
	var c byteCount
	escapeTo(&c, b)
	return int(c)
}

// texts is values as a reply's values.
func texts(values ...string) [][]byte {
	out := make([][]byte, len(values))
	for i, v := range values {
		out[i] = []byte(v)
	}
	return out
}

// boolean is UPnP's boolean as UDA 1.1 (§2.5) has it sent: 1 or 0.
func boolean(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// ui4 is v as a ui4, held to its range.
func ui4(v int64) string { return strconv.FormatInt(min(max(v, 0), math.MaxUint32), 10) }

// sat is a count held to int64's range.
func sat(v uint64) int64 { return int64(min(v, math.MaxInt64)) }

// ui4Of is an in-argument's text as the ui4 row 9 passed it.
func ui4Of(text string) uint32 {
	v, _ := strconv.ParseUint(text, 10, 32)
	return uint32(v)
}

// productSource is Product's Source(Index): the one source, at index 0;
// any other index is fault 801 (faultSourceNotFound).
func productSource(r readRequest) ([][]byte, *faultReply) {
	if ui4Of(r.args[0]) != 0 {
		return nil, &faultSourceNotFound
	}
	return texts(sourceName, sourceName, sourceName, boolean(true)), nil
}

// transportState is Playlist's TransportState for the engine's transport:
// Playing, or Buffering while nothing of a starting session is audible yet;
// Paused; Stopped; a release by what follows it (Stopped for a stop, Paused
// for a pause, Buffering for a play); maintenance Stopped.
func transportState(st engine.State, starting bool, pending engine.Intent) string {
	switch st {
	case engine.Playing:
		if starting {
			return "Buffering"
		}
		return "Playing"
	case engine.Paused:
		return "Paused"
	case engine.Releasing:
		switch pending {
		case engine.IntentPause:
			return "Paused"
		case engine.IntentPlay:
			return "Buffering"
		}
	}
	return "Stopped"
}

func playlistTransportState(r readRequest) ([][]byte, *faultReply) {
	return texts(transportState(r.snap.State(), r.snap.Starting(), r.snap.Pending())), nil
}

// playlistRead is Playlist's Read(Id): the entry's reference, the Uri
// exactly as the controller inserted it (an Insert keeps it as sent, PR 5a4),
// or a bare track id an entry written before keeps, and its metadata; an ID
// the queue does not hold is fault 800.
func playlistRead(r readRequest) ([][]byte, *faultReply) {
	e, ok := r.snap.Read(ui4Of(r.args[0]))
	if !ok {
		return nil, &faultIDNotFound
	}
	return [][]byte{[]byte(e.Ref.String()), e.Meta}, nil
}

// idList is ReadList's IdList: decimal ui4s, each by row 9's rule (no sign,
// at most 4294967295), separated by XML's white space (space, tab, carriage
// return, line feed), leading, trailing and repeated white space taken,
// none at all an empty list. A token that is not a ui4 is false.
func idList(text string) ([]uint32, bool) {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' })
	ids := make([]uint32, 0, len(fields))
	for _, f := range fields {
		if !valueOfType(f, typeUI4) {
			return nil, false
		}
		ids = append(ids, ui4Of(f))
	}
	return ids, true
}

// playlistReadList is Playlist's ReadList(IdList): a TrackList with an
// Entry (Id, Uri, Metadata) for each ID the queue holds, in the order
// asked, as often as asked, an ID it does not hold left out, its Uri and
// Metadata escaped once into the TrackList, as OpenHome's reference player
// answers (ohPipeline's ProviderPlaylist.cpp, lines 349-397: an ID it does
// not find skipped at 389, Converter::ToXmlEscaped at 382 and 385). A token
// that is not a ui4 is fault 402, where ohPipeline skips it (391): an IdList
// that is not one is no request a controller meant. More IDs than the engine
// reads at once (engine.MaxReadList, Snapshot.ReadList's bound) or a
// TrackList past maxTrackList, its closing tag counted, is fault 605. The
// entries are read one at a time, each held only while it is escaped, so the
// bound holds before the next copy is taken.
func playlistReadList(r readRequest) ([][]byte, *faultReply) {
	ids, ok := idList(r.args[0])
	if !ok {
		return nil, &faultReply{code: errInvalidArgs}
	}
	if len(ids) > engine.MaxReadList {
		return nil, &faultReply{code: errStringTooLong}
	}
	var b bytes.Buffer
	b.WriteString("<TrackList>")
	for _, id := range ids {
		e, ok := r.snap.Read(id)
		if !ok {
			continue
		}
		b.WriteString("<Entry><Id>" + strconv.FormatUint(uint64(e.ID), 10) + "</Id><Uri>")
		escapeTo(&b, []byte(e.Ref.String()))
		b.WriteString("</Uri><Metadata>")
		escapeTo(&b, e.Meta)
		b.WriteString("</Metadata></Entry>")
		if b.Len()+len(trackListEnd) > maxTrackList {
			return nil, &faultReply{code: errStringTooLong}
		}
	}
	b.WriteString(trackListEnd)
	return [][]byte{b.Bytes()}, nil
}

// idArray is IdArray's Array: the IDs as big-endian ui4s, in queue order,
// in base64 (bin.base64, padded); an empty queue is "".
func idArray(ids []uint32) string {
	b := make([]byte, 4*len(ids))
	for i, id := range ids {
		binary.BigEndian.PutUint32(b[4*i:], id)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// playlistIDArray is Playlist's IdArray: the token and the IDs, from the
// one snapshot.
func playlistIDArray(r readRequest) ([][]byte, *faultReply) {
	token, ids := r.snap.IDArray()
	return texts(ui4(int64(token)), idArray(ids)), nil
}

// playlistIDArrayChanged is Playlist's IdArrayChanged(Token): whether the
// token the controller holds is not the queue's now.
func playlistIDArrayChanged(r readRequest) ([][]byte, *faultReply) {
	return texts(boolean(ui4Of(r.args[0]) != r.snap.Token())), nil
}

// played is the occurrence heard and the whole seconds of its content
// played (floor), from one snapshot: ok false while nothing is heard, and
// seconds 0 until the backend measures a position of it.
type played struct {
	ok      bool
	track   engine.Track
	seconds int64
}

func heard(snap *engine.Snapshot) played {
	tr, ok := snap.Track()
	if !ok {
		return played{}
	}
	p := played{ok: true, track: tr}
	if pos := snap.Position(); pos.Occurrence == tr.Occurrence && pos.RateHz > 0 && pos.Frames > 0 {
		p.seconds = pos.Frames / int64(pos.RateHz)
	}
	return p
}

// duration is the track's length in whole seconds, rounded to the nearest
// (a half up), from the decoder's exact length, its total frames over its
// rate (engine.Details); 0 while it is not known. Never the controller's
// metadata, which is untrusted, nor the library index's, a tag's rounded
// value.
func (p played) duration() int64 {
	d := p.track.Details
	if !p.ok || !p.track.HasDetails || d.Frames <= 0 || d.RateHz <= 0 {
		return 0
	}
	rate := int64(d.RateHz)
	q, rem := d.Frames/rate, d.Frames%rate
	if rem >= rate-rem {
		q++
	}
	return q
}

// timeTime is Time's Time (timeOf).
func timeTime(r readRequest) ([][]byte, *faultReply) {
	tracks, duration, seconds := timeOf(r.snap.TrackCount(), heard(r.snap))
	return texts(ui4(tracks), ui4(duration), ui4(seconds)), nil
}

// timeOf is what Time reports: the tracks heard since the engine started,
// the length of the one heard and the seconds of it played, each held to
// timeMax.
func timeOf(tracks uint64, p played) (int64, int64, int64) {
	return min(sat(tracks), timeMax), min(p.duration(), timeMax), min(p.seconds, timeMax)
}

// infoTrack is Info's Track: the occurrence heard's reference and metadata,
// as it was handed off (engine.Track); empty while nothing is heard.
func infoTrack(r readRequest) ([][]byte, *faultReply) {
	p := heard(r.snap)
	if !p.ok {
		return texts("", ""), nil
	}
	return [][]byte{[]byte(p.track.Ref.String()), p.track.Meta}, nil
}

// infoDetails is Info's Details: Time's Duration, the PCM bit rate (rate ×
// bits × channels, bits per second), the bit depth, the sample rate,
// lossless, and the codec, from what the backend reported of the content
// heard (engine.Details); zero and empty while nothing is heard or nothing
// was reported.
func infoDetails(r readRequest) ([][]byte, *faultReply) {
	p := heard(r.snap)
	if !p.ok || !p.track.HasDetails {
		return texts("0", "0", "0", "0", boolean(false), ""), nil
	}
	d := p.track.Details
	rate, bits, channels := max(int64(d.RateHz), 0), max(int64(d.Bits), 0), max(int64(d.Channels), 0)
	bitRate := int64(math.MaxUint32)
	if rate <= math.MaxUint32 && bits <= 64 && channels <= 64 {
		bitRate = min(rate*bits*channels, math.MaxUint32)
	}
	return texts(ui4(min(p.duration(), timeMax)), ui4(bitRate), ui4(bits), ui4(rate), boolean(d.Lossless), d.Codec), nil
}
