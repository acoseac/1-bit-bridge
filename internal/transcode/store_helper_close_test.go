package transcode

import (
	"context"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestAStoreHelperClosesBeforeItsTempDirGoes pins the close inside the
// helper. A cleanup registered before the helper runs after every
// cleanup the helper registers, so it sees a store that is already
// closed. The helper registers that close after TempDir, and cleanups
// run last-registered first, so the close runs before the directory
// goes.
func TestAStoreHelperClosesBeforeItsTempDirGoes(t *testing.T) {
	t.Run("batch", func(t *testing.T) {
		assertHelperClosesStore(t, openTempStoreForBatch)
	})
	t.Run("pool", func(t *testing.T) {
		assertHelperClosesStore(t, openTempStoreForPool)
	})
}

func assertHelperClosesStore(t *testing.T, open func(*testing.T) *manifest.Store) {
	t.Helper()
	var s *manifest.Store
	t.Cleanup(func() {
		if s == nil {
			t.Error("helper returned no store")
			return
		}
		_, err := s.CountTracks(context.Background())
		if err == nil || !strings.Contains(err.Error(), "sql: database is closed") {
			t.Errorf("store still open after the helper's cleanup: %v", err)
		}
	})
	s = open(t)
}
