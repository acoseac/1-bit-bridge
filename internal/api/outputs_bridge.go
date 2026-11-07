package api

import (
	"context"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// GET /v1/outputs lists the bridge's own player beside MPD's outputs, as an
// element of kind bridge (B470 PR 5b). Two gates, never one: the MPD gate
// (localOutputs.Active, mpdOn: output.mpd resolves on and the host is
// Linux), which alone starts and answers the loopback track route, shows
// the player checklist and advertises outputReading, and serve's player
// (BridgeOutput.Enabled, bridgeOn), which probes, dials and starts nothing.
// /v1/health advertises localOutputs while either holds, and bridgeOutput
// while the player's does. This package imports no player package: serve
// hands it plain facts (BridgeOutputFacts), and the handler builds the wire
// element from them (bridgeElement), never encoding a domain type.

// Kind bridge's words (PROTOCOL.md, the bridge object).
const (
	// OutputKindBridge is the kind of the bridge's own player's element.
	OutputKindBridge = "bridge"
	// bridgeOutputIDPrefix is the element's id before the player's UUID.
	bridgeOutputIDPrefix = "bridge:"
	// bridgePlayerSoftware is player.software for kind bridge.
	bridgePlayerSoftware = "bridge"
	// LibraryScopeSelf is bridge.libraryScope: the tracks this bridge would
	// not refuse as not_indexed, routed or outside_library.
	LibraryScopeSelf = "self"
	// BridgeStateUnavailable is the state a reason comes with.
	BridgeStateUnavailable = "unavailable"
	// TimeSeekSupported, TimeSeekUnsupported and TimeSeekUnknown are the
	// words bridge.timeSeek says of a kind of content (B674): whether the
	// player seeks within a track of it.
	TimeSeekSupported   = "supported"
	TimeSeekUnsupported = "unsupported"
	TimeSeekUnknown     = "unknown"
)

// TimeSeekWords is every word bridge.timeSeek sends or names, in
// PROTOCOL.md's order.
func TimeSeekWords() []string {
	return []string{TimeSeekSupported, TimeSeekUnsupported, TimeSeekUnknown}
}

// OutputBridge is the bridge's own player, the object of an element of kind
// bridge. Every key is present: OpenHome, Reason and LastError are null
// when they say nothing, and Codecs is never null.
type OutputBridge struct {
	DSDPlayback *OutputDSDPlayback `json:"dsdPlayback,omitempty"`
	// DSDDAC is the bound DAC's DSD (B728 B8): optional, omitted while no
	// DAC is available, as an older bridge omits it.
	DSDDAC   *OutputDSDDAC   `json:"dsdDAC,omitempty"`
	Playback *OutputPlayback `json:"playback,omitempty"`
	// OpenHome is the player's OpenHome Source while the opt-in started it
	// and it listens on a LAN address, null otherwise.
	OpenHome *OutputOpenHome `json:"openHome"`
	// State is stopped, playing, paused or unavailable.
	State string `json:"state"`
	// Reason is why the player is unavailable, a word, while State is
	// unavailable; null otherwise.
	Reason *string `json:"reason"`
	// LastError is the last play attempt's failure while no later attempt
	// succeeded, a word (device_busy), and null otherwise.
	LastError *string `json:"lastError"`
	// LibraryScope is which tracks the player plays: self, this bridge's
	// own.
	LibraryScope string `json:"libraryScope"`
	// FixedVolume is an output with no volume control (the mixer none).
	FixedVolume bool `json:"fixedVolume"`
	// Codecs are the containers the player plays in this release.
	Codecs []string `json:"codecs"`
	// DSDMode and AllowDoP are the legacy DSD keys as the boot config holds
	// them (output.player.dsdMode and allowDoP), which since B728's cutover
	// (B7) decide nothing: the player's DSD state does, and DSDPlayback's
	// tuples are what plays.
	DSDMode  string `json:"dsdMode"`
	AllowDoP bool   `json:"allowDoP"`
	// TimeSeek is whether the player seeks within a track, by the kind of
	// content (B674): optional, omitted when no fact says it, as an older
	// bridge omits it.
	TimeSeek *OutputTimeSeek `json:"timeSeek,omitempty"`
}

// OutputTimeSeek is what the player's implementation does with a seek within
// a track (OpenHome's SeekSecondAbsolute and SeekSecondRelative), by the kind
// of content: pcm and dsd (DSD native or in DoP), each supported, unsupported
// or unknown (TimeSeekWords), and omitted when no fact says it. It describes
// what the player can do, not whether a seek can be made now: a player
// stopped, or with nothing heard, refuses a seek it supports.
type OutputTimeSeek struct {
	PCM string `json:"pcm,omitempty"`
	DSD string `json:"dsd,omitempty"`
}

// OutputOpenHome is the player's OpenHome Source: its UDN, and the
// host:port of each LOCATION it advertises, which a client checks a
// discovered Source against, since the UDN alone is SSDP's, in clear.
type OutputOpenHome struct {
	UDN       string   `json:"udn"`
	Endpoints []string `json:"endpoints"`
}

// BridgeOutputFacts is the bridge's own player for one request of GET
// /v1/outputs, as cmd/bridge reads it from the player serve runs: plain
// values. Listed false, or no UUID, lists no element. Reason is read while
// State is unavailable; OpenHome nil, or with no endpoint, is no Source.
type BridgeOutputFacts struct {
	DSDPlayback *OutputDSDPlayback
	DSDDAC      *OutputDSDDAC
	Playback    *OutputPlayback
	Listed      bool
	UUID        string
	State       string
	Reason      string
	LastError   string
	OpenHome    *BridgeOpenHomeFacts
	Codecs      []string
	FixedVolume bool
	DSDMode     string
	AllowDoP    bool
	// TimeSeekPCM and TimeSeekDSD are whether the player seeks within a
	// track of PCM and of DSD, each a TimeSeekWords word; "" says nothing,
	// and both "" leave bridge.timeSeek out (B674).
	TimeSeekPCM, TimeSeekDSD string
	// DAC is the boot binding's resolved card, projected by cmd/bridge.
	// It contains no device identity; IsMPDsOutput is always nil.
	DAC *OutputDAC
}

// BridgeOpenHomeFacts is the player's OpenHome Source as BridgeOutputFacts
// carry it.
type BridgeOpenHomeFacts struct {
	UDN       string
	Endpoints []string
}

// BridgeOutput is serve's player as GET /v1/outputs and /v1/health ask it.
type BridgeOutput interface {
	// Enabled is serve's boot decision that the player is enabled
	// (config.PlayerEnabled on the boot snapshot) and that serve holds its
	// data directory's lock: fixed for the process, cheap and with no I/O.
	// /v1/health reads it for localOutputs and bridgeOutput, and the route
	// for its 404.
	Enabled() bool
	// Facts is the player now, built from snapshots and at most one read of
	// the player's identity file, which it never makes or writes. DAC is a
	// privacy-safe projection of the boot binding, nil while unavailable or
	// stale. Any hardware read has a short deadline and at most one in flight
	// per server, even when a kernel read outlives cancellation. Concurrent
	// callers share that read within their own deadlines. Known serial/port
	// substrings in DAC labels are redacted and capability lists are non-nil.
	Facts(ctx context.Context) BridgeOutputFacts
}

// WithBridgeOutput wires serve's player into GET /v1/outputs (B470 PR 5b):
// before the listeners start, whether or not the player starts.
func (s *Server) WithBridgeOutput(b BridgeOutput) *Server {
	s.bridgeOutput = b
	return s
}

// bridgeOutputOn is the player's gate: serve wired its player, and decided
// at boot that it is enabled and its data directory's lock is held.
func (s *Server) bridgeOutputOn() bool {
	return s.bridgeOutput != nil && s.bridgeOutput.Enabled()
}

// bridgeElement is the bridge's own player's element, built afresh from its
// facts on each call, never from the MPD body's cache; false while the
// player's gate is off or the player has no identity.
func (s *Server) bridgeElement(ctx context.Context) (OutputInfo, bool) {
	if !s.bridgeOutputOn() {
		return OutputInfo{}, false
	}
	f := s.bridgeOutput.Facts(ctx)
	if !f.Listed || f.UUID == "" {
		return OutputInfo{}, false
	}
	b := &OutputBridge{
		DSDPlayback: f.DSDPlayback, Playback: f.Playback,
		State:        f.State,
		LibraryScope: LibraryScopeSelf,
		FixedVolume:  f.FixedVolume,
		Codecs:       append([]string{}, f.Codecs...),
		DSDMode:      f.DSDMode,
		AllowDoP:     f.AllowDoP,
	}
	if f.State == BridgeStateUnavailable && f.Reason != "" {
		why := f.Reason
		b.Reason = &why
	}
	if f.LastError != "" {
		last := f.LastError
		b.LastError = &last
	}
	if oh := f.OpenHome; oh != nil && len(oh.Endpoints) > 0 {
		b.OpenHome = &OutputOpenHome{UDN: oh.UDN, Endpoints: append([]string{}, oh.Endpoints...)}
	}
	if f.TimeSeekPCM != "" || f.TimeSeekDSD != "" {
		b.TimeSeek = &OutputTimeSeek{PCM: f.TimeSeekPCM, DSD: f.TimeSeekDSD}
	}
	return OutputInfo{
		ID:        bridgeOutputIDPrefix + f.UUID,
		Kind:      OutputKindBridge,
		Player:    OutputPlayer{Software: bridgePlayerSoftware, Version: version.ServerVersion},
		PathRules: []OutputPathRule{},
		Bridge:    b,
		DAC:       f.DAC,
	}, true
}

// composeOutputs is GET /v1/outputs' body: the MPD elements when mpdOn,
// exactly as the builder answers them (its flight and its caches,
// localOutputs.Response), and then the bridge's own element, built afresh
// (bridgeElement), so its state is never a cached MPD body's age behind. The
// slice is new on every call, a copy of the MPD part and then the element:
// the cached body is never written to. With mpdOn false the MPD builder is
// not entered. An MPD failure is the body's error, the bridge's element with
// it: the route answers 500, as before.
func (s *Server) composeOutputs(ctx context.Context, mpdOn bool) (OutputsResponse, error) {
	var mpdPart []OutputInfo
	if mpdOn {
		resp, err := s.localOutputs.Response(ctx)
		if err != nil {
			return OutputsResponse{}, err
		}
		mpdPart = resp.Outputs
	}
	out := make([]OutputInfo, 0, len(mpdPart)+1)
	out = append(out, mpdPart...)
	if el, ok := s.bridgeElement(ctx); ok {
		out = append(out, el)
	}
	return OutputsResponse{Outputs: out}, nil
}

// OutputDSDTuple is one exact approved tuple and its indivisible policy.
type OutputDSDTuple struct {
	Format    string `json:"format"`
	BitRate   int    `json:"bitRate"`
	Channels  int    `json:"channels"`
	PolicyID  string `json:"policyId"`
	Pause     string `json:"pause"`
	TailMS    int64  `json:"tailMs"`
	PrefillMS int64  `json:"prefillMs"`
	// Transport is dop for a DoP tuple; absent, a native DSD one.
	Transport string `json:"transport,omitempty"`
	// Evidence is the tuple's rung of the evidence ladder and EvidenceOn its
	// date, YYYY-MM-DD (B728 B8): each absent while there is none.
	Evidence   string `json:"evidence,omitempty"`
	EvidenceOn string `json:"evidenceOn,omitempty"`
}

// OutputDSDConverted is a rate the set converts to PCM (B728 B8), with
// the plan it converts by: every claim of a converted occurrence is its
// plan's.
type OutputDSDConverted struct {
	BitRate    int    `json:"bitRate"`
	RateHz     int    `json:"rateHz"`
	ValidBits  int    `json:"validBits"`
	PassbandHz int    `json:"passbandHz"`
	Dither     string `json:"dither"`
	Filter     string `json:"filter"`
}

// OutputDSDRefused is a rate the set plays neither way, and the word it
// refuses that rate with.
type OutputDSDRefused struct {
	BitRate int    `json:"bitRate"`
	Reason  string `json:"reason"`
}

// OutputDSDWarning is one thing a client must be able to explain: a code,
// and migration_record_refused's reason word.
type OutputDSDWarning struct {
	Code   string `json:"code"`
	Reason string `json:"reason,omitempty"`
}

// OutputDSDDAC is the bound DAC's DSD as the set published now says it
// (B728 B8): its key, its profile, the choice that applies and its
// revision, what each mode offers, and each transport's first use.
type OutputDSDDAC struct {
	DACKey   string              `json:"dacKey"`
	Profile  *OutputDSDProfile   `json:"profile,omitempty"`
	Choice   OutputDSDChoice     `json:"choice"`
	Options  []OutputDSDOption   `json:"options"`
	FirstUse []OutputDSDFirstUse `json:"firstUse"`
}

// OutputDSDProfile is the DAC's profile in the bridge's database: its ID,
// its name and its caveats.
type OutputDSDProfile struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Caveats []OutputDSDCaveat `json:"caveats"`
}

