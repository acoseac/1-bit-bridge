package manifest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A folder's cover (cover.jpg, folder.jpg, cover.png, folder.png) is read
// when a track in the folder is extracted, and a track is extracted only
// when its audio file changed, its extractor version is stale, its local-art
// cache file went missing or its lyrics sidecar drifted. So until 2026-09-29
// a cover added beside tracks already indexed was never read, nor was one
// whose read failed during the scan that indexed them (backlog B141): the
// skip gate kept their rows, without the cover, until their audio files
// changed.
//
// Each row now records the identity of the folder art it was extracted
// against (folderArtKey: every candidate's name, size and mtime), and the
// skip gate compares it with the folder's identity now (folderArtDrifted).
// The identity comes from the directory listing the lyrics sidecar check
// already reads once per directory per scan, plus a stat of each candidate
// in it, once per directory per scan: an unchanged library pays one stat per
// cover file a scan, beside the listing it shares.

// localArtOutcome is what the local-artwork pipeline (extractLocalArtwork)
// concluded for one extraction.
type localArtOutcome uint8

const (
	// localArtNotLooked: the pipeline did not run (no artwork cache
	// directory, or an extraction that stopped before it: a file its
	// extractor refused), so an empty ArtworkMBID says nothing about the
	// file's art.
	localArtNotLooked localArtOutcome = iota
	// localArtSettled: every picture and cover the pipeline looked at was
	// read and judged, so what it answered, a cover or none, is a verdict.
	localArtSettled
	// localArtUnsettled: a folder's cover could not be seen or read (its
	// directory's listing, its stat or its read failed), so the answer may
	// change once it can be: the row keeps the art it had
	// (keepArtOfUnsettledRead), records folderArtUnsettledKey, and a later
	// scan reads the cover again.
	localArtUnsettled
)

// folderArtUnsettledKey is the folder-art key a row records when its
// extraction could not see or read a cover it looked at. It is never equal to
// a key folderArtKey computes (those are made of candidate names, digits and
// the separators ':', ',' and '|'), so the skip gate goes back to such a row
// on the next scan that can read the cover.
const folderArtUnsettledKey = "?"

// folderArtNotLookedKey is the folder-art key of a row whose extraction never
// reached the artwork pipeline: a file its extractor refused before it (the
// scan writes that one by its name), or an extractor that has none. No cover
// can change what such a row is given, so the skip gate never goes back to it
// for one; its audio file changing, or an ExtractorVersion bump, re-reads it.
const folderArtNotLookedKey = "-"

