package manifest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// The rows a library held before tracks.sample_format existed get their kind
// without a rescan (backlog B513): what a codec decides on every open, and a
// header read for each WAV and AIFF row, the formats a header must tell.

// rewindToBeforeSampleFormat makes s's rows what a store from before the
// column holds: every row undecided (NULL, no stat recorded), every row at the
// delta cursor cursor, enriched (enriched_at 7), no backfill completion mark,
// and the ladder at v58, so the next migrate runs v59 and the open-time fill.
func rewindToBeforeSampleFormat(t *testing.T, s *Store, cursor int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE tracks SET sample_format = NULL, sample_format_size = NULL,
		sample_format_mtime_ns = NULL, indexed_at = ?, enriched_at = 7`, cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM scan_state WHERE k = ?`, sampleFormatBackfillKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 58`); err != nil {
		t.Fatal(err)
	}
}

// requireSampleFormats fails unless each path's column is what want says:
// "NULL" for a row nothing has decided, else the value.
func requireSampleFormats(t *testing.T, s *Store, want map[string]string) {
	t.Helper()
	for path, w := range want {
		col := storedSampleFormat(t, s, path)
		got := "NULL"
		if col.Valid {
			got = col.String
		}
		if got != w {
			t.Errorf("%s: sample_format = %q, want %q", path, got, w)
		}
	}
}

// TestTheOpenFillDecidesWhatACodecDecidesAndLeavesTheCursor: every open
// decides, without reading a file, each row its codec decides. A FLAC or ALAC
// row is "integer"; every row whose codec carries no PCM kind (a lossy codec,
// DSD, a compressed WAV or AIFF-C, an unread AIFF-C, no codec) is "", and so
// is a UPnP-routed row, whose header the bridge never reads, a routed FLAC
// included. A WAV or AIFF row is left NULL for the header read. Nothing moves
// a row's delta cursor (indexed_at) or its enrichment, and a second open
// changes nothing.
func TestTheOpenFillDecidesWhatACodecDecidesAndLeavesTheCursor(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	pcm, dsd := false, true
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := []*Track{
		{Path: "a/flac.flac", Codec: "FLAC", IsDSD: &pcm, SampleFormat: SampleFormatInteger},
		{Path: "a/alac.m4a", Codec: "ALAC", SampleFormat: SampleFormatInteger},
		{Path: "a/aac.m4a", Codec: "AAC"},
		{Path: "a/lossy.mp3", Codec: "MP3"},
		{Path: "a/dsd.dsf", Codec: "DSF", IsDSD: &dsd},
		{Path: "a/alaw.wav", Codec: "ALAW"},
		{Path: "a/unread.aifc", Codec: "AIFC"},
		{Path: "a/nocodec.flac"},
		{Path: "a/float.wav", Codec: "WAV", IsDSD: &pcm, SampleFormat: SampleFormatFloat},
		{Path: "a/fl32.aifc", Codec: "AIFF", SampleFormat: SampleFormatFloat},
		{Path: "up/routed.flac", Codec: "FLAC"},
		{Path: "up/routed.wav", Codec: "WAV"},
	}
	for _, r := range rows {
		r.Size, r.ModTime = 1, at
	}
	if err := s.UpsertTrackBatch(ctx, rows); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"up/routed.flac", "up/routed.wav"} {
		if err := s.UpsertUPnPRouting(ctx, &UPnPRouting{SourcePath: p, ServerUDN: "uuid:up", ObjectID: p, ResURL: "http://192.0.2.1/" + p, LastSeenAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	const cursor = 42
	rewindToBeforeSampleFormat(t, s, cursor)
	want := map[string]string{
		"a/flac.flac":    SampleFormatInteger,
		"a/alac.m4a":     SampleFormatInteger,
		"a/aac.m4a":      "",
		"a/lossy.mp3":    "",
		"a/dsd.dsf":      "",
		"a/alaw.wav":     "",
		"a/unread.aifc":  "",
		"a/nocodec.flac": "",
		"a/float.wav":    "NULL",
		"a/fl32.aifc":    "NULL",
		"up/routed.flac": "",
		"up/routed.wav":  "",
	}
	for _, pass := range []string{"the first open", "a second open"} {
		if err := s.migrate(); err != nil {
			t.Fatalf("%s: %v", pass, err)
		}
		requireSampleFormats(t, s, want)
		for path := range want {
			if got := trackIndexedAt(t, s, path); got != cursor {
				t.Errorf("%s: %s indexed_at = %d, want the cursor %d left alone", pass, path, got, cursor)
			}
			if got := trackColumn(t, s, path, "enriched_at"); got != 7 {
				t.Errorf("%s: %s enriched_at = %d, want 7 left alone", pass, path, got)
			}
		}
	}
}

// TestSampleFormatIsReadyOnlyOnceEveryRowTheFillDecidesIsDecided: a header
// pass that runs to its end marks the backfill complete only while every row
// the open-time fill decides is decided. A FLAC row a rolled-back binary
// inserted after this open (its upsert leaves the kind NULL) holds the mark
// back until the next open's fill decides it; then a pass marks it, once, and
// the Provider answers what the store does. A store that cannot be read is
// not ready.
func TestSampleFormatIsReadyOnlyOnceEveryRowTheFillDecidesIsDecided(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.UpsertTrack(ctx, &Track{Path: "a/two.wav", Size: 1, ModTime: at, Codec: "WAV", SampleFormat: SampleFormatFloat}); err != nil {
		t.Fatal(err)
	}
	pcm := false
	upsertAsTheRolledBackBinary(t, s, &Track{Path: "a/one.flac", Size: 1, ModTime: at, Codec: "FLAC", IsDSD: &pcm})
	ready := func(when string, want bool) {
		t.Helper()
		got, err := s.SampleFormatReady(ctx)
		if err != nil || got != want {
			t.Fatalf("%s: SampleFormatReady = %v, %v; want %v", when, got, err, want)
		}
		if p := NewProvider(s, nil).SampleFormatReady(ctx); p != want {
			t.Fatalf("%s: Provider.SampleFormatReady = %v, want %v", when, p, want)
		}
	}
	pass := func(when string, completes bool) {
		t.Helper()
		res, err := s.BackfillSampleFormats(ctx, func(string) (string, os.FileInfo, error) {
			t.Fatalf("%s: the pass asked for a file, and no WAV or AIFF row is undecided", when)
			return "", nil, nil
		})
		if err != nil || res.Completed != completes {
			t.Fatalf("%s: the pass did %+v, %v; want Completed %v", when, res, err, completes)
		}
	}
	pass("a FLAC row undecided", false)
	ready("a FLAC row undecided", false)
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, s, map[string]string{"a/one.flac": SampleFormatInteger, "a/two.wav": SampleFormatFloat})
	ready("the fill done, no pass since", false)
	pass("every row decided", true)
	ready("a pass ran to its end", true)
	pass("a pass after the mark", false)
	ready("a pass after the mark", true)

	closed := openTestStore(t)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if NewProvider(closed, nil).SampleFormatReady(ctx) {
		t.Fatal("a store that cannot be read answered ready")
	}
}

// TestTheHeaderBackfillReadsOnlyTheUnreadWAVAndAIFFRows is the backfill
// without a rescan. A library is scanned, then rewound to a store from before
// the column; the open fill decides the FLAC and the A-law WAV, and
// BackfillSampleFormats asks the resolver for exactly the WAV and AIFF rows
// still undecided, reads their headers, and stores what each states. A row
// whose file has changed since it was written, and one whose file is gone,
// are left undecided for a later pass. The pass runs to its end and marks the
// backfill complete; no row's delta cursor moves, since the one full sync a
// client makes when the health key appears carries every kind. Enrichment
// and tags_json are untouched. A second pass asks only for the two rows left
// and changes nothing.
func TestTheHeaderBackfillReadsOnlyTheUnreadWAVAndAIFFRows(t *testing.T) {
	ctx := context.Background()
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	files := []struct {
		name string
		data []byte
		want string // the column after the passes, "NULL" for undecided
	}{
		{"float.wav", fx("wav/float.wav"), SampleFormatFloat},
		{"s24.wav", fx("wav/s24.wav"), SampleFormatInteger},
		{"af_fl64.aifc", fx("aifc/af_fl64.aifc"), SampleFormatFloat},
		{"af_be16.aiff", fx("aifc/af_be16.aiff"), SampleFormatInteger},
		{"g726.wav", fx("wav/g726.wav"), ""},
		{"alaw.wav", fx("wav/alaw.wav"), ""},
		{"lossless.flac", flacBytes(t, 44100, 16), SampleFormatInteger},
		{"changed.wav", fx("wav/af_f64.wav"), "NULL"},
		{"gone.wav", fx("wav/af_i16.wav"), "NULL"},
	}
	root := t.TempDir()
	rels := map[string]string{}
	for _, f := range files {
		rels[f.name] = writeAlone(t, root, f.name, f.data)
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	const cursor = 42
	rewindToBeforeSampleFormat(t, store, cursor)
	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	tagsBefore := map[string]map[string]any{}
	for _, f := range files {
		tagsBefore[f.name] = tagsJSONKeys(t, store, rels[f.name])
	}
	// After the rows were written, one file changes and one is removed.
	changed, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(rels["changed.wav"])), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changed.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	if err := changed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rels["gone.wav"]))); err != nil {
		t.Fatal(err)
	}

	resolver := bridgefs.New([]string{root})
	var asked []string
	resolve := func(rel string) (string, os.FileInfo, error) {
		asked = append(asked, rel)
		return resolver.ResolveChecked(rel)
	}
	res, err := store.BackfillSampleFormats(ctx, resolve)
	if err != nil {
		t.Fatal(err)
	}
	requireAsked := func(pass string, names ...string) {
		t.Helper()
		var want []string
		for _, n := range names {
			want = append(want, rels[n])
		}
		slices.Sort(want)
		slices.Sort(asked)
		if !slices.Equal(asked, want) {
			t.Errorf("%s asked the resolver for %q, want %q", pass, asked, want)
		}
	}
	requireAsked("the first pass", "float.wav", "s24.wav", "af_fl64.aifc", "af_be16.aiff", "g726.wav", "changed.wav", "gone.wav")
	if want := (SampleFormatBackfill{Read: 5, Gained: 4, None: 1, Changed: 1, Unreadable: 1, Completed: true}); res != want {
		t.Errorf("the first pass did %+v, want %+v", res, want)
	}
	wantCols := map[string]string{}
	for _, f := range files {
		wantCols[rels[f.name]] = f.want
	}
	requireSampleFormats(t, store, wantCols)
	cursors := map[string]int64{}
	for _, f := range files {
		rel := rels[f.name]
		got := trackIndexedAt(t, store, rel)
		cursors[rel] = got
		if got != cursor {
			t.Errorf("%s: the backfill moved its cursor %d -> %d; the clients' one full sync carries it", f.name, cursor, got)
		}
		if e := trackColumn(t, store, rel, "enriched_at"); e != 7 {
			t.Errorf("%s: enriched_at = %d, want 7: the backfill does not re-queue enrichment", f.name, e)
		}
		if after := tagsJSONKeys(t, store, rel); !mapsEqualJSON(tagsBefore[f.name], after) {
			t.Errorf("%s: the backfill rewrote tags_json", f.name)
		}
	}
	served := servedTracksByPath(t, store, root)
	for _, f := range files {
		got, present := served[rels[f.name]]["sampleFormat"]
		wantWire := f.want
		if wantWire == "NULL" {
			wantWire = ""
		}
		switch {
		case wantWire == "" && present:
			t.Errorf("the manifest serves %s with \"sampleFormat\" %#v, want the key absent", f.name, got)
		case wantWire != "" && got != wantWire:
			t.Errorf("the manifest serves %s with \"sampleFormat\" %#v (present %v), want %q", f.name, got, present, wantWire)
		}
	}

	asked = nil
	res, err = store.BackfillSampleFormats(ctx, resolve)
	if err != nil {
		t.Fatal(err)
	}
	requireAsked("the second pass", "changed.wav", "gone.wav")
	if want := (SampleFormatBackfill{Changed: 1, Unreadable: 1}); res != want {
		t.Errorf("the second pass did %+v, want %+v", res, want)
	}
	requireSampleFormats(t, store, wantCols)
	for rel, c := range cursors {
		if got := trackIndexedAt(t, store, rel); got != c {
			t.Errorf("the second pass moved %s's cursor %d -> %d", rel, c, got)
		}
	}
}

// mapsEqualJSON reports whether two decoded JSON objects hold the same keys
// and values.
func mapsEqualJSON(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || !jsonValuesEqual(v, w) {
			return false
		}
	}
	return true
}

func jsonValuesEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		return ok && mapsEqualJSON(av, bv)
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonValuesEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// TestTheVersionStaleStampRecordsTheKindItReads: a file re-read because its
// row's extractor version is stale, whose tags come back as they were, takes
// the light stamp leg, and the stamp records the kind the extraction read: a
// row from before the column gains it, and a kind a past extractor got wrong
// is replaced, each advancing the delta cursor without re-queueing
// enrichment. A stale row whose kind is already right keeps its cursor.
func TestTheVersionStaleStampRecordsTheKindItReads(t *testing.T) {
	root := t.TempDir()
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	floatRel := writeAlone(t, root, "float.wav", fx("wav/float.wav"))
	intRel := writeAlone(t, root, "s24.wav", fx("wav/s24.wav"))
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "first")
	const cursor = 42
	stale := func(rel string, kind any) {
		t.Helper()
		if _, err := store.db.Exec(`UPDATE tracks SET sample_format = ?, extractor_version = ?, indexed_at = ?, enriched_at = 7 WHERE path = ?`,
			kind, ExtractorVersion-1, cursor, rel); err != nil {
			t.Fatal(err)
		}
	}
	stale(floatRel, nil)
	stale(intRel, SampleFormatFloat)
	scanOnce(t, sc, "version-stale")
	requireSampleFormats(t, store, map[string]string{floatRel: SampleFormatFloat, intRel: SampleFormatInteger})
	for _, rel := range []string{floatRel, intRel} {
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
		if got := trackIndexedAt(t, store, rel); got <= cursor {
			t.Errorf("%s: the stamp changed its kind and left its cursor at %d", rel, got)
		}
		if e := trackColumn(t, store, rel, "enriched_at"); e != 7 {
			t.Errorf("%s: enriched_at = %d: the stamp leg must not re-queue enrichment", rel, e)
		}
	}
	stale(floatRel, SampleFormatFloat)
	scanOnce(t, sc, "version-stale, kind already right")
	if got := trackIndexedAt(t, store, floatRel); got != cursor {
		t.Errorf("a stamp that left the kind as it was moved the cursor %d -> %d", cursor, got)
	}
}

// openStoreAt opens the store at dbPath, as a restart of the bridge does, and
// closes it when the test ends.
func openStoreAt(t *testing.T, dbPath string) *Store {
	t.Helper()
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestTheBackfillMovesNoCursorAndTheKeyWaitsForAPassToItsEnd: the rows indexed
// before the column get their kinds with no delta cursor moved, and the
// health key (SampleFormatReady) appears only once a header pass has run to
// its end, so the one full sync a client makes on seeing it carries every
// kind the backfill recorded. The open-time fill alone is not enough, nor a
// pass the shutdown stopped, nor a restart after one; once a pass has run to
// its end, a restart keeps the answer. A file that pass could not read is not
// waited for: when a later pass reads it, its cursor advances, since the
// clients' full sync is past.
func TestTheBackfillMovesNoCursorAndTheKeyWaitsForAPassToItsEnd(t *testing.T) {
	ctx := context.Background()
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	root := t.TempDir()
	floatRel := writeAlone(t, root, "float.wav", fx("wav/float.wav"))
	aiffRel := writeAlone(t, root, "af_be16.aiff", fx("aifc/af_be16.aiff"))
	flacRel := writeAlone(t, root, "lossless.flac", flacBytes(t, 44100, 16))
	laterRel := writeAlone(t, root, "s24.wav", fx("wav/s24.wav"))
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	store := openStoreAt(t, dbPath)
	scanOnce(t, NewScanner([]string{root}, store, ""), "scan")
	const cursor = 42
	rewindToBeforeSampleFormat(t, store, cursor)
	ready := func(when string, s *Store, want bool) {
		t.Helper()
		if got, err := s.SampleFormatReady(ctx); err != nil || got != want {
			t.Errorf("%s: SampleFormatReady = %v, %v; want %v", when, got, err, want)
		}
	}
	unmoved := func(when string, rels ...string) {
		t.Helper()
		for _, rel := range rels {
			if got := trackIndexedAt(t, store, rel); got != cursor {
				t.Errorf("%s moved %s's cursor %d -> %d: the clients' one full sync carries it", when, rel, cursor, got)
			}
		}
	}

	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{flacRel: SampleFormatInteger, floatRel: "NULL", aiffRel: "NULL", laterRel: "NULL"})
	ready("the open-time fill done, no header read yet", store, false)

	resolver := bridgefs.New([]string{root})
	stopCtx, stop := context.WithCancel(ctx)
	defer stop()
	_, err := store.BackfillSampleFormats(stopCtx, func(rel string) (string, os.FileInfo, error) {
		stop()
		return resolver.ResolveChecked(rel)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a pass the shutdown stopped returned %v, want context.Canceled", err)
	}
	ready("a pass stopped before its end", store, false)
	ready("a restart after that pass", openStoreAt(t, dbPath), false)

	unreadable := func(rel string) (string, os.FileInfo, error) {
		if rel == laterRel {
			return "", nil, os.ErrPermission
		}
		return resolver.ResolveChecked(rel)
	}
	if _, err := store.BackfillSampleFormats(ctx, unreadable); err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{flacRel: SampleFormatInteger, floatRel: SampleFormatFloat, aiffRel: SampleFormatInteger, laterRel: "NULL"})
	unmoved("the backfill", floatRel, aiffRel, flacRel, laterRel)
	ready("a pass ran to its end", store, true)
	ready("a restart after it", openStoreAt(t, dbPath), true)

	if _, err := store.BackfillSampleFormats(ctx, resolver.ResolveChecked); err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{laterRel: SampleFormatInteger})
	if got := trackIndexedAt(t, store, laterRel); got <= cursor {
		t.Errorf("s24.wav got its kind after the clients' full sync and its cursor stayed at %d: no delta carries it", got)
	}
	unmoved("a pass after the backfill", floatRel, aiffRel, flacRel)
}

// pagesRead is how many pages s's one connection has asked its page cache
// for so far, hit or missed (sqlite3_db_status). A test that reads it sets the
// pool to one connection first, so the count is everything the store read.
func pagesRead(t *testing.T, s *Store) int {
	t.Helper()
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	err = conn.Raw(func(dc any) error {
		st, ok := dc.(sqlite.DBStatus)
		if !ok {
			return fmt.Errorf("the driver connection %T reports no status", dc)
		}
		hit, _, err := st.Status(sqlite.DBStatusCacheHit, false)
		if err != nil {
			return err
		}
		miss, _, err := st.Status(sqlite.DBStatusCacheMiss, false)
		if err != nil {
			return err
		}
		n = hit + miss
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheOpenFillVisitsEachUndecidedRowOnce: the open-time fill reads each
// undecided row once, the WAV and AIFF rows it leaves to the header pass
// included. Two stores hold the same 300 FLAC rows and 100 WAV rows, the WAV
// paths sorting before every FLAC path in one and after them in the other.
// Filled 10 rows a statement, both decide the same rows, and the pages the
// fill reads where the WAV rows sort first are within half again of the pages
// it reads where they sort last. A fill whose every statement starts again at
// the first undecided row reads the 100 WAV rows once per statement, 30
// times, about five times the pages.
func TestTheOpenFillVisitsEachUndecidedRowOnce(t *testing.T) {
	const flacs, wavs, chunk = 300, 100, 10
	fill := func(wavDir, flacDir string) int {
		t.Helper()
		ctx := context.Background()
		s := openTestStore(t)
		at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
		rows := make([]*Track, 0, flacs+wavs)
		for i := range flacs {
			rows = append(rows, &Track{Path: fmt.Sprintf("%s/%03d.flac", flacDir, i), Size: 1, ModTime: at, Codec: "FLAC"})
		}
		for i := range wavs {
			rows = append(rows, &Track{Path: fmt.Sprintf("%s/%03d.wav", wavDir, i), Size: 1, ModTime: at, Codec: "WAV"})
		}
		if err := s.UpsertTrackBatch(ctx, rows); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE tracks SET sample_format = NULL`); err != nil {
			t.Fatal(err)
		}
		s.db.SetMaxOpenConns(1)
		before := pagesRead(t, s)
		if err := s.fillSampleFormatsByCodec(ctx, chunk); err != nil {
			t.Fatal(err)
		}
		read := pagesRead(t, s) - before
		var decided, left int
		if err := s.db.QueryRow(`SELECT
				COALESCE(SUM(sample_format = 'integer'), 0),
				COALESCE(SUM(sample_format IS NULL), 0)
			FROM tracks`).Scan(&decided, &left); err != nil {
			t.Fatal(err)
		}
		if decided != flacs || left != wavs {
			t.Fatalf("the fill decided %d rows and left %d, want %d and %d", decided, left, flacs, wavs)
		}
		return read
	}
	first := fill("a", "b")
	last := fill("b", "a")
	t.Logf("pages read by the fill: %d with the WAV rows sorting first, %d with them last", first, last)
	if 2*first > 3*last {
		t.Errorf("the fill read %d pages with the 100 WAV rows sorting first and %d with them last: it reads the rows it leaves to the header pass again in every statement", first, last)
	}
}