// OutputDSDCaveat is a profile's caveat: a code, the transport it concerns
// (absent for both) and the note a person is shown.
type OutputDSDCaveat struct {
	Code      string `json:"code"`
	Transport string `json:"transport,omitempty"`
	Note      string `json:"note"`
}

// OutputDSDChoice is the choice that applies to the DAC: its mode, its cap
// (0 for none), the revision of the DAC's own choice (0 for none), and
// whether the DAC's DoP confirmation counts on its firmware.
type OutputDSDChoice struct {
	Mode         string `json:"mode"`
	MaxBitRate   int    `json:"maxBitRate"`
	Revision     uint64 `json:"revision"`
	DoPConfirmed bool   `json:"dopConfirmed"`
}

// OutputDSDOption is what a mode offers the DAC: native's and DoP's rates
// with their evidence, DoP's need of a confirmation and what blocks it, and
// the plan convert converts by. A member a mode does not have is absent.
type OutputDSDOption struct {
	Mode    string `json:"mode"`
	Offered bool   `json:"offered"`
	// Rates is native's and DoP's, [] when the mode offers no rate.
	Rates             *[]OutputDSDOptionRate `json:"rates,omitempty"`
	NeedsConfirmation *bool                  `json:"needsConfirmation,omitempty"`
	Blocked           string                 `json:"blocked,omitempty"`
	RateHz            int                    `json:"rateHz,omitempty"`
	ValidBits         int                    `json:"validBits,omitempty"`
	PassbandHz        int                    `json:"passbandHz,omitempty"`
	Dither            string                 `json:"dither,omitempty"`
}

