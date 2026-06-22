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

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylist"
)

// Options carries the query windows + thresholds + the injected clock. NowNS
// is supplied by the caller (the ticker passes time.Now; tests pin it) so
// this package itself stays clock-free and deterministic.
type Options struct {
	AnalysisEnabled bool
	NowNS           int64 // UnixNano "now" for all window math

	MinPlaySeconds    float64       // the "30s rule" applied in the queries
	HeavyWindow       time.Duration // Heavy Rotation lookback (14d)
	FamiliarWindow    time.Duration // Daily Mix "familiar" lookback (60d)
	ForgottenNotSince time.Duration // last play older than this = forgotten (30d)
	ForgottenMinPlays int           // historic plays to count as a favorite (3)
	HourWindow        time.Duration // time-of-day bucket lookback (90d)
	SessionWindow     time.Duration // Finish Line events lookback (90d)
	PoolLimit         int           // analyzed-pool cap (5000)

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
		AnalysisEnabled:   analysisEnabled,
		NowNS:             nowNS,
		MinPlaySeconds:    30,
		HeavyWindow:       14 * day,
		FamiliarWindow:    60 * day,
		ForgottenNotSince: 30 * day,
		ForgottenMinPlays: 3,
		HourWindow:        90 * day,
		SessionWindow:     90 * day,
		PoolLimit:         5000,
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
		// Mood bands — see CLAUDE.md / plan: less-negative ReplayGain = quieter
		// master; more-negative = louder. Mid-band gap (-8, -6] is deliberate.
		WindDownBPMMax:      90,
		WindDownLoudnessMin: -6.0,
		LiftOffBPMMin:       120,
		LiftOffLoudnessMax:  -8.0,
		Engine:              smartplaylist.DefaultOptions(analysisEnabled),
	}
}

// Regenerate assembles inputs, runs the generator, and replaces the cache.
// Returns the number of populated families written.
func Regenerate(ctx context.Context, store *manifest.Store, opts Options) (int, error) {
	in, err := assembleInputs(ctx, store, opts)
	if err != nil {
		return 0, err
	}
	gen := smartplaylist.Generate(in, opts.Engine)
	snapshot, err := toStored(gen, opts.NowNS)
	if err != nil {
		return 0, err
	}
	if err := store.ReplaceSmartPlaylists(ctx, snapshot); err != nil {
		return 0, err
	}
	return len(snapshot), nil
}

func assembleInputs(ctx context.Context, store *manifest.Store, opts Options) (smartplaylist.Inputs, error) {
	now := opts.NowNS
	eng := opts.Engine

	heavy, err := store.PlayStatsInWindow(ctx, now-opts.HeavyWindow.Nanoseconds(), 0, opts.MinPlaySeconds, eng.MaxItems)
	if err != nil {
		return smartplaylist.Inputs{}, err
	}
	familiar, err := store.PlayStatsInWindow(ctx, now-opts.FamiliarWindow.Nanoseconds(), 0, opts.MinPlaySeconds, eng.MaxItems*2)
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

	// From Artists You Love: per-artist-capped deep cuts.
	deepCuts, err := store.LovedArtistDeepCuts(ctx,
		now-opts.LovedWindow.Nanoseconds(),
		opts.LovedMinPlays,
		now-opts.DeepCutsCutoff.Nanoseconds(),
		opts.MinPlaySeconds,
		opts.ArtistDeepCutsPerArtist,
		eng.MaxItems,
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

	// Week seed — UTC ISO week, so the deterministic weekly shuffle for
	// Artist Deep Cuts + the mood bands rotates on Monday-UTC and is stable
	// across a 7-day window.
	yr, wk := time.Unix(0, opts.NowNS).UTC().ISOWeek()
	weekSeed := smartplaylist.SeedFromISOWeek(yr, wk)

	var pool []manifest.TrackFeatureRow
	if opts.AnalysisEnabled {
		if pool, err = store.AnalyzedTrackFeatures(ctx, "", opts.PoolLimit); err != nil {
			return smartplaylist.Inputs{}, err
		}
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
		PlayedPaths:       toBoolSet(played),
		Features:          features,
		PreviouslyVisible: previouslyVisible,
		WeekSeed:          weekSeed,
	}, nil
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
