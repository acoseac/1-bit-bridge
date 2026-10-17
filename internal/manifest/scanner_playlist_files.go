package manifest

import (
	"cmp"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// playlistSighting is one playlist file a walk saw: where it is and the
// walk's stat of it (through a link, as for a track). unreadable is a file
// the walk could not stat (its own stat failed, or a link's target could
// not be stat'ed): "we could not see this path", so its row, if it has one,
// is kept as it is and it is not read.
type playlistSighting struct {
	pathInfo
	unreadable bool
	statErr    error
}

// notePlaylistFile records a walked entry when it is a playlist file
// (isPlaylistFileName), and reports whether it was one, so the walk takes
// it no further: a playlist file is never a track. The entry is judged as
// an audio file is (walkedFileInfo): one that is not a file (a named pipe,
// a link to a directory or a device) is not recorded, so a row at its path
// is missing like a removed file's.
func notePlaylistFile(abs, rel string, d fs.DirEntry, through func() (fs.FileInfo, error), playlists map[string]playlistSighting) bool {
	if !isPlaylistFileName(d.Name()) {
		return false
	}
	info, verdict, err := walkedFileInfo(d.Type(), d.Info, through)
	switch verdict {
	case walkedFileNotAFile:
	case walkedFileStatFailed, walkedFileTargetUnreadable:
		playlists[rel] = playlistSighting{pathInfo: pathInfo{abs: abs, rel: rel}, unreadable: true, statErr: err}
	default:
		playlists[rel] = playlistSighting{pathInfo: pathInfo{abs: abs, rel: rel, info: info}}
	}
	return true
}

// errPlaylistFileChanged is a read that did not end where the opened file's
// stat said it would: the file was written while it was read.
var errPlaylistFileChanged = errors.New("the file changed while it was read")

// readPlaylistFile reads the playlist file at abs, opened as a file
// (fsutil.OpenAsFile, so a path swapped for a named pipe since the walk is
// refused and nothing waits on it), and parses it. The size cap reads the
// OPENED file's stat: a file of PlaylistFileMaxBytes or more is not read,
// and one with more than playlistFileMaxEntries entry lines keeps none;
// both are kept too large. A read that does not end exactly at that size is
// an error: only a completed read is written, so a failed one leaves the
// row as it was (the B134 rule for a track). The stat it returns is the
// opened file's; the pass stores the walk's instead (indexPlaylistFiles).
func readPlaylistFile(abs, rel string) (playlistFileRead, error) {
	f, info, err := fsutil.OpenAsFile(abs)
	if err != nil {
		return playlistFileRead{}, err
	}
	defer f.Close()
	r := playlistFileRead{path: rel, size: info.Size(), mtimeNs: info.ModTime().UnixNano()}
	if info.Size() >= PlaylistFileMaxBytes {
		r.tooLarge = true
		return r, nil
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(f, data); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return playlistFileRead{}, errPlaylistFileChanged
		}
		return playlistFileRead{}, err
	}
	var more [1]byte
	if n, _ := f.Read(more[:]); n > 0 {
		return playlistFileRead{}, errPlaylistFileChanged
	}
	lines, tooMany := parsePlaylistLines(data)
	if tooMany {
		r.tooLarge = true
		return r, nil
	}
	r.lines = lines
	return r, nil
}

// playlistReadWorkers bounds the reads one pass runs at once. A first scan
// reads every playlist file in the library, and on a network mount each
// read is a round trip.
const playlistReadWorkers = 8

// playlistWriteBatch bounds one write transaction: this many reads, or
// playlistWriteBatchBytes of lines, whichever comes first. The pass reads a
// window of the same bounds, by the walk's sizes, and writes it before it
// reads the next (nextPlaylistReadWindow), so it never holds more than a
// window of parsed files, however many the library has.
const (
	playlistWriteBatch      = 256
	playlistWriteBatchBytes = 16 << 20
)

// readPlaylistFiles reads toRead, a few at a time, and returns each completed
// read, in toRead's order, beside the failures.
func (s *Scanner) readPlaylistFiles(ctx context.Context, toRead []playlistSighting) ([]playlistFileRead, []playlistReadFailure) {
	read := s.readPlaylist
	if read == nil {
		read = readPlaylistFile
	}
	type result struct {
		r   playlistFileRead
		err error
	}
	results := make([]result, len(toRead))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(playlistReadWorkers, len(toRead)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if err := ctx.Err(); err != nil {
					results[i].err = err
					continue
				}
				results[i].r, results[i].err = read(toRead[i].abs, toRead[i].rel)
			}
		}()
	}
	for i := range toRead {
		next <- i
	}
	close(next)
	wg.Wait()
	var reads []playlistFileRead
	var failed []playlistReadFailure
	for i, res := range results {
		if res.err != nil {
			failed = append(failed, playlistReadFailure{rel: toRead[i].rel, err: res.err})
			continue
		}
		reads = append(reads, res.r)
	}
	return reads, failed
}

