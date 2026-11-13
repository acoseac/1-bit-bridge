package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// backupInstall writes a config whose data dir holds a bridge store, and
// answers the config's path, the data dir and its backups folder.
func backupInstall(t *testing.T) (cfgPath, dataDir, root string) {
	t.Helper()
	dir, err := fsutil.ResolveLinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfgPath, dataDir = backupInstallIn(t, dir)
	return cfgPath, dataDir, filepath.Join(dataDir, backup.BackupsDirName)
}

// backupInstallIn is backupInstall's install in the folder dir, which it
// makes where it is missing, and which is the install's data dir.
func backupInstallIn(t *testing.T, dir string) (cfgPath, dataDir string) {
	t.Helper()
	lib := filepath.Join(dir, "library")
	if err := os.MkdirAll(lib, 0o700); err != nil {
		t.Fatal(err)
	}
	backuptest.WriteSource(t, manifest.DefaultDBPath(dir))
	cfgPath = filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte("dataDir: "+yamlStr(dir)+"\nlibraryRoots:\n  - "+yamlStr(lib)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dir
}

// makeStoreNewer stamps the store at path with a user_version past this
// bridge's ladder, so every snapshot of it fails its schema check.
func makeStoreNewer(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, manifest.LadderTop()+1)); err != nil {
		t.Fatal(err)
	}
}

// snapshotDirs is the snapshot folders under root, the lock file aside.
func snapshotDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out
}

// recordOf reads the snapshot's record as it is written.
func recordOf(t *testing.T, dir string) backup.Verification {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, backup.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Verification == nil {
		t.Fatalf("%s holds no record", filepath.Base(dir))
	}
	return *m.Verification
}

