// The console's Player page (B601): the bridge's own player (B470) as the
// operator sees it, beside the web player that plays in the browser. It shows
// the player's status, turns it on or off, lets the 1-bit app cast to it or
// not (its OpenHome surface, output.player.openhome, B612), and chooses its
// DAC; it offers no DSD control and no test tone (both later work, B602).
//
// The page asks the serving process alone. cmd/bridge implements PlayerOutput
// over serve's player (its boot plan, its engine, GET /v1/outputs' own view of
// it) and the box's DAC readings, so this package imports no player package
// and the console and /v1/outputs say one thing.
//
// The three writes change output.player, which serve decides its player from
// once, at boot: each goes through the live holder (CfgHolder.UpdateChecked),
// the turn of bridge.yaml's lock every writer takes, never around it, under
// the console's lock (lockPlayerWrite), and the answer says what changes now
// and what waits for a restart. A change that turns the player off, or names
// another DAC than the one it started with, stops what it plays at once
// (serve's player withholds its output, player.md), and the page asks before
// one. The OpenHome switch stops nothing: serve reads the opt-in from its
// boot snapshot alone, and the running player's admission judges the port's
// clash on that snapshot too (config.PlayerEnabledAsBooted), so a saved opt-in
// or a listener move waits for the next start. The switch still asks the
// clash of the clone before it saves the opt-in on, since the next start
// would refuse the player over it.
//
// Every route is eligible only where the player can run, checked here, on the
// server, before anything is read or written: not on a public bridge, a
// signed-in session included; only on Linux; not over a player block that does
// not read, which a save would write back as the file held it; and not for a
// setting deployment.managedSettings gives the control plane.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// The managed-setting names of the page's three writes, as
// deployment.managedSettings lists them: the switch, the DAC and the
// OpenHome switch. A managed playerEnabled owns playerOpenHome too
// (config's managedSettingImplies): IsManagedSettingOrImplied answers it.
const (
	managedPlayerEnabled  = "playerEnabled"
	managedPlayerDevice   = "playerDevice"
	managedPlayerOpenHome = "playerOpenHome"
)

// The words an ineligible bridge's status carries (PlayerStatus.Unavailable),
// each the error code its routes refuse with.
const (
	playerNotWired       = "player_not_wired"
	playerNotAvailable   = "player_not_available"
	playerNotSupported   = "player_not_supported"
	playerBlockUnread    = "player_block_unreadable"
	playerSettingManaged = "managed-setting"
)

// PlayerOutput is the bridge's own player as the Player page asks it.
// cmd/bridge implements it over serve's player; every method reads, and none
// writes the config, which this package does.
type PlayerOutput interface {
	// Platform is the host the player runs on: "linux" is the one that runs
	// it.
	Platform() string
	// Report is the player now: what serve runs, from its boot snapshot, and
	// what live, the live config, asks for, and what waits for a restart. It
	// writes nothing and dials nothing; its one hardware read is GET
	// /v1/outputs' own bounded DAC read.
	Report(ctx context.Context, live *config.Config) PlayerReport
	// DACs lists the box's playback cards from one bounded reading of its
	// trees, each card that can be bound with a selection token the server
	// keeps (Chosen).
	DACs(ctx context.Context, live *config.Config) PlayerDACList
	// Chosen is the binding a selection token and a binding kind name now,
	// from a fresh reading: the card the token was handed out for must still
	// be the same unit at the same enumeration, and the binding the one the
	// list offered. A stale token is ErrPlayerSelectionStale, a binding that
	// would not resolve to the card now ErrPlayerBindingRefused, and a box
	// that could not be read whole ErrPlayerBoxUnread. The DAC route asks it
	// in the config's turn, just before the save, so its reading must be
	// bounded.
	Chosen(ctx context.Context, selection, kind string) (config.PlayerDevice, error)
	// SameBinding reports whether two bindings name the same thing, so a
	// choice of what the config names already writes nothing.
	SameBinding(a, b config.PlayerDevice) bool
	// Sounding reports whether the running player plays, or holds a pause,
	// now. A write asks it in the config's turn, before it saves: serve's
	// player withholds its output as soon as the save is stored, so asked
	// after the save it may have stopped already, by the write's own doing.
	Sounding() bool
	// Withholds reports whether a change of the live config from before to
	// after withholds the running player's output where before did not: the
	// player turned off, or another DAC than the one it started with.
	Withholds(before, after *config.Config) bool
}

