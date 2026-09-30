package manifest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// extractSource is what an extractor reads its audio file through: the
// *os.File fsutil.OpenAsFile opens, or what a scanner test's opener hands
// back in its place (Scanner.openAudio).
type extractSource interface {
	io.ReadSeeker
	io.ReaderAt
	io.Closer
	Stat() (fs.FileInfo, error)
}

// openAudioFile opens the audio file at absPath for an extractor: as a file
// (fsutil.OpenAsFile), or through the opener a scanner test installed
// (ExtractContext.openAudio, from Scanner.openAudio). Under
// ExtractWithContext the file comes back as a faultNotingSource, and an open
// that failed is noted too, unless it failed because the path is not a file
// (fsutil.NotAFileKind): that is an answer about the path, which the scanner
// gives first (notAFileNow), not a failure to read it.
//
// Every extractor opens its audio file here, once, and reads it through what
// this returns: that is what lets ExtractWithContext say whether the file was
// read whole, whatever each parser does with a failed read (dhowden, the MP4
// walks and the FLAC format read log or drop theirs and read on).
func openAudioFile(absPath string, ec *ExtractContext) (extractSource, error) {
	var faults *readFaults
	if ec != nil {
		faults = ec.reads
	}
	src, err := openAudioSource(absPath, ec)
	if err != nil {
		if fsutil.NotAFileKind(err) == "" {
			faults.note(err)
		}
		return nil, err
	}
	if faults == nil {
		return src, nil
	}
	return &faultNotingSource{src: src, faults: faults}, nil
}

// openAudioSource is openAudioFile's open, before the wrap.
func openAudioSource(absPath string, ec *ExtractContext) (extractSource, error) {
	if ec != nil && ec.openAudio != nil {
		return ec.openAudio(absPath)
	}
	f, _, err := fsutil.OpenAsFile(absPath)
	if err != nil {
		return nil, err // never a nil *os.File inside a non-nil interface
	}
	return f, nil
}

// readFaults holds the first failure of the reads of one extraction's audio
// file that did not complete. ExtractWithContext makes one per call.
type readFaults struct {
	first error
}

// note keeps err when it is the extraction's first failure. A nil receiver
// keeps nothing: an extractor called outside ExtractWithContext (a test's
// direct call) has no record.
func (r *readFaults) note(err error) {
	if r != nil && r.first == nil {
		r.first = err
	}
}

// faultNotingSource is an extractor's audio file that notes, in its
// extraction's readFaults, every read, seek or stat of it that did not
// complete, and answers each exactly as the file does.
//
// Two answers complete a read though they are errors, because they say what
// the file holds: the end of the file (io.EOF, io.ErrUnexpectedEOF), which a
// truncated file gives, and an offset the OS refuses before reading anything
// (a seek to before the start, seekOffsetRefused; a ReadAt at a negative
// offset), which a parser asks for when it computes the offset from a
// malformed file's bytes (a DSF metadata pointer with its top bit set, and
// dhowden's ID3v1 look 128 bytes back from the end of a file shorter than
// that). Such a file is read whole, and its row is what it holds, written as
// it always was. Anything else, an EIO, ESTALE or ETIMEDOUT from a NAS, a
// Windows lock or cloud-file error, is a read of the file that did not
// complete, and what the extractors made of the rest is not the file's.
type faultNotingSource struct {
	src    extractSource
	faults *readFaults
}

func (f *faultNotingSource) Read(p []byte) (int, error) {
	n, err := f.src.Read(p)
	if err != nil && !endOfFile(err) {
		f.faults.note(err)
	}
	return n, err
}

func (f *faultNotingSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.src.ReadAt(p, off)
	if err != nil && off >= 0 && !endOfFile(err) {
		f.faults.note(err)
	}
	return n, err
}

func (f *faultNotingSource) Seek(offset int64, whence int) (int64, error) {
	pos, err := f.src.Seek(offset, whence)
	if err != nil && !seekOffsetRefused(err) {
		f.faults.note(err)
	}
	return pos, err
}

func (f *faultNotingSource) Stat() (fs.FileInfo, error) {
	info, err := f.src.Stat()
	if err != nil {
		f.faults.note(err)
	}
	return info, err
}

func (f *faultNotingSource) Close() error { return f.src.Close() }

// endOfFile reports whether err says a read reached the end of the file.
func endOfFile(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// readIncompleteError is ExtractWithContext's answer for a file it could not
// read whole: its open, or a read, seek or stat of it, failed with something
// other than the answers faultNotingSource counts as complete. err is the
// first such failure.
type readIncompleteError struct {
	err error
}

// Error goes through fmt, which answers for an error whose own Error panics
// (an *fs.PathError without a cause; see walkErrReason).
func (e *readIncompleteError) Error() string {
	return fmt.Sprintf("the file could not be read: %v", e.err)
}

func (e *readIncompleteError) Unwrap() error { return e.err }

// readFault returns the failure that kept ExtractWithContext from reading its
// file whole, and nil for any other error, one about what the file holds (a
// parser's refusal of a file it read), or for nil.
func readFault(err error) error {
	var ri *readIncompleteError
	if errors.As(err, &ri) {
		return ri.err
	}
	return nil
}
