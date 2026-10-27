package api

import (
	"context"

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
)

// OutputBridge is the bridge's own player, the object of an element of kind
// bridge. Every key is present: OpenHome, Reason and LastError are null
// when they say nothing, and Codecs is never null.
type OutputBridge struct {
	DSDPlayback *OutputDSDPlayback `json:"dsdPlayback,omitempty"`
	Playback    *OutputPlayback    `json:"playback,omitempty"`
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
	// DSDMode and AllowDoP are the player's DSD settings as it runs.
	DSDMode  string `json:"dsdMode"`
	AllowDoP bool   `json:"allowDoP"`
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
}

// OutputDSDPlayback describes the current authorization epoch, not an open DAC.
type OutputDSDPlayback struct {
	BootID                string           `json:"bootId"`
	PolicyGeneration      uint64           `json:"policyGeneration"`
	QualificationRevision string           `json:"qualificationRevision,omitempty"`
	Reason                string           `json:"reason,omitempty"`
	Tuples                []OutputDSDTuple `json:"tuples"`
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
	LogicalRate      int    `json:"logicalRate,omitempty"`
	Channels         int    `json:"channels"`
	ValidBits        int    `json:"validBits"`
	PolicyID         string `json:"policyId,omitempty"`
}
