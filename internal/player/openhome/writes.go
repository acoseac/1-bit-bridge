package openhome

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/trackref"
)

// The write actions (B470 PR 5a4). Each is one engine command, or, for the
// Product writes that change nothing and the seeks the engine cannot make,
// none, answered as OpenHome's reference player answers it (ohPipeline,
// read at master c1047a8dc801, 2026-10-21) where it has an answer, and UPnP's
// 501 Action Failed (UDA 1.1, §3.2.2) where it has none.
//
// A write waits for the engine's own answer, as the engine's callers do,
// within one budget: from the request's arrival at the handler, the
// listener's write deadline (Limits.Write) less budgetMargin, so the reply
// is written before net/http's deadline cuts it off (1-bit's SOAP calls time
// out at 15 s, so the bridge's bound is the one that binds). What a transport
// write answers when the budget ends first depends on whether the engine
// admitted the command: one it admitted answers success, as ohPipeline
// answers a command it has accepted (it may yet run, be overtaken by a Stop,
// or be refused by an output report or maintenance, which the reads show),
// and one it never admitted answers 501 (it did not run: engine.ErrNotAdmitted).
// An edit (Insert, DeleteId, DeleteAll) answers only with its reply: it calls
// the engine only while editBudget is left (an edit can wait for a
// reservation write up to engine.ReserveTimeout), and a cancelled one is 501.

const (
	// budgetMargin is the part of the write deadline a write leaves for its
	// reply: Limits.writeBudget is Limits.Write less it.
	budgetMargin = time.Second
	// editBudget is what must be left of a write's budget when an edit calls
	// the engine: the longest an edit can wait for a reservation write
	// (engine.ReserveTimeout), past which the engine refuses it, and a second
	// for that refusal to come back. With the default limits an edit is
	// called at most 3 s after its request arrived.
	editBudget = engine.ReserveTimeout + time.Second
)

// The UPnP codes the writes add to soap.go's (UDA 1.1, §3.2.2, the same in
// UDA 1.0): 600, for an argument whose value the action cannot take (an
// Insert's URI that names no track of this bridge's, or its metadata that is
// not XML 1.0 text).
const errArgumentValueInvalid = 600

// OpenHome's codes the writes answer, each its service's, as ohPipeline
// answers them: Playlist's in ProviderPlaylist.cpp, lines 23-32 (800 Id not
// found, faultIDNotFound in actions.go; 801 Playlist full, Insert's; 802 Index
// not found, SeekIndex's; 803 Seek failed, SeekSecondAbsolute's and
// SeekSecondRelative's, lines 245-281; 804 Shuffle not currently possible,
// SetShuffle's, line 227), and the one of OpenHome/Av/Utils/FaultCode.cpp for
// an action it has not built, 899 (Product's 801, Source not found, line 21,
// is faultSourceNotFound in actions.go, which SetSourceIndex and
// SetSourceIndexByName answer as ProviderProduct.cpp does at lines 244 and
// 256).
var (
	faultPlaylistFull   = faultReply{code: 801, description: "Playlist full"}
	faultIndexNotFound  = faultReply{code: 802, description: "Index not found"}
	faultSeekFailed     = faultReply{code: 803, description: "Seek failed"}
	faultShuffle        = faultReply{code: 804, description: "Shuffle not currently possible"}
	faultNotImplemented = faultReply{code: 899, description: "Wip - action not (yet) implemented"}
)

// Library is what an Insert asks of the track its URI names (Config.Library):
// whether the track id names a track of the bridge's own that the library
// serves now, by every check an open makes short of the file. nil is a
// member; an *engine.Unplayable is a refusal by its reason; any other error
// (library.ErrCancelled) is no answer.
type Library interface {
	Member(ctx context.Context, id string) error
}

// Hosts is what an Insert asks of the host and port its URI names
// (Config.Hosts, Inventory).
type Hosts interface {
	// Names reports whether u's host and port name this bridge now: one it
	// advertises or binds, on a port its tracks are served on.
	Names(u trackref.URI) bool
}

// writer answers one write action: its out-arguments' values, in the SCPD's
// order, or a fault.
type writer func(wr writeRequest) ([][]byte, *faultReply)

