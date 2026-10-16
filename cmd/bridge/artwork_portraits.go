package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The artist portraits at the top of the artwork cache (backlog B85): the
// enricher fetches one per artist name into `artist-name-<sha256>.jpg`
// (enrich.ArtistImagePathByName) and hard-links each artist MBID that names
// it to `artist-<mbid>.jpg` (enrich.ArtistImagePath), the path the console
// and `/v1/artist-image` read. Neither carries a size suffix, so the
// cover walk skipped both as non-cache files, and a portrait whose artist no
// track row names any longer stayed for ever.

const (
	artistPortraitPrefix   = "artist-"
	artistCanonicalPrefix  = "artist-name-"
	artistPortraitSuffix   = ".jpg"
	artistCanonicalHashLen = 64 // hex SHA-256 of the folded artist name
)

// portraitGrace keeps a portrait modified this recently, whatever the store
// says: `artwork --gc` can run beside `bridge serve`, whose enricher writes a
// canonical (or, where links are refused, a copy) and then commits the
// artist MBID to the track row. A portrait written during the run, or in the
// minutes before it, is never the GC's to remove (a pre-merge review of
// B85). It does not cover a new hard link to an OLD canonical, whose
// mtime is the canonical's; that gap, between the link and the row's
// commit, is a few milliseconds after the artist set is read again right
// before this pass (collectPortraits).
const portraitGrace = 10 * time.Minute

// artworkGCNow is the clock the grace reads, a seam so the tests can age the
// fixtures they write; production never changes it.
var artworkGCNow = time.Now

// portraitRemove is how the portrait pass removes a file, a seam so a test
// can cancel the run between two removals; production never changes it.
var portraitRemove = os.Remove

// artworkPortrait is one portrait file at the top of the cache.
type artworkPortrait struct {
	path string
	// mbid is the lowercased artist MBID of an `artist-<mbid>.jpg`; empty
	// for a name-hashed canonical.
	mbid string
	info os.FileInfo
}

// portraitMBID returns the MBID of an `artist-<mbid>.jpg` name, or false.
func portraitMBID(name string) (string, bool) {
	if !strings.HasPrefix(name, artistPortraitPrefix) || !strings.HasSuffix(name, artistPortraitSuffix) {
		return "", false
	}
	mbid := strings.TrimSuffix(strings.TrimPrefix(name, artistPortraitPrefix), artistPortraitSuffix)
	if !manifest.IsValidMBID(mbid) {
		return "", false
	}
	return strings.ToLower(mbid), true
}

// isPortraitCanonical reports whether name is an `artist-name-<sha256>.jpg`.
func isPortraitCanonical(name string) bool {
	if !strings.HasPrefix(name, artistCanonicalPrefix) || !strings.HasSuffix(name, artistPortraitSuffix) {
		return false
	}
	hash := strings.TrimSuffix(strings.TrimPrefix(name, artistCanonicalPrefix), artistPortraitSuffix)
	if len(hash) != artistCanonicalHashLen {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isTopLevelPortrait reports whether path is a portrait the portrait pass
// decides, so the cover walk leaves it to that pass. A file named like one
// below the top of the cache is no portrait: the enricher writes only there.
func isTopLevelPortrait(walkRoot, path string) bool {
	if filepath.Dir(path) != walkRoot {
		return false
	}
	name := filepath.Base(path)
	if isPortraitCanonical(name) {
		return true
	}
	_, ok := portraitMBID(name)
	return ok
}

// canBePortrait reports whether an entry whose type bits are t can be a
// portrait: a list of kinds refused (fsutil.NotAFile's, and a symlink),
// never "is a regular file". On Windows a portrait OneDrive has dehydrated
// (files on demand, a data dir under a synced folder) is ModeIrregular and
// is still the enricher's file, which "is a regular file" would keep for
// ever (a pre-merge review of B85). A symlink is not a portrait the
// enricher wrote and is left alone.
func canBePortrait(t fs.FileMode) bool {
	return t&fs.ModeSymlink == 0 && fsutil.NotAFile(t) == ""
}

// artworkPortraits lists the portraits at the top of the cache: one
// ReadDir, and an lstat per portrait (canBePortrait says which entries can
// be one).
func artworkPortraits(walkRoot string) (byMBID, canonical []artworkPortrait, err error) {
	entries, err := os.ReadDir(walkRoot)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !canBePortrait(e.Type()) {
			continue
		}
		name := e.Name()
		isCanonical := isPortraitCanonical(name)
		mbid, isMBID := portraitMBID(name)
		if !isCanonical && !isMBID {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed since the listing
			}
			return nil, nil, err
		}
		p := artworkPortrait{path: filepath.Join(walkRoot, name), mbid: mbid, info: info}
		if isCanonical {
			canonical = append(canonical, p)
		} else {
			byMBID = append(byMBID, p)
		}
	}
	return byMBID, canonical, nil
}