// TestBridgeBackupVerifiesWhatItKeepsAndSaysSo is the CLI's half of the
// brief's one path (backlog B653): bridge backup takes its snapshot, says it
// was written, verifies it, says both verdicts, and exits 0 for a verified
// one.
func TestBridgeBackupVerifiesWhatItKeepsAndSaysSo(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	var stdout, stderr bytes.Buffer
	if code := backupCmd([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("bridge backup = %d; stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"Snapshot written: ", "database: VERIFIED (quick check; user_version ", "files:    VERIFIED"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
	dirs := snapshotDirs(t, root)
	if len(dirs) != 1 {
		t.Fatalf("%d snapshots, want 1", len(dirs))
	}
	if rec := recordOf(t, dirs[0]); !rec.Passes() {
		t.Errorf("the record reads %+v", rec)
	}
}

// TestRepeatedFailedVerificationsThroughTheTickerAndBridgeBackupStillPrune
// is the brief's red for the callers' prune: a snapshot whose verification
// fails is kept, said to be kept and not a recovery point, and the prune
// after it runs whatever the verdict, through bridge backup and through
// serve's ticker alike, so failures never pile up past retention. Red on
// main: no verification, and so no such snapshot.
func TestRepeatedFailedVerificationsThroughTheTickerAndBridgeBackupStillPrune(t *testing.T) {
	t.Run("bridge backup", func(t *testing.T) {
		cfgPath, dataDir, root := backupInstall(t)
		t.Cleanup(func() { awaitHoldsLetGo(t, root) })
		makeStoreNewer(t, manifest.DefaultDBPath(dataDir))
		for i := 0; i < 4; i++ {
			var stdout, stderr bytes.Buffer
			code := backupCmd([]string{"--config", cfgPath, "--keep", "2"}, &stdout, &stderr)
			if code != 1 || !strings.Contains(stdout.String(), "Snapshot written: ") ||
				!strings.Contains(stderr.String(), "the snapshot was kept, and it is not a verified recovery point: database FAILED: schema-newer") {
				t.Fatalf("run %d: exit %d\nstdout %s\nstderr %s", i, code, stdout.String(), stderr.String())
			}
			if strings.Contains(stderr.String(), "no snapshot was kept") {
				t.Errorf("run %d said no snapshot was kept of one it kept", i)
			}
			time.Sleep(1100 * time.Millisecond)
		}
		if n := len(snapshotDirs(t, root)); n != 2 {
			t.Errorf("four failed snapshots at --keep 2 left %d, want 2", n)
		}
	})
	t.Run("the ticker", func(t *testing.T) {
		_, dataDir, root := backupInstall(t)
		t.Cleanup(func() { awaitHoldsLetGo(t, root) })
		src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
		makeStoreNewer(t, src.ManifestDB)
		// Three earlier failures, as earlier passes left them.
		now := time.Now().UTC()
		for i := 1; i <= 3; i++ {
			writeSnapshotDirAt(t, root, now.Add(-time.Duration(i)*time.Hour), &backup.Verification{Format: 1,
				DatabaseState: backup.StateFailed, DatabaseReasons: []backup.Reason{backup.ReasonSchemaNewer}, FilesState: backup.StateVerified})
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		drainLoopOnCleanup(t, cancel, done, "the backup ticker")
		var stdout, stderr safeBuffer
		status := &sweepStatus[struct{}]{}
		go func() {
			defer close(done)
			runBackupTicker(ctx, src, func() int { return 2 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
		}()
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.IsZero() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the startup pass did not finish; stderr %s", stderr.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		waitClosed(t, done, "the backup ticker")
		if !strings.Contains(stderr.String(), "was kept, and it is not a verified recovery point: database FAILED: schema-newer") {
			t.Errorf("stderr %q does not say the kept snapshot failed", stderr.String())
		}
		if !strings.Contains(stdout.String(), "pruned 2 older snapshot(s)") {
			t.Errorf("stdout %q does not say the prune ran", stdout.String())
		}
		if n := len(snapshotDirs(t, root)); n != 2 {
			t.Errorf("the ticker left %d snapshots at keep 2, want 2", n)
		}
	})
}

// TestBridgeBackupVerifyRecordsAndSaysTheVerdict pins `bridge backup
// --verify <snapshot> [--full]`: it records and says the verdict, --full at
// the full level, exits 0 only for a snapshot both of whose verdicts are
// VERIFIED, 1 for one that is not or one written before verification
// (nothing recorded), 2 for a folder that holds no snapshot or its flags
// misused, and says when another verification holds the snapshots' lock.
func TestBridgeBackupVerifyRecordsAndSaysTheVerdict(t *testing.T) {
	cfgPath, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	// Each run names the install's config, whose backups folder the
	// snapshot must be in (backlog B653's second round), and returns once the
	// verification it made has let go of what it held: it answers before its
	// worker lets go (backlog B871's round 3).
	run := func(args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := backupCmd(append([]string{"--config", cfgPath}, args...), &stdout, &stderr)
		awaitBackupHoldsDropped(t, "bridge backup --verify")
		return code, stdout.String(), stderr.String()
	}
	if code, out, errs := run("--verify", dir, "--full"); code != 0 || !strings.Contains(out, "database: VERIFIED (full check") {
		t.Fatalf("--verify --full = %d\n%s%s", code, out, errs)
	}
	if rec := recordOf(t, dir); rec.Level != backup.CheckFull || !rec.Passes() {
		t.Errorf("the record after --full is %+v", rec)
	}
	// A quick run over the full one keeps the full one's verdict; where no
	// directory can be synced no check over a VERIFIED is recorded
	// (TestBridgeBackupVerifySaysWhatARecheckFoundWhereItCannotBeRecorded).
	if fsutil.SyncsParentDir {
		if code, out, errs := run("--verify", dir); code != 0 || !strings.Contains(out, "full check") {
			t.Errorf("--verify after --full = %d\n%s%s", code, out, errs)
		}
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--full"}, 2},
		{[]string{"--verify", "--full", dir}, 2},
		{[]string{"--verify", filepath.Join(root, "no-such-snapshot")}, 2},
		{[]string{"--verify", dir, "extra"}, 2},
	} {
		if code, _, _ := run(tc.args...); code != tc.want {
			t.Errorf("bridge backup %v = %d, want %d", tc.args, code, tc.want)
		}
	}
	t.Run("a snapshot that fails", func(t *testing.T) {
		// Its record holds the VERIFIED of the check above.
		requireDirectorySync(t)
		crt := filepath.Join(dir, backup.ManifestDBFileName)
		data, err := os.ReadFile(crt)
		if err != nil {
			t.Fatal(err)
		}
		data[len(data)-1] ^= 0xFF
		if err := os.WriteFile(crt, data, 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, errs := run("--verify", dir)
		if code != 1 || !strings.Contains(out, "database: FAILED: hash-mismatch") || !strings.Contains(errs, "not a verified recovery point") {
			t.Errorf("--verify of a damaged snapshot = %d\n%s%s", code, out, errs)
		}
	})
	t.Run("a snapshot from before verification", func(t *testing.T) {
		t.Cleanup(func() { awaitHoldsLetGo(t, root) })
		legacy := writeSnapshotDir(t, root, time.Now().UTC().Add(-48*time.Hour))
		before, _ := os.ReadFile(filepath.Join(legacy, backup.ManifestFile))
		code, out, _ := run("--verify", legacy)
		after, _ := os.ReadFile(filepath.Join(legacy, backup.ManifestFile))
		if code != 1 || !strings.Contains(out, "written before snapshots were verified") || !bytes.Equal(before, after) {
			t.Errorf("--verify of a legacy snapshot = %d, manifest changed %v\n%s", code, !bytes.Equal(before, after), out)
		}
	})
	t.Run("another verification holds the lock", func(t *testing.T) {
		prev := verifyCmdLockWait
		verifyCmdLockWait = 200 * time.Millisecond
		t.Cleanup(func() { verifyCmdLockWait = prev })
		f, err := fsutil.OpenLockFile(root, backup.LockFileName)
		if err != nil {
			t.Fatal(err)
		}
		if err := fsutil.TryLock(f); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = fsutil.Unlock(f); _ = f.Close() })
		code, _, errs := run("--verify", dir)
		if code != 1 || !strings.Contains(errs, "held their lock") {
			t.Errorf("--verify beside a held lock = %d: %s", code, errs)
		}
	})
}

// TestVerifyBesideTheTickersVerificationOfOneSnapshot is the brief's
// one-writer red: bridge backup --verify --full and the ticker's own
// verification of the snapshot it just wrote, in either order, run one
// after the other, each recording its verdict, and the record ends at the
// full check's: neither verdict is lost, and the quick one never downgrades
// the full one. The second of the two checks a VERIFIED, which where no
// directory can be synced is not recorded
// (TestBridgeBackupVerifySaysWhatARecheckFoundWhereItCannotBeRecorded), so it
// skips there.
func TestVerifyBesideTheTickersVerificationOfOneSnapshot(t *testing.T) {
	requireDirectorySync(t)
	for _, tickerFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("ticker first %v", tickerFirst), func(t *testing.T) {
			cfgPath, dataDir, root := backupInstall(t)
			src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
			parked, release := make(chan string, 1), make(chan struct{})
			waiting := make(chan struct{})
			var once, waits sync.Once
			if tickerFirst {
				// The first verification to end its check, the ticker's, waits
				// there holding the lock.
				t.Cleanup(backup.SetAfterCheckForTest(func(dir string) {
					once.Do(func() { parked <- dir; <-release })
				}))
				t.Cleanup(backup.SetOnSlotWaitForTest(func() { waits.Do(func() { close(waiting) }) }))
			}
			releaseCheck := sync.OnceFunc(func() { close(release) })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			drainLoopOnCleanup(t, cancel, done, "the backup ticker")
			stdout := holdOn("wrote ")
			var callerDone chan struct{}
			t.Cleanup(func() {
				stdout.letGo()
				releaseCheck()
				cancel()
				waitClosed(t, done, "the backup ticker")
				if callerDone != nil {
					waitClosed(t, callerDone, "backup verify")
				}
				awaitVerificationsEnd(t, root)
			})
			var stderr safeBuffer
			status := &sweepStatus[struct{}]{}
			go func() {
				defer close(done)
				runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, stdout, &stderr, status)
			}()
			stdout.wait(t)
			dirs := snapshotDirs(t, root)
			if len(dirs) != 1 {
				t.Fatalf("%d snapshots, want the ticker's one", len(dirs))
			}
			dir := dirs[0]
			verified := make(chan int, 1)
			var vout, verr safeBuffer
			if tickerFirst {
				stdout.letGo()
				<-parked
				callerDone = make(chan struct{})
				go func() {
					defer close(callerDone)
					verified <- backupCmdContext(context.Background(), []string{"--config", cfgPath, "--verify", dir, "--full"}, &vout, &verr)
				}()
				// The --verify waits its turn behind the ticker's verification,
				// this process's one at a time: once it is waiting, let the
				// ticker go on.
				select {
				case <-waiting:
				case code := <-verified:
					releaseCheck() // the ticker's park, which the drain would wait on
					t.Fatalf("--verify returned %d before it waited its turn\n%s%s", code, vout.String(), verr.String())
				}
				releaseCheck()
			} else {
				// The ticker is held between its capture and its verification:
				// the --verify runs first, whole.
				code := backupCmdContext(context.Background(), []string{"--config", cfgPath, "--verify", dir, "--full"}, &vout, &verr)
				verified <- code
				stdout.letGo()
			}
			select {
			case code := <-verified:
				if code != 0 {
					t.Errorf("--verify --full = %d\n%s%s", code, vout.String(), verr.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatal("--verify did not return")
			}
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.IsZero() {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the ticker's pass did not finish; stderr %s", stderr.String())
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			waitClosed(t, done, "the backup ticker")
			if !strings.Contains(stdout.String(), "verified "+dir) {
				t.Errorf("the ticker did not say it verified its snapshot: %s%s", stdout.String(), stderr.String())
			}
			if rec := recordOf(t, dir); !rec.Passes() || rec.Level != backup.CheckFull {
				t.Errorf("the record ends %s at %q, want VERIFIED at the full check's level", rec.DatabaseState, rec.Level)
			}
		})
	}
}

// TestATickerStoppedWhileItsVerdictsSyncHangsReturnsWithinItsBound is the
// brief's shutdown red at serve's ticker, which serve's backup join waits
// for: stopped while the verdict's write is held in a sync that never
// answers, the ticker returns within backup.StopWriteBound of the stop,
// abandoning the write, and says nothing of the stop.
func TestATickerStoppedWhileItsVerdictsSyncHangsReturnsWithinItsBound(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	release := make(chan struct{})
	entered := make(chan struct{})
	var enter sync.Once
	letGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(backup.SetSyncVerdictForTest(func(string) error {
		enter.Do(func() { close(entered) })
		<-release
		return nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	t.Cleanup(func() { letGo(); cancel(); waitClosed(t, done, "the backup ticker"); awaitVerificationsEnd(t, root) })
	var stdout, stderr safeBuffer
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, nil)
	}()
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the verdict's write never reached its sync")
	}
	stoppedAt := time.Now()
	cancel()
	select {
	case <-done:
		if took := time.Since(stoppedAt); took > backup.StopWriteBound+time.Second {
			t.Errorf("the ticker returned %v after the stop; the bound is %v", took, backup.StopWriteBound)
		}
	case <-time.After(backup.StopWriteBound + 10*time.Second):
		t.Fatal("the ticker did not return while its verdict's write was held in a sync")
	}
	if got := stderr.String(); got != "" {
		t.Errorf("a stopped verification was reported: %s", got)
	}
}

// TestBothDoctorsCarryTheBackupLines: bridge doctor's Deps, which the
// console's doctor builds too (adminDoctorRunnerFor through
// buildDoctorDepsWith), carry the backups' lines, read from the snapshots'
// manifests: the newest written and the newest database-verified, apart.
func TestBothDoctorsCarryTheBackupLines(t *testing.T) {
	cfgPath, dataDir, _ := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	res := backup.SnapshotAndVerify(t.Context(), src, backup.SnapshotOptions{})
	// Its verification answers before its worker lets go: that worker's end
	// comes before the doctor reads the folder and before it goes (backlog
	// B871's round 3).
	awaitBackupHoldsDropped(t, "the snapshot's verification")
	if !res.Verify.Record.Passes() {
		t.Fatalf("the snapshot did not verify: %+v %v", res.Verify.Record, res.VerifyErr)
	}
	d := buildDoctorDepsWith(cfgPath, false, doctorDACHost())
	if d.Backups == nil {
		t.Fatal("the doctor's Deps carry no backups lines")
	}
	report := doctor.Run(t.Context(), d)
	seen := map[string]doctor.Check{}
	for _, c := range report.Checks {
		seen[c.Name] = c
	}
	for _, name := range []string{"backup-newest", "backup-verified"} {
		c, ok := seen[name]
		if !ok || c.Status != doctor.OK {
			t.Errorf("%s: %+v (present %v), want ok", name, c, ok)
		}
		if strings.Contains(c.Summary, dataDir) {
			t.Errorf("%s names the data dir: %q", name, c.Summary)
		}
	}
}