// Chosen's refusals, which the DAC route answers with their own codes.
var (
	// ErrPlayerSelectionStale is a token the server does not hold, one that
	// expired, or one whose card is no longer the unit, or the binding, the
	// list showed.
	ErrPlayerSelectionStale = errors.New("the DAC list is out of date")
	// ErrPlayerBindingRefused is a binding of a kind that would not name the
	// chosen card now (no serial, no anchored port, a second unit of the
	// model).
	ErrPlayerBindingRefused = errors.New("that binding would not name the chosen DAC now")
	// ErrPlayerBoxUnread is a reading of the box's cards that could not be
	// had whole, or in time.
	ErrPlayerBoxUnread = errors.New("the box's sound cards could not be read")
)

// PlayerStatus is GET /api/output/player: whether the page's controls apply
// on this bridge, why not, and the player's report. Lists are never null.
type PlayerStatus struct {
	// Available is a bridge that can run its own player: Linux, not public,
	// a player block that reads, a player wired. Unavailable is the word
	// when it is not (player_not_wired, player_not_available,
	// player_not_supported, player_block_unreadable) and Why the sentence.
	Available   bool   `json:"available"`
	Unavailable string `json:"unavailable"`
	Why         string `json:"why"`
	// Managed is which of the page's three writes the control plane owns.
	Managed PlayerManaged `json:"managed"`
	PlayerReport
}

// PlayerManaged says which settings deployment.managedSettings gives the
// control plane: the page hides that control, and its route refuses it.
// OpenHome is playerOpenHome's, which a managed playerEnabled implies: the
// page hides the whole switch panel for Enabled, OpenHome with it, and only
// the OpenHome switch for OpenHome alone.
type PlayerManaged struct {
	Enabled  bool `json:"enabled"`
	Device   bool `json:"device"`
	OpenHome bool `json:"openHome"`
}

// PlayerReport is the player as cmd/bridge reads it from the serving
// process.
type PlayerReport struct {
	Running    PlayerRunning     `json:"running"`
	Configured PlayerConfigured  `json:"configured"`
	Pending    []PlayerPending   `json:"pending"`
	Transports []PlayerTransport `json:"transports"`
}

