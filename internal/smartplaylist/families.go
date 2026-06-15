package smartplaylist

import (
	"fmt"
	"math"
	"sort"
)

// --- shared item helpers ---

func pathsOf(stats []PlayStat) []string {
	out := make([]string, len(stats))
	for i, s := range stats {
		out[i] = s.Path
	}
	return out
}

// itemsFromPaths hydrates paths into items via the feature map, dropping
// paths that no longer resolve (a since-deleted track), capped at maxItems.
func itemsFromPaths(paths []string, features map[string]TrackFeature, maxItems int) []Item {
	var items []Item
	for _, p := range paths {
		f, ok := features[p]
		if !ok {
			continue
		}
		items = append(items, Item{Position: len(items), Path: p, Title: f.Title, Artist: f.Artist})
		if len(items) >= maxItems {
			break
		}
	}
	return items
}

func itemsFromFeatures(feats []TrackFeature, maxItems int) []Item {
	var items []Item
	for _, f := range feats {
		items = append(items, Item{Position: len(items), Path: f.Path, Title: f.Title, Artist: f.Artist})
		if len(items) >= maxItems {
			break
		}
	}
	return items
}

// --- listening families ---

func buildHeavyRotation(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	items := itemsFromPaths(pathsOf(in.HeavyRotation), in.Features, opts.MaxItems)
	if len(items) < opts.MinHeavyRotation {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "heavy-rotation", Kind: KindHeavyRotation,
		Title: "Heavy Rotation", Subtitle: "Your most-played lately",
		Items: items,
	}, true
}

func buildRecentlyPlayed(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	items := itemsFromPaths(pathsOf(in.Recent), in.Features, opts.MaxItems)
	if len(items) < opts.MinRecentlyPlayed {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "recently-played", Kind: KindRecentlyPlayed,
		Title: "Recently Played", Subtitle: "Pick up where you left off",
		Items: items,
	}, true
}

func buildForgotten(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	items := itemsFromPaths(pathsOf(in.Forgotten), in.Features, opts.MaxItems)
	if len(items) < opts.MinForgotten {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "forgotten-favorites", Kind: KindForgottenFavorites,
		Title: "Forgotten Favorites", Subtitle: "Loved before, missing lately",
		Items: items,
	}, true
}

// --- Auto Mix (harmonic) ---

func buildAutoMix(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	if !opts.AnalysisEnabled || len(in.AnalyzedPool) < opts.MinAutoMixPool {
		return GeneratedPlaylist{}, false
	}
	seed := pickHarmonicSeed(in)
	seq := sequenceHarmonic(seed, in.AnalyzedPool, opts.MaxItems)
	if len(seq) < 5 {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "auto-mix", Kind: KindAutoMix,
		Title: "Auto Mix", Subtitle: "A harmonic flow through your library",
		Items: itemsFromFeatures(seq, opts.MaxItems),
	}, true
}

// pickHarmonicSeed prefers the top heavy-rotation track that has a key (so the
// mix starts from a favourite); falls back to the first analyzed-pool track.
func pickHarmonicSeed(in Inputs) TrackFeature {
	for _, s := range in.HeavyRotation {
		if f, ok := in.Features[s.Path]; ok && f.KeyRoot != nil {
			return f
		}
	}
	if len(in.AnalyzedPool) > 0 {
		return in.AnalyzedPool[0]
	}
	return TrackFeature{}
}

// --- Time of Day ---

func buildTimeOfDay(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	if len(in.HourBuckets) == 0 {
		return GeneratedPlaylist{}, false
	}
	type pp struct {
		path  string
		plays int
	}
	byHour := map[int][]pp{}
	hourTotal := map[int]int{}
	for _, hb := range in.HourBuckets {
		byHour[hb.Hour] = append(byHour[hb.Hour], pp{hb.Path, hb.Plays})
		hourTotal[hb.Hour] += hb.Plays
	}
	// Gate: the busiest hour must carry enough signal to be meaningful. The
	// API picks the device's local hour at request time; an empty target hour
	// is omitted there.
	busiest := 0
	for _, t := range hourTotal {
		if t > busiest {
			busiest = t
		}
	}
	if busiest < opts.MinTimeOfDayPlays {
		return GeneratedPlaylist{}, false
	}

	hourly := map[int][]Item{}
	for h, lst := range byHour {
		sort.SliceStable(lst, func(i, j int) bool {
			if lst[i].plays != lst[j].plays {
				return lst[i].plays > lst[j].plays
			}
			return lst[i].path < lst[j].path // deterministic tie-break
		})
		var items []Item
		for _, e := range lst {
			f, ok := in.Features[e.path]
			if !ok {
				continue
			}
			items = append(items, Item{Position: len(items), Path: e.path, Title: f.Title, Artist: f.Artist})
			if len(items) >= opts.MaxItems {
				break
			}
		}
		if len(items) > 0 {
			hourly[h] = items
		}
	}
	if len(hourly) == 0 {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "time-of-day", Kind: KindTimeOfDay,
		// The API retitles by the device's local window (Morning/Evening/…).
		Title: "For Right Now", Subtitle: "What you usually play around this time",
		HourlyItems: hourly,
	}, true
}

// --- Daily Mix ---

