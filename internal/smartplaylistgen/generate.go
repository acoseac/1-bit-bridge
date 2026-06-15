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
		Engine:            smartplaylist.DefaultOptions(analysisEnabled),
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

	var pool []manifest.TrackFeatureRow
	if opts.AnalysisEnabled {
		if pool, err = store.AnalyzedTrackFeatures(ctx, "", opts.PoolLimit); err != nil {
			return smartplaylist.Inputs{}, err
		}
	}

	// Hydrate features for every candidate path (listening lists + hour
	// buckets), then fold in the analyzed pool's own features.
	pathSet := map[string]struct{}{}
	for _, lst := range [][]manifest.PlayStatRow{heavy, familiar, forgotten, recent} {
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
	features := make(map[string]smartplaylist.TrackFeature, len(featRows)+len(pool))
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

	return smartplaylist.Inputs{
		HeavyRotation: toStats(heavy),
		Familiar:      toStats(familiar),
		Forgotten:     toStats(forgotten),
		Recent:        toStats(recent),
		HourBuckets:   toHourPaths(hourBuckets),
		Events:        toEvents(events),
		AnalyzedPool:  poolFeats,
		PlayedPaths:   toBoolSet(played),
		Features:      features,
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
