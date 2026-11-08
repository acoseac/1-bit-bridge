package doctor

import (
	"context"
	"fmt"
	"strings"
)

const checkNameWaveformRegeneration = "waveform-regeneration"

// WaveformRegeneration is what Deps.WaveformRegeneration answers: the waveform
// catalog as the regeneration guard judges it (internal/integrity's
// JudgeWaveformCatalog, backlog B652), over the rows a bounded probe looked
// at, and the curves whose regeneration stopped.
type WaveformRegeneration struct {
	// Rows is how many analysis rows record a curve; Looked how many of them
	// the probe classified, Rows unless its budget stopped it.
	Rows, Looked int
	// Relocated counts curves at their canonical place under the waveform
	// directory, which the next lookup adopts with nothing re-decoded.
	Relocated int
	// Missing counts curves at neither location, and Mismatched a partial
	// copy at the canonical one: the curves proven missing, which the next
	// lookup of each asks to be rendered again.
	Missing, Mismatched int
	// Refusal is the kind of refusal the judgment makes (integrity's
	// WaveformRefusalKind, as its string), "" when it would admit; Reason
	// says what was seen.
	Refusal, Reason string
	// Undecided is a probe that looked at part of the catalog and could not
	// tell whether the guard refuses.
	Undecided bool
	// Stopped is how many curves are proven missing whose regeneration
	// stopped (two failures against this build and the source's version),
	// and StoppedSample names the first few, library-relative.
	Stopped       int
	StoppedSample []string
	// WaveformDir is the directory judged, for the summary.
	WaveformDir string
}

// The refusal kinds a judgment can report, as integrity spells them. Words
// of their own here rather than an import: internal/doctor imports no
// integrity type for the probe's answer, and
// TestTheDoctorWordsEveryRefusalKind (cmd/bridge) holds the two lists
// together.
const (
	waveformRefusalDirMissing    = "waveformDirMissing"
	waveformRefusalDirAway       = "waveformDirAway"
	waveformRefusalDirUnreadable = "waveformDirUnreadable"
	waveformRefusalEmptied       = "waveformDirEmptied"
	waveformRefusalCatalogUnread = "waveformCatalogUnread"
)

// WaveformRefusalKinds is every kind the check words, for the test that
// holds it to integrity's list.
func WaveformRefusalKinds() []string {
	return []string{
		waveformRefusalDirMissing, waveformRefusalDirAway, waveformRefusalDirUnreadable,
		waveformRefusalEmptied, waveformRefusalCatalogUnread,
	}
}

