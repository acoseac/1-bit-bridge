package manifest

// A hybrid WavPack file (.wv) is lossy on its own; the correction file
// beside it (.wvc) is what makes its decode lossless (backlog B489). The
// correction file is a dependency of its .wv's row, never a track: the walk
// never indexes one (it is not in Ext), and a row reads one only for a
// hybrid stream.
//
// What a hybrid row records about its correction file is one of four
// observations, kept apart:
//
//   - confirmed absence: the folder was listed whole and holds no correction
//     file for it (or holds only something that is not a file under its
//     name, which is no correction file, as a directory called cover.jpg is
//     no cover). The row is WAVPACK-LOSSY.
//   - a matching file: one whose first audio block is a correction block of
//     this stream and whose last block ends the file at the stream's last
//     sample (correctionMatches). The row is WAVPACK, lossless, and carries
//     the file on the wire (Track.Correction).
//   - an invalid or mismatched file: one there and read whole that is not
//     (another file's, cut short, not WavPack). The row is WAVPACK-LOSSY with
//     no correction, and the extraction says so once: the row records the
//     file's stat, so a later scan reads it again only once it changes.
//   - an incomplete observation: a folder that could not be listed, a stat or
//     a read that failed, a file replaced while it was read. Nothing is
//     concluded: the extraction is a read that did not complete
//     (readIncompleteError), so the row keeps what it had and a new file gets
//     none until a scan sees the correction file whole, as an incomplete read
//     of the audio does (backlog B134).
//
// The skip gate asks a row that recorded an observation whether it still
// holds (correctionDrifted): a correction file appearing, vanishing, or
// changing its size or mtime re-reads the .wv, though the .wv did not change.
// The comparison is the walk's own for an audio file (statUnchanged: the size
// and the mtime to the nanosecond, as the filesystem reports them on each
// read), never a looser one, and a folder that cannot be seen answers no:
// the row stays, and the next scan asks again.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// correctionExt is the correction file's extension, compared without regard
// to case, as the walk compares an audio file's.
const correctionExt = ".wvc"

// msgCorrectionMismatch is the line a correction file that is not its
// stream's (another stream's, cut short, not WavPack) gets, once for each of
// its stats.
const msgCorrectionMismatch = "wavpack: the correction file beside a hybrid file does not match it; indexing the file as lossy"

// correctionState is the kind of observation a row records.
type correctionState byte

const (
	// correctionNone: the row depends on no correction file (it is not a
	// hybrid WavPack stream).
	correctionNone     correctionState = 0
	correctionAbsent   correctionState = '-'
	correctionMatched  correctionState = '+'
	correctionRejected correctionState = '!'
)

// correctionObservation is what one look at a .wv's correction file found:
// its state, and for a file there, its name and stat.
type correctionObservation struct {
	state   correctionState
	name    string
	size    int64
	mtimeNS int64
}

// key is the observation as the row records it (tracks.correction_key,
// v61): "" for none, "-" for confirmed absence, and for a file the state,
// its size, its mtime in nanoseconds and its name, colon-separated, the name
// last since a name may hold a colon.
func (o correctionObservation) key() string {
	switch o.state {
	case correctionNone:
		return ""
	case correctionAbsent:
		return "-"
	}
	return string(o.state) + strconv.FormatInt(o.size, 10) + ":" + strconv.FormatInt(o.mtimeNS, 10) + ":" + o.name
}

// parseCorrectionKey reads a recorded key back, and false for one this build
// never writes (which the gate then reads as drifted, so the row is read
// again and records one it does).
func parseCorrectionKey(key string) (correctionObservation, bool) {
	switch key {
	case "":
		return correctionObservation{}, true
	case "-":
		return correctionObservation{state: correctionAbsent}, true
	}
	state := correctionState(key[0])
	if state != correctionMatched && state != correctionRejected {
		return correctionObservation{}, false
	}
	parts := strings.SplitN(key[1:], ":", 3)
	if len(parts) != 3 || parts[2] == "" {
		return correctionObservation{}, false
	}
	size, err1 := strconv.ParseInt(parts[0], 10, 64)
	mtime, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return correctionObservation{}, false
	}
	return correctionObservation{state: state, name: parts[2], size: size, mtimeNS: mtime}, true
}

