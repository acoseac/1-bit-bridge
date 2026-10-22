package manifest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The values Track.SampleFormat and Variant.SampleFormat carry (backlog
// B513): what a PCM header states its samples are. A row whose header states
// neither carries none, never one of these by default.
const (
	SampleFormatInteger = "integer"
	SampleFormatFloat   = "float"
)

// knownSampleFormat is v when it is one of the two kinds, else "".
func knownSampleFormat(v string) string {
	switch v {
	case SampleFormatInteger, SampleFormatFloat:
		return v
	}
	return ""
}

// ksDataFormatSubtypePCM and ksDataFormatSubtypeIEEEFloat are the
// WAVE_FORMAT_EXTENSIBLE subformat GUIDs of integer PCM and IEEE float
// (ksmedia.h's KSDATAFORMAT_SUBTYPE_PCM, 00000001-0000-0010-8000-00AA00389B71,
// and KSDATAFORMAT_SUBTYPE_IEEE_FLOAT, 00000003-0000-0010-8000-00AA00389B71),
// as a fmt chunk holds them: Data1, Data2 and Data3 little-endian, Data4 as
// written.
var (
	ksDataFormatSubtypePCM       = [16]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}
	ksDataFormatSubtypeIEEEFloat = [16]byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}
)

// wavSampleFormat is the sample kind a WAV fmt chunk body states: integer for
// WAVE_FORMAT_PCM, float for WAVE_FORMAT_IEEE_FLOAT, and for
// WAVE_FORMAT_EXTENSIBLE the kind its subformat GUID names, compared whole:
// KSDATAFORMAT_SUBTYPE_PCM or KSDATAFORMAT_SUBTYPE_IEEE_FLOAT. Every other
// code, an extensible header too short to hold its GUID, and every other GUID
// state none, a GUID whose first two bytes are PCM's code included (the
// Ambisonic B-format subformat's are). wavEncodingOf names the file by those
// two bytes, which is the codec's rule and not this one.
func wavSampleFormat(body []byte) string {
	if len(body) < 2 {
		return ""
	}
	switch binary.LittleEndian.Uint16(body[0:2]) {
	case wavFormatPCM:
		return SampleFormatInteger
	case wavFormatIEEEFloat:
		return SampleFormatFloat
	case wavFormatExtensible:
		if len(body) < 40 {
			return ""
		}
		switch [16]byte(body[24:40]) {
		case ksDataFormatSubtypePCM:
			return SampleFormatInteger
		case ksDataFormatSubtypeIEEEFloat:
			return SampleFormatFloat
		}
	}
	return ""
}

// variantSampleFormat is the sample kind of a rendition of format (a
// track_variants.format value): FLAC is integer-only, and every rendition the
// bridge writes is FLAC. A format it does not know states none.
func variantSampleFormat(format string) string {
	if strings.EqualFold(format, "flac") {
		return SampleFormatInteger
	}
	return ""
}

// sampleFormatColumn is the value a track write stores in
// tracks.sample_format: t's kind, or "" where its header states none. An
// isDSD row stores "" whatever t carries: no PCM kind reaches a DSD row.
func sampleFormatColumn(t *Track) string {
	if t.IsDSD != nil && *t.IsDSD {
		return ""
	}
	return knownSampleFormat(t.SampleFormat)
}

// spliceSampleFormat copies tracks.sample_format onto t.SampleFormat, as every
// reader that serves a Track does (GetTrack, scanListedTrack), after t's
// tags_json is decoded. NULL, "", a value this build does not know, and any
// value on an isDSD row are no kind.
func spliceSampleFormat(t *Track, col sql.NullString) {
	t.SampleFormat = ""
	if !col.Valid || (t.IsDSD != nil && *t.IsDSD) {
		return
	}
	t.SampleFormat = knownSampleFormat(col.String)
}

// The sizes of a format chunk the WAV and AIFF walks read, which the header
// read shares: a fmt or COMM chunk declaring a size outside them is skipped
// whole, and a later one may name the file.
const (
	wavFmtChunkMin   = 16
	wavFmtChunkMax   = 1 << 10
	aiffCOMMChunkMin = 18
	aiffCOMMChunkMax = 1 << 10
)