// The header pass writes a row only from a read of the bytes the row
// describes, and only while the row is still undecided. Each of the three
// tests below puts one writer between the pass's page read and its write,
// inside the resolver call, which comes after the page read and before the
// file is opened.

// headerPassRow is a store holding one float WAV row that predates the
// column, opened again by this binary, and the resolver over its library.
func headerPassRow(t *testing.T) (store *Store, root, rel string, resolver *bridgefs.Resolver) {
	t.Helper()
	root = t.TempDir()
	rel = writeAlone(t, root, "float.wav", compressedPCMFixture(t, "wav/float.wav"))
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	rewindToBeforeSampleFormat(t, store, 42)
	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	return store, root, rel, bridgefs.New([]string{root})
}

// TestAResolverThatAnswersNoStatLeavesTheRowUnreadable: a resolver that maps
// a path and answers no stat and no error (SampleFormatResolver is the
// caller's function) leaves the row undecided and counted Unreadable: the
// pass has no stat to compare the row's with, and reads nothing.
func TestAResolverThatAnswersNoStatLeavesTheRowUnreadable(t *testing.T) {
	store, _, rel, resolver := headerPassRow(t)
	res, err := store.BackfillSampleFormats(context.Background(), func(p string) (string, os.FileInfo, error) {
		abs, _, err := resolver.ResolveChecked(p)
		return abs, nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{rel: "NULL"})
	if res.Unreadable != 1 || res.Read != 0 || res.Changed != 0 {
		t.Errorf("the pass did %+v, want the row counted Unreadable and nothing read", res)
	}
}

// TestTheHeaderPassLeavesTheKindAScanWroteMidPass: a scan that writes the row
// after the pass read its page decides the row, and the pass's write
// (sample_format IS NULL) does not land: the scan's kind stands. The scan
// here writes "integer", which no header in the test states, so the two
// writes can be told apart.
func TestTheHeaderPassLeavesTheKindAScanWroteMidPass(t *testing.T) {
	ctx := context.Background()
	store, _, rel, resolver := headerPassRow(t)
	res, err := store.BackfillSampleFormats(ctx, func(p string) (string, os.FileInfo, error) {
		abs, info, err := resolver.ResolveChecked(p)
		if err == nil {
			if uerr := store.UpsertTrack(ctx, &Track{Path: p, Size: info.Size(), ModTime: info.ModTime(), Codec: "WAV", SampleFormat: SampleFormatInteger}); uerr != nil {
				t.Error(uerr)
			}
		}
		return abs, info, err
	})
	if err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{rel: SampleFormatInteger})
	if res.Read != 1 || res.Gained != 0 {
		t.Errorf("the pass did %+v, want one header read and no row gained", res)
	}
}