// writeRequest is what a write is answered from: the context of its budget
// (the request's, ended at the budget's deadline), the handler, the action
// and its in-arguments' text, in the SCPD's order, as row 9 passed it.
type writeRequest struct {
	ctx    context.Context
	h      *handler
	action *action
	args   []string
}

// writes are the write actions, by service and action: every action of the
// four services that is not a read (TestTheReadsAndTheWritesAreEveryAction).
var writes = map[string]map[string]writer{
	"Product": {
		"SetStandby":           productSetStandby,
		"SetSourceIndex":       productSetSourceIndex,
		"SetSourceIndexByName": productSetSourceIndexByName,
	},
	"Playlist": {
		"Play":     transport(func(ctx context.Context, e Engine, _ []string) error { return e.Play(ctx) }),
		"Pause":    transport(func(ctx context.Context, e Engine, _ []string) error { return e.Pause(ctx) }),
		"Stop":     transport(func(ctx context.Context, e Engine, _ []string) error { return e.Stop(ctx) }),
		"Next":     transport(func(ctx context.Context, e Engine, _ []string) error { return e.Next(ctx) }),
		"Previous": transport(func(ctx context.Context, e Engine, _ []string) error { return e.Previous(ctx) }),
		"SeekId": transport(func(ctx context.Context, e Engine, args []string) error {
			return e.PlayID(ctx, ui4Of(args[0]))
		}),
		"SeekIndex": transport(func(ctx context.Context, e Engine, args []string) error {
			return e.PlayIndex(ctx, int(ui4Of(args[0])))
		}),
		"SetRepeat": transport(func(ctx context.Context, e Engine, args []string) error {
			return e.SetRepeat(ctx, booleanOf(args[0]))
		}),
		"SetShuffle":         playlistSetShuffle,
		"SeekSecondAbsolute": playlistSeekSecond,
		"SeekSecondRelative": playlistSeekSecond,
		"Insert":             playlistInsert,
		"DeleteId": edit(func(ctx context.Context, e Engine, args []string) error {
			return e.Delete(ctx, ui4Of(args[0]))
		}),
		"DeleteAll": edit(func(ctx context.Context, e Engine, _ []string) error { return e.DeleteAll(ctx) }),
	},
}

