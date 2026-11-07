package playback

import (
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TransportConverted is a converted occurrence's transport, as its playback
// observation names it (B728 B11; the design's section 7.1): DSD the player
// converted to PCM, its PCM the plan's. B8 publishes it; until then GET
// /v1/outputs sends no observation of such an occurrence.
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
	if v.State != "revoked" && b.liveOutput != nil && b.liveOutput.sink != nil {
		switch b.liveOutput.sink.Position().State {
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
	admitted := o.admitted
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
		switch out.sink.Position().State {
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
	b.liveOutput = out
	b.liveFacts = &PlaybackFacts{BootID: b.policy.BootID, PolicyGeneration: generation, Session: s.id, Occurrence: o.id, State: state, Format: string(out.transport.Format), RateHz: out.transport.RateHz, LogicalRate: out.transport.LogicalRate, Channels: out.transport.Channels, ValidBits: out.transport.ValidBits, PolicyID: out.policyID,
		Transport: transport, PackingVersion: out.transport.PackingVersion, ObservedAt: time.Now(), QualificationRevision: revision}
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
