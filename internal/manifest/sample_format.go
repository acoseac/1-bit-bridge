package manifest

import (
	"context"
	"database/sql"
	"os"
)

// The values Track.SampleFormat and Variant.SampleFormat carry (backlog B513).
const (
	SampleFormatInteger = "integer"
	SampleFormatFloat   = "float"
)

// SampleFormatResolver maps a manifest path to the file it names.
type SampleFormatResolver func(rel string) (abs string, info os.FileInfo, err error)

// SampleFormatBackfill counts what one BackfillSampleFormats pass did.
type SampleFormatBackfill struct {
	Read       int
	Gained     int
	None       int
	Unreadable int
	Changed    int
}

// BackfillSampleFormats is a scaffold.
func (s *Store) BackfillSampleFormats(ctx context.Context, resolve SampleFormatResolver) (SampleFormatBackfill, error) {
	return SampleFormatBackfill{}, nil
}

// SampleFormatReady is a scaffold.
func (s *Store) SampleFormatReady(ctx context.Context) (bool, error) {
	return false, nil
}

// readHeaderSampleFormat is a scaffold.
func readHeaderSampleFormat(src extractSource, ext string) (string, error) {
	return "", nil
}

// spliceSampleFormat is a scaffold.
func spliceSampleFormat(t *Track, col sql.NullString) {}

// The fill, candidate and readiness statements: scaffolds.
const (
	sampleFormatUndecidedRowidsSQL = ``
	sampleFormatFillSQL            = ``
	sampleFormatCandidatesSQL      = ``
	sampleFormatReadySQL           = ``
)
