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
// nothing after a restart. For each DAC model it keeps the answers of one
// unit, the one a probe was last begun for, each accepted, with its
// significant bits and its pause flag, or refused (EINVAL); a candidate it
// keeps no answer for under exactly the unit asked is not probed. Each model
// has an epoch, which a choice written for it (Forget) and a probe begun for
// another of its units (Begin) move, and a report is kept only in the epoch
// its probe was begun in, so nothing from before either ever lands after it.
// Nothing in it is a rung by itself: B7 reads it per tuple, a native tuple by
// its format at the frame rate and a DoP tuple by its carrier (S32_LE at the
// rate over 16, ProbeResult.DoPCarrier). An accepted answer is that tuple's
// driver_accepted, which B7 records in the DSD state's evidence with the
// kernel release (driverAcceptedAt, kernel); a refused one takes the tuple
// out of what is offered; not probed leaves the tuple to the stream file's
// altsets. It is safe for concurrent use.
type ProbeCache struct {
	mu     sync.Mutex
	models map[string]*probeModel
}

// probeModel is what the cache keeps for one DAC model: the unit its answers
// are for, the answers, and the model's epoch.
type probeModel struct {
	unit    ProbeUnit
	epoch   uint64
	answers map[ProbeCandidate]ProbeResult
}

// NewProbeCache is an empty cache.
func NewProbeCache() *ProbeCache {
	return &ProbeCache{models: map[string]*probeModel{}}
}

// ProbeEpoch is a probe's place in its model's history: the unit the probe
// was begun for, and the model's epoch when it was (Begin). Record keeps a
// report only while its epoch is still the model's.
type ProbeEpoch struct {
	unit  ProbeUnit
	epoch uint64
}

// Begin is what a caller asks before it probes u, and hands Record after:
// the model's epoch now. A probe begun for another unit of the model than
// the one its answers are kept for (another firmware, kernel or enumeration:
// a firmware update, a new kernel, a replug or a reset) lapses those answers
// at once and moves the epoch, so a probe of the older unit that ends later,
// in either order with the newer one's, keeps nothing and drops nothing: an
// older enumeration's late report never displaces a newer one's answers.
func (c *ProbeCache) Begin(u ProbeUnit) ProbeEpoch {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.models[u.Model]
	if m == nil {
		m = &probeModel{}
		c.models[u.Model] = m
	}
	if m.epoch == 0 || m.unit != u {
		m.unit, m.answers = u, nil
		m.epoch++
	}
	return ProbeEpoch{unit: u, epoch: m.epoch}
}

// Record keeps what rep established, under e's unit, while e is still its
// model's epoch: each accepted and each refused answer, replacing one kept
// for the candidate before. A choice written for the model since (Forget),
// or a probe begun since for another of its units (Begin), moved the epoch:
// the report then predates them, and nothing of it is kept and nothing kept
// is dropped. A not-probed answer keeps and changes nothing, so a busy node
// (EBUSY), or a run beside another of the card's substreams
// (ProbeReport.Shared), is never kept as a refusal and leaves an earlier
// answer standing. It reports whether the report was taken.
func (c *ProbeCache) Record(e ProbeEpoch, rep ProbeReport) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.models[e.unit.Model]
	if m == nil || m.epoch != e.epoch {
		return false
	}
	for _, r := range rep.Results {
		if r.Answer != ProbeAccepted && r.Answer != ProbeRefused {
			continue
		}
		if m.answers == nil {
			m.answers = map[ProbeCandidate]ProbeResult{}
		}
		if r.Answer == ProbeRefused {
			r.Msbits, r.Pause = 0, false
		}
		m.answers[r.Candidate] = r
	}
	return true
}

// Answer is the answer kept for cand under exactly u, or ProbeNotProbed.
func (c *ProbeCache) Answer(u ProbeUnit, cand ProbeCandidate) ProbeResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.models[u.Model]; m != nil && m.unit == u {
		if r, ok := m.answers[cand]; ok {
			return r
		}
	}
	return ProbeResult{Candidate: cand, Answer: ProbeNotProbed}
}

// Forget drops every answer kept for model and moves its epoch: a choice
// written for that DAC (the design's section 7.2), which probes it again
// where it is free and where it is not leaves it not probed rather than
// answered from before the write. A probe begun before the write keeps
// nothing when it ends; the write's own, begun after it, keeps its answers.
func (c *ProbeCache) Forget(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.models[model]; m != nil {
		m.answers = nil
		m.epoch++
	}
}