// OutputDSDOptionRate is a rate a mode offers, and its evidence.
type OutputDSDOptionRate struct {
	BitRate    int    `json:"bitRate"`
	Evidence   string `json:"evidence,omitempty"`
	EvidenceOn string `json:"evidenceOn,omitempty"`
}

// OutputDSDFirstUse is a transport's first use: pending, acknowledged or
// observed, and the rates a migrated acknowledgement covers (absent for
// every rate).
type OutputDSDFirstUse struct {
	Transport string `json:"transport"`
	State     string `json:"state"`
	BitRates  []int  `json:"bitRates,omitempty"`
}

// OutputDSDPlayback describes the DSD set published now, not an open DAC:
// since B728's cutover (B7) the player's effective DSD set, which its
// backend admits by.
type OutputDSDPlayback struct {
	BootID                string           `json:"bootId"`
	PolicyGeneration      uint64           `json:"policyGeneration"`
	QualificationRevision string           `json:"qualificationRevision,omitempty"`
	Reason                string           `json:"reason,omitempty"`
	Tuples                []OutputDSDTuple `json:"tuples"`
	// Converted, Refused and Warnings are the set's rates converted to PCM,
	// its rates refused and what a client must be able to explain (B728
	// B8): each an array, [] when empty, which an older bridge omits.
	Converted []OutputDSDConverted `json:"converted"`
	Refused   []OutputDSDRefused   `json:"refused"`
	Warnings  []OutputDSDWarning   `json:"warnings"`
	// Model and BCDDevice are the bound DAC's model and descriptor firmware
	// code the set was computed for; QualifiedOn and QualificationRevision
	// are an owner record's, which a harness backend's epoch states and
	// production reads no more (B728 B7), so serve sends neither. Never the
	// DAC's port or serial.
	Model       string `json:"model,omitempty"`
	BCDDevice   string `json:"bcdDevice,omitempty"`
	QualifiedOn string `json:"qualifiedOn,omitempty"`
}

