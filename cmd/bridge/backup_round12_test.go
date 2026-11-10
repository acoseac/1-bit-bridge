package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

func TestBackupReportersSayWhenVerificationDidNotStart(t *testing.T) {
	for _, refusal := range []error{backup.ErrBackupsUnlocked, backup.ErrBackupsBusy, backup.ErrCheckStillRunning} {
		t.Run(refusal.Error(), func(t *testing.T) {
			var out, errs bytes.Buffer
			err := fmt.Errorf("admission: %w", refusal)
			if reportVerdict(&out, &errs, "", backup.VerifyResult{}, err) {
				t.Error("refused verification reported success")
			}
			reportTickerVerdict(context.Background(), &out, &errs, "startup", backup.Result{Dir: "snap", VerifyErr: err})
			if strings.Count(errs.String(), "the verification did not start") != 2 || !strings.Contains(errs.String(), refusal.Error()) {
				t.Errorf("CLI and ticker did not name admission: %s", errs.String())
			}
			if strings.Contains(errs.String(), "what this check found") || out.Len() != 0 {
				t.Errorf("a refused check claimed findings: %s%s", out.String(), errs.String())
			}
		})
	}
	t.Run("busy cancellation wrote incomplete", func(t *testing.T) {
		var out, errs bytes.Buffer
		res := backup.VerifyResult{Write: backup.WriteRecorded, Recorded: true, RecordRead: true,
			Found:  backup.Verification{DatabaseState: backup.StateIncomplete, FilesState: backup.StateIncomplete},
			Record: backup.Verification{DatabaseState: backup.StateIncomplete, FilesState: backup.StateIncomplete}}
		err := errors.Join(backup.ErrBackupsBusy, context.Canceled)
		reportVerdict(&out, &errs, "", res, err)
		reportTickerVerdict(context.Background(), &out, &errs, "startup", backup.Result{Dir: "snap", Verify: res, VerifyErr: err})
		if strings.Contains(errs.String(), "did not start") || strings.Count(errs.String(), "it wrote its record") != 2 {
			t.Errorf("lost the cancellation's write outcome: %s%s", out.String(), errs.String())
		}
	})
}

