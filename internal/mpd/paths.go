package mpd

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Path-rule states. proven is a SameFile match that find confirmed.
// mismatch is a SameFile match a find did not. unproven is no single
// SameFile match. unchecked is no music directory, no lookup, or no
// tracks to sample.
const (
	PathProven    = "proven"
	PathMismatch  = "mismatch"
	PathUnproven  = "unproven"
	PathUnchecked = "unchecked"

	pathSample = 20
)

// LibraryRoot is one bridge library root and the indexed paths under it.
// ID is the root's basename, which is the first segment of a stored path
// when Multi is set. Tracks are those stored paths, forward-slash, not
// MPD URIs. This is not a wire type.
type LibraryRoot struct {
	ID     string
	Path   string
	Multi  bool
	Tracks []string
}

// PathRule is one root's prefix swap and how far it was proved. The
// prefixes are what the app applies: an MPD URI is prepend plus the
// stored path with strip removed. They are filled only when one SameFile
// match named the root. This is not a wire type.
type PathRule struct {
	Root              string
	StripBridgePrefix string
	PrependMpdPrefix  string
	State             string
	Checked           int
	Found             int
}

// PathLookup reports whether an MPD URI is in the database. A false or
// an error is a miss.
type PathLookup func(ctx context.Context, mpdPath string) (bool, error)

// ProveRoots returns one rule per root, in order. musicDir is MPD's
// music_directory. A root matches when it is that directory, or when
// exactly one top-level entry's real path is the root or contains it.
// The prefix is that entry plus the relative path under it. find is
// asked for at most 20 tracks, evenly from the sorted list, and only
// after a match.
func ProveRoots(ctx context.Context, musicDir string, roots []LibraryRoot, lookup PathLookup) []PathRule {
	out := make([]PathRule, 0, len(roots))
	for _, root := range roots {
		out = append(out, proveRoot(ctx, musicDir, root, lookup))
	}
	return out
}

func proveRoot(ctx context.Context, musicDir string, root LibraryRoot, lookup PathLookup) PathRule {
	rule := PathRule{Root: root.ID, State: PathUnchecked}
	if musicDir == "" {
		return rule
	}
	prefix, matched := musicPrefix(musicDir, root.Path)
	if !matched {
		rule.State = PathUnproven
		return rule
	}
	rule.PrependMpdPrefix = prefix
	if root.Multi && root.ID != "" {
		rule.StripBridgePrefix = root.ID + "/"
	}
	if lookup == nil || len(root.Tracks) == 0 {
		return rule
	}
	for _, stored := range sampleTracks(root.Tracks, pathSample) {
		rule.Checked++
		mpdPath, ok := bridgePathToMPD(stored, rule.StripBridgePrefix, rule.PrependMpdPrefix)
		if !ok {
			continue
		}
		found, err := lookup(ctx, mpdPath)
		if err == nil && found {
			rule.Found++
		}
	}
	if rule.Found == rule.Checked {
		rule.State = PathProven
	} else {
		rule.State = PathMismatch
	}
	return rule
}

// musicPrefix returns the MPD prefix of root inside musicDir. The
// directory itself is an empty prefix. A top-level entry matches when
// the root's real path is that entry's real path or lies under it, and
// os.SameFile agrees on musicDir/entry plus that relative path. More
// than one match, or none, is not a match.
func musicPrefix(musicDir, root string) (string, bool) {
	musicInfo, err := os.Stat(musicDir)
	if err != nil || !musicInfo.IsDir() {
		return "", false
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", false
	}
	entries, err := os.ReadDir(musicDir)
	if err != nil {
		return "", false
	}
	if os.SameFile(musicInfo, rootInfo) {
		for _, e := range entries {
			if !dirEntryMayHoldMusic(e) {
				continue
			}
			info, err := os.Stat(filepath.Join(musicDir, e.Name()))
			if err == nil && info.IsDir() && os.SameFile(info, rootInfo) {
				return "", false
			}
		}
		return "", true
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	matches := 0
	prefix := ""
	for _, e := range entries {
		if !dirEntryMayHoldMusic(e) {
			continue
		}
		entryPath := filepath.Join(musicDir, e.Name())
		real, err := filepath.EvalSymlinks(entryPath)
		if err != nil {
			continue
		}
		rel, ok := pathUnder(real, rootReal)
		if !ok {
			continue
		}
		proved := filepath.Join(entryPath, rel)
		info, err := os.Stat(proved)
		if err != nil || !info.IsDir() || !os.SameFile(info, rootInfo) {
			continue
		}
		matches++
		if rel == "." {
			prefix = e.Name() + "/"
		} else {
			prefix = e.Name() + "/" + filepath.ToSlash(rel) + "/"
		}
	}
	if matches != 1 {
		return "", false
	}
	return prefix, true
}

// dirEntryMayHoldMusic reports whether an entry can be a library root.
// A regular file cannot. An unknown type still can, so it is not skipped.
func dirEntryMayHoldMusic(e os.DirEntry) bool {
	typ := e.Type()
	return typ != 0 && (typ&os.ModeDir != 0 || typ&os.ModeSymlink != 0)
}

// pathUnder reports whether target is base or a path beneath it, and
// the relative path from base to target.
func pathUnder(base, target string) (string, bool) {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// bridgePathToMPD applies the prefix swap. strip has to be a prefix of
// the stored path; otherwise the path is not this root's and is a miss.
func bridgePathToMPD(stored, strip, prepend string) (string, bool) {
	rest := stored
	if strip != "" {
		var ok bool
		rest, ok = strings.CutPrefix(stored, strip)
		if !ok {
			return "", false
		}
	}
	return prepend + rest, true
}

// sampleTracks returns up to n paths, the sorted list when it is shorter
// than n, otherwise n evenly spaced entries. The first is included.
func sampleTracks(paths []string, n int) []string {
	cp := append([]string(nil), paths...)
	sort.Strings(cp)
	if n <= 0 || len(cp) <= n {
		return cp
	}
	out := make([]string, n)
	for i := range out {
		out[i] = cp[i*len(cp)/n]
	}
	return out
}
