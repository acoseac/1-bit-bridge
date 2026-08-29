package manifest

import (
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
)

// UploadDuplicateRef is one existing track that looks like an incoming upload.
type UploadDuplicateRef struct {
	// Key is the "<lowercased basename>\x00<size>" the caller matched on.
	Key string
	// Path is the existing library-relative path.
	Path string
}

// FindTracksByBasenameAndSize reports which of the given (basename, size) pairs
// already exist in the library.
//
// This backs the upload pre-flight, which warns an operator that an album folder
// they are about to upload looks like one they already have — the case an
// artist-folder upload overlapping an earlier album-folder upload produces,
// where the two land at DIFFERENT paths so nothing collides and both copies
// survive on disk.
//
// Matching on (basename, size) is deliberately cheap: no hashing, and no read
// of the incoming bytes. It is a hint the UI presents, never a decision — a
// track legitimately appearing on both an album and a compilation is a real
// library, and that case is serve-time duplicate suppression's job.
//
// The size filter is not index-backed, so this is a table scan. That is
// acceptable because it runs ONCE per upload session — the cost class of the
// click-driven admin walks, not of anything per-chunk. Do not call it per file.
func (s *Store) FindTracksByBasenameAndSize(ctx context.Context, sizes []int64, basenames map[string][]string) ([]UploadDuplicateRef, error) {
	if len(sizes) == 0 || len(basenames) == 0 {
		return nil, nil
	}
	// One bound JSON array rather than a placeholder-concatenated IN list:
	// the same shape ResetEnrichedByArtistMBIDs uses, which keeps the
	// statement a true constant (go:S2077) and dodges the bind ceiling.
	blob, err := json.Marshal(sizes)
	if err != nil {
		return nil, err
	}
	const q = `
		SELECT path, size
		FROM tracks
		WHERE size IN (SELECT value FROM json_each(?))`
	rows, err := s.db.QueryContext(ctx, q, string(blob))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []UploadDuplicateRef
	for rows.Next() {
		var p string
		var size int64
		if err := rows.Scan(&p, &size); err != nil {
			return nil, err
		}
		key := uploadDupeKey(path.Base(p), size)
		if _, ok := basenames[key]; ok {
			out = append(out, UploadDuplicateRef{Key: key, Path: p})
		}
	}
	return out, rows.Err()
}

// UploadDupeKey builds the match key. Exported so the caller composes the same
// key it will look results up by; a private twin is a second thing to keep in
// step for no gain.
func UploadDupeKey(basename string, size int64) string { return uploadDupeKey(basename, size) }

func uploadDupeKey(basename string, size int64) string {
	var b strings.Builder
	b.Grow(len(basename) + 24)
	// Case-folded so a re-upload from a case-insensitive filesystem still
	// matches. NUL-joined so a basename containing the separator cannot
	// forge a different key.
	b.WriteString(strings.ToLower(basename))
	b.WriteByte(0)
	b.WriteString(strconv.FormatInt(size, 10))
	return b.String()
}

// TotalTrackBytes is the summed size of every indexed track.
//
// It answers "what is my library using" for the console's space widget. Every
// row counts, including duplicate-suppressed ones: a suppressed copy is still
// occupying the volume, which is precisely the number an operator deciding what
// to delete needs to see.
func (s *Store) TotalTrackBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM tracks`).Scan(&total)
	return total, err
}