func TestADisprovedNoBarrierRecordCannotAnchorAFreshProcessPrune(t *testing.T) {
	const childRoot = "BRIDGE_ROUND12_PRUNE_ROOT"
	if root := os.Getenv(childRoot); root != "" {
		pruned, err := backup.Prune(root, 1)
		awaitHoldsLetGo(t, root)
		if err != nil || pruned.Skipped {
			t.Fatalf("fresh-process prune: %+v, %v", pruned, err)
		}
		return
	}
	for _, placement := range []string{"bundle", "root fallback", "database failed beside incomplete files"} {
		t.Run(placement, func(t *testing.T) {
			cfgPath, _, root := backupInstall(t)
			t.Cleanup(func() { awaitHoldsLetGo(t, root) })
			anchor := verifiedInstallSnapshot(t, cfgPath)
			awaitHoldsLetGo(t, root)
			// Age a genuinely verified bundle, preserving its bound name.
			var old backup.Manifest
			data, err := os.ReadFile(filepath.Join(anchor, backup.ManifestFile))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &old); err != nil {
				t.Fatal(err)
			}
			old.CreatedAt = time.Now().UTC().Add(-72 * time.Hour)
			aged := filepath.Join(root, old.CreatedAt.Format("2006-01-02T15-04-05Z"))
			old.Verification.Snapshot = filepath.Base(aged)
			if err := os.Rename(anchor, aged); err != nil {
				t.Fatal(err)
			}
			anchor = aged
			data, err = json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(anchor, backup.ManifestFile), data, 0o600); err != nil {
				t.Fatal(err)
			}
			intermediate := writeSnapshotDirAt(t, root, time.Now().Add(-48*time.Hour), &backup.Verification{Format: 1,
				DatabaseState: backup.StateFailed, FilesState: backup.StateVerified})
			dir := verifiedInstallSnapshot(t, cfgPath)
			awaitHoldsLetGo(t, root)
			flipBundleByte(t, dir, backup.ManifestDBFileName)
			t.Cleanup(backup.SetSyncsParentDirForTest(false))
			if placement == "root fallback" {
				t.Cleanup(backup.SetVerdictWriteFailuresForTest(nil, nil, func(path string) error {
					if filepath.Dir(path) == dir {
						return errors.New("injected bundle veto refusal")
					}
					return nil
				}))
			}
			t.Cleanup(func() { awaitHoldsLetGo(t, root) })
			checkCtx, cancelCheck := context.WithCancel(context.Background())
			defer cancelCheck()
			if placement == "database failed beside incomplete files" {
				t.Cleanup(backup.SetFilesCheckIncompleteForTest(dir))
				t.Cleanup(func() { awaitHoldsLetGo(t, root) })
			}
			res, err := backup.VerifySnapshot(checkCtx, dir, backup.VerifyOptions{BackupsRoot: root})
			awaitHoldsLetGo(t, root)
			if !errors.Is(err, backup.ErrRecheckNotRecorded) || res.Found.DatabaseState != backup.StateFailed {
				t.Fatalf("disproving recheck: %+v, %v", res, err)
			}
			if checkCtx.Err() != nil {
				t.Fatalf("the files seam stopped the verification caller: %v", checkCtx.Err())
			}
			if placement == "database failed beside incomplete files" &&
				(res.Found.FilesState != backup.StateIncomplete || !slices.Equal(res.Found.FilesReasons, []backup.Reason{backup.ReasonDeadline})) {
				t.Fatalf("files phase did not end INCOMPLETE: %+v", res.Found)
			}
			if res.Record.DatabaseState != backup.StateIncomplete {
				t.Errorf("disproved record still trusted at read-back: %+v", res.Record)
			}
			entries, err := backup.List(root)
			if err != nil {
				t.Fatal(err)
			}
			listed := false
			for _, entry := range entries {
				if entry.DirName == filepath.Base(dir) {
					listed = true
					if entry.Verdict.DatabaseState != backup.StateIncomplete || entry.Verdict.FilesState != backup.StateIncomplete {
						t.Errorf("listing still trusts the disproved record: %+v", entry.Verdict)
					}
				}
			}
			if !listed {
				t.Error("listing lost the disproved snapshot")
			}
			st, err := backup.ReadStatus(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if newest, ok := st.NewestVerified(); !ok || newest.DirName != filepath.Base(anchor) {
				t.Errorf("newest verified = %+v, %v; want older anchor", newest, ok)
			}
			if action := decideStartupPass(st, time.Now(), 24*time.Hour).action; action == startupSkipVerified {
				t.Error("startup still trusted the disproved snapshot")
			}
			for _, line := range backupLines(t, cfgPath) {
				if line.Name == "backup-newest" && strings.Contains(line.Summary, "VERIFIED") {
					t.Errorf("doctor still trusted the disproved snapshot: %s", line.Summary)
				}
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestADisprovedNoBarrierRecordCannotAnchorAFreshProcessPrune$")
			cmd.Env = append(os.Environ(), childRoot+"="+root)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fresh process: %v\n%s", err, out)
			}
			if _, err := os.Stat(anchor); err != nil {
				t.Errorf("fresh-process prune removed the older trustworthy anchor: %v", err)
			}
			if _, err := os.Stat(intermediate); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("retention kept an intermediate failure: %v", err)
			}
		})
	}
}

func TestAFailedNoBarrierVetoIsReportedAndSkipsPruning(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	dir := verifiedInstallSnapshot(t, cfgPath)
	awaitHoldsLetGo(t, root)
	flipBundleByte(t, dir, backup.ManifestDBFileName)
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	var attempts atomic.Int32
	t.Cleanup(backup.SetVerdictWriteFailuresForTest(nil, nil, func(string) error {
		attempts.Add(1)
		return errors.New("injected veto write refusal")
	}))
	var pruned atomic.Bool
	t.Cleanup(backup.SetOnPruneForTest(func() { pruned.Store(true) }))
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	res := backup.Finish(t.Context(), dir, func() int { return 1 }, backup.VerifyOptions{BackupsRoot: root})
	awaitHoldsLetGo(t, root)
	if attempts.Load() != 2 {
		t.Errorf("veto attempts = %d, want bundle and root", attempts.Load())
	}
	if !res.PruneSkipped || pruned.Load() || res.VerifyErr == nil || !strings.Contains(res.VerifyErr.Error(), "disproved record could not be vetoed") {
		t.Fatalf("failed veto did not skip/report: %+v, prune ran %v", res, pruned.Load())
	}
	var out, errs bytes.Buffer
	reportVerdict(&out, &errs, "", res.Verify, res.VerifyErr)
	reportTickerVerdict(context.Background(), &out, &errs, "startup", res)
	if strings.Count(errs.String(), "disproved record could not be vetoed") != 2 {
		t.Errorf("reporters hid the veto failure: %s", errs.String())
	}
}
