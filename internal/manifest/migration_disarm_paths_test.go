package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

func TestMigrationMarkerDiagnosticsCarryNoPaths(t *testing.T) {
	for _, remove := range []bool{true, false} {
		t.Run(fmt.Sprintf("removable=%v", remove), func(t *testing.T) {
			dir := t.TempDir()
			db := olderStoreIn(t, dir)
			marker := updater.StatePath(dir)
			if err := os.Mkdir(marker, 0o700); err != nil {
				t.Fatal(err)
			}
			if !remove {
				if err := os.Mkdir(filepath.Join(marker, "held"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			logs := loggingtest.Record(t)
			s, err := OpenStore(db)
			if s != nil {
				defer s.Close()
			}
			if remove {
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, rec := range logs.Records() {
					found = found || strings.Contains(rec.Message, "removed an install marker that could not be read")
				}
				if !found {
					t.Error("missing unreadable-marker warning")
				}
			} else {
				if err == nil {
					t.Fatal("store migrated past an unreadable marker")
				}
				for _, path := range []string{dir, marker} {
					if strings.Contains(err.Error(), path) {
						t.Errorf("store refusal carries %q: %v", path, err)
					}
				}
				if !strings.Contains(err.Error(), "install marker") {
					t.Errorf("store refusal lost context: %v", err)
				}
			}
			if leaked := logs.Carrying(dir, marker); len(leaked) != 0 {
				t.Errorf("marker diagnostic leaked paths: %v", leaked)
			}
		})
	}
}
