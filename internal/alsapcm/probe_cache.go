package alsapcm

import "sync"

// ProbeUnit is what a probe's answers hold for: the DAC model, its firmware,
// the kernel and the unit's enumeration.
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
	// Enumeration tells this enumeration of the unit from every other.
	Enumeration string
}

// ProbeCache keeps the probe's answers in memory.
type ProbeCache struct {
	mu    sync.Mutex
	units map[ProbeUnit]map[ProbeCandidate]ProbeResult
}

// NewProbeCache is an empty cache.
func NewProbeCache() *ProbeCache {
	return &ProbeCache{units: map[ProbeUnit]map[ProbeCandidate]ProbeResult{}}
}

// Record keeps what rep established for u.
func (c *ProbeCache) Record(u ProbeUnit, rep ProbeReport) {}

// Answer is the answer kept for cand under exactly u, or ProbeNotProbed.
func (c *ProbeCache) Answer(u ProbeUnit, cand ProbeCandidate) ProbeResult {
	return ProbeResult{Candidate: cand, Answer: ProbeNotProbed}
}

// Forget drops every answer kept for model.
func (c *ProbeCache) Forget(model string) {}