// PlayerRunning is the player serve runs: decided from the boot snapshot of
// the config, and its engine now.
type PlayerRunning struct {
	// Started is a player serve started at boot.
	Started bool `json:"started"`
	// State is GET /v1/outputs' word for it (stopped, playing, paused,
	// unavailable), or off for a player serve did not start; Reason the
	// word beside unavailable or off, and Detail the sentence.
	State  string `json:"state"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	// Transport is the engine's own state (stopped, starting, playing,
	// paused, releasing, maintenance), "" with no engine.
	Transport string `json:"transport"`
	// LastError is the last play attempt's failure while no later one
	// succeeded (device_busy), "" otherwise.
	LastError string `json:"lastError"`
	// Withheld is why the live config withholds the output now, whatever the
	// DAC does: disabled, binding_changed, or "".
	Withheld string `json:"withheld"`
	// OpenHome is the player's OpenHome Source listening on the LAN.
	OpenHome bool `json:"openHome"`
	// OpenHomeDetail is why the Source the boot snapshot asked for is not on
	// the LAN now (no LAN address bound yet, a bind that failed, a surface
	// that did not start, a player that did not), "" while it is and while
	// the boot snapshot did not ask for it. The surface follows the box's
	// addresses itself and tries a failed bind again at its next listing, so
	// the page offers no restart for it.
	OpenHomeDetail string `json:"openHomeDetail"`
	// Binding is the binding the player started with, nil for none.
	Binding *PlayerBinding `json:"binding"`
	// DAC is that binding's DAC, nil before the watcher has read the box.
	DAC *PlayerDAC `json:"dac"`
	// Track is the occurrence heard, nil while none is.
	Track *PlayerTrack `json:"track"`
	// Signal is what the DAC receives, the backend's observation, nil while
	// nothing is open.
	Signal *PlayerSignal `json:"signal"`
}

// PlayerBinding is a binding as the page shows it: the serial as its label
// alone, which a serial pasted where it does not belong cannot leak through.
type PlayerBinding struct {
	// Kind is serial, uniqueModel or port.
	Kind string `json:"kind"`
	// USBID is the model's vendor:product, "" for a port binding.
	USBID       string `json:"usbId"`
	SerialLabel string `json:"serialLabel"`
	// Port is a port binding's port.
	Port string `json:"port"`
	// Endpoint is the playback stream on the card.
	Endpoint string `json:"endpoint"`
}

// PlayerDAC is the DAC the running player's binding names.
type PlayerDAC struct {
	// State is the binding's state as the watcher resolved it last
	// (available, device_missing, …), and Reason its sentence; Stale is a
	// resolution the watcher could not read the box again since.
	State  string `json:"state"`
	Reason string `json:"reason"`
	Stale  bool   `json:"stale"`
	// Name is the card's name, serial and port taken out, "" while the DAC
	// is not available; USBID its model.
	Name  string `json:"name"`
	USBID string `json:"usbId"`
	// What the card's playback altsets take.
	MaxPCMRate     int      `json:"maxPCMRate"`
	PCMBitDepths   []int    `json:"pcmBitDepths"`
	NativeDSDRates []int    `json:"nativeDSDRates"`
	DSDFormats     []string `json:"dsdFormats"`
}

// PlayerTrack is the occurrence the player plays: its queue entry, the
// library's row for it, and what its decoder reported.
type PlayerTrack struct {
	Entry uint32 `json:"entry"`
	// Named is the library's row found for the reference: Title, Artist,
	// Album and Path (the path the library index stores) are its.
	Named  bool   `json:"named"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Path   string `json:"path"`
	// The decoder's report: codec, bits, rate, channels, and the length in
	// seconds (0 when the container declares none).
	Codec    string  `json:"codec"`
	Bits     int     `json:"bits"`
	RateHz   int     `json:"rateHz"`
	Channels int     `json:"channels"`
	Duration float64 `json:"duration"`
	// Position is what the DAC has played of it, in seconds.
	Position float64 `json:"position"`
}

// PlayerSignal is what the DAC receives, as the backend opened it.
type PlayerSignal struct {
	// Transport is pcm, dop or native.
	Transport string `json:"transport"`
	// Format is the ALSA format (S24_3LE, DSD_U32_BE, …).
	Format    string `json:"format"`
	RateHz    int    `json:"rateHz"`
	Channels  int    `json:"channels"`
	ValidBits int    `json:"validBits"`
	// State is the sink's (open, released, …).
	State string `json:"state"`
}

// PlayerConfigured is what the live config asks for: what the next start
// decides.
type PlayerConfigured struct {
	// Enabled is output.player.enabled as written.
	Enabled bool `json:"enabled"`
	// Starts is a config the next start runs the player on; Reason says
	// why not, when it asks for one.
	Starts bool   `json:"starts"`
	Reason string `json:"reason"`
	// Binding is the live binding, nil for none.
	Binding *PlayerBinding `json:"binding"`
	// OpenHome is output.player.openhome as written: whether the next start
	// advertises the player on the LAN for the 1-bit app to cast to, once the
	// player runs. OpenHomePort is the port its listener takes on each LAN
	// address (the effective openhomePort: 0 is a port the kernel picks for
	// each address), and OpenHomeName the name a controller lists it by
	// (output.player.room as the surface carries it, or its default).
	OpenHome     bool   `json:"openHome"`
	OpenHomePort int    `json:"openHomePort"`
	OpenHomeName string `json:"openHomeName"`
}

// PlayerPending is one change that waits for the next start: start, stop,
// dac or openHome (the opt-in written differs from the boot snapshot's), and
// its sentence.
type PlayerPending struct {
	Change string `json:"change"`
	Text   string `json:"text"`
}

// PlayerTransport is one transport's standing on the running player's DAC:
// pcm, dop or native. Plays is what the player does now; Capable, Mode and
// Approved are the three facts it rests on, never one standing for the
// others.
type PlayerTransport struct {
	Kind  string `json:"kind"`
	Plays bool   `json:"plays"`
	// Capable is yes, no or unknown: what the DAC's altsets take.
	Capable string `json:"capable"`
	// Mode is the setting the player runs with for it.
	Mode string `json:"mode"`
	// Approved are the owner-approved tuples of the backend's published
	// epoch, by rate; [] for PCM, which needs no approval.
	Approved []string `json:"approved"`
	// Note is one sentence about it.
	Note string `json:"note"`
}