// statUnchanged is the walk's comparison of a file's stat with a row's: the
// same size, and the same mtime to the nanosecond. The skip gate asks it of
// an audio file (runScanWorker) and of a correction file
// (correctionDrifted), so the two cannot come to disagree. Exact, never a
// tolerance: a filesystem that rounds an mtime (FAT32's 2 s, an SMB or NFS
// mount's) rounds it the same way at every read, so an unchanged file reads
// equal, and a tolerance would take a real change inside it for none.
func statUnchanged(size, mtimeNS int64, info fs.FileInfo) bool {
	return size == info.Size() && mtimeNS == info.ModTime().UnixNano()
}

// errCorrectionReplaced is a correction file that changed between the stat
// that named it and its read: the observation is incomplete.
var errCorrectionReplaced = errors.New("the correction file changed while it was read")

// correctionCandidate is the correction file a folder's listing names for
// the .wv called base: an entry whose name is the .wv's stem and the
// correction extension, the stem compared exactly and the extension without
// regard to case, as the walk names a track (two spellings of a name are two
// files; an extension's case is not). Of several, the first in the listing's
// order (by name), so every look takes the same one.
func correctionCandidate(l *sidecarListing, base string) string {
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	for _, name := range l.wvcNames {
		if strings.TrimSuffix(name, filepath.Ext(name)) == stem {
			return name
		}
	}
	return ""
}

// lookAtCorrection is the stat-only half of an observation, which the
// extraction and the skip gate share so they cannot answer differently: the
// folder's listing (memoized per scan), the candidate in it, and its stat.
// absent is confirmed absence (no candidate, one gone since the listing, or
// one that is not a file); err is a look that did not complete (a folder
// that could not be listed, a stat that failed otherwise).
func lookAtCorrection(absWV string, ec *ExtractContext) (name string, info fs.FileInfo, absent bool, err error) {
	dir := filepath.Dir(absWV)
	l := dirListingFor(ec, dir)
	if !l.listed {
		return "", nil, false, errFolderNotListed
	}
	name = correctionCandidate(l, filepath.Base(absWV))
	if name == "" {
		return "", nil, true, nil
	}
	info, err = os.Stat(filepath.Join(dir, name))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil, true, nil
	case err != nil:
		return "", nil, false, err
	case fsutil.NotAFile(info.Mode()) != "":
		return "", nil, true, nil
	}
	return name, info, false, nil
}

// observeCorrection looks at the correction file of the hybrid stream wv in
// the .wv at absWV: confirmed absence, a matching file, or one there that is
// not (each an answer), or an error, an observation that did not complete.
// The file is opened as a file (fsutil.OpenAsFile, or the scanner's test
// seam), and its stat is compared before and after its read with the stat
// that named it, so a file replaced meanwhile is no answer.
func observeCorrection(absWV string, wv wvStream, ec *ExtractContext) (correctionObservation, error) {
	name, info, absent, err := lookAtCorrection(absWV, ec)
	if err != nil {
		return correctionObservation{}, err
	}
	if absent {
		return correctionObservation{state: correctionAbsent}, nil
	}
	obs := correctionObservation{state: correctionRejected, name: name,
		size: info.Size(), mtimeNS: info.ModTime().UnixNano()}
	full := filepath.Join(filepath.Dir(absWV), name)
	f, err := openCorrectionFile(full, ec)
	if err != nil {
		// Gone, or replaced by something else, between its stat and its open:
		// the look did not complete. Something that is not a file now is that
		// replacement, never the .wv's own kind (notAFileNow reads the
		// extraction's error as the audio file's).
		if fsutil.NotAFileKind(err) != "" {
			return correctionObservation{}, errCorrectionReplaced
		}
		return correctionObservation{}, err
	}
	defer f.Close()
	if err := sameCorrectionStat(f, obs); err != nil {
		return correctionObservation{}, err
	}
	wvc, readErr := readWavPackCorrection(f, obs.size)
	if readErr != nil && isReadFailure(readErr) {
		return correctionObservation{}, readErr
	}
	if err := sameCorrectionStat(f, obs); err != nil {
		return correctionObservation{}, err
	}
	// Any other error is an answer about what the file holds: no block a
	// decoder takes, a block cut short or malformed. The file is not this
	// stream's correction file.
	if readErr == nil && correctionMatches(wv, wvc) {
		obs.state = correctionMatched
	}
	return obs, nil
}

