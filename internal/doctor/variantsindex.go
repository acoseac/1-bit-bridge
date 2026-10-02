package doctor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/integrity"
)

const checkNameVariantsIndex = "variants-index"

// VariantsIndex is what Deps.VariantsIndex answers: the two counts that
// must agree — how many rows `track_variants` holds, and how many sidecar
// files sit under the directory this bridge serves them from — plus which
// of those files no row references.
//
// The probe builds its known set the way the forward sweeps do
// (integrity.KnownSidecarSet: recorded path ∪ canonical path), so a
// catalog that merely MOVED reads as fully referenced here. What shows up
// is a catalog that stopped describing its tree.
type VariantsIndex struct {
	// Rows is `SELECT count(*) FROM track_variants`.
	Rows int
	// Files is the sidecar files the walk classified; Known and Orphans
	// partition it.
	Files   int
	Known   int
	Orphans int
	// OrphanSample names the first few orphans, RELATIVE to VariantsDir —
	// enough for an operator to recognise their own renditions without
	// putting a column of absolute paths into a report that gets pasted
	// into issues.
	OrphanSample []string
	// WouldRefuseGC is integrity.MassOrphanRefusal's verdict on these
	// numbers: true when `bridge upscale --gc` would refuse to unlink
	// them. Computed by the probe rather than restated here, so the
	// doctor and the sweep cannot disagree about the threshold.
	//
	// Only ever set on a COMPLETE walk. The sweep's ratio term is not
	// monotone in the walk, so a verdict derived from a budgeted prefix
	// could tell an operator that `--gc` refuses when it would proceed —
	// the confident-wrong-answer shape this whole check exists to catch,
	// one level up, and reachable at the default threshold
	// (CodeRabbit on #940).
	WouldRefuseGC bool
	// OrphansExceedRows is integrity.MassOrphanLowerBound: more
	// unreferenced files than the catalog has rows in total, which is the
	// lost-index signal itself. Both its terms ARE monotone in the walk,
	// so unlike WouldRefuseGC it is sound on a truncated prefix — which is
	// what lets the check keep warning about the shape on the very trees
	// (large ones) where truncation is certain.
	OrphansExceedRows bool
	// Truncated is set when the walk stopped at its entry budget. The
	// check then scopes its claim to what it looked at instead of
	// answering for the whole tree — as it does for Unreadable, which is
	// the same fact arriving by a different route.
	Truncated bool
	// Budget is the entry cap the probe walked under, 0 for an unbounded
	// walk. Reported rather than inferred from Files: it is the number
	// the scoped message is about, and it is the one thing a caller
	// wiring this probe can get wrong in a way no count reveals — a
	// closure that passed 0 would walk a 200k-file tree on a
	// settings-page render and every count would still look right.
	Budget int
	// Unreadable counts ENTRIES the walk could not resolve: a directory it
	// could not list, and a link or junction it could not stat. This said
	// "directories" until 2026-09-28, the wording #969 had corrected in
	// both CLI sweeps and not here. Their contents are missing from Files
	// and Orphans.
	Unreadable int
	// UnlistedDirs is how many of those are directories the walk could not
	// list, which may hold any number of files: like Truncated they make
	// the RATIO a statement about part of the tree, and WouldRefuseGC is
	// withheld for them on the same terms (CodeRabbit on #940). The rest
	// are at most one file each and the verdict weighs them
	// (integrity.MassOrphanRefusalFor).
	UnlistedDirs int
	// GCRefusesPartialWalk is integrity.PartialWalkRefusal's verdict: true
	// when `bridge upscale --gc` refuses to act on a walk like this one
	// without --allow-partial-walk. Sound on a truncated walk, because a
	// directory this walk could not list is one the whole walk cannot list
	// either.
	GCRefusesPartialWalk bool
	// VariantsDir is the directory the counts were taken against.
	VariantsDir string
}

