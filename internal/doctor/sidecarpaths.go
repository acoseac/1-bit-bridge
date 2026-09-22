package doctor

import (
	"context"
	"fmt"
)

const checkNameSidecarPaths = "sidecar-paths"

// RelocatedSidecars is what Deps.RelocatedSidecars answers: how many
// rows of each sidecar table record a file outside the directory this
// bridge currently writes that kind of sidecar to.
type RelocatedSidecars struct {
	// Variants is the `track_variants` count, VariantBytes their recorded
	// size — the console's "N legacy variants" number.
	Variants     int
	VariantBytes int64
	// Waveforms is the `track_analysis` count (rows with a waveform only).
	Waveforms int
	// VariantsDir / WaveformDir name the directories the counts were taken
	// against, for the summary.
	VariantsDir string
	WaveformDir string
}

// checkSidecarPaths reports sidecar rows that point outside the
// directories they are served from.
//
// Both sidecar tables store ABSOLUTE paths — `track_variants.sidecar_path`
// under the variants dir, `track_analysis.waveform_path` under
// `<dataDir>/waveforms` — so a database copied to a host where either
// directory has a different path leaves every row naming the old one.
//
// BOTH are self-healing now, by different routes, and neither re-decodes
// anything. A variant row is adopted by the hourly integrity sweep and
// on first play (2026-09-20). A waveform row is adopted on the first
// analysis lookup — #954 wired integrity.LocateWaveform into
// analysisStoreAdapter — and has no proactive sweep, so it heals when
// its track is next asked for and not before.
//
// What this check is still for is the row whose file is at NEITHER
// location. For waveforms that is the silent case: the analysis skip
// gate keys on the SOURCE's mtime and size, never the curve, so the
// candidate walk never offers the track again and a plain
// `bridge analyze` regenerates nothing.
//
// Warn, not fail: `bridge init` refuses to proceed on a fail, and a
// stale path in a sidecar table is not a reason to refuse a new install.
// Skipped when no probe is wired (a first run with no manifest, or a
// caller that chose not to open the database) and on a managed bridge,
// whose directories are the control plane's to move and whose reader
// has no shell for the commands the hint names.
func checkSidecarPaths(ctx context.Context, d Deps) Check {
	if d.Managed {
		return ok(checkNameSidecarPaths, "sidecar directories are the host's — check skipped")
	}
	if d.RelocatedSidecars == nil {
		return ok(checkNameSidecarPaths, "no manifest to check (run after the first scan)")
	}
	rel, err := d.RelocatedSidecars(ctx)
	if err != nil {
		// "Don't know" is not "fine": say the probe failed rather than
		// answering ok from a database that could not be read.
		return warn(checkNameSidecarPaths, "could not read the sidecar tables",
			"the manifest database was not readable: "+err.Error())
	}
	if rel.Variants == 0 && rel.Waveforms == 0 {
		return ok(checkNameSidecarPaths, "every recorded sidecar path is under its current directory")
	}
	summary := fmt.Sprintf("%d variant row(s) and %d waveform row(s) record a sidecar outside the current directory",
		rel.Variants, rel.Waveforms)
	hint := ""
	if rel.Variants > 0 {
		hint += fmt.Sprintf("%d variant row(s) (%s) point outside %s. Rows whose file sits at its source-mirrored "+
			"path under that directory are adopted by the hourly integrity sweep and on first play; rows still listed "+
			"after a sweep point at files that are not there — `bridge variants move --to %s --confirm MOVE` relocates "+
			"any still at the old path, `bridge upscale --gc` reaps the rest. ",
			rel.Variants, humanBytes(rel.VariantBytes), rel.VariantsDir, rel.VariantsDir)
	}
	if rel.Waveforms > 0 {
		hint += fmt.Sprintf("%d waveform row(s) point outside %s. A curve whose file sits at its source-mirrored "+
			"path under that directory is adopted on the first analysis lookup for the track — the row is rewritten "+
			"and nothing is re-decoded — but there is no proactive waveform sweep, so that happens when its curve is "+
			"next requested and not before. Rows still listed afterwards point at curves which are NOT there, and the "+
			"analysis skip gate reads the row rather than the file, so a plain `bridge analyze` will not notice them: "+
			"`bridge analyze --force` rebuilds those.",
			rel.Waveforms, rel.WaveformDir)
	}
	return warn(checkNameSidecarPaths, summary, hint)
}
