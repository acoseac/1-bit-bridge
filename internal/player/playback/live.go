package playback

import (
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"slices"
)

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
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	generation := b.policy.Generation
	if o.permission != nil {
		generation = o.permission.generation
	}
	state := "open"
	if out.sink != nil {
		switch out.sink.Position().State {
		case alsapcm.SinkReleased, alsapcm.SinkStopped, alsapcm.SinkEnded:
			state = "released"
		}
	}
	b.liveOutput = out
	b.liveFacts = &PlaybackFacts{BootID: b.policy.BootID, PolicyGeneration: generation, Session: s.id, Occurrence: o.id, State: state, Format: string(out.transport.Format), RateHz: out.transport.RateHz, LogicalRate: out.transport.LogicalRate, Channels: out.transport.Channels, ValidBits: out.transport.ValidBits, PolicyID: out.policyID}
}
func (b *Backend) observeNote(n Note) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if f := b.liveFacts; f != nil && f.Session == n.Session && b.liveOutput == n.output && f.State != "revoked" {
		switch n.Kind {
		case NoteClosed, NoteReleased:
			f.State = "released"
		case NoteReopened:
			f.State = "open"
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
