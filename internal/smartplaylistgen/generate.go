// Package smartplaylistgen orchestrates smart-playlist regeneration: it
// assembles the pure engine's Inputs from manifest.Store aggregations, runs
// internal/smartplaylist.Generate, and writes the populated families to the
// `smart_playlists` cache. It is the single regeneration entry point shared
// by the daily background ticker (cmd/bridge) and the admin "Regenerate now"
// button — keeping the engine pure (no store) and the store free of
// generation logic.
package smartplaylistgen

import (
	"context"
	"encoding/json"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
)

// logger records a discovery shelf that dropped the already-shown exclusion
// so it could keep its minimum. One line per family per regeneration.
var logger = logging.Component("smartplaylist")

// discoveryServedDays is how many UTC days a path a discovery shelf served
// stays excluded: the days before the run's own, never the run's own day.
// A build later that day (the settle run after a restart, "Regenerate now",
// a per-family regenerate) then sees the inputs the day's first build saw
// and builds the same shelves. Rows older than the window are pruned when
// the cache is built.
const discoveryServedDays = 7

// heavyRotationCandidateLimit is how many play-ranked rows Heavy Rotation
// reads before the diversity caps run. The display cap is Engine.MaxItems
// (50). Reading only that many lets one artist fill the window; the caps
// then keep a handful, which is under the floor, and the mix falls back to
// that uncapped prefix. The query's own ceiling is 5000. Reading the
// ceiling gives the caps a pool that can fill the shelf from every artist
// who met the play floor. The fallback stays for a pool that really is one
// artist.
const heavyRotationCandidateLimit = 5000

// Options carries the query windows + thresholds + the injected clock. NowNS
// is supplied by the caller (the ticker passes time.Now; tests pin it) so
// this package itself stays clock-free and deterministic.
type Options struct {
	AnalysisEnabled bool
	NowNS           int64 // UnixNano "now" for all window math

	MinPlaySeconds        float64       // the "30s rule" applied in the queries
	HeavyWindow           time.Duration // Heavy Rotation lookback (14d)
	HeavyRotationMinPlays int           // Heavy Rotation preferred play-count floor (3), degrades to 1
	FamiliarWindow        time.Duration // Daily Mix "familiar" lookback (60d)
	ForgottenNotSince     time.Duration // last play older than this = forgotten (30d)
	ForgottenMinPlays     int           // historic plays to count as a favorite (3)
	HourWindow            time.Duration // time-of-day bucket lookback (90d)
	SessionWindow         time.Duration // Finish Line events lookback (90d)
	PoolLimit             int           // analyzed-pool cap (5000)

	// Drive Mix lookback (CarPlay-only plays). 60d default — wider than the
	// 14d Heavy Rotation window so commutes that don't include daily-driver
	// listening still populate the family.
	DriveWindow time.Duration

	// On Repeat window — only days inside this look-back contribute to the
	// repeat-day count.
	OnRepeatWindow time.Duration
	// OnRepeat thresholds (passed through to the manifest query).
	OnRepeatMinTotalPlays int
	OnRepeatMinRepeatDays int

	// From Artists You Love — "loved" cohort window + "deep cut" exclusion
	// window. A loved artist is one with ≥ LovedMinPlays qualifying plays in
	// [now - LovedWindow, now); a deep cut is a track by such an artist with
	// NO qualifying play in [now - DeepCutsCutoff, now).
	LovedWindow             time.Duration
	LovedMinPlays           int
	DeepCutsCutoff          time.Duration
	ArtistDeepCutsPerArtist int
	// Candidate pool size handed to LovedArtistDeepCuts, applied AFTER the
	// week-seeded hash (3 per artist, then this many rows). Must stay above
	// the display cap so the weekly shuffle has room inside the window.
	// Sized to ~10× that cap.
	ArtistDeepCutsPoolLimit int

	// Mood band BPM + ReplayGain thresholds. Wind Down: bpm ≤ WindDownBPMMax
	// AND replaygain > WindDownLoudnessMin (less-negative = quieter). Lift
	// Off: bpm ≥ LiftOffBPMMin AND replaygain ≤ LiftOffLoudnessMax (more-
	// negative = louder). Mid-band gap is deliberate.
	WindDownBPMMax      int
	WindDownLoudnessMin float64
	LiftOffBPMMin       int
	LiftOffLoudnessMax  float64

	Engine smartplaylist.Options
}