func buildDailyMix(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	var familiar []TrackFeature
	for _, s := range in.Familiar {
		if f, ok := in.Features[s.Path]; ok {
			familiar = append(familiar, f)
		}
	}
	if len(familiar) < opts.MinDailyFamiliar {
		return GeneratedPlaylist{}, false
	}

	// Discovery is analysis-gated for v1 (ranked from the key-bearing pool).
	var discovery []TrackFeature
	if opts.AnalysisEnabled {
		discovery = discoveryCandidates(familiar, in.AnalyzedPool, in.PlayedPaths)
	}

	target := opts.MaxItems
	nDisc := int(math.Round(float64(target) * opts.DailyDiscoveryRatio))
	if nDisc > len(discovery) {
		nDisc = len(discovery)
	}
	nFam := target - nDisc
	if nFam > len(familiar) {
		nFam = len(familiar)
	}
	merged := interleave(familiar[:nFam], discovery[:nDisc])
	return GeneratedPlaylist{
		Slug: "daily-mix", Kind: KindDailyMix,
		Title: "Daily Mix", Subtitle: "Favorites and a few you might've missed",
		Items: itemsFromFeatures(merged, target),
	}, true
}

// discoveryCandidates ranks unplayed key-bearing tracks by closeness to the
// familiar set's centroid (dominant genre + median BPM). Lower score first;
// deterministic (path-stable input order + stable sort).
func discoveryCandidates(familiar, pool []TrackFeature, played map[string]bool) []TrackFeature {
	domGenre := dominantGenre(familiar)
	medBPM := medianBPM(familiar)
	var cands []TrackFeature
	for _, f := range pool {
		if played[f.Path] {
			continue
		}
		cands = append(cands, f)
	}
	sort.SliceStable(cands, func(i, j int) bool {
		si, sj := discoveryScore(cands[i], domGenre, medBPM), discoveryScore(cands[j], domGenre, medBPM)
		if si != sj {
			return si < sj
		}
		return cands[i].Path < cands[j].Path
	})
	return cands
}

func discoveryScore(f TrackFeature, domGenre string, medBPM float64) float64 {
	score := 0.0
	if domGenre != "" && f.Genre != domGenre {
		score += 100 // genre mismatch penalised, but not excluded
	}
	if medBPM > 0 && f.BPM != nil {
		score += math.Abs(float64(*f.BPM) - medBPM)
	}
	return score
}

// interleave distributes discovery tracks evenly through the familiar set
// (familiar-heavy, matching the ~70/30 ratio the caller sized the slices to).
func interleave(familiar, discovery []TrackFeature) []TrackFeature {
	total := len(familiar) + len(discovery)
	if total == 0 {
		return nil
	}
	out := make([]TrackFeature, 0, total)
	fi, di := 0, 0
	for i := 0; i < total; i++ {
		// Place a discovery track when we're "behind" the even ratio.
		wantDisc := di*total < i*len(discovery)
		switch {
		case wantDisc && di < len(discovery):
			out = append(out, discovery[di])
			di++
		case fi < len(familiar):
			out = append(out, familiar[fi])
			fi++
		case di < len(discovery):
			out = append(out, discovery[di])
			di++
		}
	}
	return out
}

func dominantGenre(feats []TrackFeature) string {
	counts := map[string]int{}
	for _, f := range feats {
		if f.Genre != "" {
			counts[f.Genre]++
		}
	}
	best, bestN := "", 0
	for g, n := range counts {
		if n > bestN || (n == bestN && (best == "" || g < best)) {
			best, bestN = g, n
		}
	}
	return best
}

func medianBPM(feats []TrackFeature) float64 {
	var bpms []float64
	for _, f := range feats {
		if f.BPM != nil && *f.BPM > 0 {
			bpms = append(bpms, float64(*f.BPM))
		}
	}
	if len(bpms) == 0 {
		return 0
	}
	sort.Float64s(bpms)
	return bpms[len(bpms)/2]
}

// --- The Finish Line ---

func buildFinishLine(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	avg, sessions := averageSessionDuration(in.Events, opts.SessionGapSeconds)
	if sessions < opts.MinSessions || avg <= 0 {
		return GeneratedPlaylist{}, false
	}
	// Candidate pool: recent then heavy favourites with a known duration,
	// deduped, order-preserving.
	seen := map[string]bool{}
	var feats []TrackFeature
	for _, p := range append(pathsOf(in.Recent), pathsOf(in.HeavyRotation)...) {
		if seen[p] {
			continue
		}
		f, ok := in.Features[p]
		if !ok || f.Duration <= 0 {
			continue
		}
		seen[p] = true
		feats = append(feats, f)
	}
	// Greedy chain toward the average session length.
	var chosen []TrackFeature
	var sum float64
	for _, f := range feats {
		if sum >= avg || len(chosen) >= opts.MaxItems {
			break
		}
		chosen = append(chosen, f)
		sum += f.Duration
	}
	if len(chosen) < 3 || sum < avg*0.8 {
		return GeneratedPlaylist{}, false
	}
	mins := int(math.Round(avg / 60))
	return GeneratedPlaylist{
		Slug: "finish-line", Kind: KindFinishLine,
		Title:    "The Finish Line",
		Subtitle: fmt.Sprintf("About %d min — your usual session", mins),
		Items:    itemsFromFeatures(chosen, opts.MaxItems),
	}, true
}

// averageSessionDuration segments chronological events into listening
// sessions — a gap from the previous event's END to the next event's START
// greater than gapSeconds starts a new session — and returns the mean
// session length (seconds) and the session count.
func averageSessionDuration(events []Event, gapSeconds float64) (avg float64, sessions int) {
	if len(events) == 0 {
		return 0, 0
	}
	gapNS := int64(gapSeconds * 1e9)
	var total, cur float64
	sessions = 1
	cur = events[0].DurationUsed
	prevEnd := events[0].StartedAt + int64(events[0].DurationUsed*1e9)
	for i := 1; i < len(events); i++ {
		e := events[i]
		if e.StartedAt-prevEnd > gapNS {
			total += cur
			cur = 0
			sessions++
		}
		cur += e.DurationUsed
		prevEnd = e.StartedAt + int64(e.DurationUsed*1e9)
	}
	total += cur
	return total / float64(sessions), sessions
}
