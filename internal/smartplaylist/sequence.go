package smartplaylist

import "math"

// normalizedBPMDistance is the smallest tempo gap between two tracks,
// accounting for half/double-time equivalence (75 ↔ 150 BPM is a perfect
// match). Returns 0 when either tempo is unknown (an unknown tempo must not
// penalise an otherwise-harmonic transition). Per the Gemini consult
// (2026-06-14): min(|a−b|, |2a−b|, |a/2−b|).
func normalizedBPMDistance(a, b float64) float64 {
	if a <= 0 || b <= 0 {
		return 0
	}
	d := math.Abs(a - b)
	d2 := math.Abs(a*2 - b)
	dh := math.Abs(a/2 - b)
	return math.Min(d, math.Min(d2, dh))
}

func bpmOf(f TrackFeature) float64 {
	if f.BPM == nil {
		return 0
	}
	return float64(*f.BPM)
}

// keyCostWeight makes harmonic compatibility the PRIMARY sort key and BPM
// proximity the tie-break: one cost tier (≈ one wheel step) outweighs any
// realistic BPM gap.
const keyCostWeight = 1000.0

// harmonicCand pairs a track with its resolved wheel position.
type harmonicCand struct {
	f TrackFeature
	c Camelot
}

// sequenceHarmonic greedily orders pool into a smooth harmonic flow starting
// from seed: at each step it picks the unused track with the lowest combined
// cost (keyCost·weight + BPM distance), preferring compatible keys and close
// tempos. Incompatible keys (cost 3) are skipped in the primary pass; if the
// compatible frontier is exhausted before reaching max, it makes ONE
// nearest-BPM "reset" jump and continues. Fully deterministic given a stable
// pool (final tie-break by path), so the daily Auto Mix doesn't reshuffle.
func sequenceHarmonic(seed TrackFeature, pool []TrackFeature, max int) []TrackFeature {
	// Keep only key-bearing tracks (others can't be harmonically sequenced).
	var cands []harmonicCand
	for _, f := range pool {
		if f.KeyRoot == nil {
			continue
		}
		c, ok := toCamelot(*f.KeyRoot, f.KeyMode)
		if !ok {
			continue
		}
		cands = append(cands, harmonicCand{f, c})
	}
	if len(cands) == 0 {
		return nil
	}

	// Resolve the seed's position; fall back to the first candidate.
	seedC, ok := Camelot{}, false
	if seed.KeyRoot != nil {
		seedC, ok = toCamelot(*seed.KeyRoot, seed.KeyMode)
	}
	if !ok {
		seed = cands[0].f
		seedC = cands[0].c
	}

	used := map[string]bool{seed.Path: true}
	result := []TrackFeature{seed}
	prev, prevC := seed, seedC

	for len(result) < max {
		bestIdx := -1
		bestCost := math.MaxFloat64
		for i := range cands {
			if used[cands[i].f.Path] {
				continue
			}
			kc := compatibilityCost(prevC, cands[i].c)
			if kc >= 3 {
				continue // incompatible — skip in the primary pass
			}
			cost := float64(kc)*keyCostWeight + normalizedBPMDistance(bpmOf(prev), bpmOf(cands[i].f))
			if cost < bestCost || (cost == bestCost && bestIdx >= 0 && cands[i].f.Path < cands[bestIdx].f.Path) {
				bestCost = cost
				bestIdx = i
			}
		}
		if bestIdx == -1 {
			// Compatible frontier exhausted — one reset jump to the
			// nearest-BPM unused track keeps the mix going.
			bestIdx = nearestUnusedByBPM(cands, used, prev)
			if bestIdx == -1 {
				break
			}
		}
		next := cands[bestIdx]
		result = append(result, next.f)
		used[next.f.Path] = true
		prev, prevC = next.f, next.c
	}
	return result
}

// nearestUnusedByBPM returns the index of the unused candidate with the
// smallest tempo gap to prev (path tie-break), or -1 if none remain.
func nearestUnusedByBPM(cands []harmonicCand, used map[string]bool, prev TrackFeature) int {
	bestIdx := -1
	bestD := math.MaxFloat64
	for i := range cands {
		if used[cands[i].f.Path] {
			continue
		}
		d := normalizedBPMDistance(bpmOf(prev), bpmOf(cands[i].f))
		if d < bestD || (d == bestD && bestIdx >= 0 && cands[i].f.Path < cands[bestIdx].f.Path) {
			bestD = d
			bestIdx = i
		}
	}
	return bestIdx
}
