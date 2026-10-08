package manifest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkUpsertStampCost measures the track-upsert stamp on a store of
// benchTrackRows tracks. batch-500 is one UpsertTrackBatch of 500
// conflict updates. stamp-query-1 and stamp-query-500 are
// selectNextDeltaStampSQL alone, once and once per row of that batch.
// update-bound-500 writes indexed_at from a bound integer;
// update-subquery-500 evaluates nextDeltaStampSQL inside each of the 500
// statements. The difference is the per-row cost of putting the watermark
// seeks in the upsert. A full scan is benchFullScanTracks/500 batches.
func BenchmarkUpsertStampCost(b *testing.B) {
	s := openBenchStore(b, benchTrackRows)
	b.Cleanup(func() { s.Close() })
	ctx := context.Background()
	paths := benchPaths(500)
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		b.Fatal(err)
	}

	b.Run("batch-500", func(b *testing.B) {
		rows := benchTracks(paths, 100)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for _, tr := range rows {
				tr.Size++
			}
			if err := s.UpsertTrackBatch(ctx, rows); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("stamp-query-1", func(b *testing.B) {
		tx := beginBenchTx(b, s)
		defer tx.Rollback()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := queryStamp(tx, int64(i)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("stamp-query-500", func(b *testing.B) {
		tx := beginBenchTx(b, s)
		defer tx.Rollback()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for n := 0; n < 500; n++ {
				if _, err := queryStamp(tx, int64(i)); err != nil {
					b.Fatal(err)
				}
			}
		}
	})

	b.Run("update-bound-500", func(b *testing.B) {
		benchIndexedAtUpdates(b, s, paths, false)
	})

	b.Run("update-subquery-500", func(b *testing.B) {
		benchIndexedAtUpdates(b, s, paths, true)
	})
}

const (
	benchTrackRows      = 25_000
	benchFullScanTracks = 50_000
)

func openBenchStore(b *testing.B, n int) *Store {
	b.Helper()
	s, err := OpenStore(filepath.Join(b.TempDir(), "bridge.db"))
	if err != nil {
		b.Fatal(err)
	}
	// One statement. The later columns take their defaults.
	if _, err := s.db.ExecContext(context.Background(), `
		WITH RECURSIVE c(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < ?
		)
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		SELECT 'Music/' || n || '.flac', 1, 1, x'7b7d', n FROM c`, n); err != nil {
		s.Close()
		b.Fatal(err)
	}
	return s
}

func benchPaths(n int) []string {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf("Music/%d.flac", i+1)
	}
	return paths
}

func benchTracks(paths []string, size int64) []*Track {
	now := time.Unix(0, 1)
	rows := make([]*Track, len(paths))
	for i, p := range paths {
		rows[i] = &Track{Path: p, Size: size, ModTime: now}
	}
	return rows
}

func beginBenchTx(b *testing.B, s *Store) *sql.Tx {
	b.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	return tx
}

func queryStamp(tx *sql.Tx, now int64) (int64, error) {
	var stamp int64
	err := tx.QueryRow(selectNextDeltaStampSQL, now).Scan(&stamp)
	return stamp, err
}

func benchIndexedAtUpdates(b *testing.B, s *Store, paths []string, subquery bool) {
	b.Helper()
	ctx := context.Background()
	q := `UPDATE tracks SET indexed_at = ? WHERE path = ?`
	if subquery {
		q = `UPDATE tracks SET indexed_at = ` + nextDeltaStampSQL + ` WHERE path = ?`
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, p := range paths {
			var err error
			if subquery {
				_, err = stmt.ExecContext(ctx, int64(i), p)
			} else {
				_, err = stmt.ExecContext(ctx, int64(i+benchTrackRows+1), p)
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
