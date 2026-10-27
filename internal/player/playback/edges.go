package playback

import (
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

func edgeKey(t alsapcm.Transport, p *engine.NativePlan) string {
	if p != nil {
		return fmt.Sprintf("native/%s/%d/2/msb/%s/%s/%d/%d/30", p.Tuple.Format, p.Tuple.BitRate, p.PolicyID, p.Pause, p.Tail.Milliseconds(), p.Prefill.Milliseconds())
	}
	return fmt.Sprintf("pcm/%s/%d/%d/%d/zero", t.Format, t.RateHz, t.Channels, t.ValidBits)
}
func (b *Backend) checkEdge(t alsapcm.Transport, p *engine.NativePlan, permission *permission) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if b.lastEdge == "" {
		return nil
	}
	// Inert test admissions predate owner approval. Production's only admission
	// carries a permission, including when the outgoing occurrence was DSD.
	if permission == nil && b.lastPlan == nil {
		return nil
	}
	to := edgeKey(t, p)
	edges := []engine.TransitionEdge(nil)
	if p != nil {
		edges = p.Edges
	} else if b.lastPlan != nil {
		edges = b.lastPlan.Edges
	}
	for _, e := range edges {
		if e.From == b.lastEdge && e.To == to {
			return nil
		}
	}
	return errors.New("DSD transition edge is not owner-approved")
}
func (b *Backend) rememberEdge(t alsapcm.Transport, p *engine.NativePlan, permission *permission) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.lastEdge = edgeKey(t, p)
	b.lastPlan = nil
	if p != nil && permission != nil {
		cp := *p
		b.lastPlan = &cp
	}
}