// DefaultOptions returns the production windows + thresholds.
func DefaultOptions(nowNS int64, analysisEnabled bool) Options {
	const day = 24 * time.Hour
	return Options{
		AnalysisEnabled:       analysisEnabled,
		NowNS:                 nowNS,
		MinPlaySeconds:        30,
		HeavyWindow:           14 * day,
		HeavyRotationMinPlays: 3,
		FamiliarWindow:        60 * day,
		ForgottenNotSince:     30 * day,
		ForgottenMinPlays:     3,
		HourWindow:            90 * day,
		SessionWindow:         90 * day,
		PoolLimit:             5000,
		// Drive Mix: 60d CarPlay-only window (Heavy Rotation pattern).
		DriveWindow: 60 * day,
		// On Repeat: 30d window. ≥ 4 total plays AND ≥ 3 distinct days with
		// ≥ 2 plays each = "sustained obsession" (per the plan).
		OnRepeatWindow:        30 * day,
		OnRepeatMinTotalPlays: 4,
		OnRepeatMinRepeatDays: 3,
		// From Artists You Love: 30d loved cohort window, 90d deep-cut
		// exclusion. ≥ 3 plays in window = loved artist; per-artist cap 3.
		LovedWindow:             30 * day,
		LovedMinPlays:           3,
		DeepCutsCutoff:          90 * day,
		ArtistDeepCutsPerArtist: 3,
		ArtistDeepCutsPoolLimit: 200,
		// Mood bands — see CLAUDE.md / plan: less-negative ReplayGain = quieter
		// master; more-negative = louder. Mid-band gap (-8, -6] is deliberate.
		WindDownBPMMax:      90,
		WindDownLoudnessMin: -6.0,
		LiftOffBPMMin:       120,
		LiftOffLoudnessMax:  -8.0,
		Engine:              smartplaylist.DefaultOptions(analysisEnabled),
	}
}

// Regenerate assembles inputs, runs the generator, and replaces the cache,
// then replaces the day's served rows of each discovery shelf it built.
// Returns the number of populated families written.
func Regenerate(ctx context.Context, store *manifest.Store, opts Options) (int, error) {
	in, err := assembleInputs(ctx, store, opts)
	if err != nil {
		return 0, err
	}
	gen := smartplaylist.Generate(in, opts.Engine)
	logExclusionRelaxations(gen)
	snapshot, err := toStored(gen, opts.NowNS)
	if err != nil {
		return 0, err
	}
	if err := store.ReplaceSmartPlaylists(ctx, snapshot); err != nil {
		return 0, err
	}
	if err := recordDiscoveryServed(ctx, store, gen, opts.NowNS); err != nil {
		return 0, err
	}
	return len(snapshot), nil
}

// RegenerateFamily rebuilds ONE family and commits only its cache row,
// leaving every other cached family untouched — the per-family "Regenerate"
// admin affordance. The full engine still runs (the families share the same
// assembled inputs, so the dominant cost — the history/analysis queries — is
// identical either way); only the requested slug's output is written. A slug
// the fresh run no longer populates is REMOVED from the cache, matching the
// wholesale path where an empty family simply isn't written — a mix whose
// source signal dried up must not linger stale forever. Of the served
// rows, only the slug's own for the day are replaced, and only when the
// slug is a discovery shelf the run built.
//
// Returns generated (the slug is in the fresh engine output and was
// written), existed (a cached row was present before the call), and the
// fresh item count (0 when !generated).
func RegenerateFamily(ctx context.Context, store *manifest.Store, opts Options, slug string) (generated, existed bool, itemCount int, err error) {
	in, err := assembleInputs(ctx, store, opts)
	if err != nil {
		return false, false, 0, err
	}
	gen := smartplaylist.Generate(in, opts.Engine)
	var fresh *manifest.StoredSmartPlaylist
	var served []smartplaylist.GeneratedPlaylist
	for i := range gen {
		if gen[i].Slug != slug {
			continue
		}
		// toStored's Position comes from the slice index and is ignored —
		// ReplaceSmartPlaylistFamily preserves the cached slot (or appends).
		rows, serr := toStored(gen[i:i+1], opts.NowNS)
		if serr != nil {
			return false, false, 0, serr
		}
		fresh = &rows[0]
		itemCount = generatedItemCount(gen[i])
		served = gen[i : i+1]
		break
	}
	existed, err = store.ReplaceSmartPlaylistFamily(ctx, slug, fresh)
	if err != nil {
		return false, existed, 0, err
	}
	logExclusionRelaxations(served)
	if err := recordDiscoveryServed(ctx, store, served, opts.NowNS); err != nil {
		return fresh != nil, existed, itemCount, err
	}
	return fresh != nil, existed, itemCount, nil
}