// TestTheHeaderPassLeavesARowWhoseStatMovedMidPass: a writer that moves the
// row to another size and mtime and leaves it undecided, as an older binary's
// upsert of a changed file does, makes the bytes the pass read describe
// another version of the row; the pass's write (the size and mtime it read
// against) does not land, and the row stays undecided.
func TestTheHeaderPassLeavesARowWhoseStatMovedMidPass(t *testing.T) {
	ctx := context.Background()
	store, _, rel, resolver := headerPassRow(t)
	pcm := false
	res, err := store.BackfillSampleFormats(ctx, func(p string) (string, os.FileInfo, error) {
		abs, info, err := resolver.ResolveChecked(p)
		if err == nil {
			upsertAsTheRolledBackBinary(t, store, &Track{Path: p, Size: info.Size() + 1, ModTime: info.ModTime().Add(time.Hour), Codec: "WAV", IsDSD: &pcm})
		}
		return abs, info, err
	})
	if err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{rel: "NULL"})
	if res.Gained != 0 {
		t.Errorf("the pass did %+v, want no row gained", res)
	}
}

// TestTheHeaderPassReadOfAFileReplacedAfterItsStatIsChanged: a file replaced
// between the resolver's stat and the read is caught by the stat of the file
// the read opened, so the replacement's header never lands on the row that
// describes the file before it: the row stays undecided and counts as
// Changed.
func TestTheHeaderPassReadOfAFileReplacedAfterItsStatIsChanged(t *testing.T) {
	ctx := context.Background()
	store, _, rel, resolver := headerPassRow(t)
	integer := compressedPCMFixture(t, "wav/af_i16.wav")
	res, err := store.BackfillSampleFormats(ctx, func(p string) (string, os.FileInfo, error) {
		abs, info, err := resolver.ResolveChecked(p)
		if err == nil {
			writeFixtureBytes(t, abs, integer)
			later := info.ModTime().Add(time.Hour)
			if cerr := os.Chtimes(abs, later, later); cerr != nil {
				t.Error(cerr)
			}
		}
		return abs, info, err
	})
	if err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{rel: "NULL"})
	if res.Changed != 1 || res.Read != 0 {
		t.Errorf("the pass did %+v, want the row counted Changed and no header read", res)
	}
}

