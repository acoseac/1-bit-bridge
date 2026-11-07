package playback

import "context"

// RequestProbeDSD asks for the silent probe of the bound DAC (ProbeDSD).
func (b *Backend) RequestProbeDSD(ctx context.Context, cond ProbeConditions) {
	pending := b.ProbeDSD(ctx, cond)
	b.probeState.mu.Lock()
	b.probeState.pending = pending
	b.probeState.mu.Unlock()
}

// ProbeDSDPending reports whether a probe is owed.
func (b *Backend) ProbeDSDPending() bool {
	b.probeState.mu.Lock()
	defer b.probeState.mu.Unlock()
	return b.probeState.pending
}

// RequestRefreshDSD asks for the supervisor's refresh (RefreshDSD).
func (b *Backend) RequestRefreshDSD(_ context.Context, force bool) { b.RefreshDSD(force) }

// DSDWrites is closed once no write of the player's DSD state runs.
func (b *Backend) DSDWrites() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