// booleanOf is a boolean in-argument's text as row 9 passed it: 1, true or
// yes, in any case, is true.
func booleanOf(text string) bool {
	switch strings.ToLower(text) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// transport is a transport write: the engine's command with the budget's
// context, its answer mapped by transportFault.
func transport(do func(ctx context.Context, e Engine, args []string) error) writer {
	return func(wr writeRequest) ([][]byte, *faultReply) {
		return nil, transportFault(wr.action, do(wr.ctx, wr.h.engine, wr.args))
	}
}

// transportFault is what a transport write answers for the engine's answer
// err: success for nil; success for ReasonSuperseded, a later command having
// replaced the playback the command asked for, whatever else it did standing
// (another controller's command, or 1-bit's own Stop, replacing a start under
// way must not fault the first); success for a cancellation of a command the
// engine admitted (its budget ended while it waited: it may yet run, which
// the reads show); 501 for one it never admitted (engine.ErrNotAdmitted: it
// did not run); and every other refusal by the reason table (faultOf).
func transportFault(a *action, err error) *faultReply {
	switch reason := engine.ReasonOf(err); {
	case err == nil, reason == engine.ReasonSuperseded:
		return nil
	case reason == engine.ReasonCancelled && !errors.Is(err, engine.ErrNotAdmitted):
		return nil
	default:
		f := faultOf(a, reason)
		return &f
	}
}

// edit is an edit that answers nothing but its reply: called only with
// editBudget left of the write's budget (otherwise 501, nothing done), its
// refusals by the reason table, a cancellation 501.
func edit(do func(ctx context.Context, e Engine, args []string) error) writer {
	return func(wr writeRequest) ([][]byte, *faultReply) {
		if f := editGate(wr.ctx); f != nil {
			return nil, f
		}
		if err := do(wr.ctx, wr.h.engine, wr.args); err != nil {
			f := faultOf(wr.action, engine.ReasonOf(err))
			return nil, &f
		}
		return nil, nil
	}
}

// editGate is 501 when less than editBudget is left of the write's budget:
// the engine could hold the edit past it for a reservation write, and answer
// it after the reply was written, so it is not called.
func editGate(ctx context.Context) *faultReply {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < editBudget {
		return &faultReply{code: errActionFailed}
	}
	return nil
}

// reasonFaults is the fault an engine's refusal of a write answers, by its
// reason (errors.go's every reason: TestEveryEngineReasonHasItsFault): the
// OpenHome codes where ohPipeline has the condition, and UPnP's 501 Action
// Failed where it has none, the state of the player preventing the action.
// A reason outside the table, the backend's and the resolver's own among
// them, is 501 too (faultOf). Insert's 801 is ReasonFull alone: 1-bit takes
// an 801 after its first insert for a player that is full, and plays what is
// in, so a reservation not durable, a file not moved aside, IDs exhausted and
// metadata too large must not answer it.
var reasonFaults = map[engine.Reason]faultReply{
	engine.ReasonIDNotFound:       faultIDNotFound,
	engine.ReasonFull:             faultPlaylistFull,
	engine.ReasonIndexNotFound:    faultIndexNotFound,
	engine.ReasonMetadataTooLarge: {code: errStringTooLong},
	engine.ReasonNotInRelease:     faultShuffle,
	// The engine closing between the no-engine check and the command: 5a3's
	// no-engine fault.
	engine.ReasonClosed: {code: errActionFailed},
	// No counterpart in ohPipeline: the player's state prevents the action.
	engine.ReasonCancelled:          {code: errActionFailed},
	engine.ReasonConflict:           {code: errActionFailed},
	engine.ReasonMaintenance:        {code: errActionFailed},
	engine.ReasonLease:              {code: errActionFailed},
	engine.ReasonBadReference:       {code: errActionFailed},
	engine.ReasonTooMany:            {code: errActionFailed},
	engine.ReasonEmptyQueue:         {code: errActionFailed},
	engine.ReasonIDsExhausted:       {code: errActionFailed},
	engine.ReasonReservation:        {code: errActionFailed},
	engine.ReasonQueueIdentity:      {code: errActionFailed},
	engine.ReasonFileStuck:          {code: errActionFailed},
	engine.ReasonOutputUnknown:      {code: errActionFailed},
	engine.ReasonOutputUnavailable:  {code: errActionFailed},
	engine.ReasonReleaseUnconfirmed: {code: errActionFailed},
	engine.ReasonBackend:            {code: errActionFailed},
	engine.ReasonTimeout:            {code: errActionFailed},
	engine.ReasonSuperseded:         {code: errActionFailed},
	engine.ReasonOvertaken:          {code: errActionFailed},
	engine.ReasonStopped:            {code: errActionFailed},
	engine.ReasonEndOfQueue:         {code: errActionFailed},
	engine.ReasonUnplayable:         {code: errActionFailed},
	engine.ReasonAllUnplayable:      {code: errActionFailed},
	engine.ReasonFault:              {code: errActionFailed},
	engine.ReasonUnitChanged:        {code: errActionFailed},
	engine.ReasonRemovedAudible:     {code: errActionFailed},
	engine.ReasonMissing:            {code: errActionFailed},
	engine.ReasonRefusedContent:     {code: errActionFailed},
	engine.ReasonDSDOff:             {code: errActionFailed},
	engine.ReasonXrun:               {code: errActionFailed},
	engine.ReasonDisconnected:       {code: errActionFailed},
	engine.ReasonDeviceError:        {code: errActionFailed},
	engine.ReasonDecodeFailed:       {code: errActionFailed},
}

// faultOf is the fault a's engine refusal of reason answers: the table's, an
// OpenHome code only on the action that has it (801 on Insert, 802 on
// SeekIndex, 804 on SetShuffle; 800 on every action that names an ID), and
// 501 for every other reason, one outside the table included.
func faultOf(a *action, reason engine.Reason) faultReply {
	f, ok := reasonFaults[reason]
	if !ok {
		return faultReply{code: errActionFailed}
	}
	switch {
	case f == faultPlaylistFull && a.name != "Insert",
		f == faultIndexNotFound && a.name != "SeekIndex",
		f == faultShuffle && a.name != "SetShuffle":
		return faultReply{code: errActionFailed}
	}
	return f
}

// productSetStandby is Product's SetStandby(Value): the player has no
// standby (Standby reads false), so false changes nothing and succeeds, and
// true is 899, an action ohPipeline answers for what it has not built
// (FaultCode.cpp): no Stop is made of it.
func productSetStandby(wr writeRequest) ([][]byte, *faultReply) {
	if booleanOf(wr.args[0]) {
		return nil, &faultNotImplemented
	}
	return nil, nil
}

// productSetSourceIndex is Product's SetSourceIndex(Value): the one source
// is index 0, which is selected already; any other index is 801, Source not
// found, as ProviderProduct.cpp answers it (line 244).
func productSetSourceIndex(wr writeRequest) ([][]byte, *faultReply) {
	if ui4Of(wr.args[0]) != 0 {
		return nil, &faultSourceNotFound
	}
	return nil, nil
}

// productSetSourceIndexByName is Product's SetSourceIndexByName(Value): the
// one source is Playlist, by its name exactly; any other is 801 (line 256).
func productSetSourceIndexByName(wr writeRequest) ([][]byte, *faultReply) {
	if wr.args[0] != sourceName {
		return nil, &faultSourceNotFound
	}
	return nil, nil
}

// playlistSetShuffle is Playlist's SetShuffle(Value): false is the engine's
// SetShuffle(false), a transport write; true is 804, Shuffle not currently
// possible (shuffle is release 7a's), answered without the engine.
func playlistSetShuffle(wr writeRequest) ([][]byte, *faultReply) {
	if booleanOf(wr.args[0]) {
		return nil, &faultShuffle
	}
	return nil, transportFault(wr.action, wr.h.engine.SetShuffle(wr.ctx, false))
}

// playlistSeekSecond is Playlist's SeekSecondAbsolute and SeekSecondRelative:
// the engine has no seek within a track (release 7a's), so every value is
// 803, Seek failed, as ohPipeline answers a seek it cannot make, without the
// engine. 1-bit logs it, and the track plays from its start.
func playlistSeekSecond(writeRequest) ([][]byte, *faultReply) { return nil, &faultSeekFailed }

// playlistInsert is Playlist's Insert(AfterId, Uri, Metadata): the URI
// confined to this bridge's own tracks (confine), the metadata checked
// (metadataFault), then the edit gate, then the engine's InsertAfter, with
// the URI's canonical form as the entry's reference (trackref.URI.String: its
// name lower-cased, never the spelling sent; B470 PR 5a4, round 2), so Read
// returns that form; NewId its answer.
func playlistInsert(wr writeRequest) ([][]byte, *faultReply) {
	meta := []byte(wr.args[2])
	u, f := wr.h.confine(wr.ctx, wr.args[1])
	if f != nil {
		return nil, f
	}
	if f := metadataFault(meta); f != nil {
		return nil, f
	}
	if f := editGate(wr.ctx); f != nil {
		return nil, f
	}
	ref, err := engine.NewReference(u.String())
	if err != nil {
		return nil, &faultReply{code: errActionFailed}
	}
	id, err := wr.h.engine.InsertAfter(wr.ctx, ui4Of(wr.args[0]), ref, meta)
	switch {
	case err != nil:
		f := faultOf(wr.action, engine.ReasonOf(err))
		return nil, &f
	case id == 0:
		// An insert is never answered without an ID above 0.
		return nil, &faultReply{code: errActionFailed}
	}
	return texts(strconv.FormatUint(uint64(id), 10)), nil
}

// confine is an Insert's URI confinement: the URI is the canonical URI of one
// of this bridge's own tracks (trackref.Parse: past trackref.MaxLength bytes
// 605, any other shape 600, a host with a byte that is not ASCII among them),
// naming a host the bridge advertises or binds and a port its tracks are
// served on (Hosts: 600 otherwise), never a host that cannot name this
// bridge to a controller on its LAN (lanHostRefused: 600), and a track id the
// library serves now (Library: a track that is not the library's 600; no
// answer, or a library that failed, 501). It returns the URI parsed, whose
// canonical form the Insert stores.
func (h *handler) confine(ctx context.Context, uri string) (trackref.URI, *faultReply) {
	u, err := trackref.Parse(uri)
	switch {
	case errors.Is(err, trackref.ErrTooLong):
		return trackref.URI{}, &faultReply{code: errStringTooLong}
	case err != nil:
		return trackref.URI{}, &faultReply{code: errArgumentValueInvalid}
	case lanHostRefused(u), h.hosts == nil || !h.hosts.Names(u):
		return trackref.URI{}, &faultReply{code: errArgumentValueInvalid}
	case h.library == nil:
		return trackref.URI{}, &faultReply{code: errActionFailed}
	}
	var refused *engine.Unplayable
	switch err := h.library.Member(ctx, u.ID); {
	case err == nil:
		return u, nil
	case errors.As(err, &refused) && notALibraryTrack(refused.Reason):
		return trackref.URI{}, &faultReply{code: errArgumentValueInvalid}
	default:
		return trackref.URI{}, &faultReply{code: errActionFailed}
	}
}

// notALibraryTrack reports whether the library's refusal of a track id says
// the URI names no track the bridge plays (an argument the action cannot
// take, 600): no row has it, an upstream server routes it, it is an SACD
// image's virtual track, or no root configured now holds it. The library's
// other refusals (two rows with the id, an index it cannot read, a panic)
// are the bridge's state, 501. The reasons are internal/player/library's,
// named here as they are written there.
func notALibraryTrack(reason engine.Reason) bool {
	switch reason {
	case "not_indexed", "routed", "sacd_virtual_track", "outside_library":
		return true
	}
	return false
}

// tailnetV4 and tailnetV6 are Tailscale's address ranges: CGNAT's 100.64/10
// and Tailscale's ULA, fd7a:115c:a1e0::/48.
var (
	tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// lanHostRefused reports whether u's host is one that cannot name this bridge
// to a controller on its LAN, whatever the bridge advertises or binds: a
// loopback, unspecified or multicast address, or localhost, which name the
// controller's own host; a link-local one, refused outright in this release
// (its zone names an interface of the host that reads it, which no
// controller can know; a custom endpoint can carry one); and a tailnet's
// address or name, which neither 1-bit derives (its BridgeDLNAURLResolver
// takes an RFC 1918 address, else a host the DLNA listener can serve) nor the
// DLNA listener serves (backlog B48).
func lanHostRefused(u trackref.URI) bool {
	if a := u.Addr; a.IsValid() {
		return a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() ||
			a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || tailnetV4.Contains(a) || tailnetV6.Contains(a)
	}
	n := u.Name
	return n == "localhost" || strings.HasSuffix(n, ".localhost") || n == "ts.net" || strings.HasSuffix(n, ".ts.net")
}

// metadataFault is an Insert's metadata check: at most the engine's 64 KiB
// (605 past it), and XML 1.0 text, every character in its Char production,
// so no reply ever needs the escaper's replacement for it (600 otherwise).
// It is never parsed, nor checked for well-formedness: a sloppy DIDL-Lite is
// stored as it was sent, and empty metadata is taken. The text reaching it is
// the decoded one (row 7: a character XML forbids is 400 there, literal or
// referenced).
func metadataFault(meta []byte) *faultReply {
	if len(meta) > engine.MaxMetadataBytes {
		return &faultReply{code: errStringTooLong}
	}
	if !xmlText(meta) {
		return &faultReply{code: errArgumentValueInvalid}
	}
	return nil
}

// xmlText reports whether b is UTF-8 whose every character is in XML 1.0's
// Char production: tab, line feed, carriage return, U+0020 to U+D7FF, U+E000
// to U+FFFD and U+10000 to U+10FFFF.
func xmlText(b []byte) bool {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && size <= 1:
			return false
		case r == '\t' || r == '\n' || r == '\r',
			r >= 0x20 && r <= 0xD7FF,
			r >= 0xE000 && r <= 0xFFFD,
			r >= 0x10000 && r <= 0x10FFFF:
		default:
			return false
		}
		b = b[size:]
	}
	return true
}
