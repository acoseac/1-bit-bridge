package manifest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// TestGetTrackWithStatReadsTheColumnsTheSkipGateReads: the track is
// GetTrack's, and the size and mtime are the columns GetTrackStat reads,
// not the copy inside tags_json: a zeroed mtime_ns, which forces a
// re-extraction, reads 0 here as it does there. A path with no row is none.
func TestGetTrackWithStatReadsTheColumnsTheSkipGateReads(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	rate, bits := 96000.0, 24
	when := time.Date(2026, 10, 19, 12, 0, 0, 123456789, time.UTC)
	if err := store.UpsertTrack(ctx, &Track{Path: "Album/a.flac", Size: 4321, ModTime: when, Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
		t.Fatal(err)
	}
	got, size, mtime, err := store.GetTrackWithStat(ctx, "Album/a.flac")
	if err != nil || got == nil || got.Codec != "FLAC" || size != 4321 || mtime != when.UnixNano() {
		t.Fatalf("track %+v size %d mtime %d err %v", got, size, mtime, err)
	}
	if want, err := store.GetTrack(ctx, "Album/a.flac"); err != nil || want.Codec != got.Codec || want.rowVersion != got.rowVersion {
		t.Fatalf("GetTrack %+v err %v", want, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE tracks SET mtime_ns = 0 WHERE path = ?`, "Album/a.flac"); err != nil {
		t.Fatal(err)
	}
	_, _, mtime, err = store.GetTrackWithStat(ctx, "Album/a.flac")
	st, err2 := store.GetTrackStat(ctx, "Album/a.flac")
	if err != nil || err2 != nil || mtime != 0 || st.MTimeNS != 0 {
		t.Fatalf("a zeroed mtime_ns reads %d here and %d in the skip gate (%v, %v)", mtime, st.MTimeNS, err, err2)
	}
	if got, size, mtime, err := store.GetTrackWithStat(ctx, "Album/missing.flac"); got != nil || size != 0 || mtime != 0 || err != nil {
		t.Fatalf("a missing row: %+v %d %d %v", got, size, mtime, err)
	}
}

// TestGetTrackWithStatIsOneStatement: the row's format and the size and
// time the skip gate compares describe one version of the row only while
// one statement reads them all, since a scan can commit between two (an
// ALAC row rescanned as AAC would pair the old codec with the new file's
// size and time, which the file on disk then matches). The statements the
// store's connections are handed are counted at the driver: one query for a
// row there and one for a row that is not.
func TestGetTrackWithStatIsOneStatement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	rate, bits := 44100.0, 16
	if err := store.UpsertTrack(ctx, &Track{Path: "Album/a.m4a", Size: 1000, ModTime: time.Now(), Codec: "ALAC", SampleRate: &rate, BitsPerSample: &bits}); err != nil {
		t.Fatal(err)
	}
	var statements atomic.Int32
	counted := sql.OpenDB(countingConnector{dsn: dsn.File(path, "_pragma=busy_timeout(5000)"), n: &statements})
	t.Cleanup(func() { _ = counted.Close() })
	reader := &Store{db: counted}
	for _, p := range []string{"Album/a.m4a", "Album/missing.m4a"} {
		statements.Store(0)
		got, size, _, err := reader.GetTrackWithStat(ctx, p)
		if err != nil || (p == "Album/a.m4a") != (got != nil && got.Codec == "ALAC" && size == 1000) {
			t.Fatalf("%s: %+v size %d err %v", p, got, size, err)
		}
		if n := statements.Load(); n != 1 {
			t.Fatalf("%s: the row was read in %d statements, want one", p, n)
		}
	}
}

// countingConnector opens modernc's connections and counts every statement
// they are handed: a query, an exec or a prepare.
type countingConnector struct {
	dsn string
	n   *atomic.Int32
}

func (c countingConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: conn, n: c.n}, nil
}

func (c countingConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type countingConn struct {
	driver.Conn
	n *atomic.Int32
}

func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.n.Add(1)
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.n.Add(1)
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.n.Add(1)
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}