// generatedItemCount reports a generated family's display size: distinct
// track paths across the hourly pools for time-of-day, the flat item count
// otherwise (mirrors the admin's smartPlaylistItemCount, computed here from
// the engine output instead of the serialized blob).
func generatedItemCount(g smartplaylist.GeneratedPlaylist) int {
	if g.Kind == smartplaylist.KindTimeOfDay {
		seen := map[string]struct{}{}
		for _, items := range g.HourlyItems {
			for _, it := range items {
				seen[it.Path] = struct{}{}
			}
		}
		return len(seen)
	}
	return len(g.Items)
}

func assembleInputs(ctx context.Context, store *manifest.Store, opts Options) (smartplaylist.Inputs, error) {
	now := opts.NowNS
	eng := opts.Engine
	// Week seed — UTC ISO week, so the candidate windows and the
	// deterministic weekly shuffles rotate on Monday-UTC and stay stable
	// across a 7-day window. Computed up front: the analyzed pool and the
	// deep-cut query order by it.
	yr, wk := time.Unix(0, opts.NowNS).UTC().ISOWeek()
	weekSeed := smartplaylist.SeedFromISOWeek(yr, wk)

	// Heavy Rotation: prefer a ≥HeavyRotationMinPlays floor so one-time and
	// twice-played tracks don't flood the mix, but DEGRADE the floor (…→2→1)
	// if that would starve the mix below MinHeavyRotation during a quiet week.
	// SQLite is in-process (zero network) so a few sequential queries are
	// cleaner + more testable than a single windowed-rank query (both review
	// rounds preferred the Go loop). floor==1 is a no-op HAVING → the full
	// pool, so the family never hides purely because of the floor.
	var heavy []manifest.PlayStatRow
	// `max(1, …)` so a config of 0 / negative doesn't make the `floor >= 1`
	// loop skip entirely and starve the mix (Gemini on PR #497); floor 1 is a
	// no-op HAVING = the full pool.
	for floor := max(1, opts.HeavyRotationMinPlays); floor >= 1; floor-- {
		var qerr error
		heavy, qerr = store.PlayStatsInWindow(ctx, now-opts.HeavyWindow.Nanoseconds(), 0, opts.MinPlaySeconds, floor, heavyRotationCandidateLimit)
		if qerr != nil {
			return smartplaylist.Inputs{}, qerr
		}
		if len(heavy) >= eng.MinHeavyRotation {
			break
		}
	}
	// familiar: unfiltered pool (minPlays 0) — the Daily Mix family keeps
	// single-play tracks by design.
	familiar, err := store.PlayStatsInWindow(ctx, now-opts.FamiliarWindow.Nanoseconds(), 0, opts.MinPlaySeconds, 0, eng.MaxItems*2)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	forgotten, err := store.PlayStatsForgotten(ctx, opts.MinPlaySeconds, now-opts.ForgottenNotSince.Nanoseconds(), opts.ForgottenMinPlays, eng.MaxItems)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	recent, err := store.RecentDistinctPlays(ctx, opts.MinPlaySeconds, eng.MaxItems)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	hourBuckets, err := store.PlayCountsByHourPath(ctx, now-opts.HourWindow.Nanoseconds(), opts.MinPlaySeconds, nil)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	events, err := store.OrderedPlayEvents(ctx, now-opts.SessionWindow.Nanoseconds(), opts.MinPlaySeconds, 0)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	played, err := store.AllPlayedPaths(ctx, opts.MinPlaySeconds)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}

	// Drive Mix: CarPlay-only heavy rotation.
	drive, err := store.PlayStatsByInterfaceInWindow(ctx, now-opts.DriveWindow.Nanoseconds(), "CarPlay", opts.MinPlaySeconds, eng.MaxItems)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}

	// On Repeat: two-level per-day repeat aggregation.
	onRepeat, err := store.OnRepeatCandidates(ctx,
		now-opts.OnRepeatWindow.Nanoseconds(),
		opts.MinPlaySeconds,
		opts.OnRepeatMinTotalPlays,
		opts.OnRepeatMinRepeatDays,
		eng.MaxItems,
	)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}

	// From Artists You Love: three tracks per loved artist, then a pool of
	// ArtistDeepCutsPoolLimit (200). Both cuts order by the week seed, so
	// the pool is not the alphabetical first 200 and not each artist's
	// lowest paths. The pool stays larger than the display cap so the
	// weekly shuffle still has room inside the window (Gemini + CodeRabbit
	// on PR #431).
	deepCuts, err := store.LovedArtistDeepCuts(ctx,
		now-opts.LovedWindow.Nanoseconds(),
		opts.LovedMinPlays,
		now-opts.DeepCutsCutoff.Nanoseconds(),
		opts.MinPlaySeconds,
		opts.ArtistDeepCutsPerArtist,
		opts.ArtistDeepCutsPoolLimit,
		weekSeed,
	)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}

	// Mood bands. Gated on AnalysisEnabled — without analysis the BPM /
	// loudness signals aren't populated, so the bands would always be empty.
	var quietPool, loudPool []manifest.TrackFeatureRow
	if opts.AnalysisEnabled {
		if quietPool, err = store.QuietSlowTrackFeatures(ctx, opts.WindDownBPMMax, opts.WindDownLoudnessMin); err != nil {
			return smartplaylist.Inputs{}, err
		}
		if loudPool, err = store.LoudFastTrackFeatures(ctx, opts.LiftOffBPMMin, opts.LiftOffLoudnessMax); err != nil {
			return smartplaylist.Inputs{}, err
		}
	}

	// Favorites: the bridge-local hearted tracks (F4 favorites backup).
	// NOT gated on AnalysisEnabled — the pool hydrates from tags_json
	// regardless; analysis only upgrades the family's ordering to the
	// harmonic arm inside the builder.
	favRows, err := store.FavoritedTrackFeatures(ctx)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}

	// Hysteresis: load the prior cache and build the "was visible last
	// regeneration" map. The regenerator only writes populated families, so
	// presence in the cache (slug exists) = visible last run. Hysteresis-
	// capable builders (On Repeat) read this through Inputs.PreviouslyVisible.
	priorCache, err := store.LoadSmartPlaylists(ctx)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	previouslyVisible := make(map[string]bool, len(priorCache))
	for _, p := range priorCache {
		previouslyVisible[p.Slug] = true
	}

	var pool []manifest.TrackFeatureRow
	if opts.AnalysisEnabled {
		if pool, err = store.AnalyzedTrackFeatures(ctx, "", weekSeed, opts.PoolLimit); err != nil {
			return smartplaylist.Inputs{}, err
		}
	}

	// The days before this run's own, in UTC like the week seed. What this
	// day already served excludes nothing, so a second build today gets
	// the inputs the first one got.
	servedSet, err := store.SmartPlaylistServedOnEarlierDays(ctx, now, discoveryServedDays)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	servedRecently := make(map[string]bool, len(servedSet))
	for p := range servedSet {
		servedRecently[p] = true
	}

	// Hydrate features for every candidate path (listening lists + hour
	// buckets + the new families' paths), then fold in the analyzed pool's
	// own features.
	pathSet := map[string]struct{}{}
	for _, lst := range [][]manifest.PlayStatRow{heavy, familiar, forgotten, recent, drive, onRepeat, deepCuts} {
		for _, s := range lst {
			pathSet[s.Path] = struct{}{}
		}
	}
	for _, hb := range hourBuckets {
		pathSet[hb.Path] = struct{}{}
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	featRows, err := store.TrackFeaturesForPaths(ctx, paths)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	features := make(map[string]smartplaylist.TrackFeature, len(featRows)+len(pool)+len(quietPool)+len(loudPool))
	for _, r := range featRows {
		features[r.Path] = toFeature(r)
	}
	poolFeats := make([]smartplaylist.TrackFeature, 0, len(pool))
	for _, r := range pool {
		f := toFeature(r)
		poolFeats = append(poolFeats, f)
		if _, ok := features[r.Path]; !ok {
			features[r.Path] = f
		}
	}
	// Mood-band rows come back fully hydrated; map them and add to Features
	// (so signEnergy's post-pass can find their loudness when stamping the
	// halo envelope on the mood-band families).
	quietPoolFeats := make([]smartplaylist.TrackFeature, 0, len(quietPool))
	for _, r := range quietPool {
		f := toFeature(r)
		quietPoolFeats = append(quietPoolFeats, f)
		if _, ok := features[r.Path]; !ok {
			features[r.Path] = f
		}
	}
	loudPoolFeats := make([]smartplaylist.TrackFeature, 0, len(loudPool))
	for _, r := range loudPool {
		f := toFeature(r)
		loudPoolFeats = append(loudPoolFeats, f)
		if _, ok := features[r.Path]; !ok {
			features[r.Path] = f
		}
	}
	// Favorites rows come back fully hydrated; map them and add to Features
	// (the mood-band pattern — signEnergy resolves loudness through
	// Inputs.Features when stamping the family's halo envelope).
	favFeats := make([]smartplaylist.TrackFeature, 0, len(favRows))
	for _, r := range favRows {
		f := toFeature(r)
		favFeats = append(favFeats, f)
		if _, ok := features[r.Path]; !ok {
			features[r.Path] = f
		}
	}

	return smartplaylist.Inputs{
		HeavyRotation:     toStats(heavy),
		Familiar:          toStats(familiar),
		Forgotten:         toStats(forgotten),
		Recent:            toStats(recent),
		HourBuckets:       toHourPaths(hourBuckets),
		Events:            toEvents(events),
		AnalyzedPool:      poolFeats,
		Drive:             toStats(drive),
		OnRepeat:          toStats(onRepeat),
		ArtistDeepCuts:    toStats(deepCuts),
		QuietSlowPool:     quietPoolFeats,
		LoudFastPool:      loudPoolFeats,
		Favorites:         favFeats,
		PlayedPaths:       toBoolSet(played),
		Features:          features,
		PreviouslyVisible: previouslyVisible,
		WeekSeed:          weekSeed,
		ServedRecently:    servedRecently,
	}, nil
}

