package analyze

import (
	"io"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// containerDuration is the duration a DSF or an uncompressed DFF names
// in its own header. DST, a file that is neither, a header that cannot
// be read, and a DSF header that fails Check return false so the caller
// keeps ffprobe's duration.
func containerDuration(path string) (float64, bool) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".dsf":
		return dsfContainerDuration(path)
	case ".dff":
		return dffContainerDuration(path)
	default:
		return 0, false
	}
}

func dsfContainerDuration(path string) (float64, bool) {
	f, err := openContainer(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	h, err := manifest.ReadDSFAudioHeader(f)
	if err != nil {
		return 0, false
	}
	if err := h.Check(); err != nil {
		return 0, false
	}
	d := h.DurationSec()
	if d <= 0 {
		return 0, false
	}
	return d, true
}

func dffContainerDuration(path string) (float64, bool) {
	f, err := openContainer(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	h, err := manifest.ReadDFFAudioHeader(f)
	if err != nil {
		return 0, false
	}
	return h.DurationSec()
}

func openContainer(path string) (io.ReadSeekCloser, error) {
	f, _, err := fsutil.OpenAsFile(path)
	return f, err
}
