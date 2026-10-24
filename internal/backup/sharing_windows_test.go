package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// releaseOnReadRetry uses the context's backoff notification to release a real
// conflicting Windows handle only AFTER a failed read. No wall-clock release
// can race ahead of the read and accidentally let a non-retrying backup pass.
type releaseOnReadRetry struct {
	context.Context
	release func()
	waits   int
}

func (c *releaseOnReadRetry) Done() <-chan struct{} {
	c.waits++
	if c.release != nil {
		c.release()
		c.release = nil
	}
	return c.Context.Done()
}

// sameDir reports whether a and b name one directory. WriteBytesDurable
// renames onto the path its root resolves to (filepath.EvalSymlinks), and on
// a Windows runner that expands t.TempDir()'s 8.3 short name (RUNNER~1), so a
// string comparison with the path as written never matches: the rename seam
// then held nothing and the test proved no retry (CI, 2026-10-24).
func sameDir(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	return err == nil && os.SameFile(ai, bi)
}

func TestSnapshotRetriesAHandleHeldAcrossRename(t *testing.T) {
	for _, name := range []string{engine.ReservationFileName, config.PlayerUUIDFile, "tokens.json", "captured-player-uuid"} {
		t.Run(name, func(t *testing.T) {
			reservationStorage(t)
			dir := t.TempDir()
			identity := filepath.Join(dir, config.PlayerUUIDFile)
			if err := config.WritePlayerUUIDFile(identity, boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, boxPlayerUUID, 90000, 80000)
			fileName := name
			capturedUUID := name == "captured-player-uuid"
			if capturedUUID {
				fileName = config.PlayerUUIDFile
			}
			path := filepath.Join(dir, fileName)
			data := []byte("tokens held across rename")
			if name != "tokens.json" {
				data = readBytes(t, path)
			}
			ctx := &releaseOnReadRetry{Context: t.Context()}
			// The existing rename seam holds the staged file just as an indexer
			// can. DELETE sharing permits its rename; lack of READ sharing
			// prevents the subsequent backup from opening the renamed file.
			prev := atomicwrite.SetRenameFuncForTest(func(src, dst string) error {
				match := strings.EqualFold(filepath.Base(dst), fileName) && sameDir(filepath.Dir(dst), dir)
				if capturedUUID {
					match = strings.EqualFold(filepath.Base(dst), config.PlayerUUIDFile) && !sameDir(filepath.Dir(dst), dir)
				}
				if !match {
					return os.Rename(src, dst)
				}
				p, err := windows.UTF16PtrFromString(src)
				if err != nil {
					return err
				}
				h, err := windows.CreateFile(p, windows.GENERIC_WRITE,
					windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
					windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
				if err != nil {
					return err
				}
				if err := os.Rename(src, dst); err != nil {
					_ = windows.CloseHandle(h)
					return err
				}
				ctx.release = func() {
					if err := windows.CloseHandle(h); err != nil {
						t.Error(err)
					}
				}
				// Prove the fixture fails with precisely CI's error while the
				// handle stays held until Snapshot enters its read backoff.
				f, openErr := os.Open(dst)
				if openErr == nil {
					_ = f.Close()
				}
				if !errors.Is(openErr, windows.ERROR_SHARING_VIOLATION) {
					t.Fatalf("unprotected open = %v", openErr)
				}
				return nil
			})
			t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(prev) })
			t.Cleanup(func() {
				if ctx.release != nil {
					ctx.release()
					ctx.release = nil
				}
			})
			if !capturedUUID {
				if err := atomicwrite.WriteBytesDurable(dir, path, data, ".sharing-*.tmp"); err != nil {
					t.Fatal(err)
				}
				if ctx.release == nil {
					t.Fatal("the rename seam held no handle on the renamed file: the fixture never engaged")
				}
			}
			src := backup.Sources{DataDir: dir, PlayerUUID: identity}
			if name == "tokens.json" {
				src.TokensJSON = path
			}
			snap, err := backup.Snapshot(ctx, src)
			if err != nil {
				t.Fatal(err)
			}
			if ctx.waits != 1 {
				t.Fatalf("retry waits = %d, want one failed read then success", ctx.waits)
			}
			if got := string(readBytes(t, filepath.Join(snap, fileName))); got != string(data) {
				t.Fatalf("snapshot bytes = %q, want %q", got, data)
			}
			mark := markAt(t, snap)
			if mark.IDs != 90000 || mark.Tokens != 80000 {
				t.Fatalf("snapshot lost floor: %+v", mark)
			}
		})
	}
}

func TestSnapshotRefusesAReservationStillHeldOrCancelled(t *testing.T) {
	for _, name := range []string{"held", "cancelled", "expired"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			identity := filepath.Join(dir, config.PlayerUUIDFile)
			if err := config.WritePlayerUUIDFile(identity, boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			putMark(t, dir, boxPlayerUUID, 90000, 80000)
			p, err := windows.UTF16PtrFromString(filepath.Join(dir, engine.ReservationFileName))
			if err != nil {
				t.Fatal(err)
			}
			h, err := windows.CreateFile(p, windows.GENERIC_WRITE,
				windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
				windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(h)
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "expired" {
				var expire context.CancelFunc
				base, expire = context.WithDeadline(t.Context(), time.Unix(0, 0))
				defer expire()
			}
			ctx := &releaseOnReadRetry{Context: base}
			want := error(windows.ERROR_SHARING_VIOLATION)
			waits := 4
			if name == "cancelled" {
				// Cancel only once the real read has failed, keeping the handle
				// held throughout; cancellation must not become a missing floor.
				ctx.release = cancel
				want, waits = context.Canceled, 1
			} else if name == "expired" {
				want, waits = context.DeadlineExceeded, 1
			}
			snap, err := backup.Snapshot(ctx, backup.Sources{DataDir: dir, PlayerUUID: identity})
			if !errors.Is(err, want) || snap != "" || ctx.waits != waits {
				t.Fatalf("snapshot = %q, %v after %d waits; want %v after %d", snap, err, ctx.waits, want, waits)
			}
			assertNoPlayerResetAdvice(t, err.Error())
			if !strings.Contains(err.Error(), engine.ReservationFileName) || !strings.Contains(err.Error(), want.Error()) {
				t.Errorf("read failure does not name its file and cause: %v", err)
			}
			if name == "held" && (!strings.Contains(err.Error(), "busy") || !strings.Contains(err.Error(), "retry the backup")) {
				t.Errorf("exhausted read lacks busy/retry advice: %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(dir, backup.BackupsDirName))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed snapshot left %v: %v", entries, err)
			}
		})
	}
}