// isFolderArtCandidate reports whether name is one of folderArtCandidates,
// compared as the lookup compares them (case-insensitively).
func isFolderArtCandidate(name string) bool {
	for _, candidate := range folderArtCandidates {
		if strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

// folderArtDirState is one directory's folder-art candidates as a scan saw
// them: the identity a row records and the skip gate compares, and the
// candidates the lookup then reads (scanFolderArtwork).
type folderArtDirState struct {
	// seen is false when the directory could not be listed, or a candidate
	// in it could not be stat'ed: nothing can then be said about its art.
	seen bool
	// key is "name:size:mtimeNS" for each candidate that was there, in
	// listing order, joined by ','; "" when the directory holds none.
	key string
	// names are the candidates that were there, by their listed names.
	names []string
	// failure is the stat or listing that did not complete, when seen is
	// false.
	failure error
}

// folderArtDirStateOf is dir's folder-art state for this scan, taken once
// per directory per scan when ec carries the scan's index (dirListingFor).
//
// It is taken BEFORE the lookup reads a cover (folderArtFor asks for it
// first), so the identity a row records is never newer than the cover it was
// given: a cover replaced between the two is read new under the old identity,
// and the next scan, seeing the new one, re-extracts the row and converges.
func folderArtDirStateOf(ec *ExtractContext, dir string) folderArtDirState {
	l := dirListingFor(ec, dir)
	l.artOnce.Do(func() { l.art = folderArtStateOfListing(dir, l) })
	return l.art
}

// folderArtStateOfListing stats each candidate l lists in dir.
//
// A candidate that is gone since the listing, or a link to nothing, is not
// there (ErrNotExist), as it is not for the lookup. Any other failed stat
// leaves the state unseen: "we could not see this file" says nothing about
// what it holds, so the skip gate keeps the row as it was and a later scan
// looks again. The candidates that did stat are still named, so the lookup
// reads them as it always did.
func folderArtStateOfListing(dir string, l *sidecarListing) folderArtDirState {
	if !l.listed {
		return folderArtDirState{failure: errFolderNotListed}
	}
	st := folderArtDirState{seen: true}
	var b strings.Builder
	for _, name := range l.artNames {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if st.seen {
				st.seen, st.failure = false, err
			}
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(info.Size(), 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(info.ModTime().UnixNano(), 10))
		st.names = append(st.names, name)
	}
	st.key = b.String()
	return st
}

// errFolderNotListed is the failure a folder-art state carries when its
// directory could not be read. The listing keeps no error (the lyrics lookup
// asks it only for names), so the scan's line says only that.
var errFolderNotListed = errors.New("the directory could not be listed")

// discArtParent is the directory the folder-art lookup climbs to from dir
// when dir has no cover of its own: its parent, when dir is named like a disc
// folder (isDiscFolderName) and is not a library root, whose parent is
// outside the library. ok is false when the lookup does not climb.
// extractLocalArtwork and folderArtKey both ask it, so the key covers exactly
// the directories the lookup reads.
func discArtParent(dir string, ec *ExtractContext) (parent string, ok bool) {
	if !isDiscFolderName(filepath.Base(dir)) || ec.isLibraryRoot(dir) {
		return "", false
	}
	parent = filepath.Dir(dir)
	if parent == dir {
		return "", false // the filesystem root
	}
	return parent, true
}

// folderArtKey is the identity of the folder art a track at absPath is
// given: its directory's candidates (folderArtDirState.key) and, when the
// lookup climbs from a disc folder (discArtParent) and the parent holds
// candidates, the parent's after a '|'. seen is false when either directory
// could not be seen.
//
// The skip gate (folderArtDrifted) and the extraction (recordFolderArtKey)
// both ask it, from the same per-scan state, so a row records the key the
// gate will compute from the same folder, and the gate's answer changes once
// the row is written: the lyrics sidecar check's rule.
func folderArtKey(absPath string, ec *ExtractContext) (key string, seen bool) {
	dir := filepath.Dir(absPath)
	own := folderArtDirStateOf(ec, dir)
	if !own.seen {
		return "", false
	}
	parent, climbs := discArtParent(dir, ec)
	if !climbs {
		return own.key, true
	}
	up := folderArtDirStateOf(ec, parent)
	if !up.seen {
		return "", false
	}
	if up.key == "" {
		return own.key, true
	}
	return own.key + "|" + up.key, true
}

// recordFolderArtKey stamps t with the folder-art key its row records: the
// folder's key (folderArtKey), folderArtUnsettledKey when a directory could
// not be seen or the lookup could not read a cover it looked at, so that a
// later scan goes back to it, or folderArtNotLookedKey when the extraction
// never reached the artwork pipeline. Runs once an extraction is done,
// whichever art it was given: an embedded picture wins over the folder's, and
// its row still records the folder's key, which is what the gate compares.
func recordFolderArtKey(absPath string, t *Track, ec *ExtractContext) {
	if t.localArt == localArtNotLooked {
		t.folderArtKey = folderArtNotLookedKey
		return
	}
	key, seen := folderArtKey(absPath, ec)
	if !seen || t.localArt == localArtUnsettled {
		key = folderArtUnsettledKey
	}
	t.folderArtKey = key
}

// folderArtFor is the folder-art lookup of dir for this scan: the candidates
// its state names, read, judged and cached once per directory per scan
// (ec.FolderArtCache, single-flighted: the first worker reads, the others
// wait for its answer). The state is taken first (folderArtDirStateOf), so
// no row records an identity newer than its cover.
func folderArtFor(ec *ExtractContext, dir string) folderArtResult {
	st := folderArtDirStateOf(ec, dir)
	lookup := func() folderArtResult {
		res := scanFolderArtwork(dir, st.names, ec.ArtworkCacheDir, ec.readArt)
		if !st.seen && res.failure == nil {
			// A candidate the lookup could not stat, or a directory it
			// could not list: whatever the others gave, the answer may
			// change once it can see them.
			res.failure = st.failure
		}
		return res
	}
	if ec.FolderArtCache == nil {
		return lookup() // one caller, nothing to share
	}
	promiseI, _ := ec.FolderArtCache.LoadOrStore(dir, &folderArtPromise{})
	promise := promiseI.(*folderArtPromise)
	promise.once.Do(func() { promise.res = lookup() })
	return promise.res
}

// folderArtUnreadable returns the read that did not complete when the folder
// art of the track at absPath still cannot be read: its directory's lookup,
// or its disc folder's parent's when the lookup climbs there. The skip gate
// asks it of a row that recorded folderArtUnsettledKey before re-extracting
// the row, so a cover that stays unreadable costs a scan one failed read per
// folder, never a tag read of every track beside it; the answer is the
// scan's own lookup, which the extraction then reuses.
func folderArtUnreadable(absPath string, ec *ExtractContext) error {
	dir := filepath.Dir(absPath)
	own := folderArtFor(ec, dir)
	if own.failure != nil {
		return own.failure
	}
	if own.found {
		return nil
	}
	parent, climbs := discArtParent(dir, ec)
	if !climbs {
		return nil
	}
	return folderArtFor(ec, parent).failure
}

// folderArtReadIncomplete reports whether err, from stat'ing or reading a
// folder-art candidate, is a read that did not complete: anything but the
// candidate being gone (ErrNotExist) or not being a file (fsutil.NotAFile),
// which are answers about the path.
func folderArtReadIncomplete(err error) bool {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return fsutil.NotAFileKind(err) == ""
}

// folderArtDrifted is the skip gate's question for an unchanged audio file
// about its folder's art: has a cover been added, replaced or removed since
// the row was written, or can one the row's extraction could not read be read
// now? A folder that cannot be seen answers no: the row is kept as it was,
// and a later scan asks again. rel names the file in the scan's line.
func (s *Scanner) folderArtDrifted(abs, rel string, st *TrackStat, ec *ExtractContext) bool {
	if s.artDir == "" || st.FolderArtKey == folderArtNotLookedKey {
		// No local-art pipeline, or a row no cover can change.
		return false
	}
	key, seen := folderArtKey(abs, ec)
	if !seen || key == st.FolderArtKey {
		return false
	}
	if st.FolderArtKey == folderArtUnsettledKey {
		if failure := folderArtUnreadable(abs, ec); failure != nil {
			s.artUnread.note(rel, failure)
			return false
		}
	}
	return true
}

// isLocalArtworkMBID reports whether mbid is the scanner's own sentinel for
// a cover it read (`local-<sha256>`, stampLocalArtwork), which no other
// writer of ArtworkMBID produces: the enricher writes MusicBrainz ids only,
// and never over a `local-` value.
func isLocalArtworkMBID(mbid string) bool {
	return strings.HasPrefix(mbid, localArtworkFilePrefix)
}

// keepArtOfUnsettledRead gives fresh, an extraction whose local-art lookup
// could not read a cover it looked at (localArtUnsettled), the art its row
// had, when it had any: an answer given without that cover is not one, and
// taking it would change the row twice, once now and once when the cover
// reads. The row records folderArtUnsettledKey, so a later scan reads the
// cover again. A row that had no art takes what the extraction found.
func keepArtOfUnsettledRead(fresh *Track, oldArtworkMBID string) {
	if fresh.localArt == localArtUnsettled && oldArtworkMBID != "" {
		fresh.ArtworkMBID = oldArtworkMBID
	}
}
