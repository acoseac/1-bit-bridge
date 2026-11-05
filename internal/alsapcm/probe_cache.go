package alsapcm

import "sync"

// ProbeUnit is what a probe's answers hold for: the DAC model, its firmware,
// the kernel and the unit's enumeration. An answer is kept under exactly the
// unit it was given for, and a lookup under any other finds none, so a
// change of any of the four lapses it (B728's design, sections 1.2 and 5): a
// bcdDevice change, a new kernel, a replug or a reset is probed again.
type ProbeUnit struct {
	// Model is the DAC model's identity, the canonical form of its key
	// (dacprofile.Key.Canonical: the vendor and product IDs, the interface
	// and the product string), never a serial, a port, a card number or a
	// device number.
	Model string
	// Firmware is the unit's bcdDevice, four lowercase hexadecimal digits.
	Firmware string
	// Kernel is the running kernel's release (uname -r).
	Kernel string
	// Enumeration tells this enumeration of the unit from every other: a
	// replug, a reset or a firmware update enumerates it anew. B7 sets it
	// from the hotplug feed, its instance and the generation at which the
	// binding last found the unit at a new enumeration. It is never
	// persisted.
	Enumeration string
}

// ProbeCache keeps the silent probe's answers, in memory and for the
// process alone: the player's DSD state (B3's durable evidence) holds
// neither refusals nor conversion candidates, and an enumeration means
// nothing after a restart. An answer kept is accepted, with its significant
// bits and its pause flag, or refused (EINVAL); a candidate it keeps no
// answer for under exactly the unit asked is not probed. Nothing in it is a
// rung by itself: B7 reads it per tuple, a native tuple by its format at the
// frame rate and a DoP tuple by its carrier (S32_LE at the rate over 16,
// ProbeResult.DoPCarrier). An accepted answer is that tuple's
// driver_accepted, which B7 records in the DSD state's evidence with the
// kernel release (driverAcceptedAt, kernel); a refused one takes the tuple
// out of what is offered; not probed leaves the tuple to the stream file's
// altsets. It is safe for concurrent use.
type ProbeCache struct {
	mu    sync.Mutex
	units map[ProbeUnit]map[ProbeCandidate]ProbeResult
}

// NewProbeCache is an empty cache.
func NewProbeCache() *ProbeCache {
	return &ProbeCache{units: map[ProbeUnit]map[ProbeCandidate]ProbeResult{}}
}

// ProbeEpoch is a probe's place in its model's history (Begin).
type ProbeEpoch struct {
	unit  ProbeUnit
	epoch uint64
}

// Begin is what a caller asks before it probes u, and hands Record after.
func (c *ProbeCache) Begin(u ProbeUnit) ProbeEpoch { return ProbeEpoch{unit: u} }

// Record keeps what rep established for u: each accepted and each refused
// answer, replacing one kept for the candidate before. A not-probed answer
// keeps nothing and changes nothing, so a busy node (EBUSY) is never kept as
// a refusal and leaves an earlier answer for u standing. Whatever was kept
// for u's model under another firmware, kernel or enumeration has lapsed,
// and is dropped.
func (c *ProbeCache) Record(e ProbeEpoch, rep ProbeReport) bool {
	u := e.unit
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.units {
		if k.Model == u.Model && k != u {
			delete(c.units, k)
		}
	}
	for _, r := range rep.Results {
		if r.Answer != ProbeAccepted && r.Answer != ProbeRefused {
			continue
		}
		kept := c.units[u]
		if kept == nil {
			kept = map[ProbeCandidate]ProbeResult{}
			c.units[u] = kept
		}
		if r.Answer == ProbeRefused {
			r.Msbits, r.Pause = 0, false
		}
		kept[r.Candidate] = r
	}
	return true
}

// Answer is the answer kept for cand under exactly u, or ProbeNotProbed.
func (c *ProbeCache) Answer(u ProbeUnit, cand ProbeCandidate) ProbeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.units[u][cand]; ok {
		return r
	}
	return ProbeResult{Candidate: cand, Answer: ProbeNotProbed}
}

// Forget drops every answer kept for model, under any unit: a choice written
// for that DAC (the design's section 7.2) probes it again where it is free,
// and where it is not leaves it not probed rather than answered from before
// the write.
func (c *ProbeCache) Forget(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.units {
		if k.Model == model {
			delete(c.units, k)
		}
	}
}
