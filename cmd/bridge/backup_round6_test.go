package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// pendingInstallSnapshot is a snapshot of the install at cfgPath whose
// verification is still to come: its record PENDING, with no owner to ask.
func pendingInstallSnapshot(t *testing.T, cfgPath string) string {
	t.Helper()
	cfg, resolved, err := loadCLIConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := backup.Snapshot(t.Context(), buildBackupSources(cfg, resolved))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// recordFullFailed writes into the snapshot's record what a full check that
// found both its verdicts FAILED would have left: a quick check's VERIFIED
// replaces no such database verdict (mergeVerdict).
func recordFullFailed(t *testing.T, dir string) {
	t.Helper()
	mpath := filepath.Join(dir, backup.ManifestFile)
	data, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatal(err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	v := m.Verification
	v.Level, v.CheckedAt, v.FilesCheckedAt = backup.CheckFull, now, now
	v.DatabaseState, v.DatabaseReasons = backup.StateFailed, []backup.Reason{backup.ReasonHash}
	v.FilesState, v.FilesReasons = backup.StateFailed, []backup.Reason{backup.ReasonHash}
	if data, err = json.MarshalIndent(m, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mpath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBridgeBackupVerifySaysTheWriteAndTheReadBackApart is backlog B653's
// sixth round's red for what the CLI says of a record it did not read back
// (Codex's two P2s on round 5, the Claude API's Low, and both reviews of the
// round's brief): whatever came of the verification, a record that could not
// be read back is said so, never printed, and what the check found never
// stands in for it; the command exits 1; and how far the write went is said
// from the write's outcome, apart from the read-back. A durable write says it
// wrote its record, never that it failed or that what was found was not
// recorded: over a PENDING, over a full FAILED a quick check finds VERIFIED
// (whose merge keeps the FAILED, so the record is no verified recovery point,
// whatever the check found), and through bridge backup's own snapshot. A
// recheck refused says it was not checked; one not recorded where no
// directory can be synced, and a write that failed, say that what was found
// was not recorded. Red on the round before, which printed what the check
// found as the record and exited 0 where the write had ended, and said
// nothing of a record it did not read back anywhere else.
func TestBridgeBackupVerifySaysTheWriteAndTheReadBackApart(t *testing.T) {
	const readFailed = "injected: the read-back failed"
	durable := []string{"this check found:", "database: VERIFIED", "its record could not be read back",
		"the snapshot was kept, and it wrote its record; its record could not be read back"}
	for _, tc := range []struct {
		name  string
		said  []string
		never []string
	}{
		{"a durable write", durable, []string{"not recorded", "its write failed", "its record reads"}},
		{"a quick check over a full FAILED", durable, []string{"not recorded", "its write failed", "its record reads"}},
		{"bridge backup's own snapshot", append([]string{"Snapshot written:"}, durable...), []string{"not recorded", "its write failed", "its record reads"}},
		{"a recheck refused", []string{"its record could not be read back", "the snapshot was not checked"},
			[]string{"from an earlier check", "this check found"}},
		{"a recheck not recorded", []string{"this check found:", "database: FAILED", "its record could not be read back",
			"what this check found was not recorded"}, []string{"which this check leaves as it was"}},
		{"a write that failed", []string{"this check found:", "its record could not be read back",
			"the snapshot was kept, and what this check found was not recorded; its record could not be read back", "injected: the staging failed"},
			[]string{"its record reads", "it wrote its record"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _, root := backupInstall(t)
			var dir string
			switch tc.name {
			case "a recheck refused", "a recheck not recorded":
				dir = verifiedInstallSnapshot(t, cfgPath)
			case "bridge backup's own snapshot":
			default:
				dir = pendingInstallSnapshot(t, cfgPath)
			}
			switch tc.name {
			case "a quick check over a full FAILED":
				recordFullFailed(t, dir)
			case "a recheck refused":
				requireDirectorySync(t)
				t.Cleanup(backup.SetSyncVetoForTest(func(string) error { return errors.New("injected: the veto's sync failed") }))
			case "a recheck not recorded":
				flipBundleByte(t, dir, backup.ManifestDBFileName)
				t.Cleanup(backup.SetSyncsParentDirForTest(false))
			case "a write that failed":
				t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(string) error { return errors.New("injected: the staging failed") }, nil, nil))
			}
			t.Cleanup(backup.SetReadBackFailureForTest(func(string) error { return errors.New(readFailed) }))
			// Registered after every seam, so run first.
			t.Cleanup(func() { awaitHoldsLetGo(t, root) })
			args := []string{"--config", cfgPath, "--verify", dir}
			if tc.name == "bridge backup's own snapshot" {
				args = []string{"--config", cfgPath}
			}
			var stdout, stderr bytes.Buffer
			code := backupCmd(args, &stdout, &stderr)
			said := stdout.String() + stderr.String()
			if code != 1 {
				t.Errorf("bridge backup %s = %d, want 1: its record was not read back\n%s", strings.Join(args, " "), code, said)
			}
			for _, want := range tc.said {
				if !strings.Contains(said, want) {
					t.Errorf("the command did not say %q:\n%s", want, said)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(said, never) {
					t.Errorf("the command said %q:\n%s", never, said)
				}
			}
			if !strings.Contains(stderr.String(), readFailed) {
				t.Errorf("the command did not say why its record was not read back:\n%s", said)
			}
		})
	}
}

// TestTheTickerSaysTheWriteAndTheReadBackApart is backlog B653's sixth
// round's red for the ticker's report of a record it did not read back (the
// Claude API's Low on round 5 and its review of the round's brief): the pass's
// own snapshot, whose durable write's read-back fails, is said kept, with
// what its check found, that it wrote its record and that its record could
// not be read back, and why, never that it was not recorded nor that it was
// verified; and the report says the write and the read-back apart in the
// other cases (a recheck refused, one not recorded, a write that failed, a
// read-back given up). Red on the round before, which read the record it
// had not read as no verified recovery point with no verdicts, and said
// nothing of the read-back elsewhere.
func TestTheTickerSaysTheWriteAndTheReadBackApart(t *testing.T) {
	t.Run("the pass's own snapshot", func(t *testing.T) {
		_, dataDir, root := backupInstall(t)
		src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
		t.Cleanup(backup.SetReadBackFailureForTest(func(string) error { return errors.New("injected: the read-back failed") }))
		t.Cleanup(func() { awaitHoldsLetGo(t, root) })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		drainLoopOnCleanup(t, cancel, done, "the backup ticker")
		var stdout, stderr safeBuffer
		go func() {
			defer close(done)
			runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, &sweepStatus[struct{}]{})
		}()
		const want = "and it wrote its record; its record could not be read back: "
		for deadline := time.Now().Add(60 * time.Second); !strings.Contains(stderr.String(), want); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the pass never said %q\nstdout %s\nstderr %s", want, stdout.String(), stderr.String())
			}
		}
		cancel()
		waitClosed(t, done, "the backup ticker")
		errs := stderr.String()
		if !strings.Contains(errs, "was kept, and its check found database VERIFIED") || !strings.Contains(errs, "injected: the read-back failed") {
			t.Errorf("the pass did not say what its check found and why its record was not read back:\n%s", errs)
		}
		for _, never := range []string{"which was not recorded", "it is not a verified recovery point"} {
			if strings.Contains(errs, never) {
				t.Errorf("the pass said %q of a durable write whose record was not read back:\n%s", never, errs)
			}
		}
		if strings.Contains(stdout.String(), "): verified ") {
			t.Errorf("the pass said its snapshot verified, its record not read back:\n%s", stdout.String())
		}
	})
	notRead := func(why error) error { return fmt.Errorf("%w: %w", backup.ErrRecordNotReadBack, why) }
	readFailed := errors.New("injected: the read-back failed")
	found := backup.Verification{DatabaseState: backup.StateFailed, DatabaseReasons: []backup.Reason{backup.ReasonHash},
		FilesState: backup.StateVerified}
	kept := fmt.Sprintf("snap was kept, and its check found database %s, files %s, ",
		backup.StateWords(found.DatabaseState, found.DatabaseReasons), backup.FilesWords(found))
	for _, tc := range []struct {
		name string
		res  backup.Result
		want string
	}{
		{"a recheck refused", backup.Result{Dir: "snap", VerifyErr: fmt.Errorf("%w; %w", backup.ErrRecheckRefused, notRead(readFailed)),
			Verify: backup.VerifyResult{ReadErr: readFailed}},
			"snap was not checked, and its record could not be read back: "},
		{"a recheck not recorded", backup.Result{Dir: "snap", VerifyErr: fmt.Errorf("%w; %w", backup.ErrRecheckNotRecorded, notRead(readFailed)),
			Verify: backup.VerifyResult{Found: found, ReadErr: readFailed}},
			kept + "which was not recorded; its record could not be read back: "},
		{"a write that failed", backup.Result{Dir: "snap", VerifyErr: fmt.Errorf("%w; %w", errors.New("injected: the staging failed"), notRead(readFailed)),
			Verify: backup.VerifyResult{Found: found, ReadErr: readFailed, Write: backup.WriteFailed}},
			kept + "which was not recorded; its record could not be read back: "},
		{"a durable write", backup.Result{Dir: "snap", VerifyErr: notRead(readFailed),
			Verify: backup.VerifyResult{Found: found, ReadErr: readFailed, Write: backup.WriteRecorded, Recorded: true}},
			kept + "and it wrote its record; its record could not be read back: "},
		{"a read-back given up", backup.Result{Dir: "snap", VerifyErr: fmt.Errorf("%w; %w", backup.ErrCheckGivenUp, notRead(backup.ErrReadBackGivenUp)),
			Verify: backup.VerifyResult{Found: found, ReadErr: backup.ErrReadBackGivenUp, Write: backup.WriteRecorded, Recorded: true}},
			"it wrote its record; its read-back was given up, so its record was not read: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			reportTickerVerdict(context.Background(), &stdout, &stderr, "startup", tc.res)
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("the pass said:\n%s\nwant %q", stderr.String(), tc.want)
			}
			if stdout.Len() > 0 {
				t.Errorf("the pass said on stdout, of a record it did not read back:\n%s", stdout.String())
			}
		})
	}
}
