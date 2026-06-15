// Package smartplaylist generates server-side "smart" / dynamic playlists
// (Heavy Rotation, Auto Mix, Forgotten Favorites, time-of-day, Daily Mix,
// The Finish Line) from playback-history aggregations + offline audio
// analysis.
//
// It is PURE: no SQLite, no time.Now, no I/O. The daily regenerator
// (cmd/bridge) assembles Inputs from manifest.Store queries, calls Generate,
// and persists the result via Store.ReplaceSmartPlaylists. Keeping the logic
// store-agnostic makes it unit-testable with hand-built fixtures (the same
// discipline internal/analyze uses for its DSP).
package smartplaylist

// Kind discriminates a generated-playlist family. Stable strings — the iOS
// client switches on them for section styling/icons, so do not rename.
type Kind string

const (
	KindHeavyRotation      Kind = "heavyRotation"
	KindForgottenFavorites Kind = "forgottenFavorites"
	KindRecentlyPlayed     Kind = "recentlyPlayed"
	KindAutoMix            Kind = "autoMix"
	KindDailyMix           Kind = "dailyMix"
	KindTimeOfDay          Kind = "timeOfDay"
	KindFinishLine         Kind = "finishLine"
)

// TrackFeature is the metadata + analysis a track contributes to generation.
// BPM/ReplayGainTrackDB are EFFECTIVE values (curated tag wins, analysis
// fallback — resolved by the manifest query). KeyRoot/KeyMode are
// analysis-only.
type TrackFeature struct {
	Path              string
	Title             string
	Artist            string
	Album             string
	Genre             string
	Duration          float64 // seconds (0 = unknown)
	BPM               *int    // beats/min
	KeyRoot           *int    // 0..11, C=0
	KeyMode           string  // "major" / "minor"
	ReplayGainTrackDB *float64
	SampleRate        *int // Hz (drives the per-mix modal-rate glow color)
}

// PlayStat is aggregated play data for one track path (from one of the
// windowed history queries).
type PlayStat struct {
	Path        string
	Plays       int
	LastPlayed  int64 // UnixNano
	FirstPlayed int64 // UnixNano
}

// HourPath is a (UTC hour-of-day, path, plays) triple feeding time-of-day
// pools.
type HourPath struct {
	Hour  int // 0..23, UTC
	Path  string
	Plays int
}

// Event is a minimal (start, duration) play used for session segmentation
// (The Finish Line). Must be supplied in chronological order.
type Event struct {
	StartedAt    int64 // UnixNano
	DurationUsed float64
}

// Item is one ordered entry in a generated playlist.
type Item struct {
	Position int
	Path     string
	Title    string
	Artist   string
}

// GeneratedPlaylist is one populated family the engine emits. For
// KindTimeOfDay, HourlyItems carries per-UTC-hour pools (the API shifts to
// the device's local hour at request time) and Items is empty; for every
// other kind Items is the ordered list and HourlyItems is nil.
type GeneratedPlaylist struct {
	Slug        string
	Kind        Kind
	Title       string
	Subtitle    string
	Items       []Item
	HourlyItems map[int][]Item

	// Energy is the normalized 0..1 loudness contour across this family's
	// members (one element per track, downsampled), driving the iOS
	// "waveform-signed cover" halo spline. ModalRateHz is the mix's modal
	// sample rate (tie-break → highest) for the halo glow color. Both are
	// stamped by Generate's post-pass (signEnergy); nil/0 = no analyzed
	// members (iOS falls back to a seeded waveform / fixed family color).
	Energy      []float64
	ModalRateHz int
}

// Inputs is the data snapshot the engine generates from, assembled by the
// regenerator from manifest.Store queries.
type Inputs struct {
	HeavyRotation []PlayStat     // qualifying plays, recent window, plays-desc
	Familiar      []PlayStat     // wider window, the Daily Mix "familiar" set
	Forgotten     []PlayStat     // forgotten-favorites candidates (loved, untouched lately)
	Recent        []PlayStat     // recently played, newest first
	HourBuckets   []HourPath     // (hour, path, plays), all routes
	Events        []Event        // chronological qualifying events (Finish Line)
	AnalyzedPool  []TrackFeature // tracks WITH an estimated key (harmonic + discovery)

	// PlayedPaths is the set of paths with any qualifying play (Daily Mix
	// discovery anti-join).
	PlayedPaths map[string]bool

	// Features maps every candidate path (PlayStat lists + hour buckets) to
	// its hydrated feature, for item building + harmonic sort.
	Features map[string]TrackFeature
}

// Options carries tunable thresholds + the analysis gate. The engine is pure
// (no clock); all time-windowing already happened in the SQL queries.
type Options struct {
	AnalysisEnabled bool
	MaxItems        int // cap per family

	MinHeavyRotation  int
	MinRecentlyPlayed int
	MinForgotten      int
	MinAutoMixPool    int // analyzed-pool size gate for Auto Mix
	MinTimeOfDayPlays int // the busiest single hour must reach this
	MinDailyFamiliar  int
	MinSessions       int // sessions needed to trust the Finish Line target

	SessionGapSeconds   float64 // idle gap that ends a session (Gemini: 60 min)
	DailyDiscoveryRatio float64 // fraction of the Daily Mix that is discovery
}

// DefaultOptions returns the tuned production thresholds. These are the
// "in-implementation tuning" knobs flagged in the plan — adjust here.
func DefaultOptions(analysisEnabled bool) Options {
	return Options{
		AnalysisEnabled:     analysisEnabled,
		MaxItems:            50,
		MinHeavyRotation:    10,
		MinRecentlyPlayed:   5,
		MinForgotten:        10,
		MinAutoMixPool:      20,
		MinTimeOfDayPlays:   15,
		MinDailyFamiliar:    10,
		MinSessions:         20,
		SessionGapSeconds:   60 * 60, // 60-min idle = new session (Gemini 2026-06-14)
		DailyDiscoveryRatio: 0.30,
	}
}