// PlayerDACList is GET /api/output/player/dacs: one reading of the box.
type PlayerDACList struct {
	// Read is a whole reading; Problem says why not.
	Read    bool            `json:"read"`
	Problem string          `json:"problem"`
	Cards   []PlayerDACCard `json:"cards"`
}

// PlayerDACCard is one playback endpoint of one card. Selection is the token
// a choice of it sends, "" for a card that cannot be bound, and Why says why
// not.
type PlayerDACCard struct {
	Card        int             `json:"card"`
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	USBID       string          `json:"usbId"`
	SerialLabel string          `json:"serialLabel"`
	Port        string          `json:"port"`
	Endpoint    string          `json:"endpoint"`
	Selection   string          `json:"selection"`
	Why         string          `json:"why"`
	Kinds       []PlayerDACKind `json:"kinds"`
	Recommended string          `json:"recommended"`
	// Configured is the card the live binding names on this reading, and
	// Running the one the player started with.
	Configured bool `json:"configured"`
	Running    bool `json:"running"`
}

// PlayerDACKind is whether a binding of kind names the card now, and why
// not.
type PlayerDACKind struct {
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Why       string `json:"why"`
}

// playerChangeResponse is the answer to a write: restart (saved, applies at
// the next start) or unchanged (nothing written), whether the change stopped
// what the player played, and the status after it.
type playerChangeResponse struct {
	Status      string       `json:"status"`
	Interrupted bool         `json:"interrupted"`
	Player      PlayerStatus `json:"player"`
}

// playerRefusal is why a route of the page refuses on cfg, nil when it does
// not: the player not wired, a public bridge, a setting the control plane owns
// (field, "" for a read), a host that is not Linux, a player block that does
// not read. Read before anything is read or written, and again on the clone in
// the config's turn.
func (s *Server) playerRefusal(cfg *config.Config, field string) *cfgAbort {
	switch {
	case s.deps.PlayerOutput == nil:
		return &cfgAbort{status: http.StatusServiceUnavailable, code: playerNotWired,
			msg: "this console has no player wired: the bridge's own player runs inside bridge serve"}
	case cfg == nil:
		return &cfgAbort{status: http.StatusServiceUnavailable, code: playerNotWired, msg: "no config is loaded"}
	case cfg.IsPublic():
		return &cfgAbort{status: http.StatusForbidden, code: playerNotAvailable,
			msg: "a public bridge has no player: it plays to a DAC on the box it runs on, for the LAN it is on"}
	case field != "" && cfg.Deployment.IsManagedSettingOrImplied(field):
		return &cfgAbort{status: http.StatusForbidden, code: playerSettingManaged,
			msg: "this setting is managed by the control plane on this bridge and cannot be changed here: " + field}
	}
	if goos := s.deps.PlayerOutput.Platform(); goos != "linux" {
		return &cfgAbort{status: http.StatusConflict, code: playerNotSupported,
			msg: "the bridge's own player runs on Linux, and this bridge runs on " + goos}
	}
	if err := cfg.Output.Player.Unread(); err != nil {
		return &cfgAbort{status: http.StatusConflict, code: playerBlockUnread,
			msg: "output.player in the config file does not read (" + err.Error() + "): fix the block in the file first, " +
				"since a change saved from here would write its old text back"}
	}
	return nil
}

// lockPlayerWrite takes the console's lock (Server.mu) for one of the page's
// three writes, which hold it from their read of the player configuration
// (before) to their answer. A test that parks one write inside its config
// turn is told, through testPlayerWriteWaits, that the next found the lock
// held and waits for it: the one wait no other seam shows, since a write asks
// its provider nothing between its first check and this lock, and
// config.LockWaitedForTest hears only bridge.yaml's lock, which a write takes
// later. A lock it finds free tells the test nothing, so that, with another
// write's lock taken out, the test waits for this write's read of before
// instead. With no hook, as in production, it is mu.Lock.
func (s *Server) lockPlayerWrite() {
	if waits := s.testPlayerWriteWaits; waits != nil {
		if s.mu.TryLock() {
			return
		}
		waits()
	}
	s.mu.Lock()
}

