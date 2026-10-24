//go:build darwin

package backup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestAProtectedMarkerNamesTheMarker: a sealed marker not yet minted, beside
// a valid seal and the box's floor and UUID, carries the immutable (uchg) or
// append-only (uappnd) flag, which a user may set on their own file on
// macOS. Completion reads it and writes the minted marker, whose rename over
// it is then EPERM: a flag on the marker, never on the directory. The advice
// is the marker's union, which names the flag on player-initialized.
func TestAProtectedMarkerNamesTheMarker(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	for _, flag := range []struct {
		name string
		bit  int
	}{{"uchg", unix.UF_IMMUTABLE}, {"uappnd", unix.UF_APPEND}} {
		t.Run(flag.name, func(t *testing.T) {
			dir := t.TempDir()
			sealedMint(t, dir)
			marker := filepath.Join(dir, "player-initialized")
			if err := os.WriteFile(marker, []byte(`{"version":1,"sealed":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := unix.Chflags(marker, flag.bit); err != nil {
				t.Fatalf("set the %s flag: %v", flag.name, err)
			}
			t.Cleanup(func() { _ = unix.Chflags(marker, 0) })
			want := entryAdvice(dir, "player-initialized", "")
			if line := completionWarning(t, dir, true); !strings.Contains(line, want) {
				t.Fatalf("advice for a %s marker:\n%s\nwant it to end:\n%s", flag.name, line, want)
			}
		})
	}
}
