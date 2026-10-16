package doctor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

const checkNameNameCollisions = "name-collisions"

// NameCollisions is what Deps.NameCollisions answers: the groups of names in
// the indexed library that the app reads as one name, compared in the store
// alone (backlog B420).
type NameCollisions struct {
	// Paths is how many track paths under the library roots were compared.
	// UPnP-routed rows are not among them: their names are made from another
	// server's metadata, and no copy of one is on this host to fix.
	Paths int
	// Groups is how many groups the comparison found.
	Groups int
	// Sample is the first groups in path order, each its spellings'
	// library-relative paths in byte order. The probe bounds it; Groups
	// counts past it.
	Sample [][]string
}

// nameCollisionsAdvice is the hint's fixed half, ahead of the groups.
const nameCollisionsAdvice = "The 1-bit app compares names ignoring case and Unicode composition, " +
	"so it reads the names in each group below as one: two such folders are one folder there, " +
	"and of two such files it lists one. These are almost always copies. Merge or rename them " +
	"so each name is spelled one way; the next scan indexes the result."

// checkNameCollisions reports names in one folder that differ only in case
// or Unicode composition: "Album" beside "ALBUM", or "Café" stored composed
// (NFC) beside "Café" stored decomposed (NFD). The app folds both on a
// bridge share, so it shows one name for each group: it merges two such
// folders, and of two such files it lists one. A bridge share stays folded,
// with nothing detected per host, and a collision is reported for the user
// to fix the copies (backlog B420, the owner's decision of 2026-10-16).
//
// The probe compares the indexed paths and walks no library folder; nothing
// here renames or writes, and the check prints what it found. Warn, not fail:
// `bridge init` refuses to proceed on a fail, and a copy in the library is
// no reason to refuse an install. It is not skipped on a managed bridge:
// the library is its user's, and the advice names no command. Skipped when
// no probe is wired (no manifest yet); a probe error is reported, never
// answered as clean.
func checkNameCollisions(ctx context.Context, d Deps) Check {
	if d.NameCollisions == nil {
		return ok(checkNameNameCollisions, "no manifest to check (run after the first scan)")
	}
	nc, err := d.NameCollisions(ctx)
	if err != nil {
		return warn(checkNameNameCollisions, "could not read the indexed paths",
			"the manifest database was not readable: "+err.Error())
	}
	if nc.Groups == 0 {
		return ok(checkNameNameCollisions, fmt.Sprintf(
			"%d track path(s) under the library roots; no two names in one folder differ only in case or Unicode composition",
			nc.Paths))
	}
	var hint strings.Builder
	hint.WriteString(nameCollisionsAdvice)
	for i, g := range nc.Sample {
		if i == 0 {
			hint.WriteString(" Groups: ")
		} else {
			hint.WriteString("; ")
		}
		hint.WriteString(describeNameCollision(g))
	}
	if more := nc.Groups - len(nc.Sample); more > 0 {
		fmt.Fprintf(&hint, " (+%d more)", more)
	}
	return warn(checkNameNameCollisions,
		fmt.Sprintf("%d group(s) of names in one folder that differ only in case or Unicode composition", nc.Groups),
		hint.String())
}

// describeNameCollision names one group's paths, each quoted, so a name with
// a space, a comma or a control character in it reads as one name. Where
// the group mixes a composed and a decomposed spelling, which print alike,
// each path says which it is, from the name the group's paths differ in:
// their last component.
func describeNameCollision(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	composed := make([]bool, len(paths))
	mixed := false
	for i, p := range paths {
		composed[i] = norm.NFC.IsNormalString(p[strings.LastIndexByte(p, '/')+1:])
		mixed = mixed || composed[i] != composed[0]
	}
	parts := make([]string, len(paths))
	for i, p := range paths {
		parts[i] = strconv.Quote(p)
		if mixed {
			if composed[i] {
				parts[i] += " (composed)"
			} else {
				parts[i] += " (decomposed)"
			}
		}
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