// playerStatus is the page's status on cfg. A bridge where the player cannot
// run reads nothing at all; one whose player block does not read still shows
// what serve runs.
func (s *Server) playerStatus(ctx context.Context, cfg *config.Config) PlayerStatus {
	st := PlayerStatus{PlayerReport: PlayerReport{Pending: []PlayerPending{}, Transports: []PlayerTransport{}}}
	if cfg != nil {
		st.Managed = PlayerManaged{
			Enabled:  cfg.Deployment.IsManagedSettingOrImplied(managedPlayerEnabled),
			Device:   cfg.Deployment.IsManagedSettingOrImplied(managedPlayerDevice),
			OpenHome: cfg.Deployment.IsManagedSettingOrImplied(managedPlayerOpenHome),
		}
	}
	if why := s.playerRefusal(cfg, ""); why != nil {
		st.Unavailable, st.Why = why.code, why.msg
		if why.code != playerBlockUnread {
			return st
		}
	} else {
		st.Available = true
	}
	rep := s.deps.PlayerOutput.Report(ctx, cfg)
	if rep.Pending == nil {
		rep.Pending = []PlayerPending{}
	}
	if rep.Transports == nil {
		rep.Transports = []PlayerTransport{}
	}
	st.PlayerReport = rep
	return st
}

// apiPlayerOutputStatus handles GET /api/output/player.
func (s *Server) apiPlayerOutputStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.playerStatus(r.Context(), s.deps.CfgHolder.Load()))
}

// apiPlayerOutputDACs handles GET /api/output/player/dacs: one bounded
// reading of the box's cards, refused (and nothing read) where the player
// cannot run.
func (s *Server) apiPlayerOutputDACs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	cfg := s.deps.CfgHolder.Load()
	if why := s.playerRefusal(cfg, ""); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return
	}
	list := s.deps.PlayerOutput.DACs(r.Context(), cfg)
	if list.Cards == nil {
		list.Cards = []PlayerDACCard{}
	}
	for i := range list.Cards {
		if list.Cards[i].Kinds == nil {
			list.Cards[i].Kinds = []PlayerDACKind{}
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// decodePlayerBody decodes a write's JSON body into v: one object, no key it
// does not take, nothing after it but white space. A request with no body is
// refused, so a bodyless POST, which csrfGuard lets through with no
// Content-Type to check, changes nothing. What follows the object is read as
// a second value, which must be the end of the body (io.EOF): Decoder.More
// answers false for a closing brace or bracket, so it let `{...}}` through
// (B612).
func decodePlayerBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := err.Error()
		if errors.Is(err, io.EOF) {
			msg = "a JSON body is required"
		}
		writeError(w, http.StatusBadRequest, errCodeBadJSON, msg)
		return false
	}
	var after json.RawMessage
	if err := dec.Decode(&after); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, "one JSON object, and nothing after it")
		return false
	}
	return true
}

// playerEnabledRequest is POST /api/output/player/enabled's body.
type playerEnabledRequest struct {
	Enabled *bool `json:"enabled"`
	// Interrupt is the operator's go-ahead for a change that stops what the
	// player plays now (playerWouldInterrupt).
	Interrupt bool `json:"interrupt"`
}

