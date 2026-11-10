package playback

import (
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// TransportConverted is a converted occurrence's transport, as its playback
// observation names it (B728 B11; the design's section 7.1): DSD the player
// converted to PCM, its PCM the plan's, which GET /v1/outputs publishes (B8).
const TransportConverted = "converted"

// PlaybackFacts describes an installed occurrence, never a capability estimate.
type PlaybackFacts struct {
	BootID                                   string
	PolicyGeneration                         uint64
	Session                                  engine.SessionID
	Occurrence                               engine.OccurrenceID
	State                                    string
	Format                                   string
	RateHz, LogicalRate, Channels, ValidBits int
	PolicyID                                 string
	// Transport is TransportDoP, TransportPCM or TransportConverted, and ""
	// for native DSD.
	Transport string
	// PackingVersion is the DSD packer's version the sink packs with, 0 for
	// PCM.
	PackingVersion int
	// ObservedAt is when the backend last observed this occurrence's sink:
	// its installation or its last change of state.
	ObservedAt time.Time
	// QualificationRevision is the revision of the approved record the
	// occurrence was admitted under, "" for PCM.
	QualificationRevision string
	// Converted is a converted occurrence's plan (B728 B11; the design's
	// sections 4.8 and 7.1), nil for every other: Format, RateHz and
	// ValidBits above are its PCM, LogicalRate 0.
	Converted *ConvertedPlayback
	// Evidence is a native or DoP occurrence's tuple's rung and date as the
	// set it was admitted under computed them (B728 B8), the zero value for
	// PCM and a converted occurrence.
	Evidence DSDEvidence
}

// ConvertedPlayback is a converted occurrence's plan as its observation
// carries it, every claim of it its plan's (dsdconv.Plan): the source's DSD
// rate, the filter, the passband, the dither and the gain, and the samples
// its conversion saturated at full scale so far.
type ConvertedPlayback struct {
	FromBitRate    int
	Filter         string
	PassbandHz     int
	Dither         string
	GainDB         float64
	ClippedSamples int64
}

// Facts returns the published policy and observation under one lock.
func (b *Backend) Facts() (PlayerPolicy, *PlaybackFacts) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	p := b.copyPolicyLocked()
	if b.liveFacts == nil {
		return p, nil
	}
	v := *b.liveFacts
	if c := v.Converted; c != nil {
		cp := *c
		if b.liveClipped != nil {
			cp.ClippedSamples = b.liveClipped.Load()
		}
		v.Converted = &cp
	}
	if v.State != "revoked" && b.liveOutput != nil && b.liveOutput.sink != nil {
		switch b.liveOutput.position().State {
		case alsapcm.SinkReleased, alsapcm.SinkStopped, alsapcm.SinkEnded:
			v.State = "released"
		}
	}
	return p, &v
}
func (s *session) observeOccurrence(o *occ, out *output) {
	b := s.b
	// The set a native or DoP occurrence was admitted under, which a refresh's
	// reconciliation repoints under the session's lock (B728 B7).
	s.mu.Lock()
	admitted, pp := o.admitted, o.policyPlan
	converts, plan, from := o.conv != nil, o.convPlan, o.nativeFormat.BitRateHz
	s.mu.Unlock()
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	generation := b.policy.Generation
	switch {
	case admitted != nil:
		generation = admitted.gen
	case b.cfg.DSD != nil:
		generation = b.dsd.Load().gen
	case o.permission != nil:
		generation = o.permission.generation
	}
	state := "open"
	if out.sink != nil {
		switch out.position().State {
		case alsapcm.SinkReleased, alsapcm.SinkStopped, alsapcm.SinkEnded:
			state = "released"
		}
	}
	transport, revision := TransportPCM, ""
	switch out.transport.Kind {
	case format.KindDoP:
		transport = TransportDoP
	case format.KindNativeDSD:
		transport = ""
	}
	if o.plan != nil && o.permission != nil {
		revision = o.permission.record.Body.Revision
	}
	var converted *ConvertedPlayback
	var evidence DSDEvidence
	if admitted != nil && !converts && (pp.Outcome == policy.OutcomeNative || pp.Outcome == policy.OutcomeDoP) {
		// The rung the set it was admitted under computed for its tuple (B728
		// B8); a converted occurrence plays no native or DoP path, and carries
		// none.
		evidence = DSDEvidence{Rung: string(pp.Evidence), On: pp.EvidenceOn}
	}
	b.liveClipped = nil
	if converts {
		// Its claims are its plan's (B728 B11), read from the plan, never
		// from the 176.4 kHz / 24-bit case: the PCM's format, rate and valid
		// bits are the transport's, which are the plan's.
		transport = TransportConverted
		converted = &ConvertedPlayback{FromBitRate: from, Filter: plan.Filter(), PassbandHz: plan.PassbandHz(), Dither: plan.Dither(), GainDB: plan.GainDB()}
		b.liveClipped = &o.clipped
	}
	b.liveOutput = out
	b.liveFacts = &PlaybackFacts{BootID: b.policy.BootID, PolicyGeneration: generation, Session: s.id, Occurrence: o.id, State: state, Format: string(out.transport.Format), RateHz: out.transport.RateHz, LogicalRate: out.transport.LogicalRate, Channels: out.transport.Channels, ValidBits: out.transport.ValidBits, PolicyID: out.policyID,
		Transport: transport, PackingVersion: out.transport.PackingVersion, ObservedAt: time.Now(), QualificationRevision: revision, Converted: converted, Evidence: evidence}
}
func (b *Backend) observeNote(n Note) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if f := b.liveFacts; f != nil && f.Session == n.Session && b.liveOutput == n.output && f.State != "revoked" {
		switch n.Kind {
		case NoteClosed, NoteReleased:
			f.State = "released"
			f.ObservedAt = time.Now()
		case NoteReopened:
			f.State = "open"
			f.ObservedAt = time.Now()
		}
	}
}

func (s *session) installed(o *occ, out *output, changedOutput bool) {
	s.observeOccurrence(o, out)
	if observe := s.b.cfg.Installed; observe != nil {
		var plan *engine.NativePlan
		if o.plan != nil {
			cp := *o.plan
			cp.Edges = slices.Clone(cp.Edges)
			plan = &cp
		}
		observe(Note{Plan: plan, Session: s.id, Occurrence: o.id, output: out, Kind: NoteInstalled, Transport: out.transport, Params: out.sink.Params()})
	}
	if ev, ok := s.ev.(engine.CurrentOccurrenceEvents); ok && changedOutput {
		ev.CurrentOccurrence(s.id, o.id, o.plan)
	}
}