// portraitVerdicts returns the portraits to remove and how many stay. An
// MBID portrait is in use while a track row names its artist MBID
// (artistMBIDs, the store's `$.artistMBID` set, lowercased): the enricher
// fetches a portrait only under the artist MBID it stamps on the track. A
// canonical is in use while a portrait that STAYS is the same file
// (os.SameFile, the hard link linkOrCopy makes), so a canonical whose MBID
// links all go in this run goes with them, in a dry run too. Never decided
// from the name: the hash is one-way, and the name it hashes is
// artistImageQueryName's, which no row stores.
//
// On a filesystem that refuses hard links the enricher copies instead
// (linkOrCopy), so such a canonical shares no file with anything and is
// removed. Nothing served is lost: the MBID copies stay and are what
// `/v1/artist-image` reads. The cost is one Deezer fetch if a NEW MBID of
// that name ever needs a portrait, the dedup the canonical exists for.
func portraitVerdicts(byMBID, canonical []artworkPortrait, artistMBIDs map[string]bool, now time.Time) (remove []artworkPortrait, kept int) {
	// Kept MBID portraits by size: a hard link has its file's size, so a
	// canonical is compared only with portraits of its own size.
	fresh := func(p artworkPortrait) bool { return p.info.ModTime().After(now.Add(-portraitGrace)) }
	keptBySize := make(map[int64][]os.FileInfo)
	for _, p := range byMBID {
		if artistMBIDs[p.mbid] || fresh(p) {
			kept++
			keptBySize[p.info.Size()] = append(keptBySize[p.info.Size()], p.info)
			continue
		}
		remove = append(remove, p)
	}
	for _, c := range canonical {
		if fresh(c) {
			kept++
			continue
		}
		linked := false
		for _, k := range keptBySize[c.info.Size()] {
			if os.SameFile(c.info, k) {
				linked = true
				break
			}
		}
		if linked {
			kept++
			continue
		}
		remove = append(remove, c)
	}
	return remove, kept
}

// artistMBIDsInUse returns, lowercased, the artist MBIDs track rows carry
// (`$.artistMBID`): the set portraitVerdicts keeps portraits by, read by the
// portrait pass and by the serve size cap (readArtworkCapKeep).
func artistMBIDsInUse(ctx context.Context, store *manifest.Store) (map[string]bool, error) {
	names, err := store.DistinctArtistMBIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list referenced artist ids: %w", err)
	}
	artistMBIDs := make(map[string]bool, len(names))
	for _, m := range names {
		artistMBIDs[strings.ToLower(m)] = true
	}
	return artistMBIDs, nil
}

// portraitTally is what the portrait pass adds to the GC's summary.
type portraitTally struct {
	removed, kept, failed int
}

// collectPortraits runs the portrait pass over walkRoot: it lists, decides
// and removes (or, in a dry run, names) the portraits no track row needs.
//
// An empty artist set removes nothing unless allowEmpty: every portrait
// would be an orphan, the shape of a wrong --config or a store that has not
// been enriched, and a portrait comes back only through a Deezer fetch per
// artist. The covers' refusal stops the whole run; this one only skips the
// portraits, with a line saying how to remove them, because the cover GC
// beside it is still right.
//
// A cancelled ctx stops the pass before its next removal and returns
// ctx.Err(), which runArtworkGC reports as the cover walk's interruption
// (a pre-merge review of B85).
func collectPortraits(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, artworkDir, walkRoot string, dryRun, allowEmpty bool) (portraitTally, error) {
	var tally portraitTally
	// The artist set, read here rather than with the covers' keys before
	// the cover walk: an artist enriched during that walk is in it.
	artistMBIDs, err := artistMBIDsInUse(ctx, store)
	if err != nil {
		return tally, err
	}
	byMBID, canonical, err := artworkPortraits(walkRoot)
	if err != nil {
		return tally, err
	}
	if len(byMBID)+len(canonical) == 0 {
		return tally, nil
	}
	if len(artistMBIDs) == 0 && !allowEmpty {
		tally.kept = len(byMBID) + len(canonical)
		fmt.Fprintf(stderr, "artwork gc: keeping %d artist portrait(s) — no track row names an artist, so every portrait would be removed.\n", tally.kept)
		fmt.Fprintln(stderr, "  If no track really names an artist and you want them gone, re-run with --allow-empty.")
		return tally, nil
	}
	remove, kept := portraitVerdicts(byMBID, canonical, artistMBIDs, artworkGCNow())
	tally.kept = kept
	for _, p := range remove {
		if err := ctx.Err(); err != nil {
			return tally, err
		}
		reported := artworkReportPath(artworkDir, walkRoot, p.path)
		if dryRun {
			fmt.Fprintf(stdout, "would remove: %s\n", reported)
			tally.removed++
			continue
		}
		if err := portraitRemove(p.path); err != nil {
			if os.IsNotExist(err) {
				continue // gone since the listing: nothing left to remove
			}
			fmt.Fprintf(stderr, "remove %s: %v\n", reported, artworkErrWithoutPath(err))
			tally.failed++
			continue
		}
		tally.removed++
	}
	return tally, nil
}