// apiPlayerOutputEnabled handles POST /api/output/player/enabled: it turns
// the player on or off in the config, through the live holder. Turning it on
// needs a config the player can run on (config.PlayerEnabled: the block
// valid, Linux, not public, no OpenHome port another listener binds); the
// player starts at the next start. Turning it off withholds the running
// player's output at once and stops it at the next start. A value the config
// holds already writes nothing.
func (s *Server) apiPlayerOutputEnabled(w http.ResponseWriter, r *http.Request) {
	var req playerEnabledRequest
	if !decodePlayerBody(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, "enabled is required: true or false")
		return
	}
	on := *req.Enabled
	if why := s.playerRefusal(s.deps.CfgHolder.Load(), managedPlayerEnabled); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return
	}
	// The console's lock, held from before's Load to the answer. The page's
	// two writes are the only writers of output.player in the process, and
	// both hold it, so the player configuration the interruption check
	// compares (before, and the answer's after) is the one this update
	// changes: the holder's own lock begins only at the update. Without it,
	// a DAC choice saved between this Load and the update's clone could lift
	// a withholding before still shows, and a play started since would be
	// stopped unasked and answered as not interrupted
	// (TestAWriteBehindADACChoiceAsksBeforeStoppingThePlayItLetStart). The
	// lock holds nothing else of the config as before has it: writers that
	// take no console lock (the variants folder's, the UPnP servers') may
	// change other fields before the clone.
	s.lockPlayerWrite()
	defer s.mu.Unlock()
	before := s.deps.CfgHolder.Load()
	if why := s.playerRefusal(before, managedPlayerEnabled); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return
	}
	if before.Output.Player.Enabled == on {
		writeJSON(w, http.StatusOK, playerChangeResponse{Status: string(applyUnchanged), Player: s.playerStatus(r.Context(), before)})
		return
	}
	platform := s.deps.PlayerOutput.Platform()
	var sounding bool
	err := s.deps.CfgHolder.UpdateChecked(s.deps.CfgPath, func(next *config.Config, _ func() error) error {
		if why := s.playerRefusal(next, managedPlayerEnabled); why != nil {
			return why
		}
		next.Output.Player.Enabled = on
		if on {
			if _, why := next.PlayerEnabled(platform); why != nil {
				return &cfgAbort{status: http.StatusConflict, code: "player_refused",
					msg: "the player cannot run on this config: " + why.Error()}
			}
		}
		if err := validatePlayerClone(next); err != nil {
			return err
		}
		sounding = s.deps.PlayerOutput.Sounding()
		return s.playerInterruption(sounding, req.Interrupt, before, next, playerStopsOff)
	})
	if err != nil {
		writeCfgUpdateErr(w, err)
		return
	}
	s.writePlayerChange(w, r, before, sounding)
}

// playerOpenHomeRequest is POST /api/output/player/openhome's body. It has
// no go-ahead: the switch stops nothing the player plays
// (apiPlayerOutputOpenHome).
type playerOpenHomeRequest struct {
	OpenHome *bool `json:"openhome"`
}

// apiPlayerOutputOpenHome handles POST /api/output/player/openhome: it turns
// output.player.openhome, the player's OpenHome surface, on or off in the
// config, through the live holder, so that the next start advertises the
// player on the LAN for the 1-bit app to cast to, or does not. Turning it on
// asks the surface's port of the config it would save
// (config.OpenHomePortClash), whatever output.player.enabled says: a port
// another listener of serve binds is refused with its reason, nothing saved,
// so a player turned on later finds no clash. With the player off it is
// saved, and the surface starts once the player does. A value the config
// holds already writes nothing.
//
// It needs no go-ahead (interrupt), since it stops nothing the player plays.
// Serve reads the opt-in from its boot snapshot alone (planServePlayer), so
// the surface running now runs until the next start, and none starts before
// it; and the running player's admission (the watcher's settings, serve's
// player's withholding, this page's view) judges the OpenHome port clash on
// that snapshot too (config.PlayerEnabledAsBooted, B612's second round), so
// no live opt-in reaches what runs. The settings save refuses, in its turn,
// a listener address on the surface's port while the opt-in is on, so the
// clash this route refuses cannot come the other way.
func (s *Server) apiPlayerOutputOpenHome(w http.ResponseWriter, r *http.Request) {
	var req playerOpenHomeRequest
	if !decodePlayerBody(w, r, &req) {
		return
	}
	if req.OpenHome == nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, "openhome is required: true or false")
		return
	}
	on := *req.OpenHome
	if why := s.playerRefusal(s.deps.CfgHolder.Load(), managedPlayerOpenHome); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return
	}
	// The console's lock, held from before's Load to the answer, as the
	// page's other two writes hold it: the three are the writers of
	// output.player in the process. This one compares nothing for an
	// interruption, and takes it for its unchanged answer, which reads
	// before: without it, a second switch's before predates the first's
	// save, and the change the first saves is answered unchanged, nothing
	// saved (TestTheOpenHomeSwitchWaitsItsTurnBehindAnotherPlayerWrite).
	s.lockPlayerWrite()
	defer s.mu.Unlock()
	before := s.deps.CfgHolder.Load()
	if why := s.playerRefusal(before, managedPlayerOpenHome); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return
	}
	if before.Output.Player.OpenHome == on {
		writeJSON(w, http.StatusOK, playerChangeResponse{Status: string(applyUnchanged), Player: s.playerStatus(r.Context(), before)})
		return
	}
	err := s.deps.CfgHolder.UpdateChecked(s.deps.CfgPath, func(next *config.Config, _ func() error) error {
		if why := s.playerRefusal(next, managedPlayerOpenHome); why != nil {
			return why
		}
		next.Output.Player.OpenHome = on
		if on {
			if err := next.OpenHomePortClash(); err != nil {
				return &cfgAbort{status: http.StatusConflict, code: "player_refused",
					msg: "the OpenHome surface cannot run on this config: " + err.Error()}
			}
		}
		return validatePlayerClone(next)
	})
	if err != nil {
		writeCfgUpdateErr(w, err)
		return
	}
	s.writePlayerChange(w, r, before, false)
}

