package playback

import (
	"context"
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
)

// ProbeWait is the longest a Start waits for a probe that holds the device
// slot (the design's section 5: a probe is one open, a few ioctls and a
// close).
const ProbeWait = time.Second

// ProbeSkip is why the backend ran no probe, a word for a log line.
type ProbeSkip string

const (
	// ProbeOutputUnavailable: the bound DAC is not available.
	ProbeOutputUnavailable ProbeSkip = "output_unavailable"
	// ProbeSnapshotStale: the hotplug snapshot is stale.
	ProbeSnapshotStale ProbeSkip = "snapshot_stale"
	// ProbeNotLocated: the binding resolves to no unit now.
	ProbeNotLocated ProbeSkip = "not_located"
	// ProbeSessionHolds: a session holds the device.
	ProbeSessionHolds ProbeSkip = "session_holds_device"
	// ProbeSessionPaused: a paused session holds the device.
	ProbeSessionPaused ProbeSkip = "session_paused"
	// ProbeReleasePending: a session's release has not ended.
	ProbeReleasePending ProbeSkip = "release_pending"
	// ProbeReleaseUnconfirmed: a release was never confirmed.
	ProbeReleaseUnconfirmed ProbeSkip = "release_unconfirmed"
	// ProbeRunning: another probe holds the slot.
	ProbeRunning ProbeSkip = "probe_running"
)

// ProbeSkipped is a probe the backend did not run.
type ProbeSkipped struct {
	Reason ProbeSkip
	Err    error
}

func (e *ProbeSkipped) Error() string {
	s := "player backend: the DAC was not probed: " + string(e.Reason)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *ProbeSkipped) Unwrap() error { return e.Err }

// ProbeConditions are the section 5 conditions the backend cannot see for
// itself.
type ProbeConditions struct {
	// Available is the binding resolved to its unit, available.
	Available bool
	// Fresh is the hotplug snapshot not stale.
	Fresh bool
}

// ProbeRequest is what Probe is asked.
type ProbeRequest struct {
	Conditions ProbeConditions
	Candidates []alsapcm.ProbeCandidate
}

// probeHold is the probe that holds the device slot.
type probeHold struct{ done chan struct{} }

// Probe runs the silent probe on the bound unit.
func (b *Backend) Probe(ctx context.Context, req ProbeRequest) (alsapcm.ProbeReport, error) {
	return notProbed(req.Candidates), errors.New("player backend: the silent probe is not built yet")
}

// reserveProbe takes the device slot for a probe, or says why it cannot.
func (b *Backend) reserveProbe() (*probeHold, ProbeSkip) {
	return &probeHold{done: make(chan struct{})}, ""
}

// endProbe gives the slot back.
func (b *Backend) endProbe(p *probeHold) {}

// notProbed is the report of a probe that asked nothing.
func notProbed(cands []alsapcm.ProbeCandidate) alsapcm.ProbeReport {
	rep := alsapcm.ProbeReport{Results: make([]alsapcm.ProbeResult, len(cands))}
	for i, c := range cands {
		rep.Results[i] = alsapcm.ProbeResult{Candidate: c, Answer: alsapcm.ProbeNotProbed}
	}
	return rep
}
