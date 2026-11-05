package alsapcm

import (
	"errors"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The silent probe (B728's design, section 5) asks the kernel whether the
// driver of an endpoint takes each of a list of combinations, and commits
// none: it opens the node as every open does, runs PVERSION and INFO, hands
// the node to the check after open, asks HW_REFINE of each candidate, and
// closes the node. It sends no HW_PARAMS, SW_PARAMS or PREPARE, writes
// nothing, writes no control and sends no HW_FREE, since nothing was set.
// HW_REFINE is computation in the kernel and selects no altset; the open can
// still wake a device from USB autosuspend, which is why a probe runs only
// at the moments the design names and never on a timer. What an answer
// establishes is the driver's acceptance (driver_accepted), never that the
// DAC decodes anything: for DoP, only that the kernel takes the PCM carrier.

// ProbeAnswer is the kernel's answer to one candidate of a probe.
type ProbeAnswer string

const (
	// ProbeNotProbed is a candidate the kernel was not asked about, or
	// answered with neither of the two: the node busy (EBUSY, another
	// process holding it, MPD most often), missing, closed to this process
	// or refused by the check after open, the probe ended before it, or a
	// refine that failed with any errno but EINVAL. It says nothing of the
	// candidate, and is never a refusal.
	ProbeNotProbed ProbeAnswer = "not_probed"
	// ProbeAccepted is a candidate HW_REFINE took: the driver accepts that
	// access, format, subformat, channel count and rate on the endpoint,
	// given the device's descriptors and the kernel's quirks.
	ProbeAccepted ProbeAnswer = "accepted"
	// ProbeRefused is a candidate HW_REFINE refused with EINVAL: a parameter
	// had no value left.
	ProbeRefused ProbeAnswer = "refused"
)

// ProbeCandidate is one combination a probe asks about: Format at RateHz
// frames a second, in two channels, with interleaved read-write access and
// the standard subformat, as an open asks them (exactParams). Format is a
// PCM format this backend packs or a native DSD format.
type ProbeCandidate struct {
	Format format.ALSAFormat
	RateHz int
}

func (c ProbeCandidate) String() string { return fmt.Sprintf("%s at %d Hz", c.Format, c.RateHz) }

// ProbeResult is the kernel's answer to one candidate.
type ProbeResult struct {
	Candidate ProbeCandidate
	Answer    ProbeAnswer
	// Msbits is the significant bits of a sample in an accepted candidate's
	// refined setup, the kernel's msbits: 0 when the kernel reported none,
	// which is unknown, never the format's width.
	Msbits int
	// Pause is an accepted candidate's refined setup offering the kernel's
	// PAUSE (SNDRV_PCM_INFO_PAUSE): its tuple pauses with PAUSE where it is
	// set, and with silence where it is not (the design's section 7.1).
	Pause bool
}

// DoPCarrier reports whether r establishes a DoP carrier: S32_LE accepted
// with 24 to 32 significant bits, what the DoP sink requires of its open
// (dopBits). It says only that the kernel takes the PCM carrier; nothing in
// it says the DAC decodes DSD.
func (r ProbeResult) DoPCarrier() bool {
	return false
}

// MaxProbeCandidates bounds a probe: one open, PVERSION, INFO, one HW_REFINE
// a candidate, and a close.
const MaxProbeCandidates = 64

// ProbeConfig is what Probe asks.
type ProbeConfig struct {
	// Candidates are asked in order, each once: one at least, at most
	// MaxProbeCandidates, none repeated.
	Candidates []ProbeCandidate
	// Admit, when set, is the check after open (SinkConfig.Admit's): it is
	// given what the kernel says the opened node is, before any candidate is
	// asked, and a refusal closes the node with nothing asked of it
	// (ReasonWrongDevice), so no answer is ever given for a unit that took
	// the endpoint of the one the binding named.
	Admit func(NodeInfo) error
}

// ProbeReport is what one probe established.
type ProbeReport struct {
	Endpoint Endpoint
	// Opened reports whether the probe opened the node, and so closed it,
	// once: false when it was refused before the open or at it, a busy node
	// among them.
	Opened bool
	// Node is what the kernel said the opened node is (INFO), zero when it
	// was not opened or not read.
	Node NodeInfo
	// Results are the candidates' answers, in the order asked: every one
	// ProbeNotProbed when the probe asked nothing.
	Results []ProbeResult
}

// Result is c's answer in r, ProbeNotProbed when r does not hold it.
func (r ProbeReport) Result(c ProbeCandidate) ProbeResult {
	for _, x := range r.Results {
		if x.Candidate == c {
			return x
		}
	}
	return ProbeResult{Candidate: c, Answer: ProbeNotProbed}
}

// notAsked is the report of a probe that asked nothing of ep.
func notAsked(ep Endpoint, cfg ProbeConfig) ProbeReport {
	rep := ProbeReport{Endpoint: ep, Results: make([]ProbeResult, len(cfg.Candidates))}
	for i, c := range cfg.Candidates {
		rep.Results[i] = ProbeResult{Candidate: c, Answer: ProbeNotProbed}
	}
	return rep
}

// ProbeCandidates is the design's section 5 set for DSD64, DSD128 and
// DSD256 (DSD512 is refused in every mode, and never probed).
func ProbeCandidates() []ProbeCandidate {
	return nil
}

// errProbeNotBuilt stands for the probe until it is built.
var errProbeNotBuilt = errors.New("alsapcm: the silent probe is not built yet")

// probe is Probe over a kernel seam.
func probe(_ sys, ep Endpoint, cfg ProbeConfig) (ProbeReport, error) {
	return notAsked(ep, cfg), errProbeNotBuilt
}