// playerDACRequest is POST /api/output/player/dac's body: a selection token
// GET /api/output/player/dacs handed out, the kind of binding, and the
// operator's go-ahead for a change that stops what the player plays now
// (playerWouldInterrupt).
type playerDACRequest struct {
	Selection string `json:"selection"`
	Binding   string `json:"binding"`
	Interrupt bool   `json:"interrupt"`
}

// apiPlayerOutputDAC handles POST /api/output/player/dac: it binds the player
// to the DAC a selection names, what `bridge output dac bind` writes, through
// the live holder. The binding is the one a fresh reading builds for the card
// the token was handed out for, refused when that card is no longer the unit
// or the binding the list showed. The player opens it at the next start;
// until then a running player's output is withheld, unless the binding is the
// one it started with. A binding the config holds already writes nothing.
func (s *Server) apiPlayerOutputDAC(w http.ResponseWriter, r *http.Request) {
	var req playerDACRequest
	if !decodePlayerBody(w, r, &req) {
		return
	}
	switch req.Binding {
	case config.BindingSerial, config.BindingUniqueModel, config.BindingPort:
	default:
		writeError(w, http.StatusBadRequest, errCodeBadJSON, "binding is serial, uniqueModel or port")
		return
	}
	if strings.TrimSpace(req.Selection) == "" {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, "selection is required: a token GET /api/output/player/dacs handed out")
		return
	}
	if why := s.playerRefusal(s.deps.CfgHolder.Load(), managedPlayerDevice); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return
	}
	// The console's lock, held from before's Load to the answer, as the
	// switch holds it: a write of either route saved between this Load and
	// the update's clone would leave before showing a withholding the clone
	// no longer has, so a choice that stops a play started since would be
	// saved unasked and answered as not interrupted. A choice's turn lasts as
	// long as its reading of the box (Chosen, bounded by the provider): the
	// widest window in which another write would read its before.
	s.lockPlayerWrite()
	defer s.mu.Unlock()
	before := s.deps.CfgHolder.Load()
	if why := s.playerRefusal(before, managedPlayerDevice); why != nil {
		writeError(w, why.status, why.code, why.msg)
		return
	}
	// The choice is checked against the box in the config's turn, under the
	// console's lock and bridge.yaml's, just before the save. Checked before
	// the locks, it went stale while the write waited for them, and a port or
	// model binding then named whatever unit took the chosen one's place. The
	// reading is the provider's, bounded, and the one wait the turn holds the
	// file's lock across.
	var sounding bool
	err := s.deps.CfgHolder.UpdateChecked(s.deps.CfgPath, func(next *config.Config, _ func() error) error {
		if why := s.playerRefusal(next, managedPlayerDevice); why != nil {
			return why
		}
		dev, err := s.deps.PlayerOutput.Chosen(r.Context(), req.Selection, req.Binding)
		if err != nil {
			return playerChoiceRefusal(err)
		}
		if s.deps.PlayerOutput.SameBinding(next.Output.Player.Device, dev) {
			return errPlayerUnchanged
		}
		next.Output.Player.Device = dev
		if err := validatePlayerClone(next); err != nil {
			return err
		}
		sounding = s.deps.PlayerOutput.Sounding()
		return s.playerInterruption(sounding, req.Interrupt, before, next, playerStopsDAC)
	})
	switch {
	case errors.Is(err, errPlayerUnchanged):
		writeJSON(w, http.StatusOK, playerChangeResponse{Status: string(applyUnchanged), Player: s.playerStatus(r.Context(), before)})
		return
	case err != nil:
		writeCfgUpdateErr(w, err)
		return
	}
	s.writePlayerChange(w, r, before, sounding)
}

