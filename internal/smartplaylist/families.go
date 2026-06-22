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

// --- Drive Mix (CarPlay-only Heavy Rotation) ---

func buildDriveMix(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	cap := opts.MaxDriveMixItems
	if cap <= 0 {
		cap = opts.MaxItems
	}
	items := itemsFromPaths(pathsOf(in.Drive), in.Features, cap)
	if len(items) < opts.MinDriveMix {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "drive-mix", Kind: KindDriveMix,
		Title: "Drive Mix", Subtitle: "Your road favorites",
		Items: items,
	}, true
}

// --- On Repeat (per-day repeat behaviour, with hysteresis) ---

// onRepeatSlug is the cache-cached slug for the On Repeat family AND the key
// `Inputs.PreviouslyVisible` looks up when applying the hysteresis floor. The
// bridge persists by slug (StoredSmartPlaylist.Slug) so the regenerator's
// "was visible last run" map is naturally keyed by this kebab-case string —
// NOT the camelCase wire `Kind`. Don't fork: route every read through this
// constant so a future rename can't desync.
const onRepeatSlug = "on-repeat"

func buildOnRepeat(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	cap := opts.MaxOnRepeatItems
	if cap <= 0 {
		cap = opts.MaxItems
	}
	items := itemsFromPaths(pathsOf(in.OnRepeat), in.Features, cap)

	enterFloor := opts.OnRepeatEnterFloor
	if enterFloor <= 0 {
		enterFloor = 12
	}
	exitFloor := opts.OnRepeatExitFloor
	if exitFloor <= 0 {
		exitFloor = 8
	}
	if exitFloor > enterFloor {
		exitFloor = enterFloor
	}

	qualifies := len(items) >= enterFloor ||
		(in.PreviouslyVisible[onRepeatSlug] && len(items) >= exitFloor)
	if !qualifies {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: onRepeatSlug, Kind: KindOnRepeat,
		Title: "On Repeat", Subtitle: "Tracks you couldn't stop playing",
		Items: items,
	}, true
}

// --- From Artists You Love (per-artist-capped library deep cuts) ---

func buildArtistDeepCuts(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	// The manifest query already balanced per-artist via ROW_NUMBER PARTITION
	// + ordered (artist, rn). Re-order via a deterministic weekly shuffle so
	// the visible window rotates Monday-to-Monday without churning daily.
	paths := shufflePathsByWeek(pathsOf(in.ArtistDeepCuts), in.WeekSeed)

	cap := opts.MaxArtistDeepCutsItems
	if cap <= 0 {
		cap = opts.MaxItems
	}
	items := itemsFromPaths(paths, in.Features, cap)
	if len(items) < opts.MinArtistDeepCuts {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "artist-deep-cuts", Kind: KindArtistDeepCuts,
		Title: "From Artists You Love", Subtitle: "Hidden gems by artists in your rotation",
		Items: items,
	}, true
}

// --- Mood bands (Wind Down / Lift Off) ---

// Per-family seed offsets so Wind Down and Lift Off shuffle independently of
// each other (otherwise both families with overlapping tracks would carry the
// same relative ordering). Pure constants — XORed with WeekSeed before hashing.
const (
	moodSeedOffsetWindDown uint64 = 0x57494e44_5f44574e // "WIND_DWN"
	moodSeedOffsetLiftOff  uint64 = 0x4c494654_5f4f4646 // "LIFT_OFF"
)

func buildWindDown(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	if len(in.QuietSlowPool) < opts.MinMoodBand {
		return GeneratedPlaylist{}, false
	}
	shuffled := shuffleFeaturesByWeek(in.QuietSlowPool, in.WeekSeed^moodSeedOffsetWindDown)
	cap := opts.MaxMoodBandItems
	if cap <= 0 {
		cap = opts.MaxItems
	}
	items := itemsFromFeatures(shuffled, cap)
	if len(items) < opts.MinMoodBand {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "wind-down", Kind: KindWindDown,
		Title: "Wind Down", Subtitle: "Calm music for slow moments",
		Items: items,
	}, true
}

func buildLiftOff(in Inputs, opts Options) (GeneratedPlaylist, bool) {
	if len(in.LoudFastPool) < opts.MinMoodBand {
		return GeneratedPlaylist{}, false
	}
	shuffled := shuffleFeaturesByWeek(in.LoudFastPool, in.WeekSeed^moodSeedOffsetLiftOff)
	cap := opts.MaxMoodBandItems
	if cap <= 0 {
		cap = opts.MaxItems
	}
	items := itemsFromFeatures(shuffled, cap)
	if len(items) < opts.MinMoodBand {
		return GeneratedPlaylist{}, false
	}
	return GeneratedPlaylist{
		Slug: "lift-off", Kind: KindLiftOff,
		Title: "Lift Off", Subtitle: "Tracks to get moving",
		Items: items,
	}, true
}

// --- shared deterministic shuffle helpers ---

// shufflePathsByWeek returns a stable shuffle of paths keyed by the (seed,
// path) pair so the order is stable for a given seed and rotates when the
// seed changes (the regenerator computes the seed from the UTC ISO week).
// Pure; same input always returns the same output. Path tie-break preserves
// determinism when two paths hash to the same score.
func shufflePathsByWeek(paths []string, seed uint64) []string {
	type scored struct {
		index int
		score uint64
	}
	arr := make([]scored, len(paths))
	for i, p := range paths {
		arr[i] = scored{index: i, score: hashSeedPath(seed, p)}
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].score != arr[j].score {
			return arr[i].score < arr[j].score
		}
		return paths[arr[i].index] < paths[arr[j].index]
	})
	out := make([]string, len(arr))
	for i, s := range arr {
		out[i] = paths[s.index]
	}
	return out
}

// shuffleFeaturesByWeek is the TrackFeature-typed analogue used by the mood
// bands' Feature pool flow. Sorts by INDEX into the pool (16 bytes per scored
// element) rather than carrying the 120-byte TrackFeature through the swap
// loop — meaningful for the mood-band pools, which can run to several thousand
// rows per regeneration (Gemini bot review on PR #431).
func shuffleFeaturesByWeek(pool []TrackFeature, seed uint64) []TrackFeature {
	type scored struct {
		index int
		score uint64
	}
	arr := make([]scored, len(pool))
	for i, f := range pool {
		arr[i] = scored{index: i, score: hashSeedPath(seed, f.Path)}
	}
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].score != arr[j].score {
			return arr[i].score < arr[j].score
		}
		return pool[arr[i].index].Path < pool[arr[j].index].Path
	})
	out := make([]TrackFeature, len(arr))
	for i, s := range arr {
		out[i] = pool[s.index]
	}
	return out
}

// hashSeedPath produces a 64-bit shuffle score from (seed, path) via INLINED
// FNV-1a (RFC-aligned constants — offset basis 14695981039346656037, prime
// 1099511628211). Inlined rather than `fnv.New64a()`-per-call because the
// hot path runs this up to 50_000× per mood-band regeneration and the per-
// call hasher allocation was visible GC pressure (Gemini bot review on PR
// #431). Functionally identical to the prior `hash/fnv` implementation.
func hashSeedPath(seed uint64, path string) uint64 {
	const (
		fnvOffsetBasis uint64 = 14695981039346656037
		fnvPrime       uint64 = 1099511628211
	)
	hash := fnvOffsetBasis
	var buf [8]byte
	putUint64(buf[:], seed)
	for _, b := range buf {
		hash ^= uint64(b)
		hash *= fnvPrime
	}
	for i := 0; i < len(path); i++ {
		hash ^= uint64(path[i])
		hash *= fnvPrime
	}
	return hash
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
