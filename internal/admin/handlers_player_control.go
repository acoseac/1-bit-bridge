// The console's control of the bridge's own player (B634, PR 1): the routes
// under /api/output/player/ through which the player bar drives the engine
// serve's player runs, and reads it. cmd/bridge implements PlayerControl over
// serve's player, its boot plan, the library index and the player's resolver,
// so this package imports no player package.
//
// Every command is an operation of the control's, named by its tab, its count
// in that tab and the epoch it was sent in (PlayerOp), run one at a time on
// the server; its record is published, so a lost answer is resolved by
// reading the record, never by sending the command again. The publisher
// cmd/bridge runs orders every state the bar takes by one revision; this
// package only serves its publications, encoded once.
//
// Control is not config: no control route reads the player block's Unread or
// the managed settings, takes the console's lock or bridge.yaml's, or writes
// the config. Each refuses, before anything is read, where the player is not
// offered (controlRefusal): no control wired, a public bridge, a host that is
// not Linux, a plan that does not list the player.
package admin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
	"github.com/acoseac/1-bit-bridge/internal/librarycat"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// PlayerControl is the bridge's own player as the console's player bar drives
// it (B634). cmd/bridge implements it over serve's player. A command's method
// answers once its operation has ended, or once its request's context has
// ended or the control's own wait has passed, with the record as it stands
// then; neither ends the operation's step in flight.
type PlayerControl interface {
	// Platform is the host the player runs on: "linux" is the one that runs
	// it.
	Platform() string
	// Lists reports whether serve's boot plan lists the player (it is
	// enabled on the boot snapshot with serve's data directory's lock held),
	// started or not: a plan that does not is a bridge that offers no player.
	Lists() bool
	// Now is a publication captured now: a new revision where the state
	// differs from the latest publication's, and that publication otherwise,
	// at once.
	Now(ctx context.Context) PlayerPublication
	// Latest is the latest publication, false before the first.
	Latest() (PlayerPublication, bool)
	// Wake is closed at the next publication.
	Wake() <-chan struct{}
	// Queue is the queue of a publication captured now, each entry named by
	// the library path its reference names, or its word.
	Queue(ctx context.Context) PlayerQueue
	// Plays is what the running player plays, for a row's bridge field:
	// false while no player runs.
	Plays() (PlayerPlays, bool)
	Replace(ctx context.Context, r PlayerReplace) PlayerAnswer
	Append(ctx context.Context, r PlayerAppend) PlayerAnswer
	Delete(ctx context.Context, r PlayerDelete) PlayerAnswer
	Transport(ctx context.Context, r PlayerCommand) PlayerAnswer
	SwitchOut(ctx context.Context, r PlayerSwitchOut) PlayerAnswer
	// Operation is a tab's last record read back, in the epoch the tab
	// names: its handover's tracks named by their paths, and the latest
	// publication's revision beside it.
	Operation(epoch, tab string, seq uint64) PlayerReadBack
}

// PlayerOp is an operation's identity: the tab that sent it (32 lower-case
// hexadecimal digits), its count in that tab (from 1) and the epoch the tab
// had taken when it sent it.
type PlayerOp struct {
	Tab   string `json:"tab"`
	Seq   uint64 `json:"seq"`
	Epoch string `json:"epoch"`
}

// PlayerReplace is a replace of the engine's queue: its tracks by their
// library paths, in order (an album or a playlist the route expanded), the
// start track's index, its position in whole seconds (0 for none), and
// whether to play it. NotFound is an album or a playlist the request named
// that the catalog does not hold: the operation is refused, not_found.
type PlayerReplace struct {
	Op       PlayerOp
	Digest   [32]byte
	At       string
	Tracks   []string
	Start    int
	Position int
	Play     bool
	NotFound bool
}

// PlayerAppend is an append of tracks at the end of the engine's queue.
type PlayerAppend struct {
	Op       PlayerOp
	Digest   [32]byte
	At       string
	Tracks   []string
	NotFound bool
}

// PlayerDelete is a delete of one entry.
type PlayerDelete struct {
	Op     PlayerOp
	Digest [32]byte
	At     string
	Entry  uint32
}

// PlayerCommand is one transport command: play, pause, stop, next, previous,
// index (Index), seek (Seconds) or repeat (On). Pause and stop carry no
// generation.
type PlayerCommand struct {
	Op      PlayerOp
	Digest  [32]byte
	At      string
	Command string
	Index   int
	Seconds int64
	On      bool
}

// PlayerSwitchOut is the barrier every start of the browser goes through:
// with Take, the switch to this browser, whose answer carries the bridge's
// queue for the browser to take.
type PlayerSwitchOut struct {
	Op     PlayerOp
	Digest [32]byte
	Take   bool
}

// The commands PlayerCommand takes. There is no shuffle: the engine plays its
// queue in order.
const (
	PlayerCmdPlay     = "play"
	PlayerCmdPause    = "pause"
	PlayerCmdStop     = "stop"
	PlayerCmdNext     = "next"
	PlayerCmdPrevious = "previous"
	PlayerCmdIndex    = "index"
	PlayerCmdSeek     = "seek"
	PlayerCmdRepeat   = "repeat"
)

// PlayerCommands is every command the transport route takes.
func PlayerCommands() []string {
	return []string{PlayerCmdPlay, PlayerCmdPause, PlayerCmdStop, PlayerCmdNext, PlayerCmdPrevious, PlayerCmdIndex, PlayerCmdSeek, PlayerCmdRepeat}
}

// PlayerPublication is one publication of the player's state: its epoch, its
// revision, the revision at which its queue last changed, and the state's
// bytes, encoded once (a PlayerState).
type PlayerPublication struct {
	Epoch    string
	Revision uint64
	QueueRev uint64
	Bytes    []byte
}

