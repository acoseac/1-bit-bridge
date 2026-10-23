package manifest

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
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
// A row nothing has decided is NULL. Every open first clears a kind that
// describes another version of its file (sampleFormatClearStaleSQL), then
// decides, without reading a file, each row a codec decides
// (sampleFormatFillSQL); the header pass (BackfillSampleFormats) reads the
// header of each WAV and AIFF row left. Every write of a kind records the
// size and mtime of the file it was decided from (sample_format_size,
// sample_format_mtime_ns), which are the row's own then; a writer that does
// not know the columns, a binary from before them run after this one, moves
// the row's stat and leaves the kind, which the next open clears.
//
// The backfill is delivered once: no write it makes advances indexed_at
// until a header pass has run to its end with every row the fill decides
// decided, which records the completion mark (sampleFormatBackfillKey), and
// /v1/health advertises sampleFormat from then on, so a client's one full
// sync on seeing the key carries every kind recorded before it. A kind the
// backfill gives a row after the mark (a file the finishing pass could not
// read, a row a rolled-back binary wrote) advances indexed_at, so a delta
// carries it.

// sampleFormatFillRowidsSQL is the open-time fill's inner select: the next
// chunk of undecided rows the fill decides, after the path the last chunk
// ended on, in path order, which idx_tracks_sample_format_null (on path)
// answers in. The range starts past every row an earlier chunk passed, the
// rows it decided and the WAV and AIFF rows it left to the header pass
// alike, so a fill reads each undecided row once: a chunk that started again
// at the first undecided row read the header pass's rows again in every
// chunk.
const sampleFormatFillRowidsSQL = `SELECT rowid FROM tracks
		WHERE sample_format IS NULL
		  AND path > ?
		  AND NOT (COALESCE(codec, '') IN ('WAV', 'AIFF')
		       AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path))
		ORDER BY path
		LIMIT ?`

// sampleFormatFillSQL decides one chunk of rows by their codec, reading no
// file: FLAC and ALAC are integer-only, so a FLAC or ALAC row read from a
// file, and not DSD, is "integer"; every other row it reaches (a lossy codec,
// DSD, a compressed WAV or AIFF-C, an AIFF-C whose compression is unknown, no
// codec, a UPnP-routed row, whose header the bridge never reads and whose
// codec its upstream's file name gave) states no kind, "". Each records the
// row's size and mtime as the stat its kind was decided at. Its inner select
// is sampleFormatFillRowidsSQL, verbatim. It returns the rows it decided, so
// the chunk knows where it ended and which rows gained a kind; it assigns no
// indexed_at itself (advanceGainedSampleFormatsTx). Binds (after, chunk).
const sampleFormatFillSQL = `UPDATE tracks SET
		sample_format = CASE
			WHEN COALESCE(codec, '') IN ('FLAC', 'ALAC') AND COALESCE(is_dsd, 0) = 0
			     AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path)
			THEN 'integer'
			ELSE ''
		END,
		sample_format_size = size,
		sample_format_mtime_ns = mtime_ns
	WHERE rowid IN (
		SELECT rowid FROM tracks
		WHERE sample_format IS NULL
		  AND path > ?
		  AND NOT (COALESCE(codec, '') IN ('WAV', 'AIFF')
		       AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path))
		ORDER BY path
		LIMIT ?
	)
	RETURNING path, sample_format`

// sampleFormatFillChunk bounds one transaction of the open-time fill, as
// firstIndexedBackfillChunk does: a library whose rows are all undecided does
// not grow the WAL by the whole table.
const sampleFormatFillChunk = 2000

// sampleFormatClearStaleSQL clears one chunk of kinds whose recorded stat is
// not the row's: a writer that does not know sample_format (the upsert of a
// binary from before it) rewrote the row for a file that changed and left
// the kind, which then describes the bytes before. The row is undecided
// again, and the fill or the header pass decides it. Its inner select is
// idx_tracks_sample_format_stale's predicate, verbatim, and a cleared row
// leaves that index, so a chunk never reads one twice. Binds (chunk).
const sampleFormatClearStaleSQL = `UPDATE tracks
	SET sample_format = NULL, sample_format_size = NULL, sample_format_mtime_ns = NULL
	WHERE rowid IN (
		SELECT rowid FROM tracks
		WHERE sample_format IS NOT NULL
		  AND (sample_format_size IS NOT size OR sample_format_mtime_ns IS NOT mtime_ns)
		LIMIT ?
	)`

// sampleFormatBackfillKey is the scan_state row that marks the backfill
// complete: a header pass ran to its end with every row the open-time fill
// decides decided. Its value is when (UnixNano). Never removed.
const sampleFormatBackfillKey = "sample_format_backfill_complete_ns"

