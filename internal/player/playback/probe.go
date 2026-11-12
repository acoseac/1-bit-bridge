package playback

import (
	"context"
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
)

// ProbeWait is the longest a Start waits for a probe that holds the device
// slot (B728's design, section 5: a probe is one open, a few ioctls and a
// close). Past it the Start answers the device busy (engine.ErrDeviceBusy),
// holding nothing: the probe's node is still open.
const ProbeWait = time.Second

// ProbeSkip is why the backend ran no probe, a word for a log line.
type ProbeSkip string

const (
	// ProbeOutputUnavailable: the bound DAC is not available.
	ProbeOutputUnavailable ProbeSkip = "output_unavailable"
	// ProbeSnapshotStale: the hotplug snapshot is stale, readings having
	// failed since it.
	ProbeSnapshotStale ProbeSkip = "snapshot_stale"
	// ProbeNotLocated: the binding resolves to no unit now.
	ProbeNotLocated ProbeSkip = "not_located"
	// ProbeSessionHolds: a session holds the device: it plays, it is
	// opening the device, or its Start waited for a probe and is about to
	// take the slot.
	ProbeSessionHolds ProbeSkip = "session_holds_device"
	// ProbeSessionPaused: a paused session holds the slot, whether or not
	// its pause's release has let the device go: a resume reopens it.
	ProbeSessionPaused ProbeSkip = "session_paused"
	// ProbeReleasePending: a session's release has not ended: its Release
	// has come, or the engine has taken it and the call has not come yet.
	ProbeReleasePending ProbeSkip = "release_pending"
	// ProbeReleaseUnconfirmed: a release was never confirmed, so the device
	// may still be held (the backend refuses every Start too).
	ProbeReleaseUnconfirmed ProbeSkip = "release_unconfirmed"
	// ProbeRunning: another probe holds the slot.
	ProbeRunning ProbeSkip = "probe_running"
	// ProbeChoiceWrite: a choice's write holds the configuration lease
	// (ChooseDSD, B728 B9 PR 2): the probe it owes runs once it is let go.
	ProbeChoiceWrite ProbeSkip = "choice_write"
)

// ProbeSkipped is a probe the backend did not run, and why: nothing was
// opened, and every candidate is not probed.
type ProbeSkipped struct {
	Reason ProbeSkip
	// Err is the binding's refusal, for ProbeNotLocated.
	Err error
}

func (e *ProbeSkipped) Error() string {
	s := "player backend: the DAC was not probed: " + string(e.Reason)
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *ProbeSkipped) Unwrap() error { return e.Err }

// ProbeConditions are the conditions of the design's section 5 the backend
// cannot see for itself; serve's player (B7) reads both from its hotplug
// watcher's latest snapshot.
type ProbeConditions struct {
	// Available is the binding resolved to its unit, available
	// (binding.StateAvailable).
	Available bool
	// Fresh is that snapshot not stale: no reading has failed since it.
	Fresh bool
}

// ProbeRequest is what Probe is asked.
type ProbeRequest struct {
	Conditions ProbeConditions
	// Candidates are what the kernel is asked (alsapcm.ProbeCandidates for
	// the design's set).
	Candidates []alsapcm.ProbeCandidate
	// Check, when set, is asked of the node the probe opened, after the
	// binding's own check after open and before any candidate: the effective
	// policy's check that the node is the unit the probe's answers will be
	// recorded for (ProbeDSD). A port binding's check names a place, which
	// another unit swapped in passes; this one refuses it, and the probe asks
	// it nothing (B728 B7, round 2).
	Check func(alsapcm.NodeInfo) error
}

// probeHold is a probe that holds the device slot: done is closed once it
// has given the slot back.
type probeHold struct{ done chan struct{} }

