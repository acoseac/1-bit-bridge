package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// backupBootEnv makes the re-exec'd test binary one boot of serve's backup
// ticker over the data dir it names (runBackupBoot), whose verification
// parks once its check has ended and says so (bootParked), until the test
// kills it: a boot that dies before its pass ends, whose locks the kernel
// lets go as it lets a killed bridge's go. TestMain checks it before the
// suite starts. Its name does not start with BRIDGE_, which the config's
// overrides read.
const backupBootEnv = "CMDBRIDGE_TEST_BACKUP_BOOT"

// bootParked is the line a boot prints once its verification has checked
// and parks.
const bootParked = "boot parked after its check"

// runBackupBoot is the re-exec'd test binary's whole run as one boot.
func runBackupBoot() int {
	dataDir := os.Getenv(backupBootEnv)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	backup.SetAfterCheckForTest(func(string) {
		fmt.Fprintln(os.Stdout, bootParked)
		time.Sleep(time.Hour)
	})
	runBackupTicker(context.Background(), src, func() int { return 7 }, staticInterval(24*time.Hour), nil, os.Stdout, os.Stderr, nil)
	return 0
}

// bootThatDies runs one boot of serve's backup ticker over dataDir as a
// process of its own, and kills it once its verification has checked, or
// once it says its pass took no snapshot, answering what it printed.
func bootThatDies(t *testing.T, dataDir string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), backupBootEnv+"="+dataDir)
	var stderr safeBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	kill := func() {
		_ = cmd.Process.Kill()
		for range lines {
		}
		_ = cmd.Wait()
	}
	var out strings.Builder
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				_ = cmd.Wait()
				t.Fatalf("the boot ended by itself\nstdout:\n%s\nstderr:\n%s", out.String(), stderr.String())
			}
			out.WriteString(line + "\n")
			if line == bootParked || strings.Contains(line, "skipping") {
				kill()
				return out.String()
			}
		case <-deadline:
			kill()
			t.Fatalf("the boot neither parked nor skipped\nstdout:\n%s\nstderr:\n%s", out.String(), stderr.String())
		}
	}
}

// TestARestartLoopTakesOneSnapshotAndTheFirstBootThatSurvivesVerifiesIt is
// backlog B653's second round's red for a restart loop: boots that die
// before their pass ends (each a process of its own, killed once its
// verification has checked, so the kernel lets its locks go) leave one
// snapshot, not one per boot, since each boot after the first finds the
// newest snapshot from the last day still owing its check, with no live
// owner, and finishes that check in place of a new capture; and the first
// boot that lives verifies it, the one snapshot then VERIFIED. Red on the
// round before, whose boots each captured beside an unverified snapshot.
func TestARestartLoopTakesOneSnapshotAndTheFirstBootThatSurvivesVerifiesIt(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	for boot := 1; boot <= 3; boot++ {
		out := bootThatDies(t, dataDir)
		if n := len(snapshotDirs(t, root)); n != 1 {
			t.Fatalf("after %d boots that died: %d snapshots, want 1\nthe last boot printed:\n%s", boot, n, out)
		}
		if boot > 1 && !strings.Contains(out, "verifying it in place of a new snapshot") {
			t.Errorf("boot %d did not verify the snapshot in place:\n%s", boot, out)
		}
	}
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	var stdout, stderr safeBuffer
	status := &sweepStatus[struct{}]{}
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
	}()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the boot that lives did not finish its pass\nstdout %s\nstderr %s", stdout.String(), stderr.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	waitClosed(t, done, "the backup ticker")
	dirs := snapshotDirs(t, root)
	if len(dirs) != 1 {
		t.Fatalf("the boot that lives left %d snapshots, want the one\nstdout %s", len(dirs), stdout.String())
	}
	if rec := recordOf(t, dirs[0]); !rec.Passes() {
		t.Errorf("the one snapshot reads %+v after the boot that lives, want it verified\nstdout %s\nstderr %s", rec, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "verifying it in place of a new snapshot") {
		t.Errorf("the boot that lives did not verify the snapshot in place: %s", stdout.String())
	}
}

// parkVerification parks the next verification once its check has ended,
// holding the backups folder's lock, and answers a wait for it and its
// release.
func parkVerification(t *testing.T) (parked <-chan string, release func()) {
	t.Helper()
	at, letGo := make(chan string, 1), make(chan struct{})
	var once, released sync.Once
	t.Cleanup(backup.SetAfterCheckForTest(func(dir string) {
		once.Do(func() {
			at <- dir
			<-letGo
		})
	}))
	release = func() { released.Do(func() { close(letGo) }) }
	t.Cleanup(release)
	return at, release
}

