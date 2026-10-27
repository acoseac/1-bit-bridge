package playback

import "github.com/acoseac/1-bit-bridge/internal/player/engine"

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
	b.liveFacts = &PlaybackFacts{BootID: b.policy.BootID, PolicyGeneration: generation, Session: s.id, Occurrence: o.id, State: "open", Format: string(out.transport.Format), RateHz: out.transport.RateHz, LogicalRate: out.transport.LogicalRate, Channels: out.transport.Channels, ValidBits: out.transport.ValidBits, PolicyID: out.policyID}
}
func (b *Backend) observeNote(n Note) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if f := b.liveFacts; f != nil && f.Session == n.Session && f.State != "revoked" {
		switch n.Kind {
		case NoteClosed, NoteReleased:
			f.State = "released"
		case NoteReopened:
			f.State = "open"
		}
	}
}