// playlistReadFailure is a read the pass could not complete.
type playlistReadFailure struct {
	rel string
	err error
}

// indexPlaylistFiles brings the playlist-file index in a scan's scope up to
// date with what its walk saw (seen), after the scan's track and folder
// deletion passes. scope is "" for a full scan and ScanSubtree's relScope
// for a subtree scan; it bounds the rows the pass may count missing, as the
// tracks' snapshot does.
//
// A file seen with the size and mtime its row was read at, by the current
// parser, is not read again. Any other is read, and a completed read
// replaces the row; a read that failed leaves the row as it was (or adds
// none) and is logged once for the pass. A file the walk could not stat is
// kept as it is. A row the walk did not see is counted missing and deleted
// at the scanner's threshold, after the same grace as a track, unless it
// sits under a subtree the walk could not see (errorSubtrees, which also
// carries the clean-empty root guard's sentinel) or it is the old spelling
// of a case-only rename this pass read: the tracks' rules, in their order.
//
// full is a full scan's pass. One that ends with every file its walk saw
// backed (read, or kept as it was) marks the index complete
// (playlistFilesCompleteKey); a subtree scan's pass never does.
//
// Every file the walk saw also gets this pass's evidence about its folder
// (playlistFolderEvidence, from the audio files the walk saw, tracks, and
// the rows the scan stands behind, backed), the album-folder rule's gate: a
// read file's row takes it with the read, a kept row through
// markPlaylistFolders. A row the walk did not see keeps its mark: this
// pass learned nothing of its folder, and its folder's rows are spared or
// counted missing, never added to.
func (s *Scanner) indexPlaylistFiles(ctx context.Context, scope string, full bool, seen map[string]playlistSighting,
	errorSubtrees, tracks map[string]struct{}, backed *backedPaths) {
	if ctx.Err() != nil {
		return
	}
	before, err := s.store.playlistFileStampsUnder(ctx, scope)
	if err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			scanLogger.Error("playlist files pass: list existing", "err", failure)
		}
		return
	}
	p := &playlistPass{s: s, before: before, seen: seen, backed: newBackedPaths(),
		whole: playlistFolderEvidence(seen, errorSubtrees, tracks, backed)}
	toRead := p.plan()
	for start := 0; start < len(toRead) && ctx.Err() == nil; {
		end := nextPlaylistReadWindow(toRead, start)
		p.write(ctx, p.read(ctx, toRead[start:end]))
		start = end
	}
	p.report()
	if ctx.Err() != nil {
		return
	}
	if _, err := s.store.markPlaylistFolders(ctx, p.markWhole, p.markPartial); err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			scanLogger.Error("playlist files pass: folder evidence", "err", failure)
		}
	}
	if !p.settle(ctx, errorSubtrees) || !full || !p.allSeenBacked() {
		return
	}
	if err := s.store.markPlaylistFilesComplete(ctx); err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			scanLogger.Error("playlist files pass: mark complete", "err", failure)
		}
	}
}

// allSeenBacked reports whether every playlist file the walk saw has a row
// this pass stands behind: one it read and wrote, or one it kept.
func (p *playlistPass) allSeenBacked() bool {
	p.backed.mu.Lock()
	defer p.backed.mu.Unlock()
	for rel := range p.seen {
		if _, ok := p.backed.m[rel]; !ok {
			return false
		}
	}
	return true
}

// playlistPass is one run of indexPlaylistFiles: the stored rows in scope
// (before), what the walk saw (seen), the rows this pass can stand behind
// (backed: kept or written, for the case-only rename reap), the rows
// whose missing count it clears without a write (reset), what its one
// line each reports (report): the files it could not read, and the ones
// past a cap with the first of them, and the folder evidence (whole, by
// folder) with the kept rows whose stored mark it changes (markWhole,
// markPartial).
type playlistPass struct {
	s               *Scanner
	before          map[string]playlistFileStamp
	seen            map[string]playlistSighting
	backed          *backedPaths
	reset           []string
	unread          walkTally
	tooLarge        int
	tooLargeExample string
	whole           map[string]bool
	markWhole       []string
	markPartial     []string
}