// TestTheSampleFormatQueriesUseThePartialIndex: the open-time fill, the
// header pass's candidate read and the completion mark read the undecided rows
// through idx_tracks_sample_format_null, the fill and the header pass as a
// range from the path their last chunk or page ended on, in the index's own
// order, with no sort; so each reads only undecided rows, which are none once
// the backfill is done. The open-time clear reads the stale kinds through
// idx_tracks_sample_format_stale, which on a store this binary alone has
// written holds none.
func TestTheSampleFormatQueriesUseThePartialIndex(t *testing.T) {
	fill := strings.Join(strings.Fields(sampleFormatFillSQL), " ")
	inner := strings.Join(strings.Fields(sampleFormatFillRowidsSQL), " ")
	if !strings.Contains(fill, inner) {
		t.Fatal("the fill no longer selects the statement this test explains")
	}
	s := openTestStore(t)
	for _, q := range []struct {
		name, sql, want string
		args            []any
	}{
		{"the open-time fill", sampleFormatFillRowidsSQL, "SEARCH tracks USING INDEX idx_tracks_sample_format_null (path>?)", []any{"", 1}},
		{"the header pass", sampleFormatCandidatesSQL, "SEARCH tracks USING INDEX idx_tracks_sample_format_null (path>?)", []any{"", 1}},
		{"the completion mark", sampleFormatMarkCompleteSQL, "USING INDEX idx_tracks_sample_format_null", []any{sampleFormatBackfillKey, "1"}},
		{"the open-time clear", sampleFormatClearStaleSQL, "USING INDEX idx_tracks_sample_format_stale", []any{1}},
	} {
		plan := explainPlan(t, s, q.sql, q.args...)
		t.Logf("%s: %s", q.name, plan)
		if !strings.Contains(plan, q.want) {
			t.Errorf("%s: plan %q does not read %q", q.name, plan, q.want)
		}
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s: plan %q sorts the rows it reads", q.name, plan)
		}
	}
}