// checkVariantsIndex reports sidecar files the variant catalog no longer
// describes.
//
// The 2026-09-20 field report is the shape it exists for: a database
// copied to a new host recorded every sidecar under the OLD directory, the
// integrity sweep read 10,248 ENOENTs as 10,248 disappearances and dropped
// the whole catalog, and 259.7 GiB of renditions stayed on disk with
// nothing referencing them. Adoption (see checkSidecarPaths) closes the
// case where the ROWS survive. This check is the other side: the rows are
// gone, the files are not, and until an operator notices, the
// auto-optimize sweeper re-renders them one by one over the good copies.
//
// Warn, not fail: `bridge init` refuses to proceed on a fail, and a tree
// with orphans in it is not a reason to refuse a new install. Skipped when
// no probe is wired (a first run with no manifest, or a caller that chose
// not to open the database) and on a managed bridge, whose variants
// directory is the control plane's and whose reader has no shell for the
// commands the hint names.
//
// Distinct from checkSidecarPaths on purpose, and the pair is not
// redundant: that one counts ROWS recorded outside the current directory
// (a relocation the sweeps now heal), this one counts FILES no row
// mentions at all (a relocation nothing can heal, because the evidence
// that would connect them is what went missing).
func checkVariantsIndex(ctx context.Context, d Deps) Check {
	if d.Managed {
		return ok(checkNameVariantsIndex, "the variants directory is the host's — check skipped")
	}
	if d.VariantsIndex == nil {
		return ok(checkNameVariantsIndex, "no manifest to check (run after the first scan)")
	}
	idx, err := d.VariantsIndex(ctx)
	if err != nil {
		// "Don't know" is not "fine" — the same reading checkSidecarPaths
		// takes of an unreadable manifest.
		return warn(checkNameVariantsIndex, "could not compare the variant catalog with the files on disk",
			"the manifest database or the variants directory was not readable: "+err.Error())
	}
	scope := ""
	if idx.Truncated {
		// Say what was looked at rather than answering for a tree the
		// walk stopped short of. The alarm case is still caught: a
		// catalog that lost its index has orphans everywhere, so they
		// turn up well inside the budget.
		scope = fmt.Sprintf(" (the first %d file(s) — the tree is larger)", idx.Files)
	}
	if idx.UnlistedDirs > 0 {
		scope += fmt.Sprintf("; %d director(y/ies) could not be read", idx.UnlistedDirs)
	}
	if links := idx.Unreadable - idx.UnlistedDirs; links > 0 {
		scope += fmt.Sprintf("; %d link(s) could not be resolved", links)
	}
	if idx.Files == 0 && idx.Rows > 0 {
		// The other way round, and "all referenced" would be a strange
		// thing to say about no files at all. Nothing else reports this:
		// checkSidecarPaths sees only rows recorded OUTSIDE the current
		// directory, and these point inside one that is empty or gone.
		//
		// Both sweeps already refuse to act on it — VariantsDirSweepBlockReason
		// reads a directory that is missing or holds no rendition as an
		// unmounted volume rather than as a library whose every sidecar was
		// deleted — so this is a warning, not an emergency, and it says so.
		// That held for an EMPTY directory alone until backlog B223: one
		// holding anything else (a .DS_Store, empty folders) read as
		// healthy, and the watcher reaped every row while this said nothing
		// was being deleted.
		return warn(checkNameVariantsIndex,
			fmt.Sprintf("the catalog holds %d variant row(s) and %s holds no sidecar files%s", idx.Rows, idx.VariantsDir, scope),
			"A variants volume that is unmounted looks exactly like this, and the integrity sweep and `bridge upscale --gc` "+
				"both refuse to reap rows while the directory is missing or holds no rendition, so nothing is being deleted. "+
				"If the volume should be mounted, mount it. If the renditions really are gone, "+
				"`bridge upscale --gc --allow-mass-delete` clears the rows, and the job pool re-renders from source.")
	}
	if idx.Orphans == 0 {
		// A clean result over a walk that saw part of the tree may not be
		// phrased as a clean result over the tree. The two partial cases
		// are NOT the same kind of thing, though, and are split on that:
		//
		// A directory the walk could not read is a fault on the host —
		// actionable, not reported anywhere else, and it means the sweeps
		// and the serving path may not be able to see those sidecars
		// either. That is a warning.
		//
		// A budget that ran out is a property of THIS check (we chose the
		// budget) and of a large library, which is not a problem. Any
		// bridge over doctorVariantsIndexBudget sidecars truncates on
		// every single run, so warning would put a permanent,
		// unactionable line in every large healthy operator's report —
		// the shape this tree already records from the Managed checks,
		// where two unactionable warnings made a healthy appliance read
		// as two problems. It stays ok, and says what it looked at
		// instead of claiming the whole tree.
		if idx.Unreadable > 0 {
			return warn(checkNameVariantsIndex,
				fmt.Sprintf("%d variant row(s); every one of the %d sidecar file(s) the walk could reach is referenced%s",
					idx.Rows, idx.Files, scope),
				unreachableHint(idx))
		}
		if idx.Truncated {
			return ok(checkNameVariantsIndex,
				fmt.Sprintf("%d variant row(s); the first %d sidecar file(s) are all referenced (the tree is larger)",
					idx.Rows, idx.Files))
		}
		return ok(checkNameVariantsIndex, fmt.Sprintf("%d variant row(s), %d sidecar file(s), all referenced",
			idx.Rows, idx.Files))
	}
	summary := fmt.Sprintf("%d of %d sidecar file(s) under %s are referenced by no row, against %d row(s) in the catalog%s",
		idx.Orphans, idx.Files, idx.VariantsDir, idx.Rows, scope)
	var hint strings.Builder
	if idx.OrphansExceedRows {
		hint.WriteString("There are more unreferenced files than the catalog has rows in total, which is what a LOST INDEX " +
			"looks like rather than a tree of junk — a bridge.db restored from an older snapshot, or a host move whose " +
			"sweep reaped the rows before they could be adopted. Do not pass --allow-mass-orphans until you are sure, " +
			"because an unlinked rendition has to be transcoded again from source. If the rows are recoverable, restore " +
			"them first. ")
	}
	switch {
	case idx.WouldRefuseGC:
		hint.WriteString("`bridge upscale --gc` REFUSES this shape, so it will not unlink anything. ")
	case idx.GCRefusesPartialWalk:
		// Known from here, and sound on a truncated walk too: a directory
		// this walk could not list is one `--gc`'s own walk cannot list
		// either, and since 2026-09-28 it refuses such a walk rather than
		// take its verdict from the rest. Until then this hint said that
		// `--gc` "measures the whole tree", which a directory it could not
		// list made false.
		hint.WriteString("`bridge upscale --gc` refuses to act on a walk that could not list part of the tree, as this one " +
			"could not, so it unlinks nothing until those directories are listable by the user running it; " +
			"--allow-partial-walk lets it act on the part it can list, where the mass-orphan check still runs. ")
	case idx.Truncated:
		// The sweep's own ratio is over the WHOLE tree and this walk
		// stopped at its budget. What the sweep will decide cannot be told
		// from here, and saying either "refuses" or "reclaims them" would
		// be a guess dressed as a fact. Point at the thing that measures
		// the whole tree; it is safe to run, because refusing is its
		// default, and it refuses a walk of its own that could not list
		// part of the tree.
		hint.WriteString("Whether `bridge upscale --gc` reclaims these or refuses them cannot be told from a partial walk — " +
			"run it and read what it says; it walks the whole tree and unlinks nothing when it refuses. ")
	default:
		// All but the recent ones (backlog B205): `--gc` leaves a file
		// modified in the grace before it starts for a later run, since a
		// running bridge may still be writing it or recording its row, and
		// this count includes such files.
		fmt.Fprintf(&hint, "`bridge upscale --gc` reclaims them, all but any modified in the %d minutes before it starts, "+
			"which a running bridge may still be writing and a later run reclaims. ",
			int(integrity.OrphanGracePeriod/time.Minute))
	}
	hint.WriteString("Unreferenced so far: ")
	hint.WriteString(strings.Join(idx.OrphanSample, ", "))
	if idx.Orphans > len(idx.OrphanSample) {
		fmt.Fprintf(&hint, " (+%d more)", idx.Orphans-len(idx.OrphanSample))
	}
	return warn(checkNameVariantsIndex, summary, hint.String())
}

// unreachableHint is the advice for a walk that found every sidecar it
// could reach referenced and could not reach all of them. A directory it
// could not list is a fault on the host: its sidecars are counted nowhere,
// `bridge upscale --gc` refuses such a walk (GCRefusesPartialWalk), so does
// the background orphan sweep, and the serving path may not be able to
// open them either. A link it could not resolve alone is the smaller case:
// the sweeps weigh it as at most one file and go on, and only what it
// points at is unreachable.
func unreachableHint(idx VariantsIndex) string {
	if idx.UnlistedDirs == 0 {
		return "A link under the variants directory could not be resolved, so whatever it points at was not counted " +
			"here, and the serving path may not be able to open it either. Check the ownership and mode of its " +
			"target against the user this bridge runs as."
	}
	hint := "A directory under the variants directory could not be read, so its sidecars were neither counted " +
		"here nor seen by `bridge upscale --gc`, and the serving path may not be able to open them either. "
	if idx.GCRefusesPartialWalk {
		hint += "`--gc` and the background orphan sweep refuse to act on a walk that could not list part of the " +
			"tree, so neither reclaims anything until it is readable. "
	}
	return hint + "Check the ownership and mode of that tree against the user this bridge runs as."
}
