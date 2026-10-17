package manifest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sort"
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
// playlistWriteBatchBytes of lines, whichever comes first.
const (
	playlistWriteBatch      = 256
	playlistWriteBatchBytes = 16 << 20
)

// readPlaylistFiles reads todo, a few at a time, and returns each completed
// read, in todo's order, beside the failures.
func (s *Scanner) readPlaylistFiles(ctx context.Context, todo []playlistSighting) ([]playlistFileRead, []playlistReadFailure) {
	read := s.readPlaylist
	if read == nil {
		read = readPlaylistFile
	}
	type result struct {
		r   playlistFileRead
		err error
	}
	results := make([]result, len(todo))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(playlistReadWorkers, len(todo)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if err := ctx.Err(); err != nil {
					results[i].err = err
					continue
				}
				results[i].r, results[i].err = read(todo[i].abs, todo[i].rel)
			}
		}()
	}
	for i := range todo {
		next <- i
	}
	close(next)
	wg.Wait()
	var reads []playlistFileRead
	var failed []playlistReadFailure
	for i, res := range results {
		if res.err != nil {
			failed = append(failed, playlistReadFailure{rel: todo[i].rel, err: res.err})
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
func (s *Scanner) indexPlaylistFiles(ctx context.Context, scope string, seen map[string]playlistSighting, errorSubtrees map[string]struct{}) {
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
	p := &playlistPass{s: s, before: before, seen: seen, backed: newBackedPaths()}
	todo := p.plan()
	p.write(ctx, p.read(ctx, todo))
	if ctx.Err() != nil {
		return
	}
	p.settle(ctx, errorSubtrees)
}

// playlistPass is one run of indexPlaylistFiles: the stored rows in scope
// (before), what the walk saw (seen), the rows this pass can stand behind
// (backed: kept or written, for the case-only rename reap), and the rows
// whose missing count it clears without a write (reset).
type playlistPass struct {
	s      *Scanner
	before map[string]playlistFileStamp
	seen   map[string]playlistSighting
	backed *backedPaths
	reset  []string
}

// keep backs the stored row at rel, as it is, and clears its missing count:
// the walk saw the file, so it is not missing, whatever the read did.
func (p *playlistPass) keep(rel string) {
	st, had := p.before[rel]
	if !had {
		return
	}
	p.backed.add(rel)
	if st.missingCount > 0 {
		p.reset = append(p.reset, rel)
	}
}

// plan returns the files to read, in path order: every seen file but one
// whose row was read at its size and mtime by the current parser, and one
// the walk could not stat, both kept as they are.
func (p *playlistPass) plan() []playlistSighting {
	var todo []playlistSighting
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
			todo = append(todo, sg)
		}
	}
	unseen.report(msgPlaylistFilesUnseen, "err")
	sort.Slice(todo, func(i, j int) bool { return todo[i].rel < todo[j].rel })
	return todo
}

// read reads todo and returns the completed reads, each under the WALK's
// stat, which is what the next walk compares, as a track's row is. The
// opened file's can differ (a file written between the walk and the read,
// or a Windows directory entry whose metadata lags the file's): stored, it
// would read the file again on every scan, where the walk's converges after
// one more read. A read that failed keeps the stored row, logged once.
func (p *playlistPass) read(ctx context.Context, todo []playlistSighting) []playlistFileRead {
	reads, failed := p.s.readPlaylistFiles(ctx, todo)
	walked := make(map[string]fs.FileInfo, len(todo))
	for _, sg := range todo {
		walked[sg.rel] = sg.info
	}
	for i := range reads {
		if info := walked[reads[i].path]; info != nil {
			reads[i].size, reads[i].mtimeNs = info.Size(), info.ModTime().UnixNano()
		}
	}
	var unread walkTally
	for _, f := range failed {
		if ctx.Err() == nil {
			unread.note(f.rel, walkErrReason(f.err))
		}
		p.keep(f.rel)
	}
	unread.report(msgPlaylistFilesUnread, "err")
	return reads
}

// write stores reads, a batch per transaction (playlistWriteBatch reads or
// playlistWriteBatchBytes of lines). A batch that does not land is not
// backed: a rename whose new spelling it held keeps its old row, and the
// next scan reads its files again.
func (p *playlistPass) write(ctx context.Context, reads []playlistFileRead) {
	tooLarge, example := 0, ""
	for start := 0; start < len(reads); {
		end, size := start, 0
		for end < len(reads) && end-start < playlistWriteBatch && size < playlistWriteBatchBytes {
			for _, l := range reads[end].lines {
				size += len(l) + 1
			}
			end++
		}
		batch := reads[start:end]
		start = end
		if _, err := p.s.store.writePlaylistFiles(ctx, batch); err != nil {
			if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
				scanLogger.Error("playlist files pass: write", "err", failure, "files", len(batch))
			}
			continue
		}
		for _, r := range batch {
			p.backed.add(r.path)
			if r.tooLarge {
				tooLarge++
				if example == "" {
					example = r.path
				}
			}
		}
	}
	if tooLarge > 0 {
		scanLogger.Info(msgPlaylistFilesTooLarge, "files", tooLarge, "example", example,
			"maxBytes", PlaylistFileMaxBytes, "maxEntries", playlistFileMaxEntries)
	}
}

// settle is the deletion step: a row the walk did not see is spared under a
// subtree it could not see, reaped at once as the old spelling of a
// case-only rename this pass backs, and otherwise counted missing, in the
// tracks' order (the errored-subtree check first, before anything that can
// delete).
func (p *playlistPass) settle(ctx context.Context, errorSubtrees map[string]struct{}) {
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
	if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
		scanLogger.Error("playlist files pass: missing count", "err", failure, "missing", len(missing))
		return
	}
	if spared > 0 {
		scanLogger.Warn("spared playlist files from deletion pass (parent walk error)",
			"spared", spared, "subtrees", len(errorSubtrees))
	}
	if len(missing) > 0 || len(renamed) > 0 {
		scanLogger.Info("playlist files missing this scan",
			"missing", len(missing), "renamed", len(renamed), "deleted", deleted, "threshold", threshold)
	}
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