func logExclusionRelaxations(gen []smartplaylist.GeneratedPlaylist) {
	for _, g := range gen {
		if g.ExclusionRelaxed {
			logger.Info("smart mix dropped the already-shown exclusion to keep its minimum", "slug", g.Slug)
		}
	}
}

// recordDiscoveryServed replaces the run's own UTC day's served rows for each
// discovery shelf gen holds with that shelf's items, and prunes past the
// window. A shelf gen does not hold keeps its rows: a per-family run passes
// only its own family, and a family that dropped out of a later full run
// was still shown until it did.
func recordDiscoveryServed(ctx context.Context, store *manifest.Store, gen []smartplaylist.GeneratedPlaylist, nowNS int64) error {
	served := map[string][]string{}
	for _, g := range gen {
		if !smartplaylist.DiscoveryShelf(g.Slug) {
			continue
		}
		paths := make([]string, 0, len(g.Items))
		for _, it := range g.Items {
			if it.Path != "" {
				paths = append(paths, it.Path)
			}
		}
		served[g.Slug] = paths
	}
	return store.ReplaceSmartPlaylistServedToday(ctx, nowNS, discoveryServedDays, served)
}

// toStored converts the engine output into cache rows, serializing each
// family's items into the items_json blob (hour-keyed for time-of-day, a flat
// array otherwise). Position is the homepage order from the engine's slice.
func toStored(gen []smartplaylist.GeneratedPlaylist, nowNS int64) ([]manifest.StoredSmartPlaylist, error) {
	out := make([]manifest.StoredSmartPlaylist, 0, len(gen))
	for i, g := range gen {
		var blob []byte
		var err error
		if g.Kind == smartplaylist.KindTimeOfDay {
			hb := manifest.SmartPlaylistHourlyBlob{Hourly: make(map[int][]manifest.SmartPlaylistItem, len(g.HourlyItems))}
			for h, items := range g.HourlyItems {
				hb.Hourly[h] = toStoredItems(items)
			}
			blob, err = json.Marshal(hb)
		} else {
			blob, err = json.Marshal(toStoredItems(g.Items))
		}
		if err != nil {
			return nil, err
		}
		// Serialize the energy envelope only when non-empty; an absent blob
		// tells the wire handler (and iOS) to fall back to the seeded
		// waveform rather than render a degenerate flat halo.
		var energyJSON []byte
		if len(g.Energy) > 0 {
			if energyJSON, err = json.Marshal(g.Energy); err != nil {
				return nil, err
			}
		}
		out = append(out, manifest.StoredSmartPlaylist{
			Slug:        g.Slug,
			Kind:        string(g.Kind),
			Title:       g.Title,
			Subtitle:    g.Subtitle,
			Position:    i,
			RefreshedAt: nowNS,
			ItemsJSON:   blob,
			EnergyJSON:  energyJSON,
			ModalRateHz: g.ModalRateHz,
		})
	}
	return out, nil
}