// errPlayerUnchanged ends a DAC choice's turn with nothing saved: the config
// names the chosen binding already.
var errPlayerUnchanged = errors.New("the config names that DAC already")

// playerWouldInterrupt is the code of a write refused because it would stop
// what the player plays or holds paused now and came without the operator's
// go-ahead (interrupt). The page asks the operator in its message's words and
// sends the write again with the go-ahead; nothing was saved.
const playerWouldInterrupt = "would_interrupt"

// What a refused interruption says it stops: the switch turning the player
// off, and the choice of another DAC than the one it started with.
const (
	playerStopsOff = "The player is playing, or holds a pause, now. Turning it off stops it at once: it plays nothing until it is turned on again, and the bridge does not start it at its next restart."
	playerStopsDAC = "The player is playing, or holds a pause, now. Choosing this DAC stops it at once: it keeps the DAC it started with until the bridge restarts, and plays nothing until then, or until that DAC is chosen again."
)

// playerInterruption is the last check of a write's turn: a change that
// withholds the running player's output while it sounds (both read in the
// turn, just before the save) needs the operator's go-ahead, and without it
// is refused with what it stops, nothing saved. Asked in the turn, never from
// the page's last status, which may predate a play an OpenHome controller
// started since.
func (s *Server) playerInterruption(sounding, goAhead bool, before, next *config.Config, stops string) error {
	if !sounding || goAhead || !s.deps.PlayerOutput.Withholds(before, next) {
		return nil
	}
	return &cfgAbort{status: http.StatusConflict, code: playerWouldInterrupt, msg: stops}
}

// playerChoiceRefusal is the answer to a choice the provider refused: a stale
// selection, a binding that would not name the card now, a box that could not
// be read, each with its code; anything else is logged, and internal.
func playerChoiceRefusal(err error) *cfgAbort {
	switch {
	case errors.Is(err, ErrPlayerSelectionStale):
		return &cfgAbort{status: http.StatusConflict, code: "stale_selection", msg: err.Error() + ": list the DACs again, and choose"}
	case errors.Is(err, ErrPlayerBindingRefused):
		return &cfgAbort{status: http.StatusConflict, code: "binding_refused", msg: err.Error()}
	case errors.Is(err, ErrPlayerBoxUnread):
		return &cfgAbort{status: http.StatusServiceUnavailable, code: "box_unread", msg: err.Error()}
	}
	logger.Error("player page: a DAC choice failed", "err", err)
	return &cfgAbort{status: http.StatusInternalServerError, code: "internal", msg: errMsgInternal}
}

// validatePlayerClone is the last check of a write's clone: the player block
// it would save takes every value it holds (PlayerConfig.Problem), and the
// whole config validates as every console save does.
func validatePlayerClone(next *config.Config) error {
	if err := next.Output.Player.Problem(); err != nil {
		return &cfgAbort{status: http.StatusConflict, code: "player_refused", msg: err.Error()}
	}
	if err := next.NormalizeAndValidate(); err != nil {
		return &cfgAbort{status: http.StatusBadRequest, code: "validate", msg: err.Error()}
	}
	return nil
}

// writePlayerChange answers a write that saved: restart, whether it stopped
// what the player played (sounding, as the write's turn found the player just
// before its save, and the change withholding the output), and the status
// after it.
func (s *Server) writePlayerChange(w http.ResponseWriter, r *http.Request, before *config.Config, sounding bool) {
	after := s.deps.CfgHolder.Load()
	writeJSON(w, http.StatusOK, playerChangeResponse{
		Status:      string(applyRestart),
		Interrupted: sounding && s.deps.PlayerOutput.Withholds(before, after),
		Player:      s.playerStatus(r.Context(), after),
	})
}

// outputPageData is the Player page's first paint: whether a restart is
// supervised (the restart button's wording, as Settings words it).
type outputPageData struct {
	IsSupervised bool
}

// pageOutput renders the Player page. Its status is fetched by the page
// (GET /api/output/player), never seeded: a save on the page changes it.
func (s *Server) pageOutput(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "output", outputPageData{IsSupervised: s.deps.IsSupervised})
}
