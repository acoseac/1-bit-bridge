package playback

import (
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// The transports a published tuple and a playback observation name (B470
// 2d.2): native DSD, the default, is named by none.
const (
	// TransportDoP is DSD over PCM, a DoP plan's transport: the effective
	// DSD policy's (B728 B7), or an approved DoP policy's in the harness.
	TransportDoP = "dop"
	// TransportPCM is a PCM occurrence's, which a playback observation
	// names; a tuple is never PCM.
	TransportPCM = "pcm"
)

// TupleTransport is how a published tuple names its transport:
// TransportDoP for DoP, "" for native DSD however its record spells it.
func TupleTransport(t engine.NativeTuple) string {
	if t.Transport == TransportDoP {
		return TransportDoP
	}
	return ""
}

// EdgeKey is a transport boundary's key, as an approval's Edges name it:
// qualification.NativeKey's for a plan, the PCM key for a PCM transport.
// The qualification harness's evidence recorder keys the edges it passes
// with it.
func EdgeKey(t alsapcm.Transport, p *engine.NativePlan) string { return edgeKey(t, p) }

func edgeKey(t alsapcm.Transport, p *engine.NativePlan) string {
	if p != nil {
		kind := p.Tuple.Transport
		if kind == "" {
			kind = "native"
		}
		return fmt.Sprintf("%s/%s/%d/2/msb/%s/%s/%d/%d/30", kind, p.Tuple.Format, p.Tuple.BitRate, p.PolicyID, p.Pause, p.Tail.Milliseconds(), p.Prefill.Milliseconds())
	}
	return fmt.Sprintf("pcm/%s/%d/%d/%d/zero", t.Format, t.RateHz, t.Channels, t.ValidBits)
}
func (b *Backend) checkEdge(t alsapcm.Transport, p *engine.NativePlan, permission *permission) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if b.lastEdge == "" {
		return nil
	}
	// Edges gate the harness's approval admission alone (a plan admitted with
	// a permission, the outgoing one remembered with its own): production's
	// effective DSD policy (B728 B7) admits with no permission, and needs no
	// edge, since no transition between transports needs a report in
	// production, as the tests' inert admissions need none.
	if p == nil || (permission == nil && b.lastPlan == nil) {
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
