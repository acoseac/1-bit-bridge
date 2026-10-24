//go:build unix

package backup_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func TestPlayerHistoryNamedPipeRefusesWithoutWaiting(t *testing.T) {
	reservationStorage(t)
	for _, op := range []string{"reset", "backup", "restore_live", "restore_snapshot"} {
		t.Run(op, func(t *testing.T) {
			dir, snap := t.TempDir(), identitySnapshot(t, true)
			if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), boxPlayerUUID); err != nil {
				t.Fatal(err)
			}
			hp := filepath.Join(dir, historyFile)
			if op == "restore_snapshot" {
				hp = filepath.Join(snap, historyFile)
				listSnapshotHistory(t, snap)
			}
			if err := syscall.Mkfifo(hp, 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var err error
				switch op {
				case "reset":
					_, err = backup.ResetPlayer(dir)
				case "backup":
					_, err = backup.Snapshot(t.Context(), backup.Sources{DataDir: dir})
				default:
					err = backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
				}
				done <- err
			}()
			// The watchdog is only for a broken nonblocking-open control. Normal
			// operation must refuse without a pipe writer or a timer firing.
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "named pipe") {
					t.Fatalf("pipe: %v", err)
				}
			case <-timer.C:
				fd, err := syscall.Open(hp, syscall.O_WRONLY|syscall.O_NONBLOCK, 0600)
				if err == nil {
					_ = syscall.Close(fd)
				}
				<-done
				t.Fatal("history waited for a pipe writer")
			}
			if id, _ := config.ReadPlayerUUID(dir); id != boxPlayerUUID {
				t.Fatal("pipe refusal changed identity")
			}
			_ = os.Remove(hp)
		})
	}
}
