package admin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestTheConsoleReportsCancellationAndEveryWriteHandoff drives the job on
// its server context, including the outer expiration with no worker answer.
func TestTheConsoleReportsCancellationAndEveryWriteHandoff(t *testing.T) {
	for _, mode := range []string{"published then outer expiration", "published then worker answer", "cancelled check", "given-up write"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := newTestServer(t)
			src := liveStore(t, t.TempDir())
			capture, err := backup.CaptureSnapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(capture.Release)
			dir, err := fsutil.ResolveLinks(capture.Dir)
			if err != nil {
				t.Fatal(err)
			}
			mpath := filepath.Join(dir, backup.ManifestFile)
			// FAILED/FAILED needs no veto and reaches the write on every platform.
			for _, name := range []string{backup.ManifestDBFileName, "server.crt"} {
				if mode == "cancelled check" {
					break
				}
				if err := os.WriteFile(filepath.Join(dir, name), []byte("damaged"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			s.deps.ScanCtx = ctx
			release := make(chan struct{})
			letGo := sync.OnceFunc(func() { close(release) })
			entered := make(chan struct{})
			enter := sync.OnceFunc(func() { close(entered) })
			var expired, published atomic.Bool
			// Let SQLite finish at its normal deadline. Only the worker cases'
			// parked write/read-back expire; cancellation drives the outer case.
			if mode == "published then worker answer" || mode == "given-up write" {
				t.Cleanup(backup.SetStepBoundForTest(dir, 300*time.Millisecond, "its verdict's write", "its read of the record after it"))
			}
			switch mode {
			case "cancelled check":
				t.Cleanup(backup.SetOnLowPriorityForTest(func() { enter(); <-release }))
			case "given-up write":
				t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(path string) error {
					if path == dir {
						enter()
						<-release
					}
					return nil
				}, nil, nil))
			default:
				t.Cleanup(backup.SetSyncVerdictForTest(func(path string) error {
					if path == mpath {
						data, err := os.ReadFile(path)
						published.Store(err == nil && strings.Contains(string(data), `"databaseState": "FAILED"`))
						enter()
						<-release
					}
					return fsutil.SyncParentDir(path)
				}))
				t.Cleanup(backup.SetReadBackFailureForTest(func(path string) error {
					if path == dir {
						<-release
					}
					return errors.New("injected read-back failure")
				}))
			}
			t.Cleanup(backup.SetVerifyHandoffForTest(func(path string) {
				if path == capture.Dir && mode == "published then outer expiration" {
					<-release
				}
			}, func(path string) {
				if path == capture.Dir {
					expired.Store(true)
				}
			}))
			logs := loggingtest.Record(t)
			// This cleanup is registered after every seam and logger restore.
			t.Cleanup(func() { letGo(); cancel(); s.WaitForBackupJobs(); backuptest.AwaitIdle(t) })
			s.verifyInBackground(capture, filepath.Dir(dir), 0)
			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				t.Fatal("verification never reached its stall")
			}
			if mode == "published then outer expiration" {
				cancel()
			}
			if mode == "cancelled check" {
				cancel()
				letGo()
			}
			s.WaitForBackupJobs()
			var want string
			switch mode {
			case "published then outer expiration":
				want = "write=unknown"
				if !expired.Load() {
					t.Error("the outer expiration branch was not reached")
				}
			case "cancelled check":
				want = "recordRead=true"
			default:
				want = "did not finish before it was given up"
				if expired.Load() {
					t.Error("outer expiration won instead of the worker answer")
				}
			}
			all := strings.Join(logs.All(), "\n")
			if !strings.Contains(all, want) || !strings.Contains(all, "write=") || !strings.Contains(all, "recordRead=") {
				t.Errorf("missing %q or outcome fields:\n%s", want, all)
			}
			if strings.Contains(all, "not recorded") {
				t.Errorf("write misreported:\n%s", all)
			}
			if strings.HasPrefix(mode, "published") && !published.Load() {
				t.Error("the verdict had not published before sync stalled")
			}
		})
	}
}
