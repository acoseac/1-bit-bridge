package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/trash"
	"github.com/acoseac/1-bit-bridge/internal/upload"
)

// TestARestoreAndACommitToOnePathTakeTheSameDestinationLock — a restore and a
// commit of the same path share the upload destination lock. The restore's
// existence check blocks while it holds the lock; the commit's check must not
// run until that lock is released.
func TestARestoreAndACommitToOnePathTakeTheSameDestinationLock(t *testing.T) {
	root := t.TempDir()
	rel := "Artist/01.flac"
	abs := filepath.Join(root, "Artist", "01.flac")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("SEED"), 0o644); err != nil {
		t.Fatal(err)
	}

	restoreEntered := make(chan struct{})
	restoreRelease := make(chan struct{})
	commitEntered := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(restoreRelease) }) }
	t.Cleanup(release)

	on := true
	roots := func() []string { return []string{root} }
	trashMgr := trash.New(roots, bridgefs.New([]string{root}), func() bool { return on }, trash.DefaultTTL,
		trash.WithDestStat(func(p string) (os.FileInfo, error) {
			close(restoreEntered)
			<-restoreRelease
			return os.Stat(p)
		}),
	)
	up := upload.NewManager(upload.Config{}, roots,
		upload.WithFreeBytes(func(string) (int64, error) { return 1 << 40, nil }),
		upload.WithDestStat(func(p string) (os.FileInfo, error) {
			close(commitEntered)
			return os.Stat(p)
		}),
	)
	trash.WithDestinationLock(up.LockDestination)(trashMgr)

	if _, err := trashMgr.Trash("", []string{rel}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := trashMgr.List()
	if err != nil || len(entries) != 1 {
		t.Fatalf("list = %v, %d entries", err, len(entries))
	}
	body := []byte("NEWDATA!")
	s, err := up.Create([]upload.FileDecl{{Path: rel, Size: int64(len(body))}}, upload.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := up.WriteChunk(s.ID, s.Files[0].ID, 0, bytes.NewReader(body), nil, 0); err != nil {
		t.Fatal(err)
	}

	restoreDone := make(chan error, 1)
	go func() {
		_, err := trashMgr.Restore([]string{entries[0].ID})
		restoreDone <- err
	}()
	select {
	case <-restoreEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("restore never reached its existence check")
	}

	commitDone := make(chan error, 1)
	go func() {
		_, err := up.Commit(s.ID)
		commitDone <- err
	}()
	select {
	case <-commitEntered:
		t.Fatal("commit checked the destination while restore held it")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-commitEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("commit never checked the destination after restore released it")
	}
	if err := <-restoreDone; err != nil {
		t.Errorf("restore: %v", err)
	}
	if err := <-commitDone; err != nil {
		t.Errorf("commit: %v", err)
	}
	if got, err := os.ReadFile(abs); err != nil || string(got) != "ORIGINAL" {
		t.Errorf("destination = %q, %v", got, err)
	}
}
