package playback

import (
	"context"
	"sync"
)

// The effective DSD policy's work the supervisor asks for (B762): the silent
// probe (RequestProbeDSD) and the refresh (RequestRefreshDSD), each run on a
// goroutine of the backend's, one at a time. An open or an ioctl the kernel
// holds (a USB DAC stalling in a control request, bounded only by USB's own
// timeouts), a read of the box, and the refreshes' lock such a read holds,
// hold that goroutine alone: the supervisor's loop asks and goes on, so a live
// disable, a new snapshot and the engine's end reach it as they come. The
// asker's context ends what it asked, and a run asked for an ended context is
// not made; each run is counted among the backend's goroutines from the ask on
// (Wait), and the probe's among the writes of the player's DSD state too
// (DSDWrites), since it may record what the DAC accepted.

// coalesced runs the work it is asked for on a goroutine of its own, one at
// a time: work asked while one runs is kept, the latest replacing any kept
// before it, and run once that one has returned, on the same goroutine. A
// panic in the work is recovered and said (recovered), and the work kept
// next still runs.
type coalesced struct {
	mu      sync.Mutex
	running bool
	next    func()
}

// ask keeps work, and starts the goroutine that runs it through spawn,
// which counts it, when none runs: so it is counted before ask returns.
func (c *coalesced) ask(spawn func(func()), recovered func(r any), work func()) {
	c.mu.Lock()
	c.next = work
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	spawn(func() {
		for {
			c.mu.Lock()
			f := c.next
			c.next = nil
			if f == nil {
				c.running = false
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()
			func() {
				defer func() {
					if r := recover(); r != nil {
						recovered(r)
					}
				}()
				f()
			}()
		}
	})
}

// RequestProbeDSD asks for the silent probe of the bound DAC (ProbeDSD) and
// returns at once. One probe runs at a time: an ask while one runs is kept,
// with its conditions, and run once that one has returned, so a DAC
// unplugged and plugged in again while an older enumeration's probe is held
// in the kernel is probed as soon as that probe lets go of the device, where
// it is still bound and the device free (ProbeDSD's conditions, the device
// slot). The probe's answers are taken only while current (publishProbe), and
// the policy's reading they call for is asked of the refresh's goroutine. ctx
// ends the probe as it ends ProbeDSD, its answers dropped; its goroutine
// counts among the writes of the player's DSD state (DSDWrites).
func (b *Backend) RequestProbeDSD(ctx context.Context, cond ProbeConditions) {
	if p := b.cfg.DSD; p == nil || p.Probes == nil {
		return
	}
	spawn := func(f func()) { b.spawnWrite("the silent probe", f) }
	b.probeState.run.ask(spawn, func(r any) { b.logPanic("the silent probe's goroutine", r) }, func() {
		if ctx.Err() != nil {
			return
		}
		if _, refresh := b.probeDSD(ctx, cond); refresh {
			b.RequestRefreshDSD(ctx, true)
		}
	})
}

// ProbeDSDPending reports whether a probe is owed that only the supervisor's
// next ask makes: one the device slot refused, one a choice written for the
// DAC made due (ForgetDSDProbe), one whose answers were dropped
// (publishProbe). A snapshot of the watcher's asks one anyway.
func (b *Backend) ProbeDSDPending() bool {
	b.probeState.mu.Lock()
	defer b.probeState.mu.Unlock()
	return b.probeState.pending
}

// RequestRefreshDSD asks for the supervisor's refresh (RefreshDSD) and
// returns at once. One runs at a time: an ask while one runs is kept, force
// joined with every force asked meanwhile, and run once that one has
// returned, so an input that changed while a reading was held is read again.
// The refreshes publish one at a time, in the order their readings were taken
// (refreshDSD's lock), whichever goroutine reads.
func (b *Backend) RequestRefreshDSD(ctx context.Context, force bool) {
	if b.cfg.DSD == nil {
		return
	}
	if force {
		b.refreshForced.Store(true)
	}
	b.refreshRun.ask(b.spawn, func(r any) { b.logPanic("the effective DSD policy's refresh", r) }, func() {
		if ctx.Err() == nil {
			b.RefreshDSD(b.refreshForced.Swap(false))
		}
	})
}

// DSDWrites is closed once no write of the player's DSD state runs, nor the
// probe's goroutine, which may start one: at once when none does as it is
// asked, and a write started after that is not covered. Serve's player asks
// it once its supervisor, the probe's only asker, has left, and keeps its
// share of the data directory's lock until it is closed.
func (b *Backend) DSDWrites() <-chan struct{} {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return b.wzero
}

// dsdWriteRefused is the line that says a write of the player's DSD state
// came after the writes were closed.
const dsdWriteRefused = "player: a write of the player's DSD state came after the player's stop closed them, and is not made"

// CloseDSDWrites returns DSDWrites.
func (b *Backend) CloseDSDWrites() <-chan struct{} { return b.DSDWrites() }
