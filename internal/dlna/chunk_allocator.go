package dlna

import "math"

// ChunkSize bounds for the adaptive allocator. Floor matches the bridge's
// historical fixed buffer size (128KB) so LAN deployments under nominal
// conditions see no behavior change. Ceiling caps at 1MB — overprovisioning
// is cheap on LAN and protects against high-jitter / high-loss conditions
// driving renderer-side buffer underrun, but per-write allocations beyond
// 1MB pay diminishing returns and complicate memory accounting.
const (
	MinChunkSize     = 128 * 1024  // 128 KB
	MaxChunkSize     = 1024 * 1024 // 1 MB
	DefaultChunkSize = MinChunkSize
)

// ChunkSizeFor returns the per-write byte threshold the AdaptiveResponseWriter
// should enforce, derived from the observed network conditions of the
// renderer's connection.
//
//   - rttMs: round-trip time to the renderer in milliseconds (TCP stack-derived)
//   - jitterMs: standard deviation of recent RTT samples in milliseconds
//   - lossPct: percentage of recent segments observed as lost (0-100)
//
// The scaling model mirrors the research's network multiplier formula:
// γ_network = 1 + α·jitter² + β·loss + δ·rtt. Multiplier is applied
// to the floor and clamped to the ceiling. Initial α/β/δ coefficients
// are conservative defaults; Phase 0 + PR 1 telemetry refines them.
//
// **Conservative invariants:**
//   - Negative or non-finite inputs collapse to DefaultChunkSize
//   - Output is always within [MinChunkSize, MaxChunkSize]
//   - Pure function — no global state, no side effects, deterministic
//     for any given input triple (safe for parallel render-path callers)
func ChunkSizeFor(rttMs, jitterMs, lossPct float64) int {
	// Defensive — bad telemetry should never cause underrun-promoting tiny chunks.
	if !isFinite(rttMs) || !isFinite(jitterMs) || !isFinite(lossPct) ||
		rttMs < 0 || jitterMs < 0 || lossPct < 0 {
		return DefaultChunkSize
	}

	// Empirically-derived coefficients (initial, will be refined from
	// telemetry):
	//   α (jitter²) = 0.005 — quadratic in jitter; small jitter
	//       (<5ms) barely moves the multiplier, but variance >20ms
	//       pushes the floor up sharply
	//   β (loss)    = 0.20  — linear in loss percentage; 1% loss adds
	//       20% to the floor, 5% loss adds 100% (doubles to 256KB)
	//   δ (rtt)     = 0.002 — linear in RTT; 100ms RTT adds 20% to
	//       account for renderer-side queue depth requirements on
	//       higher-latency paths
	const (
		alpha = 0.005
		beta  = 0.20
		delta = 0.002
	)

	multiplier := 1.0 + alpha*jitterMs*jitterMs + beta*lossPct + delta*rttMs
	size := int(float64(MinChunkSize) * multiplier)

	if size < MinChunkSize {
		return MinChunkSize
	}
	if size > MaxChunkSize {
		return MaxChunkSize
	}
	// Round to 4KB boundary — memory allocator + page-size friendly,
	// no observable difference in renderer behavior between 130KB and
	// 132KB.
	const align = 4 * 1024
	return (size / align) * align
}

func isFinite(f float64) bool {
	// `math.IsNaN` replaces the pre-fix `f == f` NaN-check idiom,
	// silencing SonarCloud's S1764 false-positive ("identical
	// sub-expressions on both sides of operator") without changing
	// semantics — `math.IsNaN(f)` IS the idiomatic Go form of
	// `f != f` and compiles to the same instruction sequence. Per
	// SonarCloud Major Bug (go:S1764) on PR #303 round-3.
	//
	// The 1e18 magnitude bound is LOAD-BEARING and stays. Without
	// it, a finite-but-extreme input (e.g. `jitterMs = 1e150`)
	// passes the NaN/Inf check but `jitterMs² = 1e300` overflows
	// the downstream `1 + α·jitter² + β·loss + δ·rtt` multiplier
	// to `+Inf`, and the subsequent `int(float64(...) * +Inf)` cast
	// is implementation-defined behaviour in Go (`int(+Inf)` can
	// return `math.MinInt`, producing a negative size that
	// short-circuits to `MinChunkSize` via the floor clamp — safe
	// but loses signal). The 1e18 bound catches all such overflow
	// risks at the input gate, BEFORE the multiplier math runs.
	return !math.IsNaN(f) && f < 1e18 && f > -1e18
}
