package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EvalSymlinksOrClean returns filepath.EvalSymlinks(p) when it succeeds.
// When the leaf doesn't exist yet (typical for a brand-new install where
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
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
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
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, missing)
		}
		cur = parent
	}
}

// IsUnderAny reports whether candidate resolves AT or UNDER any of roots,
// resolving symlinks on BOTH sides via EvalSymlinksOrClean before the
// filepath.Rel comparison. Returns the resolved (cleaned) root it matched, or
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
	// Walk up to the nearest existing directory so there's something to stat.
	dir := p
	for {
		if _, err := os.Stat(dir); err == nil {
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
	fi1, err1 := os.Stat(dir)
	fi2, err2 := os.Stat(filepath.Join(filepath.Dir(dir), swapped))
	if err1 == nil && err2 == nil {
		// Both names resolve. Same inode → case-insensitive (one physical dir);
		// distinct inodes → a genuinely case-sensitive volume that happens to
		// hold both spellings as separate dirs.
		return os.SameFile(fi1, fi2)
	}
	// The swapped spelling doesn't resolve → case-sensitive (it would resolve
	// to the same dir on a case-insensitive volume).
	return false
}