// readHeaderSampleFormat reads the sample kind from the header of a WAV or
// AIFF file the way its extractor reads it, and nothing else: ext picks the
// walk as extractByFormat's dispatch does, the walk steps over every chunk
// but the format chunk without reading it (an ID3 tag and its pictures, a
// LIST, the audio), and the first format chunk of a size the extractor reads
// names the file, as in extractWAVWithContext and extractAIFFWithContext. A
// file that is not its format, or that ends before such a chunk, states no
// kind: "", nil. An error is a read or a seek that did not complete, which
// says nothing about the file. TestTheHeaderReadAgreesWithTheExtractor holds
// it to the extractor on every shape.
func readHeaderSampleFormat(src extractSource, ext string) (string, error) {
	switch ext {
	case ".wav":
		return readWAVHeaderSampleFormat(src)
	case ".aif", ".aiff", ".aifc":
		return readAIFFHeaderSampleFormat(src)
	}
	return "", nil
}

func readWAVHeaderSampleFormat(src extractSource) (string, error) {
	var header [12]byte
	if _, err := io.ReadFull(src, header[:]); err != nil {
		return "", endOfHeader(err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return "", nil
	}
	for {
		var sub [8]byte
		if _, err := io.ReadFull(src, sub[:]); err != nil {
			return "", endOfHeader(err)
		}
		size := binary.LittleEndian.Uint32(sub[4:8])
		if string(sub[0:4]) == "fmt " && size >= wavFmtChunkMin && size <= wavFmtChunkMax {
			body := make([]byte, size)
			if _, err := io.ReadFull(src, body); err != nil {
				return "", endOfHeader(err)
			}
			return wavSampleFormat(body), nil
		}
		if err := seekPastChunk(src, int64(size)); err != nil {
			return "", err
		}
	}
}

func readAIFFHeaderSampleFormat(src extractSource) (string, error) {
	var header [12]byte
	if _, err := io.ReadFull(src, header[:]); err != nil {
		return "", endOfHeader(err)
	}
	formType := string(header[8:12])
	if string(header[0:4]) != "FORM" || (formType != "AIFF" && formType != "AIFC") {
		return "", nil
	}
	for {
		var sub [8]byte
		if _, err := io.ReadFull(src, sub[:]); err != nil {
			return "", endOfHeader(err)
		}
		size := binary.BigEndian.Uint32(sub[4:8])
		if string(sub[0:4]) == "COMM" && size >= aiffCOMMChunkMin && size <= aiffCOMMChunkMax {
			body := make([]byte, size)
			if _, err := io.ReadFull(src, body); err != nil {
				return "", endOfHeader(err)
			}
			return aifcEncodingOf(body, formType).sampleFormat, nil
		}
		if err := seekPastChunk(src, int64(size)); err != nil {
			return "", err
		}
	}
}

// endOfHeader is the header read's answer to a read that failed: nil where
// the file ended (it states no kind), err for any other failure.
func endOfHeader(err error) error {
	if endOfFile(err) {
		return nil
	}
	return err
}

// The backfill of rows indexed before tracks.sample_format (migration v59).
// A row nothing has decided is NULL. Every open decides, without reading a
// file, each row a codec decides (sampleFormatFillSQL), and the header pass
// (BackfillSampleFormats) reads the header of each WAV and AIFF row left.
// The four statements below each carry the partial index's predicate,
// sample_format IS NULL, so once the backfill is done each reads nothing.

// sampleFormatUndecidedRowidsSQL is the open-time fill's inner select: up to a
// chunk of undecided rows that are not the header pass's (a WAV or AIFF row
// read from a file). Unordered: ORDER BY rowid makes SQLite skip
// idx_tracks_sample_format_null (the first-indexed fill's lesson). The header
// pass's rows stay in the index, so a chunk passes over them; once the fill
// is done the select reads only them and returns none.
const sampleFormatUndecidedRowidsSQL = `SELECT rowid FROM tracks
	WHERE sample_format IS NULL
	  AND NOT (COALESCE(codec, '') IN ('WAV', 'AIFF')
	       AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path))
	LIMIT ?`

// sampleFormatFillSQL decides one chunk of rows by their codec, reading no
// file: FLAC and ALAC are integer-only, so a FLAC or ALAC row read from a
// file, and not DSD, is "integer"; every other row it reaches (a lossy codec,
// DSD, a compressed WAV or AIFF-C, an AIFF-C whose compression is unknown, no
// codec, a UPnP-routed row, whose header the bridge never reads and whose
// codec its upstream's file name gave) states no kind, "". Its inner select
// is sampleFormatUndecidedRowidsSQL, verbatim. It assigns no indexed_at.
const sampleFormatFillSQL = `UPDATE tracks SET sample_format = CASE
		WHEN COALESCE(codec, '') IN ('FLAC', 'ALAC') AND COALESCE(is_dsd, 0) = 0
		     AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path)
		THEN 'integer'
		ELSE ''
	END
	WHERE rowid IN (
		SELECT rowid FROM tracks
		WHERE sample_format IS NULL
		  AND NOT (COALESCE(codec, '') IN ('WAV', 'AIFF')
		       AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path))
		LIMIT ?
	)`

// sampleFormatFillChunk bounds one autocommit UPDATE of the open-time fill,
// as firstIndexedBackfillChunk does: a library whose rows are all undecided
// does not grow the WAL by the whole table.
const sampleFormatFillChunk = 2000

// sampleFormatReadySQL counts the undecided rows the open-time fill decides:
// a WAV or AIFF row read from a file is the header pass's and is not counted.
const sampleFormatReadySQL = `SELECT COUNT(*) FROM tracks
	WHERE sample_format IS NULL
	  AND NOT (COALESCE(codec, '') IN ('WAV', 'AIFF')
	       AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path))`

// sampleFormatCandidatesSQL is one page of the header pass: the undecided
// WAV and AIFF rows read from a file, after the path the last page ended on,
// in path order, which the partial index (on path) answers in. A row the
// pass leaves undecided is not read again in that pass.
const sampleFormatCandidatesSQL = `SELECT path, size, mtime_ns FROM tracks
	WHERE sample_format IS NULL
	  AND COALESCE(codec, '') IN ('WAV', 'AIFF')
	  AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path)
	  AND path > ?
	ORDER BY path
	LIMIT ?`

// sampleFormatDecideSQL records the kind a header read found, only while the
// row is still undecided and still describes the bytes that were read: a scan
// that wrote the row since has decided it, or the file has changed. Binds
// (kind, path, size, mtime_ns).
const sampleFormatDecideSQL = `UPDATE tracks SET sample_format = ?
	WHERE path = ? AND sample_format IS NULL AND size = ? AND mtime_ns = ?`

// sampleFormatPage is how many rows the header pass reads before it writes
// what it found, in one transaction.
const sampleFormatPage = 200

// backfillSampleFormatByCodec runs the open-time fill (sampleFormatFillSQL) a
// chunk per statement until no row it decides is left. It runs at the end of
// every migrate, so the first open of a library indexed before the column
// decides every FLAC and ALAC row, and a row a rolled-back binary inserted is
// decided when this binary opens the store again. It reads no file and
// assigns no indexed_at: a client reads these rows through the one-time full
// sync the sampleFormat health key starts (SampleFormatReady, PROTOCOL.md),
// and a delta of the whole library is what that avoids.
func (s *Store) backfillSampleFormatByCodec(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.db.ExecContext(ctx, sampleFormatFillSQL, sampleFormatFillChunk)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// SampleFormatReady reports whether every row the open-time fill decides has
// its kind, which is when /v1/health may advertise sampleFormat. A WAV or
// AIFF row whose header is unread does not hold the key back: the header pass
// advances its indexed_at when it gives it a kind, so a delta carries it. A
// failed count is not ready.
func (s *Store) SampleFormatReady(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, sql.ErrConnDone
	}
	var n int
	if err := s.db.QueryRowContext(ctx, sampleFormatReadySQL).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// SampleFormatResolver maps a manifest path to the file it names: its absolute
// path and its stat, as the API's resolver does (fs.Resolver.ResolveChecked,
// a link followed). A manifest path names its own root, so it is never joined
// onto one.
type SampleFormatResolver func(rel string) (abs string, info os.FileInfo, err error)

// SampleFormatBackfill is what one pass of BackfillSampleFormats did: Read
// headers read whole, of which Gained rows got a kind (each advancing its
// indexed_at) and None were found to state none; Unreadable rows whose file
// the resolver could not map or stat, or that would not open or read whole;
// and Changed rows whose file is not the one the row describes. The last two
// stay undecided for a later pass.
type SampleFormatBackfill struct {
	Read       int
	Gained     int
	None       int
	Unreadable int
	Changed    int
}

// errSampleFormatFileChanged is a header read of a file whose size or mtime
// is not the row's: the bytes read are not the bytes the row describes.
var errSampleFormatFileChanged = errors.New("manifest: the file is not the one its row describes")

// sampleFormatRow is one row the header pass reads: its path, the size and
// mtime the row was written with, and the kind its header states.
type sampleFormatRow struct {
	path    string
	size    int64
	mtimeNS int64
	kind    string
}

// BackfillSampleFormats reads the header of every WAV and AIFF row whose
// sample kind is still undecided, a row from before tracks.sample_format
// existed, and records the kind each states. It is the backfill's file half,
// bounded by design: the rows it reads are the formats a file can hold either
// kind in (a FLAC or ALAC row is decided by its codec at open, and every
// other format holds no PCM kind), it reads each file's header and nothing
// past its format chunk (readHeaderSampleFormat), and a row it decided is not
// read again. No rescan: the rows come from the store, and resolve maps each
// path to its file.
//
// A row is written only from a read of the bytes it describes: the file's
// size and mtime are the row's when the resolver stats it and when the read is
// done, and the write holds only while the row is still undecided and still
// has them (sampleFormatDecideSQL), so a scan that rewrote it meanwhile wins.
// A row that gains a kind advances its indexed_at, through
// bumpIndexedAtByPathSQL, so a client's next delta carries it; one whose
// header states none is recorded as such and does not move. A file that
// cannot be read, or that has changed, leaves its row undecided: serve runs a
// pass at boot and after every successful scan, and a scan writes a changed
// file's row with its kind.
func (s *Store) BackfillSampleFormats(ctx context.Context, resolve SampleFormatResolver) (SampleFormatBackfill, error) {
	var res SampleFormatBackfill
	after := ""
	for {
		page, err := s.sampleFormatCandidates(ctx, after)
		if err != nil || len(page) == 0 {
			return res, err
		}
		after = page[len(page)-1].path
		decided := make([]sampleFormatRow, 0, len(page))
		for _, r := range page {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if kind, ok := readRowSampleFormat(r, resolve, &res); ok {
				r.kind = kind
				decided = append(decided, r)
			}
		}
		if err := s.decideSampleFormats(ctx, decided, &res); err != nil {
			return res, err
		}
	}
}

// sampleFormatCandidates is the page of undecided WAV and AIFF rows after the
// path after.
func (s *Store) sampleFormatCandidates(ctx context.Context, after string) ([]sampleFormatRow, error) {
	rows, err := s.db.QueryContext(ctx, sampleFormatCandidatesSQL, after, sampleFormatPage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []sampleFormatRow
	for rows.Next() {
		var r sampleFormatRow
		if err := rows.Scan(&r.path, &r.size, &r.mtimeNS); err != nil {
			return nil, err
		}
		page = append(page, r)
	}
	return page, rows.Err()
}

// readRowSampleFormat reads r's header through resolve, and answers its kind
// when the file read is the one r describes. A path the resolver cannot map
// or stat, a file that would not open or read whole, and a file that is not
// the one the row describes are counted, and their rows left for a later
// pass.
func readRowSampleFormat(r sampleFormatRow, resolve SampleFormatResolver, res *SampleFormatBackfill) (string, bool) {
	abs, info, err := resolve(r.path)
	if err != nil {
		res.Unreadable++
		return "", false
	}
	if !r.describes(info) {
		res.Changed++
		return "", false
	}
	kind, err := readFileSampleFormat(abs, r)
	switch {
	case errors.Is(err, errSampleFormatFileChanged):
		res.Changed++
		return "", false
	case err != nil:
		res.Unreadable++
		return "", false
	}
	res.Read++
	return kind, true
}

// describes reports whether info is the stat r was written with, the size and
// mtime the scanner's skip gate compares.
func (r sampleFormatRow) describes(info os.FileInfo) bool {
	return info.Size() == r.size && info.ModTime().UnixNano() == r.mtimeNS
}

// readFileSampleFormat opens abs as the extractors open a library file
// (openAudioFile, noting any read that did not complete) and reads its
// header. The open file's stat after the read must still be r's
// (errSampleFormatFileChanged otherwise): the file was replaced or rewritten
// between the resolver's stat and the read.
func readFileSampleFormat(abs string, r sampleFormatRow) (string, error) {
	ec := &ExtractContext{reads: &readFaults{}}
	src, err := openAudioFile(abs, ec)
	if err != nil {
		return "", err
	}
	defer src.Close()
	kind, err := readHeaderSampleFormat(src, strings.ToLower(filepath.Ext(abs)))
	if err == nil {
		err = ec.reads.first
	}
	if err != nil {
		return "", err
	}
	info, err := src.Stat()
	if err != nil {
		return "", err
	}
	if !r.describes(info) {
		return "", errSampleFormatFileChanged
	}
	return kind, nil
}

// decideSampleFormats writes one page's header reads, in one transaction under
// the writer lock: each row only while it is still undecided and still
// describes the bytes read (sampleFormatDecideSQL), and a row that gained a
// kind advances its indexed_at (bumpIndexedAtByPathSQL), so a client's next
// delta carries it. A row whose header states none keeps the place a client's
// cursor has for it.
func (s *Store) decideSampleFormats(ctx context.Context, rows []sampleFormatRow, res *SampleFormatBackfill) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UnixNano()
	var gained, none int
	for _, r := range rows {
		out, err := tx.ExecContext(ctx, sampleFormatDecideSQL, r.kind, r.path, r.size, r.mtimeNS)
		if err != nil {
			return err
		}
		n, err := out.RowsAffected()
		if err != nil {
			return err
		}
		switch {
		case n == 0:
			// A scan wrote the row since the page was read.
		case r.kind == "":
			none++
		default:
			if _, err := tx.ExecContext(ctx, bumpIndexedAtByPathSQL, now, r.path); err != nil {
				return err
			}
			gained++
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	res.Gained += gained
	res.None += none
	if gained > 0 {
		s.noteLibrary(ctx)
	}
	return nil
}

// writeSampleFormatTx records value as path's sample kind on the
// version-stale stamp leg (StampExtractorVersionBatch), where the file read
// is the one the row describes: when the row holds another value, it takes
// this one, and when a client would see the change (no kind to a kind, a kind
// to another or to none) the row's indexed_at advances, through
// bumpIndexedAtByPathSQL. NULL and "" both serve as no kind, so deciding an
// undecided row whose header states none moves nothing. A path with no row
// writes nothing. It reports whether it advanced indexed_at.
func writeSampleFormatTx(ctx context.Context, tx dbTx, path, value string, now int64) (bool, error) {
	var stored sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT sample_format FROM tracks WHERE path = ?`, path).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored.Valid && stored.String == value {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tracks SET sample_format = ? WHERE path = ?`, value, path); err != nil {
		return false, err
	}
	if knownSampleFormat(stored.String) == knownSampleFormat(value) {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, bumpIndexedAtByPathSQL, now, path); err != nil {
		return false, err
	}
	return true, nil
}
