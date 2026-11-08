package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EvalSymlinksOrClean returns ResolveLinks(p) when it succeeds: what
// filepath.EvalSymlinks answers, and on Windows also through a directory
// junction or a mounted folder, which EvalSymlinks has not followed since Go
// 1.23 (so a variants directory spelled by its real path under a junction'd
// root's target read as not nested; see ResolveLinks). When the leaf doesn't
// exist yet (typical for a brand-new install where
// variants_dir is created on first upscale, a library root the operator
// typed into bridge.yaml but hasn't mounted, or a `bridge variants move`
// --to that hasn't been created yet), it resolves symlinks in the NEAREST
// EXISTING ANCESTOR and re-appends the missing trailing components.
//
// The ancestor walk is load-bearing, not cosmetic: a plain filepath.Clean
// fallback leaves a symlinked PARENT component unresolved, so a path like
// `/data/link/transcoded` (where `/data/link` -> a library root and
// `transcoded` doesn't exist yet) passes a lexical containment check — and
// then os.MkdirAll writes THROUGH the symlink, dumping variant files into
// the read-only library root. Resolving the existing-ancestor prefix closes
// that bypass while still validating brand-new paths.
//
// Consolidated here (single canonical copy) so the three containment
// checks that depend on it — config.validateVariantsDir,
// admin.assertNotUnderLibraryRoots, and the `bridge variants move` CLI —
// stay in lockstep. Previously duplicated byte-for-byte in config + admin.
func EvalSymlinksOrClean(p string) string {
	if resolved, err := ResolveLinks(p); err == nil {
		return resolved
	}
	p = filepath.Clean(p)
	missing := ""
	for cur := p; ; {
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem / volume root without finding an
			// existing ancestor — fall back to the lexical clean.
			return p
		}
		missing = filepath.Join(filepath.Base(cur), missing)
		if resolved, err := ResolveLinks(parent); err == nil {
			return filepath.Join(resolved, missing)
		}
		cur = parent
	}
}

// SamePath reports whether a and b name one path once each is resolved as
// EvalSymlinksOrClean resolves it (its links, and on Windows its junctions,
// through its nearest ancestor that exists, so neither need exist yet),
// compared in the case sensitivity of the volume that holds it
// (caseInsensitiveFS, probed, as IsUnderAny compares): a path spelled through
// a link, or in another case on a volume that folds case, is the same path,
// and the strings themselves are not compared.
func SamePath(a, b string) bool {
	ra, rb := EvalSymlinksOrClean(a), EvalSymlinksOrClean(b)
	if ra == rb {
		return true
	}
	return caseInsensitiveFS(ra) && strings.ToLower(ra) == strings.ToLower(rb)
}

// SamePathAsRecorded is SamePath for a path recorded earlier together with
// how each folder on its way compared names then (FoldingOnTheWay), probing
// nothing. Two absolute paths name one path where two of their folders, as
// many names above each path's end, resolve (ResolveLinks) to one directory,
// and below the deepest two that do each name now gives is alike, by that
// folding, with the one recorded gives in its place (alikeByRecord: the same,
// or the same but for case where the folder recorded's spelling looks it up
// in was not read as case-sensitive); and where no two resolve to one (a
// drive that is not there), where the two are alike as written
// (SameAsRecorded). The resolved folders are compared exactly and the names
// below them as written, so a link on the way to another depth (macOS's
// /var, a link to /private/var one folder deeper) moves the folders above
// where it leads and never a letter: each name below is recorded's own,
// judged by the letter recorded for the folder it is looked up in. A folding
// that does not hold one letter for each folder recorded's spelling passes
// through (foldingOf: one of another spelling, its resolution's among them)
// says nothing of any, each CaseUnknown. B741's round 4: recorded as written
// was compared with now as it resolved, whole, which laid the letters over
// the resolution, shifted by the folders a link added or took away, and on
// Windows read a short (8.3) name and its long one as two folders.
//
// What a resolution does with case decides a folder that is there spelled in
// another case: on Windows it names each folder that is there by its name on
// disk, its long name in the case its volume keeps, so the two spellings
// resolve to one directory and name one folder whatever its letter; on Linux
// and macOS it keeps the spelling it is given, so they resolve apart and the
// letter of the folder above decides, as for a folder that is not there. How
// a volume compares names is never probed here, unlike SamePath: recorded's
// folders may be on a volume that is away, and a probe then answers for the
// folder left at its mount point, on the volume above (backlog B741's gap 3).
// A path that is not absolute is one with another where both resolve, as
// EvalSymlinksOrClean resolves them, to one string.
func SamePathAsRecorded(recorded, now string, folding []CaseFolding) bool {
	recorded, now = filepath.Clean(recorded), filepath.Clean(now)
	if recorded == now {
		return true
	}
	if !filepath.IsAbs(recorded) || !filepath.IsAbs(now) {
		return EvalSymlinksOrClean(recorded) == EvalSymlinksOrClean(now)
	}
	return sameAbsAsRecorded(recorded, now, folding, ResolveLinks)
}