// checkWaveformRegeneration reports the curves proven missing, whether the
// bridge would render them again, and the ones whose regeneration stopped,
// each apart from the relocated curves sidecar-paths and the lookup's
// adoption are about.
//
// A curve at neither location (or a partial copy at its canonical place) is
// rendered again on its track's next /v1/waveform lookup, waveform only,
// while analysis is on: nothing else offers it again, since the analysis
// skip gate reads the row and not the file. The bridge refuses to while the
// waveform directory is missing, holds no curve (a volume that is away, or
// one emptied by hand), cannot be read, or has lost more than a fifth of the
// catalog's curves, and this line is where that refusal is reported, in the
// guard's own words. Warn, never fail: `bridge init` refuses on a fail, and a
// missing curve is no reason to refuse an install. Skipped without a probe (no
// manifest yet) and on a managed bridge, whose data directory is the control
// plane's and whose reader has no shell for the command the hint names.
func checkWaveformRegeneration(ctx context.Context, d Deps) Check {
	if d.Managed {
		return ok(checkNameWaveformRegeneration, "the waveform directory is the host's — check skipped")
	}
	if d.WaveformRegeneration == nil {
		return ok(checkNameWaveformRegeneration, "no manifest to check (run after the first scan)")
	}
	r, err := d.WaveformRegeneration(ctx)
	if err != nil {
		return warn(checkNameWaveformRegeneration, "could not compare the waveform catalog with the curves on disk",
			"the manifest database or the waveform directory was not readable: "+err.Error())
	}
	if r.Rows == 0 {
		return ok(checkNameWaveformRegeneration, "no analysis row records a waveform yet")
	}
	gone := r.Missing + r.Mismatched
	// The scope is a budget's, which stopped the probe after some rows: a
	// refusal at the probe looked at none, and says so by naming no scope.
	scope := ""
	if r.Looked > 0 && r.Looked < r.Rows {
		scope = fmt.Sprintf(" (the first %d of %d row(s))", r.Looked, r.Rows)
	}
	var parts []string
	if gone > 0 {
		parts = append(parts, fmt.Sprintf("%d curve(s) proven missing (%d at neither location, %d a partial copy)",
			gone, r.Missing, r.Mismatched))
	}
	if r.Relocated > 0 {
		parts = append(parts, fmt.Sprintf("%d relocated, adopted on their next lookup with nothing re-decoded", r.Relocated))
	}
	if r.Stopped > 0 {
		parts = append(parts, fmt.Sprintf("%d with regeneration stopped", r.Stopped))
	}
	if r.Refusal != "" {
		summary := fmt.Sprintf("regeneration refused: %s", refusalWords(r.Refusal))
		if len(parts) > 0 {
			summary += "; " + strings.Join(parts, "; ")
		}
		return warn(checkNameWaveformRegeneration, summary+scope, refusalHint(r))
	}
	if gone == 0 && r.Stopped == 0 {
		if r.Relocated > 0 {
			return ok(checkNameWaveformRegeneration, fmt.Sprintf("%d waveform row(s); %s%s", r.Rows, parts[0], scope))
		}
		return ok(checkNameWaveformRegeneration, fmt.Sprintf("%d waveform row(s), every curve where its row says%s", r.Rows, scope))
	}
	var hint strings.Builder
	if gone > 0 {
		switch {
		case !d.AnalysisEnabled:
			hint.WriteString("Analysis is off in this config (analysis.enabled), so nothing renders them again; " +
				"switch it on and each is rendered again, waveform only, on its next lookup. ")
		case r.Undecided:
			hint.WriteString("This check looked at part of the catalog, and whether the bridge would render them " +
				"again, or refuse because too many are gone, cannot be told from it. ")
		default:
			hint.WriteString("Each is rendered again, waveform only, the next time its track's curve is asked for, " +
				"its loudness, key, tempo and the rest kept as they are; nothing else offers it again, since the " +
				"analysis skip gate reads the row and not the file. ")
		}
	}
	if r.Stopped > 0 {
		fmt.Fprintf(&hint, "Regeneration stopped for %d curve(s) after two failures against this build and the "+
			"source as it is: no more tries until a new build or a changed source. `bridge analyze --force` "+
			"analyses them whole. Stopped: %s", r.Stopped, strings.Join(r.StoppedSample, ", "))
		if r.Stopped > len(r.StoppedSample) {
			fmt.Fprintf(&hint, " (+%d more)", r.Stopped-len(r.StoppedSample))
		}
	}
	return warn(checkNameWaveformRegeneration, strings.Join(parts, "; ")+scope, strings.TrimSpace(hint.String()))
}

// refusalWords is a refusal kind's summary.
func refusalWords(kind string) string {
	switch kind {
	case waveformRefusalDirMissing:
		return "the waveform directory is gone"
	case waveformRefusalDirAway:
		return "the waveform directory holds no curve, or changed while it was read (a volume that is away)"
	case waveformRefusalDirUnreadable:
		return "the waveform directory cannot be read"
	case waveformRefusalEmptied:
		return "more than a fifth of the curves are gone (the waveform directory emptied)"
	case waveformRefusalCatalogUnread:
		return "the waveform catalog could not be read"
	}
	return kind
}

// refusalHint is a refusal's advice, with the guard's own reason.
func refusalHint(r WaveformRegeneration) string {
	hint := "No curve is rendered again, one track at a time or otherwise, while this holds: " + r.Reason + ". "
	switch r.Refusal {
	case waveformRefusalDirMissing, waveformRefusalDirAway:
		hint += "If the waveform directory is on a volume, mount it; if its curves were moved, put them back at " +
			"their source-mirrored paths under it. "
	case waveformRefusalEmptied:
		hint += "A tree emptied by hand or restored from another looks like this; if the curves can be put back, " +
			"put them back. "
	}
	return hint + "If they really are gone, `bridge analyze --force` renders every curve again from source."
}