// --- converters (manifest rows <-> engine domain types) ---

func toStats(rows []manifest.PlayStatRow) []smartplaylist.PlayStat {
	out := make([]smartplaylist.PlayStat, len(rows))
	for i, r := range rows {
		out[i] = smartplaylist.PlayStat{Path: r.Path, Plays: r.Plays, LastPlayed: r.LastPlayed, FirstPlayed: r.FirstPlayed}
	}
	return out
}

func toHourPaths(rows []manifest.HourPathRow) []smartplaylist.HourPath {
	out := make([]smartplaylist.HourPath, len(rows))
	for i, r := range rows {
		out[i] = smartplaylist.HourPath{Hour: r.Hour, Path: r.Path, Plays: r.Plays}
	}
	return out
}

func toEvents(rows []manifest.EventTimeRow) []smartplaylist.Event {
	out := make([]smartplaylist.Event, len(rows))
	for i, r := range rows {
		out[i] = smartplaylist.Event{StartedAt: r.StartedAt, DurationUsed: r.DurationUsed}
	}
	return out
}

func toFeature(r manifest.TrackFeatureRow) smartplaylist.TrackFeature {
	f := smartplaylist.TrackFeature{
		Path: r.Path, Title: r.Title, Artist: r.Artist, Album: r.Album, Genre: r.Genre,
		KeyRoot: r.KeyRoot, KeyMode: r.KeyMode, BPM: r.BPM, ReplayGainTrackDB: r.ReplayGainTrackDB,
		SampleRate: r.SampleRate,
	}
	if r.Duration != nil {
		f.Duration = *r.Duration
	}
	return f
}

func toBoolSet(m map[string]struct{}) map[string]bool {
	out := make(map[string]bool, len(m))
	for p := range m {
		out[p] = true
	}
	return out
}

func toStoredItems(items []smartplaylist.Item) []manifest.SmartPlaylistItem {
	out := make([]manifest.SmartPlaylistItem, len(items))
	for i, it := range items {
		out[i] = manifest.SmartPlaylistItem{Position: it.Position, Path: it.Path, Title: it.Title, Artist: it.Artist}
	}
	return out
}
