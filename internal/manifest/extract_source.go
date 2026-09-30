package manifest

import (
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
// (ExtractContext.openAudio, from Scanner.openAudio).
func openAudioFile(absPath string, ec *ExtractContext) (extractSource, error) {
	if ec != nil && ec.openAudio != nil {
		return ec.openAudio(absPath)
	}
	f, _, err := fsutil.OpenAsFile(absPath)
	if err != nil {
		return nil, err // never a nil *os.File inside a non-nil interface
	}
	return f, nil
}
