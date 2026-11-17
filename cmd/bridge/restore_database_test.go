package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// dbOnlyMarkerKey is the scan_state row dbOnlyInstall writes before its
// snapshot ("snapshot") and after it ("later"), which tells the restored
// database from the original.
const dbOnlyMarkerKey = "b653p2b1-command-marker"

// dbOnlyHarvest is the harvest state dbOnlyInstall keeps: a credential and a
// sync position a completed restore rewinds.
const dbOnlyHarvest = `{"token":"tok-kept","atlasBaseUrl":"https://atlas.example","expiresAt":"2030-01-01T00:00:00Z",` +
	`"resultCursor":42,"lastSubmitAt":"2026-01-01T00:00:00Z","lastBookletCheckAt":"2026-01-02T00:00:00Z"}`

// dbOnlyInstall is an install bridge restore --database-only can restore:
// its config (a closed admin port, no mDNS), a live database whose marker
// reads "later" with a WAL and a -shm planted beside it, a harvest state,
// and a snapshot of the database taken at "snapshot" whose record is
// VERIFIED, its holds let go.
type dbOnlyInstall struct {
	cfgPath, dataDir, snap string
}

// newDBOnlyInstall builds a dbOnlyInstall.
func newDBOnlyInstall(t *testing.T) *dbOnlyInstall {
	t.Helper()
	dir, err := fsutil.ResolveLinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	in := &dbOnlyInstall{cfgPath: filepath.Join(dir, "bridge.yaml"), dataDir: filepath.Join(dir, "data")}
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: %s\ndisableHttp3: true\nmdns:\n    enabled: false\n",
		yamlStr(lib), yamlStr(in.dataDir), closedAdminAddr(t))
	if err := os.WriteFile(in.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	db := manifest.DefaultDBPath(in.dataDir)
	store, err := manifest.OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	dbOnlySetMarker(t, db, "snapshot")
	root := filepath.Join(in.dataDir, backup.BackupsDirName)
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	res := backup.SnapshotAndVerify(context.Background(), backup.Sources{DataDir: in.dataDir, ManifestDB: db, BridgeYAML: in.cfgPath},
		backup.SnapshotOptions{})
	awaitHoldsLetGo(t, root)
	if !res.Kept() || res.VerifyErr != nil || res.Verify.Record.DatabaseState != backup.StateVerified {
		t.Fatalf("the snapshot: kept %v, capture %v, verification %v, record %+v", res.Kept(), res.CaptureErr, res.VerifyErr, res.Verify.Record)
	}
	in.snap = res.Dir
	dbOnlySetMarker(t, db, "later")
	dbOnlyWrite(t, db+"-wal", "the original's WAL, which only its database can read")
	dbOnlyWrite(t, db+"-shm", "the original's -shm")
	dbOnlyWrite(t, harvestStatePath(in.dataDir), dbOnlyHarvest)
	return in
}

// restore runs bridge restore over in's config with args after --config,
// answering stdin.
func (in *dbOnlyInstall) restore(t *testing.T, stdin io.Reader, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errs strings.Builder
	code = restoreCmd(context.Background(), append([]string{"--config", in.cfgPath}, args...), stdin, &out, &errs)
	return code, out.String(), errs.String()
}

// originals is what in's four original names hold now, by name.
func (in *dbOnlyInstall) originals(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	db := manifest.DefaultDBPath(in.dataDir)
	for _, p := range []string{db, db + "-wal", db + "-shm", db + "-journal"} {
		switch b, err := os.ReadFile(p); {
		case err == nil:
			out[filepath.Base(p)] = string(b)
		case !errors.Is(err, os.ErrNotExist):
			t.Fatal(err)
		}
	}
	return out
}