// sampleFormatMarkCompleteSQL records the completion mark, once, and only
// while no row the open-time fill decides is undecided: a FLAC row a
// rolled-back binary inserted after this open waits for the next open's
// fill. Binds (key, UnixNano).
const sampleFormatMarkCompleteSQL = `INSERT OR IGNORE INTO scan_state(k, v)
	SELECT ?, ?
	 WHERE NOT EXISTS (
	       SELECT 1 FROM tracks
	        WHERE sample_format IS NULL
	          AND NOT (COALESCE(codec, '') IN ('WAV', 'AIFF')
	               AND NOT EXISTS (SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path)))`

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

// sampleFormatDecideSQL records the kind a header read found, with the stat
// it was read at, only while the row is still undecided and still describes
// the bytes that were read: a scan that wrote the row since has decided it,
// and a row whose size or mtime has moved describes another version of the
// file. Binds (kind, path, size, mtime_ns).
const sampleFormatDecideSQL = `UPDATE tracks
	SET sample_format = ?, sample_format_size = size, sample_format_mtime_ns = mtime_ns
	WHERE path = ? AND sample_format IS NULL AND size = ? AND mtime_ns = ?`

// sampleFormatPage is how many rows the header pass reads before it writes
// what it found, in one transaction.
const sampleFormatPage = 200

// clearStaleSampleFormats runs at the end of every migrate, before the fill:
// it makes undecided again, a chunk per statement, every row whose kind was
// decided at another size or mtime than the row's (sampleFormatClearStaleSQL).
// Only a writer that does not know the column leaves such a row, so on a
// store this binary alone has written it reads an empty index and changes
// nothing. It assigns no indexed_at: the decision that follows does.
func (s *Store) clearStaleSampleFormats(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.db.ExecContext(ctx, sampleFormatClearStaleSQL, sampleFormatFillChunk)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
	}
}

// backfillSampleFormatByCodec runs the open-time fill (sampleFormatFillSQL)
// until no row it decides is left. It runs at the end of every migrate, so
// the first open of a library indexed before the column decides every FLAC
// and ALAC row, and a row a rolled-back binary inserted, or one whose stale
// kind clearStaleSampleFormats cleared, is decided when this binary opens the
// store again. It reads no file. Before the backfill's completion mark it
// assigns no indexed_at: the one full sync a client makes when the health key
// appears carries these kinds, and a delta of the whole library is what that
// avoids. After the mark, a row it gives a kind advances indexed_at.
func (s *Store) backfillSampleFormatByCodec(ctx context.Context) error {
	return s.fillSampleFormatsByCodec(ctx, sampleFormatFillChunk)
}

// fillSampleFormatsByCodec is the open-time fill, chunk rows a transaction,
// each chunk starting after the path the last one ended on.
func (s *Store) fillSampleFormatsByCodec(ctx context.Context, chunk int) error {
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		last, n, err := s.fillSampleFormatChunk(ctx, after, chunk)
		if err != nil || n == 0 {
			return err
		}
		after = last
	}
}

// fillSampleFormatChunk decides one chunk in one transaction and answers the
// last path it decided and how many rows it decided. The rows that gained a
// kind advance indexed_at only once the backfill is marked complete
// (advanceGainedSampleFormatsTx), in the same transaction as their kind.
func (s *Store) fillSampleFormatChunk(ctx context.Context, after string, chunk int) (string, int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, sampleFormatFillSQL, after, chunk)
	if err != nil {
		return "", 0, err
	}
	var last string
	var n int
	var gained []string
	for rows.Next() {
		var path, kind string
		if err := rows.Scan(&path, &kind); err != nil {
			_ = rows.Close()
			return "", 0, err
		}
		n++
		if path > last {
			last = path
		}
		if kind != "" {
			gained = append(gained, path)
		}
	}
	if err := rows.Close(); err != nil {
		return "", 0, err
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	advanced, err := advanceGainedSampleFormatsTx(ctx, tx, gained, s.now().UnixNano())
	if err != nil {
		return "", 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, err
	}
	if advanced {
		s.noteLibrary(ctx)
	}
	return last, n, nil
}

