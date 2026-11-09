package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// backupsBody is GET /api/backups' body, raw and decoded.
func backupsBody(t *testing.T, s *Server) (string, backupsListResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/backups", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = testConsoleHost
	rw := httptest.NewRecorder()
	s.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /api/backups = %d: %s", rw.Code, rw.Body.String())
	}
	raw, _ := io.ReadAll(rw.Body)
	var out backupsListResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return string(raw), out
}

// doctorLines are the doctor's backups lines for dataDir, as cmd/bridge's
// doctorBackups builds them: backup.ReadStatus and the folder's lock probe,
// worded by doctor.BackupChecks.
func doctorLines(t *testing.T, dataDir string) []doctor.Check {
	t.Helper()
	root := filepath.Join(dataDir, backup.BackupsDirName)
	st, err := backup.ReadStatus(t.Context(), root)
	return doctor.BackupChecks(st, err, backup.ProbeBackupsLock(root), true, time.Now())
}

// lineNamed is the doctor's line named name.
func lineNamed(t *testing.T, lines []doctor.Check, name string) doctor.Check {
	t.Helper()
	for _, c := range lines {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s line among %+v", name, lines)
	return doctor.Check{}
}

// writeBundle writes a snapshot folder under dataDir's backups holding a
// manifest with rec as its record (nil: written before verification).
func writeBundle(t *testing.T, dataDir string, created time.Time, rec *backup.Verification) string {
	t.Helper()
	dir := filepath.Join(dataDir, backup.BackupsDirName, created.UTC().Format("2006-01-02T15-04-05Z"))
	mkdirAllT(t, dir)
	m := backup.Manifest{SchemaVersion: backup.SchemaVersion, CreatedAt: created, Files: []string{backup.ManifestDBFileName, "server.crt"},
		BridgeVersion: "test", Verification: rec}
	if rec != nil {
		m.Baselines = []backup.Baseline{{Name: backup.ManifestDBFileName}, {Name: "server.crt"}}
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTheDoctorTheAPIAndThePrunerAgreeOnEveryState is the brief's surface
// red (backlog B653): for a snapshot in each state its record can read, the
// doctor's newest-snapshot line, GET /api/backups and the pruner's reading
// (backup.List) say the same two verdicts with the same reason codes, and
// neither the doctor nor the API names a path. Red on main: neither said a
// verdict at all, and the API carried the backups folder's path.
func TestTheDoctorTheAPIAndThePrunerAgreeOnEveryState(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rec := func(db backup.State, dbr []backup.Reason, files backup.State, fr []backup.Reason) *backup.Verification {
		return &backup.Verification{Format: 1, DatabaseState: db, DatabaseReasons: dbr, FilesState: files, FilesReasons: fr,
			Level: backup.CheckQuick, CheckedAt: now}
	}
	for _, tc := range []struct {
		name           string
		rec            *backup.Verification
		veto           bool
		db, files      backup.State
		dbWhy, fileWhy backup.Reason
	}{
		{name: "verified", rec: rec(backup.StateVerified, nil, backup.StateVerified, nil),
			db: backup.StateVerified, files: backup.StateVerified},
		{name: "the database failed", rec: rec(backup.StateFailed, []backup.Reason{backup.ReasonQuickCheck}, backup.StateVerified, nil),
			db: backup.StateFailed, dbWhy: backup.ReasonQuickCheck, files: backup.StateVerified},
		{name: "a file failed", rec: rec(backup.StateVerified, nil, backup.StateFailed, []backup.Reason{backup.ReasonHash}),
			db: backup.StateVerified, files: backup.StateFailed, fileWhy: backup.ReasonHash},
		{name: "incomplete", rec: rec(backup.StateIncomplete, []backup.Reason{backup.ReasonCancelled}, backup.StateIncomplete,
			[]backup.Reason{backup.ReasonCancelled}),
			db: backup.StateIncomplete, dbWhy: backup.ReasonCancelled, files: backup.StateIncomplete, fileWhy: backup.ReasonCancelled},
		{name: "a young pending", rec: &backup.Verification{Format: 1, DatabaseState: backup.StatePending, FilesState: backup.StatePending,
			PendingSince: time.Now()}, db: backup.StatePending, files: backup.StatePending},
		{name: "a stale pending", rec: &backup.Verification{Format: 1, DatabaseState: backup.StatePending, FilesState: backup.StatePending,
			PendingSince: time.Now().Add(-backup.PendingBound - time.Minute)},
			db: backup.StateIncomplete, dbWhy: backup.ReasonInterrupted, files: backup.StateIncomplete, fileWhy: backup.ReasonInterrupted},
		{name: "written before verification", rec: nil,
			db: backup.StateUnverified, dbWhy: backup.ReasonNoBaseline, files: backup.StateUnverified, fileWhy: backup.ReasonNoBaseline},
		{name: "a verdict not durable", rec: rec(backup.StateVerified, nil, backup.StateVerified, nil), veto: true,
			db: backup.StateIncomplete, dbWhy: backup.ReasonUnsynced, files: backup.StateIncomplete, fileWhy: backup.ReasonUnsynced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newTestServer(t)
			dataDir := t.TempDir()
			s.deps.BackupSources = backup.Sources{DataDir: dataDir}
			dir := writeBundle(t, dataDir, now.Add(-time.Hour), tc.rec)
			if tc.veto {
				if err := os.WriteFile(filepath.Join(dir, backup.VetoFileName), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			raw, api := backupsBody(t, s)
			if len(api.Backups) != 1 {
				t.Fatalf("the API lists %d snapshots, want 1", len(api.Backups))
			}
			got := api.Backups[0]
			wantDB := backup.StateWords(tc.db, reasonsOf(tc.dbWhy))
			wantFiles := backup.StateWords(tc.files, reasonsOf(tc.fileWhy))
			if apiDB := backup.StateWords(backup.State(got.DatabaseState), toReasons(got.DatabaseReasons)); apiDB != wantDB {
				t.Errorf("the API's database verdict %q, want %q", apiDB, wantDB)
			}
			if apiFiles := backup.StateWords(backup.State(got.FilesState), toReasons(got.FilesReasons)); apiFiles != wantFiles {
				t.Errorf("the API's files verdict %q, want %q", apiFiles, wantFiles)
			}
			line := lineNamed(t, doctorLines(t, dataDir), "backup-newest")
			if !strings.Contains(line.Summary, "database "+wantDB) || !strings.Contains(line.Summary, "files "+wantFiles) {
				t.Errorf("the doctor's line %q does not say database %q, files %q", line.Summary, wantDB, wantFiles)
			}
			entries, err := backup.List(filepath.Join(dataDir, backup.BackupsDirName))
			if err != nil || len(entries) != 1 {
				t.Fatalf("List: %v, %v", entries, err)
			}
			if prunerDB := backup.StateWords(entries[0].Verdict.DatabaseState, entries[0].Verdict.DatabaseReasons); prunerDB != wantDB {
				t.Errorf("the pruner reads the database %q, want %q", prunerDB, wantDB)
			}
			for _, c := range doctorLines(t, dataDir) {
				if strings.Contains(c.Summary+c.Hint, dataDir) || strings.Contains(c.Summary+c.Hint, string(os.PathSeparator)+backup.BackupsDirName) {
					t.Errorf("the doctor's %s line names a path: %q / %q", c.Name, c.Summary, c.Hint)
				}
			}
			if strings.Contains(raw, dataDir) || strings.Contains(raw, filepath.ToSlash(dataDir)) {
				t.Errorf("GET /api/backups names the data dir: %s", raw)
			}
		})
	}
}

func reasonsOf(r backup.Reason) []backup.Reason {
	if r == "" {
		return nil
	}
	return []backup.Reason{r}
}

func toReasons(codes []string) []backup.Reason {
	out := make([]backup.Reason, 0, len(codes))
	for _, c := range codes {
		out = append(out, backup.Reason(c))
	}
	return out
}

// liveStore makes dataDir's bridge.db a store with rows, and answers the
// backup sources over it.
func liveStore(t *testing.T, dataDir string) backup.Sources {
	t.Helper()
	db := manifest.DefaultDBPath(dataDir)
	st, err := manifest.OpenStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertTrackBatch(t.Context(), []*manifest.Track{{Path: "A/B/01.flac", Size: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	crt := filepath.Join(dataDir, "server.crt")
	if err := os.WriteFile(crt, []byte("-----BEGIN CERTIFICATE-----stub-----END CERTIFICATE-----"), 0o600); err != nil {
		t.Fatal(err)
	}
	return backup.Sources{DataDir: dataDir, ManifestDB: db, ServerCert: crt}
}

// TestAnUnsyncedVerdictReadsAsNotVerifiedEverywhere is the brief's
// publication red, through every reader: a verification whose verdict's
// folder sync fails once its rename has landed answers an error, and the
// doctor, the console's API and the pruner each read the snapshot as not
// VERIFIED (INCOMPLETE, unsynced), the doctor naming the older snapshot as
// the newest verified one, and the prune keeping that one as the anchor.
func TestAnUnsyncedVerdictReadsAsNotVerifiedEverywhere(t *testing.T) {
	s, _, _ := newTestServer(t)
	dataDir := t.TempDir()
	src := liveStore(t, dataDir)
	s.deps.BackupSources = src
	anchor := backup.SnapshotAndVerify(t.Context(), src, backup.SnapshotOptions{})
	if !anchor.Verify.Record.Passes() {
		t.Fatalf("the anchor did not verify: %+v %v", anchor.Verify.Record, anchor.VerifyErr)
	}
	time.Sleep(1100 * time.Millisecond)
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	restore := backup.SetSyncVerdictForTest(func(string) error { return errors.New("injected: the folder would not sync") })
	_, err = backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{})
	restore()
	if !errors.Is(err, backup.ErrVerdictNotDurable) {
		t.Fatalf("the verification answered %v, want ErrVerdictNotDurable", err)
	}
	_, api := backupsBody(t, s)
	if len(api.Backups) != 2 || api.Backups[0].DirName != filepath.Base(dir) {
		t.Fatalf("the API lists %+v", api.Backups)
	}
	if got := api.Backups[0]; got.DatabaseState == string(backup.StateVerified) || got.FilesState == string(backup.StateVerified) {
		t.Errorf("the API reads the unsynced snapshot as %s/%s", got.DatabaseState, got.FilesState)
	}
	if api.NewestVerified == nil || api.NewestVerified.DirName != filepath.Base(anchor.Dir) {
		t.Errorf("the API's newest verified is %+v, want the anchor %s", api.NewestVerified, filepath.Base(anchor.Dir))
	}
	lines := doctorLines(t, dataDir)
	if newest := lineNamed(t, lines, "backup-newest"); strings.Contains(newest.Summary, "database VERIFIED") {
		t.Errorf("the doctor reads the unsynced snapshot as verified: %q", newest.Summary)
	}
	if verified := lineNamed(t, lines, "backup-verified"); !strings.Contains(verified.Summary, filepath.Base(anchor.Dir)) {
		t.Errorf("the doctor's newest verified line %q does not name the anchor %s", verified.Summary, filepath.Base(anchor.Dir))
	}
	if _, err := backup.Prune(filepath.Join(dataDir, backup.BackupsDirName), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(anchor.Dir); err != nil {
		t.Errorf("the prune let the anchor go: %v", err)
	}
}

// TestACancelledCreateRequestLeavesTheVerdictToCome is the brief's red for
// the console's create: it answers once the snapshot is written, and its
// verification runs on the server's own context, so a request cancelled
// before the check has even begun leaves the verdict to come VERIFIED. Red
// on main: the create pruned on the request's context, and nothing was
// verified.
func TestACancelledCreateRequestLeavesTheVerdictToCome(t *testing.T) {
	s, _, _ := newTestServer(t)
	dataDir := t.TempDir()
	s.deps.BackupSources = liveStore(t, dataDir)
	started, goOn := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(backup.SetOnLowPriorityForTest(func() {
		once.Do(func() {
			close(started)
			<-goOn
		})
	}))
	ctx, cancel := context.WithCancel(context.Background())
	if code := doJSONContext(t, s.Handler(), ctx, http.MethodPost, "/api/backups", map[string]any{}); code != http.StatusCreated {
		t.Fatalf("POST /api/backups = %d, want 201", code)
	}
	<-started
	cancel()
	close(goOn)
	s.WaitForBackupJobs()
	_, api := backupsBody(t, s)
	if len(api.Backups) != 1 {
		t.Fatalf("the API lists %d snapshots, want the one created", len(api.Backups))
	}
	if got := api.Backups[0]; got.DatabaseState != string(backup.StateVerified) || got.FilesState != string(backup.StateVerified) {
		t.Errorf("a create whose request was cancelled left the verdict %s/%s %v %v; want VERIFIED/VERIFIED",
			got.DatabaseState, got.FilesState, got.DatabaseReasons, got.FilesReasons)
	}
}

// TestTheCreateAnswersWithTheSnapshotsNameAndNoPath: the create's 201 names
// the snapshot by its folder's name and its verification PENDING, never a
// path.
func TestTheCreateAnswersWithTheSnapshotsNameAndNoPath(t *testing.T) {
	s, _, _ := newTestServer(t)
	dataDir := t.TempDir()
	s.deps.BackupSources = liveStore(t, dataDir)
	var body map[string]any
	if code := doJSON(t, s.Handler(), http.MethodPost, "/api/backups", map[string]any{}, &body); code != http.StatusCreated {
		t.Fatalf("POST /api/backups = %d", code)
	}
	s.WaitForBackupJobs()
	if body["verification"] != string(backup.StatePending) || body["dirName"] == nil {
		t.Errorf("the create answered %v", body)
	}
	if raw := fmt.Sprint(body); strings.Contains(raw, dataDir) {
		t.Errorf("the create's answer names the data dir: %v", body)
	}
}

// TestRepeatedFailedVerificationsThroughTheConsolesCreateStillPrune is the
// brief's red for the create's prune: a snapshot that fails its verification
// is kept, and the prune after it runs whatever the verdict, so four creates
// of a database that fails leave keep snapshots, not four.
func TestRepeatedFailedVerificationsThroughTheConsolesCreateStillPrune(t *testing.T) {
	s, _, _ := newTestServer(t)
	dataDir := t.TempDir()
	src := liveStore(t, dataDir)
	// A store whose version this bridge's ladder does not reach: every
	// snapshot of it fails its schema check.
	conn, err := sql.Open("sqlite", dsn.File(src.ManifestDB, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, manifest.LadderTop()+1)); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	s.deps.BackupSources = src
	for i := 0; i < 4; i++ {
		if code := doJSON(t, s.Handler(), http.MethodPost, "/api/backups", map[string]any{"keep": 2}, nil); code != http.StatusCreated {
			t.Fatalf("POST %d = %d", i, code)
		}
		s.WaitForBackupJobs()
		time.Sleep(1100 * time.Millisecond)
	}
	_, api := backupsBody(t, s)
	if len(api.Backups) != 2 {
		t.Fatalf("four failed creates at keep 2 left %d snapshots, want 2", len(api.Backups))
	}
	for _, b := range api.Backups {
		if b.DatabaseState != string(backup.StateFailed) || !slices.Contains(b.DatabaseReasons, string(backup.ReasonSchemaNewer)) {
			t.Errorf("%s reads database %s %v, want FAILED schema-newer", b.DirName, b.DatabaseState, b.DatabaseReasons)
		}
	}
}

// pastStamp puts the backups folder's modification time an hour back and
// fills the jobs card's cache over it, so that only a change the write
// itself stamps can be what makes the next poll read the folder again,
// whatever the filesystem's granularity, and answers the card as cached.
func pastStamp(t *testing.T, s *Server, dataDir string) backupCard {
	t.Helper()
	root := filepath.Join(dataDir, backup.BackupsDirName)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(root, past, past); err != nil {
		t.Fatal(err)
	}
	s.invalidateLastBackup()
	return s.getBackupCard(t.Context())
}

// TestAVerdictWrittenByAnotherCallerShowsOnTheJobsCardAtTheNextPoll pins
// the stamp a verdict's write leaves on the backups folder (backup's
// stampChange): the console's Jobs card caches what the folder holds until
// its modification time moves, and a verdict written inside a snapshot by
// another caller (the ticker, bridge backup --verify, another process)
// moves nothing of the folder's own, so without the stamp the card kept
// the PENDING it had cached until its TTL. A verdict that passes is stamped
// when its veto is made, as well as at the write's end; one that fails,
// which takes no veto, is stamped at the end alone, which it pins: red with
// that stamp removed.
func TestAVerdictWrittenByAnotherCallerShowsOnTheJobsCardAtTheNextPoll(t *testing.T) {
	for _, tc := range []struct {
		name string
		want backup.State
	}{
		{"a verdict that passes", backup.StateVerified},
		{"a verdict that fails", backup.StateFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := newTestServer(t)
			dataDir := t.TempDir()
			src := liveStore(t, dataDir)
			s.deps.BackupSources = src
			dir, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == backup.StateFailed {
				p := filepath.Join(dir, backup.ManifestDBFileName)
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-1] ^= 0xFF
				if err := os.WriteFile(p, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if card := pastStamp(t, s, dataDir); !strings.HasPrefix(card.newestDatabase, string(backup.StatePending)) {
				t.Fatalf("the card caches %q, want the snapshot's PENDING", card.newestDatabase)
			}
			if _, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{}); err != nil {
				t.Fatal(err)
			}
			if card := s.getBackupCard(t.Context()); !strings.HasPrefix(card.newestDatabase, string(tc.want)) {
				t.Errorf("the next poll after another caller's verdict reads %q, want %s", card.newestDatabase, tc.want)
			}
		})
	}
}

// TestALeftVerifiedThatCannotBeRecordedReadsAsNotVerifiedEverywhere is
// backlog B653's second round's red for a VERIFIED a later check did not
// find, through the console and the doctor: a verified snapshot whose
// database and files fail when checked again, and whose verdict's write
// fails before its rename, keeps its VERIFIED record behind a veto, and GET
// /api/backups, the doctor's lines and the Jobs card, cached as VERIFIED
// before the write, each read it as not verified after the error: the
// staging refused through the write's seam, its veto then in the bundle,
// where only the veto's own stamp on the backups folder makes the card read
// the folder again (folded item 3); and, with no seam, a bundle that takes
// no new file (mode 0500, Unix and not as root), its veto then beside it.
// Red on the round before, which put no veto over a record that would hold
// no VERIFIED.
func TestALeftVerifiedThatCannotBeRecordedReadsAsNotVerifiedEverywhere(t *testing.T) {
	for _, how := range []string{"its staging is refused", "its bundle takes no file"} {
		t.Run(how, func(t *testing.T) {
			if how == "its bundle takes no file" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("a folder's mode 0500 refuses nothing on Windows or to root")
			}
			s, _, _ := newTestServer(t)
			dataDir := t.TempDir()
			src := liveStore(t, dataDir)
			s.deps.BackupSources = src
			res := backup.SnapshotAndVerify(t.Context(), src, backup.SnapshotOptions{})
			if !res.Verify.Record.Passes() {
				t.Fatalf("the snapshot did not verify: %+v %v", res.Verify.Record, res.VerifyErr)
			}
			for _, name := range []string{backup.ManifestDBFileName, "server.crt"} {
				p := filepath.Join(res.Dir, name)
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				data[len(data)-1] ^= 0xFF
				if err := os.WriteFile(p, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if card := pastStamp(t, s, dataDir); card.newestDatabase != string(backup.StateVerified) {
				t.Fatalf("the card caches %q, want VERIFIED", card.newestDatabase)
			}
			restore := func() {}
			if how == "its staging is refused" {
				restore = backup.SetVerdictWriteFailuresForTest(func(string) error { return errors.New("injected: the staging failed") }, nil, nil)
			} else {
				if err := os.Chmod(res.Dir, 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(res.Dir, 0o700) })
			}
			_, err := backup.VerifySnapshot(t.Context(), res.Dir, backup.VerifyOptions{})
			restore()
			if err == nil {
				t.Fatal("a verification whose write failed answered no error")
			}
			_, api := backupsBody(t, s)
			if len(api.Backups) != 1 {
				t.Fatalf("the API lists %+v", api.Backups)
			}
			if got := api.Backups[0]; got.DatabaseState == string(backup.StateVerified) || got.FilesState == string(backup.StateVerified) || api.NewestVerified != nil {
				t.Errorf("the API reads the snapshot as %s/%s, newest verified %+v", got.DatabaseState, got.FilesState, api.NewestVerified)
			}
			for _, c := range doctorLines(t, dataDir) {
				if c.Name == "backup-newest" && strings.Contains(c.Summary, "database VERIFIED") || c.Name == "backup-verified" && c.Status == doctor.OK {
					t.Errorf("the doctor's %s line reads it verified: %q", c.Name, c.Summary)
				}
			}
			if card := s.getBackupCard(t.Context()); card.newestDatabase == string(backup.StateVerified) || card.lastVerifiedAt != nil {
				t.Errorf("the Jobs card still reads %q (last verified %v) after the failed write", card.newestDatabase, card.lastVerifiedAt)
			}
		})
	}
}
