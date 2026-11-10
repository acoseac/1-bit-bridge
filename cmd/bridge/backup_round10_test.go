package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestTheTickerReportsAPanickedCheckAtShutdown drives the real check's panic
// recovery, with shutdown before publication or after the worker's answer.
func TestTheTickerReportsAPanickedCheckAtShutdown(t *testing.T) {
	for _, when := range []string{"after check before publication", "after worker answer"} {
		t.Run(when, func(t *testing.T) {
			_, dataDir, root := backupInstall(t)
			dir, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)})
			if err != nil {
				t.Fatal(err)
			}
			dir, err = fsutil.ResolveLinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
				panic("injected check panic")
			}))
			if when == "after check before publication" {
				t.Cleanup(backup.SetAfterCheckForTest(func(snap string) {
					if snap == dir {
						cancel()
					}
				}))
			}
			// The synchronous caller below returns before its worker lets go
			// of its hold. Drain that hold before either seam is restored.
			t.Cleanup(func() { cancel(); awaitVerificationsEnd(t, root) })
			v, verifyErr := backup.VerifySnapshot(ctx, dir, backup.VerifyOptions{})
			if when == "after check before publication" {
				if ctx.Err() == nil || !errors.Is(verifyErr, context.Canceled) {
					t.Fatalf("after-check stop was not returned: context %v, verification %v", ctx.Err(), verifyErr)
				}
			} else {
				if verifyErr != nil {
					t.Fatalf("check panic without a stop returned %v", verifyErr)
				}
				cancel()
			}
			if !v.RecordRead || backup.StateWords(v.Found.DatabaseState, v.Found.DatabaseReasons) != "INCOMPLETE: aborted" ||
				backup.StateWords(v.Record.DatabaseState, v.Record.DatabaseReasons) != "INCOMPLETE: aborted" {
				t.Fatalf("panic did not produce a read-back aborted verdict: %+v, %v", v, verifyErr)
			}
			var out, errs bytes.Buffer
			reportTickerVerdict(ctx, &out, &errs, "shutdown", backup.Result{Dir: dir, Verify: v, VerifyErr: verifyErr})
			if out.Len() != 0 || !strings.Contains(errs.String(), "database INCOMPLETE: aborted") ||
				!strings.Contains(errs.String(), "files INCOMPLETE: aborted") {
				t.Errorf("shutdown hid the aborted check: stdout %q, stderr %q", out.String(), errs.String())
			}
		})
	}
}

// TestTheTickerSilencesOnlyCancelledRecordsAtShutdown covers the no-error
// branch, where both verdicts and every INCOMPLETE reason govern silence.
func TestTheTickerSilencesOnlyCancelledRecordsAtShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := []backup.Reason{backup.ReasonCancelled}
	aborted := []backup.Reason{backup.ReasonAborted}
	deadline := []backup.Reason{backup.ReasonDeadline}
	for _, tc := range []struct {
		name            string
		database, files backup.State
		dbWhy, filesWhy []backup.Reason
		want            string
	}{
		{"database cancellation alone", backup.StateIncomplete, backup.StateVerified, cancelled, nil, ""},
		{"both cancelled", backup.StateIncomplete, backup.StateIncomplete, cancelled, cancelled, ""},
		{"files cancellation alone", backup.StateVerified, backup.StateIncomplete, nil, cancelled, ""},
		{"no database and files cancelled", backup.StateUnverified, backup.StateIncomplete, []backup.Reason{backup.ReasonNoDatabase}, cancelled, ""},
		{"cancelled database and failed files", backup.StateIncomplete, backup.StateFailed, cancelled, []backup.Reason{backup.ReasonHash}, "files FAILED: hash-mismatch"},
		{"cancelled database and aborted files", backup.StateIncomplete, backup.StateIncomplete, cancelled, aborted, "files INCOMPLETE: aborted"},
		{"cancelled database and expired files", backup.StateIncomplete, backup.StateIncomplete, cancelled, deadline, "files INCOMPLETE: deadline"},
		{"aborted database", backup.StateIncomplete, backup.StateVerified, aborted, nil, "database INCOMPLETE: aborted"},
		{"expired database", backup.StateIncomplete, backup.StateVerified, deadline, nil, "database INCOMPLETE: deadline"},
		{"failed database and cancelled files", backup.StateFailed, backup.StateIncomplete, []backup.Reason{backup.ReasonHash}, cancelled, "database FAILED: hash-mismatch"},
		{"database reason absent", backup.StateIncomplete, backup.StateVerified, nil, nil, "database INCOMPLETE"},
		{"files reason absent", backup.StateIncomplete, backup.StateIncomplete, cancelled, nil, "files INCOMPLETE"},
		{"extra database reason", backup.StateIncomplete, backup.StateVerified, []backup.Reason{backup.ReasonCancelled, backup.ReasonAborted}, nil, "database INCOMPLETE: cancelled, aborted"},
		{"extra files reason", backup.StateIncomplete, backup.StateIncomplete, cancelled, []backup.Reason{backup.ReasonCancelled, backup.ReasonDeadline}, "files INCOMPLETE: cancelled, deadline"},
		{"no incomplete verdict", backup.StatePending, backup.StatePending, nil, nil, "database PENDING"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := backup.Verification{
				DatabaseState: tc.database, DatabaseReasons: tc.dbWhy,
				FilesState: tc.files, FilesReasons: tc.filesWhy,
			}
			var out, errs bytes.Buffer
			reportTickerVerdict(ctx, &out, &errs, "shutdown", backup.Result{
				Dir: "snapshot", Verify: backup.VerifyResult{Record: v, RecordRead: true},
			})
			if out.Len() != 0 {
				t.Errorf("non-passing record printed on stdout: %q", out.String())
			}
			if tc.want == "" {
				if errs.Len() != 0 {
					t.Errorf("cancellation-only record printed %q", errs.String())
				}
			} else if !strings.Contains(errs.String(), tc.want) {
				t.Errorf("shutdown hid %q: stderr %q", tc.want, errs.String())
			}
		})
	}
}