// OutputPlayback is the backend's current occurrence observation.
type OutputPlayback struct {
	BootID           string `json:"bootId"`
	PolicyGeneration uint64 `json:"policyGeneration"`
	Session          uint64 `json:"session"`
	Occurrence       uint64 `json:"occurrence"`
	State            string `json:"state"`
	Format           string `json:"format"`
	RateHz           int    `json:"rateHz"`
	LogicalRate      int    `json:"logicalRate"`
	Channels         int    `json:"channels"`
	ValidBits        int    `json:"validBits"`
	PolicyID         string `json:"policyId,omitempty"`
	// Transport is dop or pcm; absent, native DSD.
	Transport string `json:"transport,omitempty"`
	// PackingVersion is the DSD packer's version, absent for PCM.
	PackingVersion int `json:"packingVersion,omitempty"`
	// ObservedAt is when the backend last observed the occurrence's sink.
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	// QualificationRevision is the approved record's revision the
	// occurrence was admitted under, absent for PCM.
	QualificationRevision string `json:"qualificationRevision,omitempty"`
	// Evidence and EvidenceOn are a native or DoP occurrence's tuple's rung
	// and its date (B728 B8), absent for PCM and a converted one.
	Evidence   string `json:"evidence,omitempty"`
	EvidenceOn string `json:"evidenceOn,omitempty"`
	// Converted is a converted occurrence's plan (B728 B8), absent for every
	// other: Format, RateHz and ValidBits are then its PCM's.
	Converted *OutputPlaybackConverted `json:"converted,omitempty"`
}

// OutputPlaybackConverted is the plan a converted occurrence plays by: the
// source's DSD rate, the filter, the passband, the dither and the gain, and
// the samples its conversion has saturated so far.
type OutputPlaybackConverted struct {
	FromBitRate    int     `json:"fromBitRate"`
	Filter         string  `json:"filter"`
	PassbandHz     int     `json:"passbandHz"`
	Dither         string  `json:"dither"`
	GainDB         float64 `json:"gainDB"`
	ClippedSamples int64   `json:"clippedSamples"`
}
