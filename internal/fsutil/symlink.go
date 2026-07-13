package fsutil

import (
	"path/filepath"
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
		rel, err := filepath.Rel(cleanedRoot, cleaned)
		if err != nil {
			continue // cross-volume on Windows; can't be nested.
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return cleanedRoot
		}
	}
	return ""
}
