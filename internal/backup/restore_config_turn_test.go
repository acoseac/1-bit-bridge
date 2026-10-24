package backup_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestARestoreLeavesAConfigChangedInItsTurn: a restore that writes
// bridge.yaml compares the file, just before the rename that would replace
// it, with what the restore found once it held the file's lock, so a config
// an edit (or a bridge older than the lock) changed meanwhile is left as that
// writer left it and the restore says so, rather than putting the snapshot's
// copy over it. Where no lock can be taken that comparison is all that keeps
// another writer's save; until B506's second round a restore had none.
func TestARestoreLeavesAConfigChangedInItsTurn(t *testing.T) {
	dataDir := t.TempDir()
	src := primeLiveState(t, dataDir)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	edited := []byte("libraryRoots:\n  - /edited/while/the/restore/ran\n")
	opts := backup.WithBeforeConfigForTest(backup.RestoreOptions{}, func() {
		if err := os.WriteFile(src.BridgeYAML, edited, 0o600); err != nil {
			t.Error(err)
		}
	})
	err = backup.RestoreWithOptions(snap, backup.Targets{
		ManifestDB: src.ManifestDB,
		TokensJSON: src.TokensJSON,
		ServerCert: src.ServerCert,
		ServerKey:  src.ServerKey,
		BridgeYAML: src.BridgeYAML,
	}, opts)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("a restore over a config another writer changed in its turn = %v, want a refusal that says the file changed", err)
	}
	if got := readBytes(t, src.BridgeYAML); !bytes.Equal(got, edited) {
		t.Errorf("the restore wrote the snapshot's bridge.yaml over the other writer's:\n%s", got)
	}
}