// playlistFolderEvidence is, for the folder of each playlist file the walk
// saw, whether the walk saw that folder whole, the album-folder rule's gate
// (the app's B421: judge a playlist only on whole evidence about its
// folder). A folder is whole when no errored subtree covers it (a read of it
// or of a folder above it that failed or was cut short, a root the
// clean-empty guard spared), nothing directly in it errored (an unreadable
// link, a subfolder whose read failed: counted against the folder, the safe
// way), and every audio file the walk saw directly in it has a row this scan
// stands behind (backed). A new file the worker could not read, one that was
// no longer a file, or one whose row did not land is a file the index does
// not count, and the folder's indexed tracks would under-count it. An SACD
// container is not asked: its rows sit under it, not in its folder.
func playlistFolderEvidence(playlists map[string]playlistSighting, errorSubtrees, tracks map[string]struct{}, backed *backedPaths) map[string]bool {
	whole := make(map[string]bool)
	for rel := range playlists {
		dir := playlistFileDir(rel)
		if _, judged := whole[dir]; !judged {
			whole[dir] = !isUnderErroredSubtree(dir, errorSubtrees)
		}
	}
	for sub := range errorSubtrees {
		if dir := playlistFileDir(sub); whole[dir] {
			whole[dir] = false
		}
	}
	backed.mu.Lock()
	defer backed.mu.Unlock()
	for rel := range tracks {
		dir := playlistFileDir(rel)
		if !whole[dir] || strings.EqualFold(path.Ext(rel), ".iso") {
			continue
		}
		if _, ok := backed.m[rel]; !ok {
			whole[dir] = false
		}
	}
	return whole
}

// nextPlaylistReadWindow is the end of the read window that starts at
// toRead[start]: playlistWriteBatch files, or playlistWriteBatchBytes of
// them by the walk's sizes, whichever comes first, and always at least one.
// A file at or past PlaylistFileMaxBytes costs nothing: it is not read.
func nextPlaylistReadWindow(toRead []playlistSighting, start int) int {
	end, size := start, int64(0)
	for end < len(toRead) && end-start < playlistWriteBatch {
		n := toRead[end].info.Size()
		if n >= PlaylistFileMaxBytes {
			n = 0
		}
		if end > start && size+n > playlistWriteBatchBytes {
			break
		}
		size += n
		end++
	}
	return end
}

// report logs, once for the pass, the files it could not read and the
// ones past a cap.
func (p *playlistPass) report() {
	p.unread.report(msgPlaylistFilesUnread, "err")
	if p.tooLarge > 0 {
		scanLogger.Info(msgPlaylistFilesTooLarge, "files", p.tooLarge, "example", p.tooLargeExample,
			"maxBytes", PlaylistFileMaxBytes, "maxEntries", playlistFileMaxEntries)
	}
}

// keep backs the stored row at rel, as it is, and clears its missing count:
// the walk saw the file, so it is not missing, whatever the read did. Its
// folder's evidence is this pass's, as for a file read.
func (p *playlistPass) keep(rel string) {
	st, had := p.before[rel]
	if !had {
		return
	}
	p.backed.add(rel)
	if st.missingCount > 0 {
		p.reset = append(p.reset, rel)
	}
	switch whole := p.whole[playlistFileDir(rel)]; {
	case whole && !st.folderWhole:
		p.markWhole = append(p.markWhole, rel)
	case !whole && st.folderWhole:
		p.markPartial = append(p.markPartial, rel)
	}
}

// plan returns the files to read, in path order: every seen file but one
// whose row was read at its size and mtime by the current parser, and one
// the walk could not stat, both kept as they are.
func (p *playlistPass) plan() []playlistSighting {
	var toRead []playlistSighting
	var unseen walkTally
	for rel, sg := range p.seen {
		st, had := p.before[rel]
		switch {
		case sg.unreadable:
			unseen.note(rel, walkErrReason(sg.statErr))
			p.keep(rel)
		case had && st.parserVersion >= playlistFileParserVersion &&
			st.size == sg.info.Size() && st.mtimeNs == sg.info.ModTime().UnixNano():
			p.keep(rel)
		default:
			toRead = append(toRead, sg)
		}
	}
	unseen.report(msgPlaylistFilesUnseen, "err")
	sort.Slice(toRead, func(i, j int) bool { return toRead[i].rel < toRead[j].rel })
	return toRead
}

// read reads toRead and returns the completed reads, each under the WALK's
// stat, which is what the next walk compares, as a track's row is. The
// opened file's can differ (a file written between the walk and the read,
// or a Windows directory entry whose metadata lags the file's): stored, it
// would read the file again on every scan, where the walk's converges after
// one more read. A read that failed keeps the stored row, and the pass logs
// it once (report).
func (p *playlistPass) read(ctx context.Context, toRead []playlistSighting) []playlistFileRead {
	reads, failed := p.s.readPlaylistFiles(ctx, toRead)
	walked := make(map[string]fs.FileInfo, len(toRead))
	for _, sg := range toRead {
		walked[sg.rel] = sg.info
	}
	for i := range reads {
		if info := walked[reads[i].path]; info != nil {
			reads[i].size, reads[i].mtimeNs = info.Size(), info.ModTime().UnixNano()
		}
		reads[i].folderWhole = p.whole[playlistFileDir(reads[i].path)]
	}
	for _, f := range failed {
		if ctx.Err() == nil {
			p.unread.note(f.rel, walkErrReason(f.err))
		}
		p.keep(f.rel)
	}
	return reads
}

