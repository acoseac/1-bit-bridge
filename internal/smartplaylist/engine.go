package smartplaylist

// Generate runs every family builder against the inputs and returns the
// POPULATED ones in homepage order (un-populated families are dropped, which
// is how the "only show populated playlists" contract is enforced end to
// end). The returned slice order is the homepage order; the caller stamps a
// dense Position from the index. Pure + deterministic given the same inputs.
func Generate(in Inputs, opts Options) []GeneratedPlaylist {
	var out []GeneratedPlaylist
	add := func(p GeneratedPlaylist, ok bool) {
		if ok {
			out = append(out, p)
		}
	}
	// Homepage order: the marquee mixes first, then the listening shelves.
	// New families (Drive / On Repeat / Artist Deep Cuts / Lift Off / Wind
	// Down) are interleaved per the plan in
	// ~/.claude/plans/we-now-have-some-playful-sedgewick.md.
	add(buildHeavyRotation(in, opts))
	add(buildDriveMix(in, opts))
	add(buildOnRepeat(in, opts))
	add(buildAutoMix(in, opts))
	add(buildDailyMix(in, opts))
	add(buildTimeOfDay(in, opts))
	add(buildArtistDeepCuts(in, opts))
	add(buildLiftOff(in, opts))
	add(buildWindDown(in, opts))
	add(buildRecentlyPlayed(in, opts))
	add(buildForgotten(in, opts))
	add(buildFinishLine(in, opts))

	// Stamp each family's energy envelope + modal sample rate from its
	// members' analysis. Pure derivation off the already-built families +
	// the feature map; drives the iOS waveform-signed-cover halo.
	feats := unifiedFeatures(in)
	for i := range out {
		out[i].Energy, out[i].ModalRateHz = signEnergy(out[i], feats)
	}
	return out
}

// unifiedFeatures folds the candidate-path feature map and the analyzed pool
// into one path→feature lookup so every family item (whether it came from a
// listening list or the harmonic pool) resolves loudness + sample rate.
func unifiedFeatures(in Inputs) map[string]TrackFeature {
	feats := make(map[string]TrackFeature, len(in.Features)+len(in.AnalyzedPool))
	for p, f := range in.Features {
		feats[p] = f
	}
	for _, f := range in.AnalyzedPool {
		if _, ok := feats[f.Path]; !ok {
			feats[f.Path] = f
		}
	}
	return feats
}

// signEnergy derives a family's normalized energy envelope + modal sample
// rate from its members' loudness/rate. The seed (family slug) keeps the
// anti-flatten micro-noise stable across regenerations.
func signEnergy(p GeneratedPlaylist, feats map[string]TrackFeature) ([]float64, int) {
	paths := familyMemberPaths(p)
	loud := make([]*float64, 0, len(paths))
	rates := make([]int, 0, len(paths))
	for _, path := range paths {
		f, ok := feats[path]
		if !ok {
			loud = append(loud, nil) // unknown → midpoint in EnergyEnvelope
			continue
		}
		loud = append(loud, f.ReplayGainTrackDB)
		if f.SampleRate != nil {
			rates = append(rates, *f.SampleRate)
		}
	}
	return EnergyEnvelope(loud, SeedFromSlug(p.Slug)), ModalRateHz(rates)
}

// familyMemberPaths returns the family's member paths in render order: the
// flat Items for most families, or the per-hour pools (deduped, hour-ordered)
// for the time-of-day family, whose Items is empty.
func familyMemberPaths(p GeneratedPlaylist) []string {
	if len(p.Items) > 0 {
		out := make([]string, 0, len(p.Items))
		for _, it := range p.Items {
			out = append(out, it.Path)
		}
		return out
	}
	if len(p.HourlyItems) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for h := 0; h < 24; h++ { // deterministic hour order
		for _, it := range p.HourlyItems[h] {
			if _, dup := seen[it.Path]; dup {
				continue
			}
			seen[it.Path] = struct{}{}
			out = append(out, it.Path)
		}
	}
	return out
}
