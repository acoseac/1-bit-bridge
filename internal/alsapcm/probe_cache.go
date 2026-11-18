package alsapcm

import "sync"

// ProbeUnit is what a probe's answers hold for: the DAC model, its maker,
// its firmware, the kernel and the unit's enumeration. An answer is kept
// under exactly the unit it was given for, and a lookup under any other finds
// none, so a change of any of the five lapses it (B728's design, sections 1.2
// and 5): another maker's unit on the model's key, a bcdDevice change, a new
// kernel, a replug or a reset is probed again.
type ProbeUnit struct {
	// Model is the DAC model's identity, the canonical form of its key
	// (dacprofile.Key.Canonical: the vendor and product IDs, the interface
	// and the product string), never a serial, a port, a card number or a
	// device number.
	Model string
	// Stamp is the unit's maker, the stamp of its manufacturer string the
	// player's DSD state keys an entry by beside the model's key
	// (dsdstate.StampOf), which the key does not hold: a unit of another
	// maker on the model's key is another model's here.
	Stamp string
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
// has an epoch, which the migration's part for it (Forget) and a probe begun
// for another of its units (Begin) move, and a report is kept only in the
// epoch its probe was begun in, so nothing from before either ever lands after
// it. A choice written for the model moves nothing (B728 B9): the answers are
// the kernel's and the descriptors', which a choice does not change.
// Nothing in it is a rung by itself: B7 reads it per tuple, a native tuple by
// its format at the frame rate and a DoP tuple by its carrier (S32_LE at the
// rate over 16, ProbeResult.DoPCarrier). An accepted answer is that tuple's
// driver_accepted, which B7 records in the DSD state's evidence with the
// kernel release (driverAcceptedAt, kernel); a refused one takes the tuple
// out of what is offered; not probed leaves the tuple to the stream file's
// altsets. It is safe for concurrent use.
type ProbeCache struct {
	mu     sync.Mutex
	models map[probeModelKey]*probeModel
}

// probeModelKey is a DAC model of one maker, as the cache keeps its answers:
// a unit's Model and Stamp.
type probeModelKey struct{ model, stamp string }

// probeModel is what the cache keeps for one DAC model: the unit its answers
// are for, the answers, and the model's epoch.
type probeModel struct {
	unit    ProbeUnit
	epoch   uint64
	answers map[ProbeCandidate]ProbeResult
}

// NewProbeCache is an empty cache.
func NewProbeCache() *ProbeCache {
	return &ProbeCache{models: map[probeModelKey]*probeModel{}}
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
	m := c.models[probeModelKey{u.Model, u.Stamp}]
	if m == nil {
		m = &probeModel{}
		c.models[probeModelKey{u.Model, u.Stamp}] = m
	}
	if m.epoch == 0 || m.unit != u {
		m.unit, m.answers = u, nil
		m.epoch++
	}
	return ProbeEpoch{unit: u, epoch: m.epoch}
}

// Record keeps what rep established, under e's unit, while e is still its
// model's epoch: each accepted and each refused answer, replacing one kept
// for the candidate before. The migration's part for the model since
// (Forget), or a probe begun since for another of its units (Begin), moved the
// epoch: the report then predates them, and nothing of it is kept and nothing kept
// is dropped. A not-probed answer keeps and changes nothing, so a busy node
// (EBUSY), or a run beside another of the card's substreams
// (ProbeReport.Shared), is never kept as a refusal and leaves an earlier
// answer standing. It reports whether the report was taken.
func (c *ProbeCache) Record(e ProbeEpoch, rep ProbeReport) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.models[probeModelKey{e.unit.Model, e.unit.Stamp}]
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
	if m := c.models[probeModelKey{u.Model, u.Stamp}]; m != nil && m.unit == u {
		if r, ok := m.answers[cand]; ok {
			return r
		}
	}
	return ProbeResult{Candidate: cand, Answer: ProbeNotProbed}
}

// Forget drops every answer kept for model of stamp's maker and moves its
// epoch: the migration's part for that DAC (playback.Backend.ForgetDSDProbe),
// which probes it again where it is free and where it is not leaves it not
// probed rather than answered from before the part. A probe begun before it
// keeps nothing when it ends; the one begun after it keeps its answers.
// Another maker's answers for the model's key are left as they are. A choice
// written for the DAC (B728 B9) forgets nothing: it makes a probe due.
func (c *ProbeCache) Forget(model, stamp string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.models[probeModelKey{model, stamp}]; m != nil {
		m.answers = nil
		m.epoch++
	}
}