// TestBridgeBackupVerifyTakesTheBackupsFoldersLockWhateverPathItIsGiven is
// backlog B653's second round's red for the lock's name: bridge backup
// --verify takes the lock of the install's backups folder, the one its
// prune takes, however the snapshot is named: `.` inside it, a path
// relative to another folder, a link to it, and a snapshot of an install
// whose data dir is reached through a link, named where it resolves. While
// the verification holds it, the prune of the backups folder, named as the
// ticker names it, is skipped; and no lock file is made anywhere else. Red
// on the round before for `.` and the link, which locked the folder the
// path's spelling named (the snapshot itself, the link's folder); the other
// two take the right lock there too (a relative path's parent and a linked
// folder's lock file are the same files), and pin the resolution that the
// refusal of a snapshot elsewhere added.
func TestBridgeBackupVerifyTakesTheBackupsFoldersLockWhateverPathItIsGiven(t *testing.T) {
	for _, tc := range []string{"the snapshot's own folder", "a path relative to another folder", "a link to the snapshot", "a data dir reached through a link"} {
		t.Run(tc, func(t *testing.T) {
			cfgPath, dataDir, root := backupInstall(t)
			if tc == "a data dir reached through a link" {
				link := filepath.Join(t.TempDir(), "data-link")
				if err := os.Symlink(dataDir, link); err != nil {
					t.Skipf("no link on this host: %v", err)
				}
				cfgPath = filepath.Join(t.TempDir(), "bridge.yaml")
				if err := os.WriteFile(cfgPath, []byte("dataDir: "+yamlStr(link)+"\nlibraryRoots:\n  - "+yamlStr(filepath.Join(dataDir, "library"))+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				root = filepath.Join(link, backup.BackupsDirName)
			}
			src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
			dir, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			arg := dir
			// Where the round before made its lock file: in the folder the
			// path's spelling names as the snapshot's parent.
			strays := []string{filepath.Join(dir, backup.LockFileName)}
			switch tc {
			case "the snapshot's own folder":
				t.Chdir(dir)
				arg = "."
			case "a path relative to another folder":
				t.Chdir(dataDir)
				arg = filepath.Join("library", "..", backup.BackupsDirName, filepath.Base(dir))
			case "a link to the snapshot":
				link := filepath.Join(t.TempDir(), "snapshot-link")
				if err := os.Symlink(dir, link); err != nil {
					t.Skipf("no link on this host: %v", err)
				}
				arg = link
				strays = append(strays, filepath.Join(filepath.Dir(link), backup.LockFileName))
			case "a data dir reached through a link":
				real, err := filepath.EvalSymlinks(dir)
				if err != nil {
					t.Fatal(err)
				}
				arg = real
			}
			parked, release := parkVerification(t)
			code := make(chan int, 1)
			var out, errs safeBuffer
			go func() {
				code <- backupCmdContext(context.Background(), []string{"--config", cfgPath, "--verify", arg}, &out, &errs)
			}()
			select {
			case <-parked:
			case c := <-code:
				t.Fatalf("--verify %s returned %d before its check ended\n%s%s", arg, c, out.String(), errs.String())
			}
			res, err := backup.Prune(root, 1)
			if err != nil || !res.Skipped || !errors.Is(res.SkippedFor, backup.ErrBackupsBusy) {
				t.Errorf("the prune beside --verify %s: %+v, %v; want it skipped, the one lock held", arg, res, err)
			}
			release()
			if c := <-code; c != 0 {
				t.Errorf("--verify %s = %d\n%s%s", arg, c, out.String(), errs.String())
			}
			for _, stray := range strays {
				if _, err := os.Lstat(stray); err == nil {
					t.Errorf("--verify %s made a lock file outside the backups folder: %s", arg, stray)
				}
			}
		})
	}
}

// TestBridgeBackupVerifyRefusesWhatIsNotInItsInstallsBackupsFolder is
// backlog B653's second round's red for a snapshot elsewhere: bridge backup
// --verify of a copy of a snapshot outside the backups folder its config
// names is refused, exit 2, nothing written in the copy and no lock taken
// beside it, and so is a run whose config does not load. Red on the round
// before, which verified the copy where it stood, under a lock of its own
// folder that no prune takes.
func TestBridgeBackupVerifyRefusesWhatIsNotInItsInstallsBackupsFolder(t *testing.T) {
	cfgPath, dataDir, _ := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	copyDir := filepath.Join(elsewhere, filepath.Base(dir))
	if err := os.Mkdir(copyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(copyDir, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(copyDir, backup.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := backupCmd([]string{"--config", cfgPath, "--verify", copyDir}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "is not a snapshot in the backups folder") {
		t.Errorf("--verify of a copy elsewhere = %d\n%s%s", code, stdout.String(), stderr.String())
	}
	after, _ := os.ReadFile(filepath.Join(copyDir, backup.ManifestFile))
	if !bytes.Equal(before, after) {
		t.Error("--verify of a copy elsewhere wrote its record")
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, backup.LockFileName)); err == nil {
		t.Error("--verify of a copy elsewhere made a lock file beside it")
	}
	stdout.Reset()
	stderr.Reset()
	if code := backupCmd([]string{"--config", filepath.Join(t.TempDir(), "absent.yaml"), "--verify", dir}, &stdout, &stderr); code != 2 {
		t.Errorf("--verify with a config that does not load = %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

// TestABackupsFolderGrantingNoLockGrowsByOneSnapshotAndTheDoctorSaysWhy:
// where the backups folder grants no lock (ENOLCK, a filesystem that keeps
// none), nothing there is verified or pruned (backlog B653's second round),
// so the ticker takes no snapshot while the newest one still owes its
// check: its passes leave one snapshot, not one each, say why, and the
// doctor's backup-lock line says so too.
func TestABackupsFolderGrantingNoLockGrowsByOneSnapshotAndTheDoctorSaysWhy(t *testing.T) {
	cfgPath, dataDir, root := backupInstall(t)
	t.Cleanup(backup.SetTryLockBackupsForTest(func(*os.File) error { return errors.ErrUnsupported }))
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	// bootPass runs one start's backup pass, which ends once the ticker
	// records it, or says it was granted no lock.
	bootPass := func() (string, string) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		drainLoopOnCleanup(t, cancel, done, "the backup ticker")
		var stdout, stderr safeBuffer
		status := &sweepStatus[struct{}]{}
		go func() {
			defer close(done)
			runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
		}()
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.IsZero() || strings.Contains(stderr.String(), "lock") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the pass did not end\nstdout %s\nstderr %s", stdout.String(), stderr.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		waitClosed(t, done, "the backup ticker")
		return stdout.String(), stderr.String()
	}
	// The first start captures, and its check is refused.
	out, errs := bootPass()
	if n := len(snapshotDirs(t, root)); n != 1 || !strings.Contains(errs, "no lock was granted on the backups folder") {
		t.Fatalf("the first pass with no lock granted: %d snapshots\nstdout %s\nstderr %s", n, out, errs)
	}
	newest := snapshotDirs(t, root)[0]
	// The next start finds it owing its check, which it cannot begin.
	out, errs = bootPass()
	if n := len(snapshotDirs(t, root)); n != 1 || !strings.Contains(errs, "no lock was granted on the backups folder") {
		t.Errorf("a start beside it: %d snapshots, want 1\nstdout %s\nstderr %s", n, out, errs)
	}
	// And once it is older than a day, a start that would take one takes
	// none, saying why.
	raw, err := os.ReadFile(filepath.Join(newest, backup.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var mf backup.Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatal(err)
	}
	mf.CreatedAt = time.Now().UTC().Add(-48 * time.Hour)
	raw, err = json.Marshal(mf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newest, backup.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, errs = bootPass()
	if n := len(snapshotDirs(t, root)); n != 1 || !strings.Contains(errs, "no snapshot taken: no lock is granted on the backups folder") {
		t.Errorf("a start past the day with no lock granted: %d snapshots, want 1, said why\nstdout %s\nstderr %s", n, out, errs)
	}
	d := buildDoctorDepsWith(cfgPath, false, doctorDACHost())
	report := doctor.Run(t.Context(), d)
	found := false
	for _, c := range report.Checks {
		if c.Name == "backup-lock" {
			found = true
			if c.Status != doctor.Warn || !strings.Contains(c.Summary, "no lock is granted") {
				t.Errorf("the doctor's backup-lock line: %+v", c)
			}
		}
	}
	if !found {
		t.Error("the doctor says nothing of a backups folder that grants no lock")
	}
}

// TestATickerStoppedWhileAVerificationsStepHangsReturnsWithinItsBound is
// backlog B653's second round's case for serve's backup drain: the ticker,
// stopped while its verification's read of the manifest, or its open of the
// backups folder's lock, does not answer (a hung mount), returns within
// backup.StopWriteBound of the stop, and says nothing of the stop.
func TestATickerStoppedWhileAVerificationsStepHangsReturnsWithinItsBound(t *testing.T) {
	for _, step := range []string{"its manifest's read", "its lock's open"} {
		t.Run(step, func(t *testing.T) {
			_, dataDir, root := backupInstall(t)
			src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
			entered, release := make(chan struct{}), make(chan struct{})
			var once, released sync.Once
			hang := func() {
				once.Do(func() {
					close(entered)
					<-release
				})
			}
			letGo := func() { released.Do(func() { close(release) }) }
			// Registered first, so it runs last: the abandoned worker, which
			// read the seam, has ended before the seam is put back.
			t.Cleanup(func() {
				letGo()
				awaitVerificationsEnd(t, root)
			})
			if step == "its manifest's read" {
				t.Cleanup(backup.SetOnManifestReadForTest(func(string) { hang() }))
			} else {
				t.Cleanup(backup.SetOpenBackupsLockForTest(func(dir, name string) (*os.File, error) {
					hang()
					return fsutil.OpenLockFile(dir, name)
				}))
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			drainLoopOnCleanup(t, cancel, done, "the backup ticker")
			var stdout, stderr safeBuffer
			go func() {
				defer close(done)
				runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, nil)
			}()
			select {
			case <-entered:
			case <-time.After(60 * time.Second):
				t.Fatalf("the verification never reached %s\nstdout %s\nstderr %s", step, stdout.String(), stderr.String())
			}
			stoppedAt := time.Now()
			cancel()
			select {
			case <-done:
				if took := time.Since(stoppedAt); took > backup.StopWriteBound+time.Second {
					t.Errorf("the ticker returned %v after the stop; the bound is %v", took, backup.StopWriteBound)
				}
			case <-time.After(backup.StopWriteBound + 10*time.Second):
				t.Fatalf("the ticker did not return while %s hung", step)
			}
			if got := stderr.String(); got != "" {
				t.Errorf("a stopped verification was reported: %s", got)
			}
			letGo()
			awaitVerificationsEnd(t, root)
		})
	}
}

// awaitVerificationsEnd waits until no verification owes a snapshot under
// root its check any more, each capture's ownership let go, which comes
// once its verification, abandoned or not, has ended and let its lock go.
func awaitVerificationsEnd(t *testing.T, root string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for _, dir := range snapshotDirs(t, root) {
		for {
			release, err := backup.TakeOwnerForTest(dir)
			if err == nil {
				release()
				break
			}
			if !errors.Is(err, backup.ErrClaimedElsewhere) {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("the verification of %s never ended", filepath.Base(dir))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestTheDoctorsVerifyHintsAreCommandsTheCLITakes: each bridge backup
// --verify the doctor's backups lines advise, its <snapshot-dir> filled in,
// is a command bridge backup takes: --verify takes the folder as its value,
// so --full goes after it, never between. Red on the round before, whose
// hint put --full first, which the CLI refuses.
func TestTheDoctorsVerifyHintsAreCommandsTheCLITakes(t *testing.T) {
	cfgPath, dataDir, _ := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var hints []string
	for _, rec := range []*backup.Verification{
		{Format: 1, DatabaseState: backup.StateFailed, DatabaseReasons: []backup.Reason{backup.ReasonQuickCheck}, FilesState: backup.StateVerified},
		{Format: 1, DatabaseState: backup.StateIncomplete, DatabaseReasons: []backup.Reason{backup.ReasonCancelled},
			FilesState: backup.StateIncomplete, FilesReasons: []backup.Reason{backup.ReasonCancelled}},
	} {
		st := backup.Status{Snapshots: []backup.SnapshotStatus{{DirName: "x", CreatedAt: now, Verdict: *rec}}}
		for _, c := range doctor.BackupChecks(st, nil, nil, true, now) {
			hints = append(hints, c.Hint)
		}
	}
	var commands []string
	for _, h := range hints {
		for _, part := range strings.Split(h, "bridge backup --verify")[1:] {
			fields := strings.Fields(strings.ReplaceAll(part, ",", " "))
			args := []string{"--config", cfgPath, "--verify"}
			for _, f := range fields {
				if f == "<snapshot-dir>" {
					f = dir
				} else if !strings.HasPrefix(f, "--") {
					break
				}
				args = append(args, f)
			}
			commands = append(commands, strings.Join(args[2:], " "))
			var stdout, stderr bytes.Buffer
			if code := backupCmd(args, &stdout, &stderr); code == 2 {
				t.Errorf("the doctor advises %q, which bridge backup refuses: %s", "bridge backup --verify"+part, stderr.String())
			}
		}
	}
	if len(commands) < 2 {
		t.Fatalf("the doctor's hints advise %d --verify commands (%q), want the failed and the unfinished one's", len(commands), hints)
	}
}