// state is every regular file under in's data dir but its backups and its
// lock files, and its bytes.
func (in *dbOnlyInstall) state(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(in.dataDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(in.dataDir, p)
		if d.IsDir() && rel == backup.BackupsDirName {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() && !strings.HasSuffix(rel, ".lock") {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// requireNothingStaged requires no staged copy, no journal and no folder of
// originals in in's data dir.
func (in *dbOnlyInstall) requireNothingStaged(t *testing.T) {
	t.Helper()
	db := manifest.DefaultDBPath(in.dataDir)
	for _, p := range []string{manifest.RestoreStagingPath(in.dataDir), manifest.RestoreJournalPath(db), filepath.Join(in.dataDir, backup.OriginalsDirName)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stands after a refused restore (%v)", filepath.Base(p), err)
		}
	}
}

func dbOnlyWrite(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dbOnlySetMarker writes the marker row into the database at path.
func dbOnlySetMarker(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO scan_state(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, dbOnlyMarkerKey, value); err != nil {
		t.Fatal(err)
	}
}

// dbOnlyMarkerOf is the marker the database at path holds, read with
// nothing beside it read or made.
func dbOnlyMarkerOf(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, "mode=ro&immutable=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT v FROM scan_state WHERE k = ?`, dbOnlyMarkerKey).Scan(&v); err != nil {
		t.Fatalf("read the marker of %s: %v", path, err)
	}
	return v
}

// stopAtTheWALBesideTheInstall runs the database-only restore of in and
// stands a WAL at its live name just before the prepared copy is renamed in,
// so the restore stops with its journal `forward` kept and the installed
// copy sent back; then it removes that WAL.
func (in *dbOnlyInstall) stopAtTheWALBesideTheInstall(t *testing.T) {
	t.Helper()
	db := manifest.DefaultDBPath(in.dataDir)
	stage := manifest.RestoreStagingPath(in.dataDir)
	var planted atomic.Bool
	restore := fsutil.SetBeforeNoReplaceRenameForTest(func(from, to string) {
		if from == stage && to == db && planted.CompareAndSwap(false, true) {
			dbOnlyWrite(t, db+"-wal", "a WAL something opened beside the new database")
		}
	})
	code, _, stderr := in.restore(t, strings.NewReader(""), "--database-only", "--yes", in.snap)
	restore()
	if code != 1 || !strings.Contains(stderr, "bridge.db-wal") || !strings.Contains(stderr, "bridge restore --database-only --config") {
		t.Fatalf("the restore stopped by a WAL at its name = %d, want 1 naming the WAL and the remedy\nstderr:\n%s", code, stderr)
	}
	if err := os.Remove(db + "-wal"); err != nil {
		t.Fatal(err)
	}
}

// TestTheWayBackNeedsNoSourceAndNeverPreparesAgain is T29 through the
// command: an unfinished restore, stopped `forward` (a WAL stood beside the
// installed copy) or `installed` (its harvest rewind failed), is finished by
// `bridge restore --database-only` alone with its snapshot removed, and no
// source worker runs: the `forward` one is put back, every original byte for
// byte; the `installed` one is completed, the restored database in place
// and its harvest rewound. While its journal stands, `--database-only` with
// a snapshot, and with `--full`, exit 2 having read and moved nothing.
func TestTheWayBackNeedsNoSourceAndNeverPreparesAgain(t *testing.T) {
	t.Run("forward", func(t *testing.T) {
		in := newDBOnlyInstall(t)
		before := in.originals(t)
		in.stopAtTheWALBesideTheInstall(t)
		refusesAnythingButTheWayBack(t, in)
		if err := os.RemoveAll(in.snap); err != nil {
			t.Fatal(err)
		}
		looked := countSourceWorkers(t)
		code, stdout, stderr := in.restore(t, strings.NewReader(""), "--database-only", "--yes")
		if code != 0 || !strings.Contains(stdout, "Put the original database files back") || !strings.Contains(stdout, "bridge start") {
			t.Fatalf("the way back = %d, want 0 putting the originals back\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if n := looked.Load(); n != 0 {
			t.Errorf("the way back ran %d source workers", n)
		}
		if got := in.originals(t); !dbOnlySame(got, before) {
			t.Errorf("the originals after the way back are not as they were: %v", dbOnlyNames(got))
		}
		if got := dbOnlyMarkerOf(t, manifest.DefaultDBPath(in.dataDir)); got != "later" {
			t.Errorf("the database after the way back holds %q, want the original", got)
		}
		if _, err := os.Lstat(manifest.RestoreStagingPath(in.dataDir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the prepared copy is left: %v", err)
		}
	})
	t.Run("installed", func(t *testing.T) {
		in := newDBOnlyInstall(t)
		harvest := harvestStatePath(in.dataDir)
		if err := os.Remove(harvest); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(harvest, 0o700); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := in.restore(t, strings.NewReader(""), "--database-only", "--yes", in.snap)
		if code != 1 || !strings.Contains(stderr, "the database is restored") || !strings.Contains(stderr, "bridge restore --database-only --config") {
			t.Fatalf("the restore whose rewind failed = %d, want 1 naming the way on\nstderr:\n%s", code, stderr)
		}
		if err := os.Remove(harvest); err != nil {
			t.Fatal(err)
		}
		dbOnlyWrite(t, harvest, dbOnlyHarvest)
		refusesAnythingButTheWayBack(t, in)
		if err := os.RemoveAll(in.snap); err != nil {
			t.Fatal(err)
		}
		looked := countSourceWorkers(t)
		code, stdout, stderr := in.restore(t, strings.NewReader(""), "--database-only", "--yes")
		if code != 0 || !strings.Contains(stdout, "Completed the restore") || !strings.Contains(stdout, "bridge start") {
			t.Fatalf("the way back = %d, want 0 completing the restore\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if n := looked.Load(); n != 0 {
			t.Errorf("the way back ran %d source workers", n)
		}
		if got := dbOnlyMarkerOf(t, manifest.DefaultDBPath(in.dataDir)); got != "snapshot" {
			t.Errorf("the database after the way back holds %q, want the restored one", got)
		}
		var h map[string]any
		if err := json.Unmarshal(readFileT(t, harvest), &h); err != nil || h["resultCursor"].(float64) != 0 || h["token"] != "tok-kept" {
			t.Errorf("the harvest state after the way back = %v (%v), want its position rewound and its credential kept", h, err)
		}
		if _, err := os.Lstat(manifest.RestoreJournalPath(manifest.DefaultDBPath(in.dataDir))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the journal stands after the way back: %v", err)
		}
	})
}

// countSourceWorkers counts every restore source worker and look that runs
// from now to the test's end.
func countSourceWorkers(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	t.Cleanup(backup.SetOnSourceWorkerEndForTest(func(string) { n.Add(1) }))
	return &n
}

// refusesAnythingButTheWayBack requires, with in's journal standing, that
// `--database-only` with its snapshot and with `--full` each exit 2, no
// source worker run and nothing in the data dir changed.
func refusesAnythingButTheWayBack(t *testing.T, in *dbOnlyInstall) {
	t.Helper()
	before := in.state(t)
	var looked atomic.Int32
	restore := backup.SetOnSourceWorkerEndForTest(func(string) { looked.Add(1) })
	defer restore()
	for _, args := range [][]string{{"--database-only", "--yes", in.snap}, {"--database-only", "--full", "--yes"}} {
		code, _, stderr := in.restore(t, strings.NewReader(""), args...)
		if code != 2 || !strings.Contains(stderr, "is finished by `bridge restore --database-only` alone") {
			t.Errorf("bridge restore %v with a journal standing = %d, want 2\nstderr:\n%s", args, code, stderr)
		}
	}
	if n := looked.Load(); n != 0 {
		t.Errorf("a refused run looked at its snapshot %d times", n)
	}
	if after := in.state(t); !dbOnlySame(after, before) {
		t.Errorf("a refused run changed the data dir: %v, was %v", dbOnlyNames(after), dbOnlyNames(before))
	}
}

func dbOnlySame(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func dbOnlyNames(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTheDatabaseOnlyLookRefusesBeforeItsPrompt is T30's and T12's command
// rows: `bridge restore --database-only` over a look that found its
// snapshot busy, or was stopped, exits 1 with no prompt and never "not a
// recognized snapshot"; over a snapshot of a newer schema than this bridge
// takes, one whose VERIFIED stands behind its veto, and one that lists no
// database with a baseline, it exits 1 before its prompt with nothing
// staged and every original as it was.
func TestTheDatabaseOnlyLookRefusesBeforeItsPrompt(t *testing.T) {
	t.Run("the look's own refusals", func(t *testing.T) {
		in := newDBOnlyInstall(t)
		for _, tc := range []struct {
			name   string
			answer error
			want   string
		}{
			{"busy", fmt.Errorf("%w: file was busy", backup.ErrSnapshotBusy), "was busy"},
			{"stopped", fmt.Errorf("the look was stopped: %w", context.Canceled), context.Canceled.Error()},
		} {
			t.Run(tc.name, func(t *testing.T) {
				prev := checkRestoreSource
				checkRestoreSource = func(context.Context, string, time.Duration) (backup.SourceLook, error) {
					return backup.SourceLook{}, tc.answer
				}
				t.Cleanup(func() { checkRestoreSource = prev })
				code, stdout, stderr := in.restore(t, strings.NewReader("yes\n"), "--database-only", in.snap)
				if code != 1 || !strings.Contains(stderr, tc.want) || strings.Contains(stderr, "not a recognized snapshot") ||
					strings.Contains(stderr, "Type 'yes'") || stdout != "" {
					t.Errorf("bridge restore --database-only whose look answered %v = %d\nstdout:\n%s\nstderr:\n%s", tc.answer, code, stdout, stderr)
				}
				in.requireNothingStaged(t)
			})
		}
	})
	for _, tc := range []struct {
		name string
		make func(t *testing.T, snap string)
		want string
	}{
		{"a newer schema", func(t *testing.T, snap string) {
			dbOnlyEditManifest(t, snap, func(m map[string]any) { m["schemaVersion"] = backup.SchemaVersion + 1 })
		}, fmt.Sprintf("schema v%d is not one this bridge supports", backup.SchemaVersion+1)},
		{"a vetoed VERIFIED", func(t *testing.T, snap string) {
			dbOnlyWrite(t, filepath.Join(snap, ".verdict-unsynced"), "")
		}, "INCOMPLETE (unsynced)"},
		{"no database baseline", func(t *testing.T, snap string) {
			dbOnlyEditManifest(t, snap, func(m map[string]any) {
				var kept []any
				for _, b := range m["baselines"].([]any) {
					if b.(map[string]any)["name"] != backup.ManifestDBFileName {
						kept = append(kept, b)
					}
				}
				m["baselines"] = kept
			})
		}, "lists no database with a baseline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newDBOnlyInstall(t)
			tc.make(t, in.snap)
			before := in.originals(t)
			code, stdout, stderr := in.restore(t, strings.NewReader("yes\n"), "--database-only", in.snap)
			if code != 1 || !strings.Contains(stderr, tc.want) || strings.Contains(stderr, "Type 'yes'") || strings.Contains(stdout, "Restore the library database") {
				t.Fatalf("bridge restore --database-only of %s = %d, want 1 before its prompt naming %q\nstdout:\n%s\nstderr:\n%s",
					tc.name, code, tc.want, stdout, stderr)
			}
			in.requireNothingStaged(t)
			if got := in.originals(t); !dbOnlySame(got, before) {
				t.Error("a refused restore changed the originals")
			}
		})
	}
}

// dbOnlyEditManifest rewrites a snapshot's manifest.json through a map.
func dbOnlyEditManifest(t *testing.T, snap string, edit func(map[string]any)) {
	t.Helper()
	p := filepath.Join(snap, backup.ManifestFile)
	var m map[string]any
	if err := json.Unmarshal(readFileT(t, p), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// configChangedWords are the restore's words for a config another writer
// changed since the command read it.
const configChangedWords = "is not the config this restore read"

// editingReader is a prompt's answer that edits a file the first time it is
// read, before it answers: a config changed while the prompt waits.
type editingReader struct {
	path   string
	answer io.Reader
	edited bool
}

func (r *editingReader) Read(p []byte) (int, error) {
	if !r.edited {
		r.edited = true
		f, err := os.OpenFile(r.path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return 0, err
		}
		_, werr := f.WriteString("# an edit made while the prompt waited\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return 0, werr
		}
	}
	return r.answer.Read(p)
}

// TestAConfigChangedDuringTheConfirmationRestoresNothing is T17 through the
// command: bridge.yaml edited while the prompt waits, and edited during the
// preparation while no lock of it is held (the preparation's checkpoint
// answer standing in for the moment), each refuse the restore at the
// config's next turn, nothing staged and no original moved.
func TestAConfigChangedDuringTheConfirmationRestoresNothing(t *testing.T) {
	t.Run("while the prompt waits", func(t *testing.T) {
		in := newDBOnlyInstall(t)
		before := in.originals(t)
		code, stdout, stderr := in.restore(t, &editingReader{path: in.cfgPath, answer: strings.NewReader("yes\n")}, "--database-only", in.snap)
		if code != 1 || !strings.Contains(stderr, configChangedWords) || strings.Contains(stdout, "Restored the library database") {
			t.Fatalf("bridge restore --database-only over a config edited at its prompt = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		in.requireNothingStaged(t)
		if got := in.originals(t); !dbOnlySame(got, before) {
			t.Error("the refused restore changed the originals")
		}
	})
	t.Run("while the copy is prepared", func(t *testing.T) {
		in := newDBOnlyInstall(t)
		before := in.originals(t)
		var edited atomic.Bool
		t.Cleanup(manifest.SetEpochCheckpointAnswerForTest(func() (int, int, int, error) {
			if edited.CompareAndSwap(false, true) {
				f, err := os.OpenFile(in.cfgPath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					return 0, 0, 0, err
				}
				_, _ = f.WriteString("# an edit made while the copy was prepared\n")
				_ = f.Close()
			}
			return 0, 0, 0, nil
		}))
		code, stdout, stderr := in.restore(t, strings.NewReader(""), "--database-only", "--yes", in.snap)
		if !edited.Load() {
			t.Fatal("the preparation's checkpoint was never answered")
		}
		if code != 1 || !strings.Contains(stderr, configChangedWords) || strings.Contains(stdout, "Restored the library database") {
			t.Fatalf("bridge restore --database-only over a config edited during its preparation = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		in.requireNothingStaged(t)
		if got := in.originals(t); !dbOnlySame(got, before) {
			t.Error("the refused restore changed the originals")
		}
	})
}

// TestTheRestoresWordsNameTheWayOn is the remedy (§4.9) and the success's
// words through the command: with a journal standing, the full restore
// exits 1 naming `bridge restore --database-only --config <the config>` and
// the folder of the originals; and a database-only restore that succeeds
// names the folder its originals are kept in, each original, and `bridge
// start`.
func TestTheRestoresWordsNameTheWayOn(t *testing.T) {
	t.Run("the full restore beside a journal", func(t *testing.T) {
		in := newDBOnlyInstall(t)
		in.stopAtTheWALBesideTheInstall(t)
		archive := backup.RestoreRemedyArchive(in.dataDir)
		if !strings.HasPrefix(archive, filepath.Join(in.dataDir, backup.OriginalsDirName)+string(filepath.Separator)) {
			t.Fatalf("the remedy's folder %s is not a run's", archive)
		}
		code, _, stderr := in.restore(t, strings.NewReader(""), "--yes", in.snap)
		if code != 1 || !strings.Contains(stderr, journalWords) || !strings.Contains(stderr, "bridge restore --database-only --config "+in.cfgPath) ||
			!strings.Contains(stderr, archive) || !strings.Contains(stderr, "docs/backup.md") {
			t.Fatalf("the full restore beside a journal = %d, want 1 naming the way on\nstderr:\n%s", code, stderr)
		}
	})
	t.Run("a database-only restore that succeeds", func(t *testing.T) {
		in := newDBOnlyInstall(t)
		code, stdout, stderr := in.restore(t, strings.NewReader(""), "--database-only", "--yes", in.snap)
		if code != 0 {
			t.Fatalf("bridge restore --database-only = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		originals := filepath.Join(in.dataDir, backup.OriginalsDirName) + string(filepath.Separator)
		if !strings.Contains(stdout, "kept in "+originals) || !strings.Contains(stdout, "bridge.db (") || !strings.Contains(stdout, "bridge.db-wal (") ||
			!strings.Contains(stdout, "bridge start") {
			t.Errorf("the restore's words do not name where its originals are and what follows\nstdout:\n%s", stdout)
		}
		if got := dbOnlyMarkerOf(t, manifest.DefaultDBPath(in.dataDir)); got != "snapshot" {
			t.Errorf("the restored database holds %q", got)
		}
		if !bytes.Contains(readFileT(t, harvestStatePath(in.dataDir)), []byte(`"resultCursor":0`)) {
			t.Error("the restore did not rewind the harvest")
		}
	})
}