// PlayerAnswer is what a command's method answers: a refusal of a request
// that never became an operation (Refused, its code, Status and Message,
// no record), or the operation's record; and the publication captured after
// the operation's last answer (or the latest, for a refusal). HandoverPaths
// is the library path of each handover track id, "" for one no track has.
type PlayerAnswer struct {
	Refused       string
	Status        int
	Message       string
	Reason        string
	Record        *PlayerRecord
	HandoverPaths []string
	Player        PlayerPublication
}

// PlayerReadBack is Operation's answer: the epoch the control runs, the
// latest publication's revision, and the record, or why not (Refused:
// epoch_changed, operation_gone).
type PlayerReadBack struct {
	Refused       string
	Epoch         string
	Revision      uint64
	Record        *PlayerRecord
	HandoverPaths []string
}

// The record statuses.
const (
	PlayerOpRunning   = "running"
	PlayerOpDone      = "done"
	PlayerOpChanged   = "changed"
	PlayerOpRefused   = "refused"
	PlayerOpCancelled = "cancelled"
	PlayerOpUnknown   = "unknown"
)

// The effects a record says its operation had.
const (
	PlayerEffectNone    = "none"
	PlayerEffectSome    = "some"
	PlayerEffectUnknown = "unknown"
)

// PlayerRecord is an operation's record. Error is the word of a final status
// other than done: changed, cancelled, outcome_unknown, or a refusal's word
// (stale, check, not_playable, …); Reason is player_not_running's. Effect
// is none only where the engine's answer proved no step took effect (a
// refusal before any change), some once a step the engine answered nil has
// run, and unknown otherwise. Sent counts the steps sent of the Of planned,
// and Last is the last step's answer, ok or the engine's word. Release is
// none, confirmed or unconfirmed, for an operation that waited on a release.
// Silence is the revision of the publication that showed the bridge silent
// (a switch-out's), and Handover what a switch-out to this browser moved.
type PlayerRecord struct {
	Tab      string          `json:"tab"`
	Seq      uint64          `json:"seq"`
	Kind     string          `json:"kind"`
	Command  string          `json:"command,omitempty"`
	Status   string          `json:"status"`
	Error    string          `json:"error,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Effect   string          `json:"effect"`
	Sent     int             `json:"sent"`
	Of       int             `json:"of"`
	Last     string          `json:"last"`
	Release  string          `json:"release"`
	Silence  uint64          `json:"silence,omitempty"`
	Seek     string          `json:"seek,omitempty"`
	LeftOut  []PlayerLeftOut `json:"leftOut,omitempty"`
	Window   *PlayerWindow   `json:"window,omitempty"`
	Room     *PlayerRoom     `json:"room,omitempty"`
	Handover *PlayerHandover `json:"handover,omitempty"`
	Entries  []uint32        `json:"entries,omitempty"`
}

// PlayerLeftOut is a track an operation's checks left out, and why.
type PlayerLeftOut struct {
	Path string `json:"path"`
	Word string `json:"word"`
}

// PlayerWindow is the part of a list a replace queued: tracks First to Last
// (from 1) of Total, where the list was longer than the queue takes.
type PlayerWindow struct {
	First int `json:"first"`
	Last  int `json:"last"`
	Total int `json:"total"`
}

// PlayerRoom is what an append that did not fit found room for.
type PlayerRoom struct {
	Entries  int `json:"entries"`
	Metadata int `json:"metadata"`
	Wanted   int `json:"wanted"`
}

// PlayerHandover is what a switch-out moved: the bridge's queue as track ids
// in order, the selected one's index (-1 for none), its position in seconds
// for PCM (0 otherwise), and whether the bridge was playing.
type PlayerHandover struct {
	Tracks   []string `json:"tracks"`
	Selected int      `json:"selected"`
	Position float64  `json:"position"`
	Playing  bool     `json:"playing"`
}

// PlayerState is a publication's state, the bytes a player frame, GET now
// and every answer carry: its order (Epoch, Revision) and the server's clock
// at its capture (At, RFC 3339); whether the player runs (Running, or Why:
// off, starting, start_failed, closed); whether it can be an output (Offered,
// or OfferedWhy: why it does not run, output_unknown, the output's word, or
// the live config's, disabled or binding_changed); the transport (stopped,
// playing, paused, releasing, maintenance; Starting while nothing of a
// session is heard; Pending, what follows a release); the hold (none, check,
// maintenance, configuring); the release (Release, confirmed or unconfirmed,
// and since when); the commands the engine takes now; Silent, the barrier's
// predicate on this capture; Tone, the test tone in the device slot; the
// generation, the queue's token and length, QueueRev (the revision at which
// the queue last changed), the selection and repeat; the occurrence heard,
// named by its library path; the anchor of its position; what the DAC
// receives and whether a seek is offered; the last stop and the last play
// attempt; and the last eight operations' records, without their handovers.
type PlayerState struct {
	Epoch       string             `json:"epoch"`
	Revision    uint64             `json:"revision"`
	At          string             `json:"at"`
	Running     bool               `json:"running"`
	Why         string             `json:"why"`
	Offered     bool               `json:"offered"`
	OfferedWhy  string             `json:"offeredWhy"`
	Transport   string             `json:"transport"`
	Starting    bool               `json:"starting"`
	Pending     string             `json:"pending"`
	Hold        string             `json:"hold"`
	Release     PlayerReleaseState `json:"release"`
	Commands    []string           `json:"commands"`
	Silent      bool               `json:"silent"`
	Tone        bool               `json:"tone"`
	Generation  string             `json:"generation"`
	Token       uint32             `json:"token"`
	Length      int                `json:"length"`
	QueueRev    uint64             `json:"queueRev"`
	Selected    uint32             `json:"selected"`
	Repeat      bool               `json:"repeat"`
	Track       *PlayerStateTrack  `json:"track"`
	Anchor      *PlayerAnchor      `json:"anchor"`
	Signal      *PlayerSignal      `json:"signal"`
	Seek        bool               `json:"seek"`
	LastStop    PlayerLastStop     `json:"lastStop"`
	LastAttempt PlayerLastAttempt  `json:"lastAttempt"`
	Operations  []PlayerRecord     `json:"operations"`
}

// PlayerReleaseState is the release: confirmed, or unconfirmed since when
// (RFC 3339).
type PlayerReleaseState struct {
	State string `json:"state"`
	Since string `json:"since,omitempty"`
}

// PlayerStateTrack is the occurrence heard: its entry, the library path its
// reference names and the row's names, never a controller's metadata; and
// what the decoder reported.
type PlayerStateTrack struct {
	Entry    uint32  `json:"entry"`
	Path     string  `json:"path"`
	Named    bool    `json:"named"`
	Title    string  `json:"title"`
	Artist   string  `json:"artist"`
	Album    string  `json:"album"`
	Codec    string  `json:"codec"`
	Bits     int     `json:"bits"`
	RateHz   int     `json:"rateHz"`
	Channels int     `json:"channels"`
	Duration float64 `json:"duration"`
}

// PlayerAnchor is the position the bar extrapolates from: its own id (the
// revision of the first publication that carried it), the occurrence and its
// entry, the seconds played as the backend measured them and when (At, RFC
// 3339 with nanoseconds), and whether it advances (playing and heard).
type PlayerAnchor struct {
	ID         uint64  `json:"id"`
	Occurrence uint64  `json:"occurrence"`
	Entry      uint32  `json:"entry"`
	Seconds    float64 `json:"seconds"`
	At         string  `json:"at"`
	Advancing  bool    `json:"advancing"`
}

// PlayerLastStop is why playback last stopped, and at which entry.
type PlayerLastStop struct {
	Reason string `json:"reason"`
	Entry  uint32 `json:"entry"`
}

// PlayerLastAttempt is the last play attempt: its count, and whether it
// failed, and for a device another process held (busy) or a node this
// process may not open (denied).
type PlayerLastAttempt struct {
	Seq    uint64 `json:"seq"`
	Failed bool   `json:"failed"`
	Busy   bool   `json:"busy"`
	Denied bool   `json:"denied"`
}

// PlayerQueue is the queue of one publication: its epoch, the revision it
// was read at, the revision at which it last changed, its token and
// selection, and its entries in order.
type PlayerQueue struct {
	Epoch    string
	ReadAt   uint64
	QueueRev uint64
	Token    uint32
	Selected uint32
	Entries  []PlayerQueueEntry
}

// PlayerQueueEntry is one entry: its ID, the library path its reference
// names, and its word where none is (a track gone, a reference of another
// form), or the word its skip gave.
type PlayerQueueEntry struct {
	ID   uint32
	Path string
	Word string
}

// PlayerPlays is what the running player plays: its containers, and each DSD
// rate's way (by the 1-bit rate in Hz) in the set its backend published.
type PlayerPlays struct {
	Codecs []string
	DSD    map[int]PlayerBridgeWay
}

// PlayerBridgeWay is how the bridge's player plays a track: pcm, native, dop,
// converted (to PCM, never bit-exact), or none with its word.
type PlayerBridgeWay struct {
	Way    string `json:"way"`
	Reason string `json:"reason,omitempty"`
}

// The ways.
const (
	BridgeWayPCM       = "pcm"
	BridgeWayNative    = "native"
	BridgeWayDoP       = "dop"
	BridgeWayConverted = "converted"
	BridgeWayNone      = "none"
)

// The words of a track the bridge's player does not play, beside the DSD
// set's own.
const (
	BridgeWordRouted       = "routed"
	BridgeWordSACDVirtual  = "sacd_virtual_track"
	BridgeWordGated        = "unknown_container"
	BridgeWordLossy        = "lossy_refused"
	BridgeWordNoContainer  = "not_a_container"
	BridgeWordDSDNotPlayed = "dsd_rate_not_played"
)

// BridgeWayOf is how the bridge's player plays a track, from its row (its
// library path, codec, routed flag, DSD flag and rate) and what the player
// plays: the one function a catalog row's bridge field and a replace's checks
// both run, so a row and an edit disagree on nothing but the library's
// membership, which the edit alone asks.
func BridgeWayOf(p, codec string, routed, isDSD bool, rateHz int, plays PlayerPlays) PlayerBridgeWay {
	none := func(w string) PlayerBridgeWay { return PlayerBridgeWay{Way: BridgeWayNone, Reason: w} }
	ext := strings.ToLower(path.Ext(p))
	switch {
	case routed:
		return none(BridgeWordRouted)
	case manifest.IsSACDVirtualPath(p):
		return none(BridgeWordSACDVirtual)
	case formatgate.Gated(p):
		return none(BridgeWordGated)
	case manifest.IsLossyCodec(codec):
		return none(BridgeWordLossy)
	case isDSD || ext == ".dsf" || ext == ".dff":
		if w, ok := plays.DSD[rateHz]; ok {
			return w
		}
		return none(BridgeWordDSDNotPlayed)
	}
	container := ""
	switch ext {
	case ".flac":
		container = "flac"
	case ".wav", ".wave":
		container = "wav"
	case ".aif", ".aiff", ".aifc":
		container = "aiff"
	case ".m4a", ".mp4":
		if strings.EqualFold(strings.TrimSpace(codec), "ALAC") {
			container = "alac"
		} else {
			return none(BridgeWordLossy)
		}
	}
	for _, c := range plays.Codecs {
		if c == container && container != "" {
			return PlayerBridgeWay{Way: BridgeWayPCM}
		}
	}
	return none(BridgeWordNoContainer)
}

// The words of the control's refusals before anything is read: no control
// wired, a public bridge, a host that is not Linux, a plan that does not list
// the player. Each says that no player is offered here, so the bar plays in
// the browser as it does on a bridge without one.
const (
	playerNotOffered = "player_not_offered"
)

// controlRefusal is why a control route refuses, nil when it does not: asked
// before anything is read. It asks neither the player block's Unread nor the
// managed settings, which guard a write of output.player that no control
// route makes: an unreadable block stops no Stop and no Pause of a player that
// runs.
func (s *Server) controlRefusal() *cfgAbort {
	c := s.deps.PlayerControl
	switch {
	case c == nil:
		return &cfgAbort{status: http.StatusServiceUnavailable, code: playerNotWired,
			msg: "this console has no control of a player wired: the bridge's own player runs inside bridge serve"}
	case s.deps.CfgHolder != nil && s.deps.CfgHolder.Load() != nil && s.deps.CfgHolder.Load().IsPublic():
		return &cfgAbort{status: http.StatusForbidden, code: playerNotAvailable,
			msg: "a public bridge has no player: it plays to a DAC on the box it runs on, for the LAN it is on"}
	}
	if goos := c.Platform(); goos != "linux" {
		return &cfgAbort{status: http.StatusConflict, code: playerNotSupported,
			msg: "the bridge's own player runs on Linux, and this bridge runs on " + goos}
	}
	if !c.Lists() {
		return &cfgAbort{status: http.StatusConflict, code: playerNotOffered,
			msg: "this bridge offers no player of its own: the player was not enabled when the bridge started"}
	}
	return nil
}

// playerControlOffered reports whether the stream sends player frames: a
// control wired on a bridge that offers its player.
func (s *Server) playerControlOffered() bool { return s.controlRefusal() == nil }

// The control routes' body bounds: a body of at most 8 MiB (the console's
// other routes take 1 MiB, which a long list of paths passes), at most
// maxControlTracks paths, each at most maxControlPathBytes and not empty.
// A long playlist is sent by its ID (playlist), as an album is, and the
// server expands it; the browser's own queue is sent as its paths.
const (
	controlMaxBodyBytes = 8 << 20
	maxControlTracks    = 20000
	maxControlPathBytes = 4096
)

// The words of a body the control routes refuse, each with its own status.
const (
	errCodeBodyTooLarge  = "body_too_large"
	errCodeTooManyTracks = "too_many_tracks"
	errCodeBadTrack      = "bad_track"
	errCodeBadOperation  = "bad_operation"
	errCodeBadGeneration = "bad_generation"
	errCodeBadCommand    = "bad_command"
)

// tabPattern is a tab's identity: 32 lower-case hexadecimal digits.
var tabPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// epochPattern is an epoch: 32 lower-case hexadecimal digits.
var epochPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// controlOp is an operation's identity as the body carries it.
type controlOp struct {
	Tab   string `json:"tab"`
	Seq   uint64 `json:"seq"`
	Epoch string `json:"epoch"`
}

func (o *controlOp) valid() bool {
	return o != nil && tabPattern.MatchString(o.Tab) && o.Seq > 0 && epochPattern.MatchString(o.Epoch)
}

func (o *controlOp) op() PlayerOp { return PlayerOp{Tab: o.Tab, Seq: o.Seq, Epoch: o.Epoch} }

// controlEditBody is the replace's and the append's body.
type controlEditBody struct {
	Op       *controlOp `json:"op"`
	At       string     `json:"at"`
	Album    *string    `json:"album"`
	Playlist *string    `json:"playlist"`
	Tracks   []string   `json:"tracks"`
	Start    int        `json:"start"`
	Position int        `json:"position"`
	Play     bool       `json:"play"`
}

// controlDeleteBody is the delete's body.
type controlDeleteBody struct {
	Op    *controlOp `json:"op"`
	At    string     `json:"at"`
	Entry uint32     `json:"entry"`
}

// controlTransportBody is the transport's body.
type controlTransportBody struct {
	Op      *controlOp `json:"op"`
	At      string     `json:"at"`
	Command string     `json:"command"`
	Index   *int       `json:"index"`
	Seconds *int64     `json:"seconds"`
	On      *bool      `json:"on"`
}

// controlSwitchOutBody is the switch-out's body.
type controlSwitchOutBody struct {
	Op   *controlOp `json:"op"`
	Take bool       `json:"take"`
}

// decodeControlBody decodes a control route's body into v: one JSON object,
// no key it does not take, nothing after it, within controlMaxBodyBytes
// (413 body_too_large past it, its own refusal, never bad-json).
func (s *Server) decodeControlBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body := &bodyReader{r: r.Body}
	dec := json.NewDecoder(http.MaxBytesReader(w, body, controlMaxBodyBytes))
	dec.DisallowUnknownFields()
	refuse := func(err error) {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			s.writeControlRefusal(w, http.StatusRequestEntityTooLarge, errCodeBodyTooLarge, "the request's body is larger than the 8 MiB the player's control takes: send a long playlist by its id")
		case body.err != nil:
			writePlayerBodyUnread(w, "player_control", body.err)
		case errors.Is(err, io.EOF):
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "a JSON body is required")
		default:
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
		}
	}
	if err := dec.Decode(v); err != nil {
		refuse(err)
		return false
	}
	var after json.RawMessage
	if err := dec.Decode(&after); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || body.err != nil {
			refuse(err)
		} else {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "one JSON object, and nothing after it")
		}
		return false
	}
	return true
}

// writeControlRefusal answers a request refused before it became an
// operation: the console's error envelope and the latest publication, no
// record.
func (s *Server) writeControlRefusal(w http.ResponseWriter, status int, code, msg string) {
	body := map[string]any{"error": code, "message": msg}
	if c := s.deps.PlayerControl; c != nil {
		if pub, ok := c.Latest(); ok {
			body["player"] = json.RawMessage(pub.Bytes)
		}
	}
	writeJSON(w, status, body)
}

// checkTracks applies the control routes' list bounds to tracks.
func (s *Server) checkTracks(w http.ResponseWriter, tracks []string) bool {
	if len(tracks) > maxControlTracks {
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeTooManyTracks,
			"at most "+strconv.Itoa(maxControlTracks)+" tracks a request: send a long playlist by its id")
		return false
	}
	for _, t := range tracks {
		if t == "" || len(t) > maxControlPathBytes {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadTrack,
				"a track is a library path of 1 to "+strconv.Itoa(maxControlPathBytes)+" bytes")
			return false
		}
	}
	return true
}

// expandTracks is the tracks an edit names: an album's, in the catalog's
// order, a playlist's, in its order, or the list as sent; notFound for an
// album or a playlist the library does not hold. Exactly one of the three.
func (s *Server) expandTracks(w http.ResponseWriter, r *http.Request, b *controlEditBody) (tracks []string, notFound bool, ok bool) {
	n := 0
	for _, set := range []bool{b.Album != nil, b.Playlist != nil, b.Tracks != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "exactly one of album, playlist and tracks")
		return nil, false, false
	}
	switch {
	case b.Album != nil:
		cat, err := s.controlCatalog(r)
		if err != nil {
			logger.Error("player control: the catalog", "err", err)
			s.writeControlRefusal(w, http.StatusServiceUnavailable, "catalog_unavailable", "the library's catalog could not be read")
			return nil, false, false
		}
		if !playerIDPattern.MatchString(*b.Album) {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "invalid album id")
			return nil, false, false
		}
		album, found := cat.AlbumByID(*b.Album)
		if !found {
			return nil, true, true
		}
		return append([]string(nil), album.TrackPaths...), false, true
	case b.Playlist != nil:
		if *b.Playlist == "" || len(*b.Playlist) > maxPlayerCollectionID || s.deps.Manifest == nil {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "invalid playlist id")
			return nil, false, false
		}
		row, items, err := s.deps.Manifest.GetPlaylist(r.Context(), *b.Playlist)
		if err != nil {
			logger.Error("player control: a playlist", "err", err)
			s.writeControlRefusal(w, http.StatusInternalServerError, "internal", errMsgInternal)
			return nil, false, false
		}
		if row == nil {
			return nil, true, true
		}
		paths := make([]string, 0, len(items))
		for _, it := range items {
			if it.Path != "" {
				paths = append(paths, it.Path)
			}
		}
		return paths, false, true
	}
	if !s.checkTracks(w, b.Tracks) {
		return nil, false, false
	}
	return b.Tracks, false, true
}

// controlCatalog is the catalog snapshot, as the player's views read it.
func (s *Server) controlCatalog(r *http.Request) (*librarycat.Catalog, error) {
	return s.libraryCatalog(r.Context())
}

// controlPrelude is every control route's first step: the refusal before
// anything is read.
func (s *Server) controlPrelude(w http.ResponseWriter) (PlayerControl, bool) {
	if why := s.controlRefusal(); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return nil, false
	}
	return s.deps.PlayerControl, true
}

// apiPlayerControlNow handles GET /api/output/player/now.
func (s *Server) apiPlayerControlNow(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	pub := c.Now(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pub.Bytes)
}

// playerQueueEntryDTO is one queue entry as GET queue answers it.
type playerQueueEntryDTO struct {
	ID    uint32          `json:"id"`
	Path  string          `json:"path,omitempty"`
	Word  string          `json:"word,omitempty"`
	Track *playerTrackDTO `json:"track"`
}

// playerQueueDTO is GET /api/output/player/queue's body.
type playerQueueDTO struct {
	Epoch    string                `json:"epoch"`
	ReadAt   uint64                `json:"readAt"`
	QueueRev uint64                `json:"queueRev"`
	Token    uint32                `json:"token"`
	Selected uint32                `json:"selected"`
	Entries  []playerQueueEntryDTO `json:"entries"`
}

// apiPlayerControlQueue handles GET /api/output/player/queue: the queue of a
// publication captured now, each entry hydrated through the catalog as the
// album view hydrates its tracks; an entry whose track is gone is kept, with
// its word.
func (s *Server) apiPlayerControlQueue(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	q := c.Queue(r.Context())
	paths := make([]string, 0, len(q.Entries))
	for _, e := range q.Entries {
		paths = append(paths, e.Path)
	}
	rows := s.hydrateByPath(r, paths)
	out := playerQueueDTO{Epoch: q.Epoch, ReadAt: q.ReadAt, QueueRev: q.QueueRev, Token: q.Token, Selected: q.Selected,
		Entries: make([]playerQueueEntryDTO, 0, len(q.Entries))}
	for _, e := range q.Entries {
		d := playerQueueEntryDTO{ID: e.ID, Path: e.Path, Word: e.Word}
		if t, found := rows[e.Path]; found && e.Path != "" {
			d.Track = &t
		} else if d.Word == "" {
			d.Word = "track_gone"
		}
		out.Entries = append(out.Entries, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// hydrateByPath is the catalog's row for each path that has one, keyed by its
// path: hydrateTracks over the distinct paths, so a repeated path is one row.
func (s *Server) hydrateByPath(r *http.Request, paths []string) map[string]playerTrackDTO {
	out := map[string]playerTrackDTO{}
	if s.deps.Manifest == nil || len(paths) == 0 {
		return out
	}
	cat, err := s.controlCatalog(r)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	var distinct []string
	for _, p := range paths {
		if p != "" && !seen[p] {
			seen[p] = true
			distinct = append(distinct, p)
		}
	}
	rows, err := s.hydrateTracks(r, cat, distinct)
	if err != nil {
		logger.Warn("player control: hydrating tracks", "err", err)
		return out
	}
	for _, t := range rows {
		out[t.Path] = t
	}
	return out
}

// playerHandoverDTO is a handover as an answer or a read back carries it:
// each track by its id, its path and its catalog row (null where it has none,
// with its word).
type playerHandoverDTO struct {
	Tracks   []playerQueueEntryHandover `json:"tracks"`
	Selected int                        `json:"selected"`
	Position float64                    `json:"position"`
	Playing  bool                       `json:"playing"`
}

// playerQueueEntryHandover is one track of a handover.
type playerQueueEntryHandover struct {
	ID    string          `json:"id"`
	Path  string          `json:"path,omitempty"`
	Word  string          `json:"word,omitempty"`
	Track *playerTrackDTO `json:"track"`
}

// controlRecordDTO is a record as an answer carries it: its fields, its
// handover hydrated.
type controlRecordDTO struct {
	PlayerRecord
	Handover *playerHandoverDTO `json:"handover,omitempty"`
}

// hydrateRecord is rec with its handover's tracks hydrated through the
// catalog, nil for none.
func (s *Server) hydrateRecord(r *http.Request, rec *PlayerRecord, paths []string) *controlRecordDTO {
	if rec == nil {
		return nil
	}
	out := &controlRecordDTO{PlayerRecord: *rec}
	out.PlayerRecord.Handover = nil
	if h := rec.Handover; h != nil {
		rows := s.hydrateByPath(r, paths)
		d := &playerHandoverDTO{Selected: h.Selected, Position: h.Position, Playing: h.Playing, Tracks: make([]playerQueueEntryHandover, 0, len(h.Tracks))}
		for i, id := range h.Tracks {
			e := playerQueueEntryHandover{ID: id}
			if i < len(paths) {
				e.Path = paths[i]
			}
			if t, found := rows[e.Path]; found && e.Path != "" {
				e.Track = &t
			} else {
				e.Word = "track_gone"
			}
			d.Tracks = append(d.Tracks, e)
		}
		out.Handover = d
	}
	return out
}

// controlStatusOf is the HTTP status and code an operation's record answers
// with (the design's §5.3 table): done 200, running 202, and every other
// status the console's error envelope with the record's word.
func controlStatusOf(rec *PlayerRecord) (int, string) {
	switch rec.Status {
	case PlayerOpDone:
		return http.StatusOK, ""
	case PlayerOpRunning:
		return http.StatusAccepted, ""
	case PlayerOpChanged:
		return http.StatusConflict, "changed"
	case PlayerOpCancelled:
		return http.StatusConflict, "cancelled"
	case PlayerOpUnknown:
		return http.StatusConflict, "outcome_unknown"
	}
	if rec.Error == "not_found" {
		return http.StatusNotFound, rec.Error
	}
	if rec.Error == "" {
		return http.StatusConflict, "player_refused"
	}
	return http.StatusConflict, rec.Error
}

// controlMessages are the sentences beside the control's codes.
var controlMessages = map[string]string{
	"changed":             "Another controller changed the queue or the transport while the console's command ran: the state shows what is there.",
	"cancelled":           "The command's steps not yet sent were ended, by a newer command or the request's end.",
	"outcome_unknown":     "The bridge's player has not said how the command ended: the state shows what it is doing.",
	"stale":               "The player changed since this page last read it: nothing was changed.",
	"check":               "The DSD check is playing on the DAC: Stop ends it.",
	"maintenance":         "The player is held for maintenance.",
	"configuring":         "The player is saving its DSD choice: nothing starts until it has.",
	"output_unknown":      "The player has not heard yet whether its DAC is there.",
	"output_unavailable":  "The DAC is not available.",
	"release_unconfirmed": "The DAC has not confirmed it is free.",
	"empty_queue":         "The queue is empty.",
	"entry_not_found":     "The queue no longer holds that entry.",
	"seek_unavailable":    "There is no track to seek in.",
	"seek_out_of_range":   "That position is past the track's end, or the track's length is not known.",
	"seek_unsupported":    "The bridge's player does not seek within DSD.",
	"queue_full":          "The queue has no room for these tracks.",
	"edits_refused":       "The player takes no edits now: its IDs could not be reserved durably.",
	"overtaken":           "A stop ran before the command, so it did not run.",
	"not_playable":        "No track of the request plays on the bridge's player.",
	"not_found":           "The library holds no such album or playlist.",
	"player_not_running":  "The bridge's player does not run.",
	"tone":                "The test tone is playing on the DAC.",
	"epoch_changed":       "The bridge has restarted since this page took its state: the command did not run.",
	"replayed":            "This command was sent before: it does not run again.",
	"operation_conflict":  "Another command was sent under this command's identity: a new tab identity is needed.",
	"operation_gone":      "The bridge no longer keeps this command's record.",
	"unplayable":          "The track could not be played.",
	"all_unplayable":      "No track of the queue could be played.",
	"backend_error":       "The player's backend could not do it.",
	"backend_timeout":     "The player's backend did not answer in time.",
	"player_refused":      "The bridge's player refused the command.",
	"superseded":          "A later command replaced this one's playback before it began.",
	"occurrence_budget":   "The player has too much prepared at once.",
	"metadata_too_large":  "A track's metadata is larger than the player takes.",
	"bad_reference":       "A track's reference is not one the player takes.",
	"player_not_offered":  "This bridge offers no player of its own.",
	"closed":              "The bridge's player is stopping.",
	"index_not_found":     "The queue has no entry at that index.",
	errCodeBodyTooLarge:   "The request's body is too large.",
	errCodeTooManyTracks:  "The request names too many tracks.",
	errCodeBadTrack:       "A track is not a library path.",
	errCodeBadOperation:   "The command carries no valid identity.",
	errCodeBadGeneration:  "The command carries no valid generation.",
	errCodeBadCommand:     "The transport takes play, pause, stop, next, previous, index, seek and repeat.",
	"catalog_unavailable": "The library's catalog could not be read.",
	"bad_tab":             "A tab is 32 lower-case hexadecimal digits.",
	"bad_seq":             "A count is a whole number above 0.",
	"bad_epoch":           "An epoch is 32 lower-case hexadecimal digits.",
	"track_gone":          "The library no longer holds this track.",
}

// controlMessage is the sentence beside code.
func controlMessage(code string) string {
	if m, ok := controlMessages[code]; ok {
		return m
	}
	return "The bridge's player refused the command (" + code + ")."
}

// writeControlAnswer answers a command's operation: its record and the
// publication captured after its last answer, under the status its record's
// status answers; a refusal before it became an operation carries no record
// and the latest publication.
func (s *Server) writeControlAnswer(w http.ResponseWriter, r *http.Request, a PlayerAnswer) {
	if a.Refused != "" {
		status := a.Status
		if status == 0 {
			status = http.StatusConflict
		}
		msg := a.Message
		if msg == "" {
			msg = controlMessage(a.Refused)
		}
		body := map[string]any{"error": a.Refused, "message": msg}
		if a.Reason != "" {
			body["reason"] = a.Reason
		}
		if len(a.Player.Bytes) > 0 {
			body["player"] = json.RawMessage(a.Player.Bytes)
		}
		writeJSON(w, status, body)
		return
	}
	rec := s.hydrateRecord(r, a.Record, a.HandoverPaths)
	status, code := controlStatusOf(a.Record)
	body := map[string]any{"record": rec}
	if len(a.Player.Bytes) > 0 {
		body["player"] = json.RawMessage(a.Player.Bytes)
	}
	if code != "" {
		body["error"], body["message"] = code, controlMessage(code)
		if a.Record.Reason != "" {
			body["reason"] = a.Record.Reason
		}
	}
	writeJSON(w, status, body)
}

// commandDigest is the digest of a command as the server decoded it (J5,
// K11): the route and the command's fields, never its identity and never the
// text the client sent, so one identity carries one command, and the same
// identity sent with another is refused operation_conflict.
func commandDigest(route string, fields any) [32]byte {
	b, err := json.Marshal(struct {
		Route  string `json:"route"`
		Fields any    `json:"fields"`
	}{route, fields})
	if err != nil {
		b = []byte(route)
	}
	return sha256.Sum256(b)
}

// opOf is the identity a body carries, or a refusal answered.
func (s *Server) opOf(w http.ResponseWriter, o *controlOp) (PlayerOp, bool) {
	if !o.valid() {
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadOperation,
			"op is the command's identity: {tab: 32 lower-case hexadecimal digits, seq: a count above 0, epoch: the epoch the page took}")
		return PlayerOp{}, false
	}
	return o.op(), true
}

// generationPattern is a generation's shape as the engine writes it: 16
// hexadecimal digits, a dot and a count (the control parses it again).
var generationPattern = regexp.MustCompile(`^[0-9a-f]{16}\.[0-9]{1,20}$`)

// atOf checks a body's generation's shape.
func (s *Server) atOf(w http.ResponseWriter, at string) bool {
	if !generationPattern.MatchString(at) {
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadGeneration, "at is the generation of the state the command was made on")
		return false
	}
	return true
}

// apiPlayerControlReplace handles POST /api/output/player/queue: a replace
// of the engine's queue (an album, a playlist or a list of paths), and its
// play from a track and a position.
func (s *Server) apiPlayerControlReplace(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	var b controlEditBody
	if !s.decodeControlBody(w, r, &b) {
		return
	}
	op, ok := s.opOf(w, b.Op)
	if !ok || !s.atOf(w, b.At) {
		return
	}
	if b.Start < 0 || b.Position < 0 {
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "start and position are 0 or more")
		return
	}
	tracks, notFound, ok := s.expandTracks(w, r, &b)
	if !ok {
		return
	}
	b.Op = nil
	s.writeControlAnswer(w, r, c.Replace(r.Context(), PlayerReplace{Op: op, Digest: commandDigest("replace", b), At: b.At, Tracks: tracks, Start: b.Start,
		Position: b.Position, Play: b.Play, NotFound: notFound}))
}

// apiPlayerControlAppend handles POST /api/output/player/queue/append.
func (s *Server) apiPlayerControlAppend(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	var b controlEditBody
	if !s.decodeControlBody(w, r, &b) {
		return
	}
	op, ok := s.opOf(w, b.Op)
	if !ok || !s.atOf(w, b.At) {
		return
	}
	if b.Start != 0 || b.Position != 0 || b.Play {
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "an append takes op, at and album, playlist or tracks")
		return
	}
	tracks, notFound, ok := s.expandTracks(w, r, &b)
	if !ok {
		return
	}
	b.Op = nil
	s.writeControlAnswer(w, r, c.Append(r.Context(), PlayerAppend{Op: op, Digest: commandDigest("append", b), At: b.At, Tracks: tracks, NotFound: notFound}))
}

// apiPlayerControlDelete handles POST /api/output/player/queue/delete.
func (s *Server) apiPlayerControlDelete(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	var b controlDeleteBody
	if !s.decodeControlBody(w, r, &b) {
		return
	}
	op, ok := s.opOf(w, b.Op)
	if !ok || !s.atOf(w, b.At) {
		return
	}
	if b.Entry == 0 {
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "entry is the ID of one of the queue's entries")
		return
	}
	b.Op = nil
	s.writeControlAnswer(w, r, c.Delete(r.Context(), PlayerDelete{Op: op, Digest: commandDigest("delete", b), At: b.At, Entry: b.Entry}))
}

// apiPlayerControlTransport handles POST /api/output/player/transport: one
// command, every one but pause and stop under the tab's generation.
func (s *Server) apiPlayerControlTransport(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	var b controlTransportBody
	if !s.decodeControlBody(w, r, &b) {
		return
	}
	op, ok := s.opOf(w, b.Op)
	if !ok {
		return
	}
	cmd := PlayerCommand{Op: op, Command: b.Command}
	switch b.Command {
	case PlayerCmdPause, PlayerCmdStop:
		if b.At != "" {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, b.Command+" carries no generation: it asks for silence whatever plays")
			return
		}
	case PlayerCmdPlay, PlayerCmdNext, PlayerCmdPrevious, PlayerCmdIndex, PlayerCmdSeek, PlayerCmdRepeat:
		if !s.atOf(w, b.At) {
			return
		}
		cmd.At = b.At
	default:
		s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadCommand, controlMessage(errCodeBadCommand))
		return
	}
	extra := map[string]bool{"index": b.Index != nil, "seconds": b.Seconds != nil, "on": b.On != nil}
	want := map[string]string{PlayerCmdIndex: "index", PlayerCmdSeek: "seconds", PlayerCmdRepeat: "on"}[b.Command]
	for k, set := range extra {
		if set != (k == want) {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "index goes with index, seconds with seek and on with repeat, each alone")
			return
		}
	}
	switch {
	case b.Index != nil:
		if *b.Index < 0 {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "index is 0 or more")
			return
		}
		cmd.Index = *b.Index
	case b.Seconds != nil:
		if *b.Seconds < 0 {
			s.writeControlRefusal(w, http.StatusBadRequest, errCodeBadJSON, "seconds is 0 or more")
			return
		}
		cmd.Seconds = *b.Seconds
	case b.On != nil:
		cmd.On = *b.On
	}
	b.Op = nil
	cmd.Digest = commandDigest("transport", b)
	s.writeControlAnswer(w, r, c.Transport(r.Context(), cmd))
}

// apiPlayerControlSwitchOut handles POST /api/output/player/switch-out: the
// barrier every start of the browser goes through.
func (s *Server) apiPlayerControlSwitchOut(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	var b controlSwitchOutBody
	if !s.decodeControlBody(w, r, &b) {
		return
	}
	op, ok := s.opOf(w, b.Op)
	if !ok {
		return
	}
	b.Op = nil
	s.writeControlAnswer(w, r, c.SwitchOut(r.Context(), PlayerSwitchOut{Op: op, Digest: commandDigest("switch-out", b), Take: b.Take}))
}

// apiPlayerControlOperation handles GET /api/output/player/operation: a
// tab's last record read back whole, in the epoch the tab names, its
// handover hydrated, beside the latest publication's revision; 409
// epoch_changed for another epoch, 404 operation_gone for a record the
// control no longer keeps.
func (s *Server) apiPlayerControlOperation(w http.ResponseWriter, r *http.Request) {
	c, ok := s.controlPrelude(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	tab, epoch := q.Get("tab"), q.Get("epoch")
	seq, err := strconv.ParseUint(q.Get("seq"), 10, 64)
	switch {
	case !tabPattern.MatchString(tab):
		writeError(w, http.StatusBadRequest, "bad_tab", controlMessage("bad_tab"))
		return
	case err != nil || seq == 0:
		writeError(w, http.StatusBadRequest, "bad_seq", controlMessage("bad_seq"))
		return
	case !epochPattern.MatchString(epoch):
		writeError(w, http.StatusBadRequest, "bad_epoch", controlMessage("bad_epoch"))
		return
	}
	rb := c.Operation(epoch, tab, seq)
	switch rb.Refused {
	case "":
	case "operation_gone":
		writeJSON(w, http.StatusNotFound, map[string]any{"error": rb.Refused, "message": controlMessage(rb.Refused), "epoch": rb.Epoch, "revision": rb.Revision})
		return
	default:
		writeJSON(w, http.StatusConflict, map[string]any{"error": rb.Refused, "message": controlMessage(rb.Refused), "epoch": rb.Epoch, "revision": rb.Revision})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"epoch": rb.Epoch, "revision": rb.Revision, "record": s.hydrateRecord(r, rb.Record, rb.HandoverPaths)})
}

// bridgePlays is what the running player plays, false while no player is
// offered or none runs: a catalog row then carries no bridge field.
func (s *Server) bridgePlays() (PlayerPlays, bool) {
	if !s.playerControlOffered() {
		return PlayerPlays{}, false
	}
	return s.deps.PlayerControl.Plays()
}