// sameCorrectionStat compares the opened correction file's stat with the one
// that named it.
func sameCorrectionStat(f extractSource, obs correctionObservation) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !statUnchanged(obs.size, obs.mtimeNS, info) {
		return errCorrectionReplaced
	}
	return nil
}

// isReadFailure reports whether err, from reading a correction file, came
// from the file rather than from a parse of what it holds: an *fs.PathError
// (an open, read, seek or stat that failed) or an errno. The end of the file
// is never one: the readers answer it as a file cut short.
func isReadFailure(err error) bool {
	var pe *fs.PathError
	var errno syscall.Errno
	return errors.As(err, &pe) || errors.As(err, &errno)
}

// openCorrectionFile opens a correction file as a file: never a named pipe,
// a device or a directory, which fsutil.OpenAsFile refuses at once. The
// scanner's test seam (ExtractContext.openCorrection) opens it in its place.
func openCorrectionFile(full string, ec *ExtractContext) (extractSource, error) {
	if ec != nil && ec.openCorrection != nil {
		return ec.openCorrection(full)
	}
	f, _, err := fsutil.OpenAsFile(full)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// applyCorrection records an observation on t: the key the row records, and
// for a matching file, the file on the wire, by its library-relative path
// beside the .wv's and its stat in the Track's own unit and format. A
// rejected file is said once, here: the row records its stat, and the skip
// gate goes back to the .wv only once that changes.
func applyCorrection(t *Track, obs correctionObservation) {
	t.correctionKey = obs.key()
	t.Correction = nil
	switch obs.state {
	case correctionMatched:
		rel := obs.name
		if dir := path.Dir(t.Path); t.Path != "" && dir != "." {
			rel = dir + "/" + obs.name
		}
		t.Correction = &CorrectionFile{Path: rel, Size: obs.size, ModTime: time.Unix(0, obs.mtimeNS).UTC()}
	case correctionRejected:
		scanLogger.Warn(msgCorrectionMismatch, "path", t.Path, "correction", obs.name)
	}
}

// correctionDrifted is the skip gate's question for an unchanged .wv about
// its correction file: has one appeared, vanished, or changed its size or
// mtime since the row recorded what it saw? A row that depends on none
// (correctionNone) is never asked; a folder or a file that cannot be seen
// now answers no, and the next scan asks again.
func correctionDrifted(abs string, st *TrackStat, ec *ExtractContext) bool {
	if st.CorrectionKey == "" {
		return false
	}
	stored, ok := parseCorrectionKey(st.CorrectionKey)
	if !ok {
		return true
	}
	name, info, absent, err := lookAtCorrection(abs, ec)
	if err != nil {
		return false
	}
	if absent || stored.state == correctionAbsent {
		return absent != (stored.state == correctionAbsent)
	}
	return name != stored.name || !statUnchanged(stored.size, stored.mtimeNS, info)
}

// String names an observation in a test's failure.
func (o correctionObservation) String() string {
	return fmt.Sprintf("%c %s %d %d", rune(o.state), o.name, o.size, o.mtimeNS)
}