// sameAbsAsRecorded is SamePathAsRecorded for two clean absolute paths, by
// resolve, ResolveLinks or a test's stand-in for what this host's volumes do
// not do (a drive that is not there, Windows' short names and case on disk).
func sameAbsAsRecorded(recorded, now string, folding []CaseFolding, resolve func(string) (string, error)) bool {
	r, n := absElements(recorded), absElements(now)
	folding = foldingOf(folding, len(r))
	// below is how many names of each path lie below the pair of folders
	// asked, from the leaves up: a name unalike is below every pair above it
	// too, so none of those can make the two one path.
	for below := 0; below < len(r) && below < len(n); below++ {
		if below > 0 && !alikeByRecord(folding, r, len(r)-below, n[len(n)-below]) {
			return false
		}
		a, errA := resolve(filepath.Join(r[:len(r)-below]...))
		b, errB := resolve(filepath.Join(n[:len(n)-below]...))
		if errA == nil && errB == nil && a == b {
			return true
		}
	}
	return SameAsRecorded(recorded, now, folding)
}

// IsUnderAny reports whether candidate resolves AT or UNDER any of roots,
// resolving links (symlinks, and on Windows junctions and mounted folders)
// on BOTH sides via EvalSymlinksOrClean before the filepath.Rel comparison. Returns the resolved (cleaned) root it matched, or
// "" when candidate is safely outside every root. Empty root entries and
// cross-volume Rel errors (different Windows volumes) are skipped.
//
// This is the single canonical containment check shared by the three sites
// that must stay in lockstep: config.validateVariantsDir, the admin
// variants-dir handler (assertNotUnderLibraryRoots), and the `bridge variants
// move` CLI. A `rel` of ".." or one starting with "../" (or "..\\") means
// candidate is ABOVE the root; anything else — including "." (equal to root)
// and dot-prefixed subpaths like ".cache/x" — means AT-or-UNDER.
func IsUnderAny(candidate string, roots []string) string {
	cleaned := EvalSymlinksOrClean(candidate)
	for _, root := range roots {
		if root == "" {
			continue
		}
		cleanedRoot := EvalSymlinksOrClean(root)
		// Compare in the filesystem's case sensitivity. On a case-insensitive
		// volume EvalSymlinks does NOT fold case to the on-disk canonical form,
		// so a case-only difference (root ".../Music" vs candidate
		// ".../music/variants" — the same physical directory) would otherwise
		// report not-nested and let a variants dir be written INSIDE the
		// library root (the PR #475 phantom-rows class). Probe the actual
		// filesystem (not just GOOS) so a Linux operator whose library sits on a
		// case-insensitive mount — FAT32 / exFAT / NTFS, common on Pi/SBC — is
		// covered too; fold both sides before Rel, still return the
		// original-case root.
		relBase, relTarget := cleanedRoot, cleaned
		if caseInsensitiveFS(cleanedRoot) {
			relBase, relTarget = strings.ToLower(cleanedRoot), strings.ToLower(cleaned)
		}
		rel, err := filepath.Rel(relBase, relTarget)
		if err != nil {
			continue // cross-volume on Windows; can't be nested.
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return cleanedRoot
		}
	}
	return ""
}

// caseInsensitiveFS reports whether the volume holding p (or its nearest
// existing ancestor) treats names case-insensitively. It probes EMPIRICALLY —
// stat the ancestor's base name and its case-swapped sibling, then compare via
// os.SameFile — rather than trusting runtime.GOOS alone. That covers the case
// the GOOS heuristic misses in both directions: a Linux operator whose library
// lives on a case-insensitive mount (FAT32 / exFAT / NTFS — common on Pi/SBC),
// and a case-sensitive volume on macOS/Windows. Falls back to the GOOS default
// only when the path can't be probed (no existing ancestor yet, or a base name
// with no case-foldable letter).
func caseInsensitiveFS(p string) bool {
	goosDefault := runtime.GOOS == "darwin" || runtime.GOOS == "windows"
	// Walk up to the nearest existing directory so there's something to stat,
	// keeping its FileInfo (fi1) so the SameFile comparison below doesn't re-stat.
	dir := p
	var fi1 os.FileInfo
	for {
		if info, err := os.Stat(dir); err == nil {
			fi1 = info
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return goosDefault // reached the volume root with nothing existing
		}
		dir = parent
	}
	base := filepath.Base(dir)
	swapped := strings.ToUpper(base)
	if swapped == base {
		swapped = strings.ToLower(base)
	}
	if swapped == base {
		return goosDefault // no case-foldable letter to probe with
	}
	fi2, err2 := os.Stat(filepath.Join(filepath.Dir(dir), swapped))
	if err2 == nil {
		// The swapped spelling resolves too. Same inode → case-insensitive (one
		// physical dir); distinct inodes → a genuinely case-sensitive volume that
		// happens to hold both spellings as separate dirs.
		return os.SameFile(fi1, fi2)
	}
	// The swapped spelling doesn't resolve → case-sensitive (it would resolve
	// to the same dir on a case-insensitive volume).
	return false
}
