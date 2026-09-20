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
// For variants that state is now self-healing: the integrity sweep and
// the serve-side lookup adopt a row whose file sits at its canonical
// place under the current directory (2026-09-20). For waveforms nothing
// adopts yet, and the failure is silent twice over: `/v1/waveform`
// answers 410 for each row, and the analysis skip gate compares the
// row's source mtime and size — never the file — so the sweeper sees
// every one as up to date and regenerates nothing. This check is where
// an operator finds that out.
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
			"after a sweep point at files that are not there — `bridge variants move --to %s` relocates any still at "+
			"the old path, `bridge upscale --gc` reaps the rest. ",
			rel.Variants, humanBytes(rel.VariantBytes), rel.VariantsDir, rel.VariantsDir)
	}
	if rel.Waveforms > 0 {
		hint += fmt.Sprintf("%d waveform row(s) point outside %s: each answers 410 on /v1/waveform and the analysis "+
			"sweeper will not regenerate them (its skip gate reads the row, not the file). No relocation exists for "+
			"waveforms yet; `bridge analyze --force` rebuilds them, or copy the old waveform tree to that path.",
			rel.Waveforms, rel.WaveformDir)
	}
	return warn(checkNameSidecarPaths, summary, hint)
}