// write stores reads, a batch per transaction (nextPlaylistBatch). A batch
// that does not land is not backed: a rename whose new spelling it held
// keeps its old row, and the next scan reads its files again. The files past
// a cap that landed are counted for the pass's one line (report).
func (p *playlistPass) write(ctx context.Context, reads []playlistFileRead) {
	for start := 0; start < len(reads); {
		end := nextPlaylistBatch(reads, start)
		batch := reads[start:end]
		start = end
		if !p.writeBatch(ctx, batch) {
			continue
		}
		for _, r := range batch {
			if r.tooLarge {
				p.tooLarge++
				p.tooLargeExample = cmp.Or(p.tooLargeExample, r.path)
			}
		}
	}
}

// nextPlaylistBatch is the end of the write batch that starts at
// reads[start]: playlistWriteBatch reads, or playlistWriteBatchBytes of
// lines, whichever comes first, and always at least one read.
func nextPlaylistBatch(reads []playlistFileRead, start int) int {
	end, size := start, 0
	for end < len(reads) && end-start < playlistWriteBatch && size < playlistWriteBatchBytes {
		for _, l := range reads[end].lines {
			size += len(l) + 1
		}
		end++
	}
	return end
}

// writeBatch stores one batch and backs its rows, and reports whether it
// landed.
func (p *playlistPass) writeBatch(ctx context.Context, batch []playlistFileRead) bool {
	if _, err := p.s.store.writePlaylistFiles(ctx, batch); err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			scanLogger.Error("playlist files pass: write", "err", failure, "files", len(batch))
		}
		return false
	}
	for _, r := range batch {
		p.backed.add(r.path)
	}
	return true
}

// settle is the deletion step: a row the walk did not see is spared under a
// subtree it could not see, reaped at once as the old spelling of a
// case-only rename this pass backs, and otherwise counted missing, in the
// tracks' order (the errored-subtree check first, before anything that can
// delete).
func (p *playlistPass) settle(ctx context.Context, errorSubtrees map[string]struct{}) bool {
	beforeSet := make(map[string]struct{}, len(p.before))
	for rel := range p.before {
		beforeSet[rel] = struct{}{}
	}
	seenSet := make(map[string]struct{}, len(p.seen))
	for rel := range p.seen {
		seenSet[rel] = struct{}{}
	}
	renames := caseOnlyRenames(beforeSet, seenSet)
	backedFolds := backedSeenFolds(renames, seenSet, p.backed)
	var missing, renamed []string
	spared := 0
	for rel := range p.before {
		switch {
		case p.isSeen(rel):
		case isUnderErroredSubtree(rel, errorSubtrees):
			spared++
		case reapCaseRename(rel, renames, backedFolds):
			renamed = append(renamed, rel)
		default:
			missing = append(missing, rel)
		}
	}
	sort.Strings(p.reset)
	sort.Strings(missing)
	sort.Strings(renamed)
	threshold := p.s.effectiveDeleteThreshold()
	deleted, err := p.s.store.settlePlaylistFiles(ctx, p.reset, missing, renamed, threshold)
	if err != nil {
		if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
			scanLogger.Error("playlist files pass: missing count", "err", failure, "missing", len(missing))
		}
		return false
	}
	if spared > 0 {
		scanLogger.Warn("spared playlist files from deletion pass (parent walk error)",
			"spared", spared, "subtrees", len(errorSubtrees))
	}
	if len(missing) > 0 || len(renamed) > 0 {
		scanLogger.Info("playlist files missing this scan",
			"missing", len(missing), "renamed", len(renamed), "deleted", deleted, "threshold", threshold)
	}
	return true
}

func (p *playlistPass) isSeen(rel string) bool {
	_, ok := p.seen[rel]
	return ok
}

// The lines a playlist pass logs, once each, for the files it could not see
// or read and for the ones past a size cap. Library-relative examples only.
const (
	msgPlaylistFilesUnseen   = "playlist files the walk could not stat; their rows are kept"
	msgPlaylistFilesUnread   = "playlist files the scan could not read; their rows are kept"
	msgPlaylistFilesTooLarge = "playlist files past the size cap; their entries are not served"
)
