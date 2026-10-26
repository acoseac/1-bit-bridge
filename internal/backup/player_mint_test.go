package backup_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// uuidV4 is the form EnsurePlayerUUID writes: a random (version 4, RFC 4122
// variant) UUID in lowercase.
var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestThePlayerUUIDIsMadeOnceAndKept: the UUID is random, made the first
// time it is asked for, kept in the data directory's player-uuid, and the
// same every time after, whatever the config or the binding become; two
// data directories never share one, so it is not derived from anything a
// config holds.
func TestThePlayerUUIDIsMadeOnceAndKept(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if _, err := config.ReadPlayerUUID(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadPlayerUUID on a fresh data dir = %v, want fs.ErrNotExist", err)
	}
	id, created, err := backup.EnsurePlayerUUID(dir)
	if err != nil || !created || !uuidV4.MatchString(id) {
		t.Fatalf("EnsurePlayerUUID = %q, %v, %v", id, created, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, config.PlayerUUIDFile))
	if err != nil || strings.TrimSpace(string(b)) != id {
		t.Fatalf("player-uuid holds %q, %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dir, config.PlayerUUIDFile)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("player-uuid mode %v, %v", st.Mode().Perm(), err)
		}
	}
	for i := 0; i < 3; i++ {
		again, created, err := backup.EnsurePlayerUUID(dir)
		if err != nil || created || again != id {
			t.Fatalf("EnsurePlayerUUID again = %q, %v, %v; want %q kept", again, created, err, id)
		}
	}
	if got, err := config.ReadPlayerUUID(dir); err != nil || got != id {
		t.Fatalf("ReadPlayerUUID = %q, %v", got, err)
	}
	other, _, err := backup.EnsurePlayerUUID(t.TempDir())
	if err != nil || other == id {
		t.Fatalf("a second data dir got %q, %v", other, err)
	}
	for _, garbage := range []string{"", "not-a-uuid\n", id + id, strings.ToUpper(id),
		" " + id + "\n", "\n" + id, "\xef\xbb\xbf" + id + "\n", id + " x\n", id + "\n" + id + "\n", id + "\x00\n", id[:35] + " \n"} {
		bad := t.TempDir()
		if err := os.WriteFile(filepath.Join(bad, config.PlayerUUIDFile), []byte(garbage), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := config.ReadPlayerUUID(bad); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("player-uuid %q read as %q, %v; want a refusal", garbage, got, err)
		}
		if got, _, err := backup.EnsurePlayerUUID(bad); err == nil {
			t.Errorf("EnsurePlayerUUID replaced a player-uuid it could not read with %q", got)
		}
	}
}

// A refused medium must not leave an identity a retry could adopt unprobed.
func TestPlayerUUIDRefusingProbeLeavesNoIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The storage-medium check is Unix-only, so on Windows the probe is never
		// consulted (the retired config test lived in a unix-only file too).
		t.Skip("the storage-medium probe is Unix-only")
	}
	reservationStorage(t)
	dir := t.TempDir()
	path := filepath.Join(dir, config.PlayerUUIDFile)
	probes := 0
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error {
		probes++
		return atomicwrite.ErrVolatile
	}))
	for attempt := 1; attempt <= 2; attempt++ {
		before := probes
		id, made, err := backup.EnsurePlayerUUID(dir)
		if !errors.Is(err, atomicwrite.ErrVolatile) || id != "" || made {
			t.Errorf("attempt %d: UUID = %q, %t, %v; want refused medium", attempt, id, made, err)
		}
		if probes <= before {
			t.Errorf("attempt %d did not probe storage", attempt)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("attempt %d left the final UUID path: %v", attempt, err)
		}
	}
}
