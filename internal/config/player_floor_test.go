package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

func TestPlayerUUIDCommitRequiresDirectorySync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PlayerUUIDFile)
	stop := errors.New("UUID directory barrier failed")
	landed := false
	prev := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
		err := os.Rename(a, b)
		if err == nil && b == path {
			landed = true
		}
		return err
	})
	defer atomicwrite.SetRenameFuncForTest(prev)
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
		if landed {
			return stop
		}
		return nil
	}))
	if err := WritePlayerUUIDFile(path, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); !errors.Is(err, stop) {
		t.Fatalf("reported durable UUID before its directory sync: %v", err)
	}
}
