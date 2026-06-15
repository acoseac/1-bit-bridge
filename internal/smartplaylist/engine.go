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
	add(buildHeavyRotation(in, opts))
	add(buildAutoMix(in, opts))
	add(buildDailyMix(in, opts))
	add(buildTimeOfDay(in, opts))
	add(buildRecentlyPlayed(in, opts))
	add(buildForgotten(in, opts))
	add(buildFinishLine(in, opts))
	return out
}