// Probe runs the silent probe (B728's design, section 5) on the bound unit,
// through Config.Probe (alsapcm.Probe), and answers its report. It runs
// only when the bound DAC is available, the hotplug snapshot is fresh, no
// session holds the device (one that plays or opens it, a paused one, one
// whose release is pending), no release was left unconfirmed, and no other
// probe runs; otherwise it opens nothing and answers ProbeSkipped, every
// candidate not probed. It takes the backend's device slot in the step that
// finds it free, under the lock a Start's hold takes it under, and holds it
// for its whole length, the binding's location and the check after open
// included, so a Start that comes meanwhile waits for it (ProbeWait). The
// probe runs on a goroutine the backend counts, and gives the slot back only
// once its node is closed: when ctx ends first Probe answers ctx's error, and
// a probe the kernel holds in a call keeps the slot until the kernel lets it
// go. Nothing calls it on a timer: since B728's B7 the effective policy's
// ProbeDSD calls it, when a probe is due.
func (b *Backend) Probe(ctx context.Context, req ProbeRequest) (alsapcm.ProbeReport, error) {
	skip := func(why ProbeSkip, err error) (alsapcm.ProbeReport, error) {
		return notProbed(req.Candidates), &ProbeSkipped{Reason: why, Err: err}
	}
	switch {
	case !req.Conditions.Available:
		return skip(ProbeOutputUnavailable, nil)
	case !req.Conditions.Fresh:
		return skip(ProbeSnapshotStale, nil)
	}
	p, why := b.reserveProbe()
	if p == nil {
		return skip(why, nil)
	}
	type answer struct {
		rep alsapcm.ProbeReport
		err error
	}
	reply := make(chan answer, 1)
	b.spawn(func() {
		a := answer{rep: notProbed(req.Candidates)}
		defer func() {
			if r := recover(); r != nil {
				b.logPanic("the silent probe", r)
				a = answer{rep: notProbed(req.Candidates), err: errors.New("player backend: the silent probe panicked; the bridge's log has why")}
			}
			// alsapcm's probe closes its node on every way out, a panic's
			// included, before the slot is given back.
			b.endProbe(p)
			reply <- a
		}()
		u, err := b.cfg.Locator.Locate(ctx)
		if err != nil {
			a.err = &ProbeSkipped{Reason: ProbeNotLocated, Err: err}
			return
		}
		admit := u.Admit
		if check := req.Check; check != nil {
			admit = func(n alsapcm.NodeInfo) error {
				if u.Admit != nil {
					if err := u.Admit(n); err != nil {
						return err
					}
				}
				return check(n)
			}
		}
		a.rep, a.err = b.cfg.Probe(u.Endpoint, alsapcm.ProbeConfig{Candidates: req.Candidates, Admit: admit})
	})
	select {
	case a := <-reply:
		return a.rep, a.err
	case <-ctx.Done():
		return notProbed(req.Candidates), ctx.Err()
	}
}

// reserveProbe takes the device slot for a probe, or says why it cannot, in
// one step under the backend's lock, the lock hold takes the slot for a
// Start under, so a Start and a probe never both hold it. It refuses a
// release never confirmed, a probe under way, and a session that holds the
// slot: one whose release is pending (its Release has come, which ends its
// life, or the engine has taken it), one paused (a pause holds the slot
// whether or not its release has let the device go), and one that holds the
// device otherwise (it plays, or is opening it); and a Start that found a
// probe holding the slot and has not taken it yet (hold's waiters), which
// is about to hold the device, so it takes the slot as the probe ends and no
// second probe slips in first (round 2). It refuses too while a choice's
// write holds the configuration lease (takeLease, B728 B9 PR 2), which owes
// the probe it would make. The session's state is read without its lock (its
// life, its fence, pausedFlag): a session takes the backend's lock under its
// own, never the reverse.
func (b *Backend) reserveProbe() (*probeHold, ProbeSkip) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.holder
	switch {
	case b.lease != nil:
		return nil, ProbeChoiceWrite
	case b.terminalUnconfirmed:
		return nil, ProbeReleaseUnconfirmed
	case b.probing != nil:
		return nil, ProbeRunning
	case b.waiters > 0:
		return nil, ProbeSessionHolds
	case h != nil && (h.ctx.Err() != nil || h.releaseTaken()):
		return nil, ProbeReleasePending
	case h != nil && h.pausedFlag.Load():
		return nil, ProbeSessionPaused
	case h != nil:
		return nil, ProbeSessionHolds
	}
	p := &probeHold{done: make(chan struct{})}
	b.probing = p
	return p, ""
}

// endProbe gives the slot back, and tells a Start that waits for it.
func (b *Backend) endProbe(p *probeHold) {
	b.mu.Lock()
	if b.probing == p {
		b.probing = nil
	}
	b.mu.Unlock()
	close(p.done)
}

// notProbed is the report of a probe that asked nothing.
func notProbed(cands []alsapcm.ProbeCandidate) alsapcm.ProbeReport {
	rep := alsapcm.ProbeReport{Results: make([]alsapcm.ProbeResult, len(cands))}
	for i, c := range cands {
		rep.Results[i] = alsapcm.ProbeResult{Candidate: c, Answer: alsapcm.ProbeNotProbed}
	}
	return rep
}
