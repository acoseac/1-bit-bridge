package transcode

// The version of its source a rendition records, and the check every render
// makes against the file on disk.
//
// A JobSpec carries the version of the source its track ROW records
// (SourceMTimeNS / SourceSize); every writer stamps the row, since the
// auto-optimize sweeper and the album gain judge a rendition against the row
// while the serve path judges it against the file (cmd/bridge's
// rendition_stamp.go). Between a change to a file and the scan that reads it
// the two disagree, and a render then reads bytes its stamp does not
// describe: the serve path refuses the result (410 variant_stale), and
// nothing renders it again, since a rendition of the family is listed
// whether or not it is fresh (backlog B53).
//
// So each enqueuer queues a render only while the file still matches its
// row, and Run checks again twice: before it starts, for a file that changed
// while its job waited in a queue (minutes behind a DSD render, hours behind
// a library-wide batch or `bridge render` run), and before it publishes, for
// one that changed while it rendered. Either answers ErrSourceChanged and
// publishes nothing. The pool strikes no source for it (the file is not bad,
// only newer than its row) and asks for a rescan of its directory
// (Pool.SetSourceRescan), so the next request finds the row current.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// SourceIsAtRow reports whether the file on disk is still the version its
// track row records. It is the scanner's own test for a changed file (its
// skip gate compares size and mtime exactly), so a file this answers false
// for is one the next scan re-reads. The serve path's 2 s tolerance does not
// belong here: that tolerance is about stamps taken through different
// mounts, and a row and a stat of the same file are taken through one.
//
// It is the one check every render entry point makes: the on-demand path,
// the auto-optimize sweeper and the CLI in cmd/bridge, the batch walks here,
// and Run itself.
func SourceIsAtRow(info os.FileInfo, rowMTimeNS, rowSize int64) bool {
	return info.Size() == rowSize && info.ModTime().UnixNano() == rowMTimeNS
}

// ErrSourceChanged is Run's answer for a source that is no longer the
// version its spec records. It is a fact about the file's version, not a
// defect in the file: the pool strikes nothing for it and asks for a rescan
// instead, and a render of the same file succeeds once a scan has read the
// change. Classified by type (errors.Is), never by its message.
var ErrSourceChanged = errors.New("the file changed on disk after its last scan")

// sourceChanged stats the source and answers ErrSourceChanged, wrapped with
// `what` happened to the render, unless the file verifiably is the version
// the spec records. A file that is no longer there is a change of version
// like any other, and a file whose stat fails (a stale NFS handle, a FUSE
// mount answering ENOTCONN, an I/O error, a permission lost) cannot be
// checked, so it is not rendered either. Rendered on, the tool failed on the
// input and the pool struck the source: a mount that dropped under a queued
// batch struck every file behind it, and three strikes suppress a file for
// 30 days (Gemini on #1093). Every enqueuer stats the file before it queues
// the job, so a stat that fails here is one that went bad while the job
// waited, and a file that stays unreadable is refused at its next enqueue,
// not re-offered here. A rescan of a directory the scanner cannot see
// touches no row.
//
// os.Stat, following a link, as the scanner's and every enqueuer's stat do:
// a linked file compares its target with its target.
func (j JobSpec) sourceChanged(what string) error {
	info, err := os.Stat(j.SourceAbsPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: it is no longer there, %s", ErrSourceChanged, what)
	}
	if err != nil {
		// The cause without the *fs.PathError's absolute path: this message
		// reaches the batch row and the log, which name library files
		// library-relative (#1055).
		cause := err
		var pe *fs.PathError
		if errors.As(err, &pe) {
			cause = pe.Err
		}
		return fmt.Errorf("%w: it could not be checked (%v), %s", ErrSourceChanged, cause, what)
	}
	if SourceIsAtRow(info, j.SourceMTimeNS, j.SourceSize) {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrSourceChanged, what)
}
