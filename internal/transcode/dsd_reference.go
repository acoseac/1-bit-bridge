package transcode

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// dsdReferenceDuration is the completeness reference for the file being
// decoded. A DSF is the fmt sample count over the sampling frequency. An
// uncompressed DFF is the DSD chunk's declared payload. A file whose
// leading bytes are not that container keeps ffprobe when it reported
// one and the manifest otherwise, as DST, a DFF payload of no bytes, a
// compression other than absent or DSD, and every other source do.
func (j JobSpec) dsdReferenceDuration(probed float64) (float64, error) {
	switch strings.ToLower(filepath.Ext(j.SourceAbsPath)) {
	case ".dsf":
		return j.dsfReferenceDuration(probed)
	case ".dff":
		return j.dffReferenceDuration(probed)
	default:
		return dsdExpectedDurationSec(probed, j.SourceDurationSec), nil
	}
}

func (j JobSpec) dsfReferenceDuration(probed float64) (float64, error) {
	f, _, err := fsutil.OpenAsFile(j.SourceAbsPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h, err := manifest.ReadDSFAudioHeader(f)
	if errors.Is(err, manifest.ErrContainerHeaderAbsent) {
		return dsdExpectedDurationSec(probed, j.SourceDurationSec), nil
	}
	if err != nil {
		return 0, err
	}
	if err := h.Check(); err != nil {
		return 0, fmt.Errorf("%w (%s)", err, j.SourceLibraryRel)
	}
	return h.DurationSec(), nil
}

func (j JobSpec) dffReferenceDuration(probed float64) (float64, error) {
	f, _, err := fsutil.OpenAsFile(j.SourceAbsPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h, err := manifest.ReadDFFAudioHeader(f)
	if errors.Is(err, manifest.ErrContainerHeaderAbsent) {
		return dsdExpectedDurationSec(probed, j.SourceDurationSec), nil
	}
	if err != nil {
		return 0, fmt.Errorf("%w (%s)", err, j.SourceLibraryRel)
	}
	if d, ok := h.DurationSec(); ok {
		return d, nil
	}
	return dsdExpectedDurationSec(probed, j.SourceDurationSec), nil
}