// sampleFormatMarkReader is the read surface the completion mark is asked on:
// the store's pool, or a transaction.
type sampleFormatMarkReader interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// sampleFormatBackfillComplete reports whether the completion mark is set.
func sampleFormatBackfillComplete(ctx context.Context, q sampleFormatMarkReader) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM scan_state WHERE k = ?`, sampleFormatBackfillKey).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// advanceGainedSampleFormatsTx advances indexed_at, through
// bumpIndexedAtByPathSQL, for each path the backfill just gave a kind, once
// the backfill is marked complete: the one full sync a client made on seeing
// the health key is past, so only a delta carries a kind decided now. Before
// the mark it moves nothing, since that full sync is still to come. A row
// decided as having none keeps its place: a client already reads it as
// having no kind. It reports whether it advanced any.
func advanceGainedSampleFormatsTx(ctx context.Context, tx dbTx, paths []string, now int64) (bool, error) {
	if len(paths) == 0 {
		return false, nil
	}
	complete, err := sampleFormatBackfillComplete(ctx, tx)
	if err != nil || !complete {
		return false, err
	}
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx, bumpIndexedAtByPathSQL, now, p); err != nil {
			return false, err
		}
	}
	return true, nil
}

// markSampleFormatBackfillComplete records the completion mark at the end of
// a header pass that ran to its end (sampleFormatMarkCompleteSQL). It reports
// whether this call recorded it: false once it is set, and false while a row
// the open-time fill decides is still undecided.
func (s *Store) markSampleFormatBackfillComplete(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, sampleFormatMarkCompleteSQL, sampleFormatBackfillKey, strconv.FormatInt(s.now().UnixNano(), 10))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SampleFormatReady reports whether the backfill is complete (its completion
// mark is set), which is when /v1/health may advertise sampleFormat: every
// row that can have a kind has it, so a client's one full sync on seeing the
// key carries them all. The mark survives a restart and is never removed; a
// kind decided after it reaches a client by a delta. A failed read is not
// ready.
func (s *Store) SampleFormatReady(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, sql.ErrConnDone
	}
	return sampleFormatBackfillComplete(ctx, s.db)
}

// SampleFormatResolver maps a manifest path to the file it names: its absolute
// path and its stat, as the API's resolver does (fs.Resolver.ResolveChecked,
// a link followed). A manifest path names its own root, so it is never joined
// onto one.
type SampleFormatResolver func(rel string) (abs string, info os.FileInfo, err error)

// SampleFormatBackfill is what one pass of BackfillSampleFormats did: Read
// headers read whole, of which Gained rows got a kind (each advancing its
// indexed_at once the backfill is marked complete) and None were found to
// state none; Unreadable rows whose file the resolver could not map or stat,
// or that would not open or read whole; and Changed rows whose file is not
// the one the row describes. The last two stay undecided for a later pass.
// Completed is set on the one pass that marked the backfill complete: it ran
// to its end with every row the open-time fill decides decided.
type SampleFormatBackfill struct {
	Read       int
	Gained     int
	None       int
	Unreadable int
	Changed    int
	Completed  bool
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
// sample kind is still undecided (a row from before tracks.sample_format
// existed, one a rolled-back binary inserted, or one whose stale kind the
// open cleared) and records the kind each states. It is the backfill's file half,
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
// The write records that size and mtime with the kind. A file that cannot be
// read, or that has changed, leaves its row undecided: serve runs a pass at
// boot and after every successful scan, and a scan writes a changed file's
// row with its kind.
//
// A pass that runs to its end, its last page read, marks the backfill
// complete (markSampleFormatBackfillComplete) unless a row the open-time fill
// decides is still undecided; a pass the shutdown stops, or that fails, marks
// nothing, and the next pass starts again. Until the mark, the rows a pass
// decides keep their indexed_at, and the one full sync a client makes when
// /v1/health then advertises sampleFormat carries them; after it, a row that
// gains a kind advances its indexed_at, through bumpIndexedAtByPathSQL, so a
// delta carries it. A row whose header states none never moves.
func (s *Store) BackfillSampleFormats(ctx context.Context, resolve SampleFormatResolver) (SampleFormatBackfill, error) {
	var res SampleFormatBackfill
	after := ""
	for {
		page, err := s.sampleFormatCandidates(ctx, after)
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			res.Completed, err = s.markSampleFormatBackfillComplete(ctx)
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
// describes the bytes read (sampleFormatDecideSQL). The rows that gained a
// kind advance their indexed_at only once the backfill is marked complete
// (advanceGainedSampleFormatsTx); a row whose header states none keeps the
// place a client's cursor has for it.
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
	var gained []string
	var none int
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
			// A scan wrote the row since the page was read, or the row
			// moved to another version of its file.
		case r.kind == "":
			none++
		default:
			gained = append(gained, r.path)
		}
	}
	advanced, err := advanceGainedSampleFormatsTx(ctx, tx, gained, s.now().UnixNano())
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	res.Gained += len(gained)
	res.None += none
	if advanced {
		s.noteLibrary(ctx)
	}
	return nil
}

// writeSampleFormatTx records value as path's sample kind on the
// version-stale stamp leg (StampExtractorVersionBatch), where the file read
// is the one the row describes, with the row's size and mtime as the stat it
// was read at: when the row holds another value, or the same one recorded at
// another stat, it takes this one, and when a client would see the change
// (no kind to a kind, a kind to another or to none) the row's indexed_at
// advances, through bumpIndexedAtByPathSQL. NULL and "" both serve as no
// kind, so deciding an undecided row whose header states none moves nothing.
// A path with no row writes nothing. It reports whether it advanced
// indexed_at.
func writeSampleFormatTx(ctx context.Context, tx dbTx, path, value string, now int64) (bool, error) {
	var stored sql.NullString
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT sample_format,
			COALESCE(sample_format_size = size AND sample_format_mtime_ns = mtime_ns, 0)
		FROM tracks WHERE path = ?`, path).Scan(&stored, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if stored.Valid && stored.String == value && current {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tracks
		SET sample_format = ?, sample_format_size = size, sample_format_mtime_ns = mtime_ns
		WHERE path = ?`, value, path); err != nil {
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
