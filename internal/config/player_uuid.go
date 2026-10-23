package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// PlayerUUIDFile is where the player's UUID is kept: this file in the data
// directory the config names (dataDir), one line holding a random
// version-4 UUID in lowercase. It is the player's identity to a controller
// (the OpenHome UDN, the /v1/outputs id), so it is made once, the first
// time a player that is enabled asks for it (EnsurePlayerUUID), and kept:
// a config rewrite leaves the data directory alone, and neither a DAC swap
// nor a rebinding touches it. bridge backup carries it, and bridge restore
// gives the box the snapshot's (internal/backup), so a restored player is
// the device its controllers knew. It is never derived from a serial, a
// card, the certificate or the library's name.
const PlayerUUIDFile = "player-uuid"

// maxPlayerUUIDFile bounds the read of PlayerUUIDFile: one UUID and its
// newline, with room to spare.
const maxPlayerUUIDFile = 128

// playerUUIDShape is the form PlayerUUIDFile holds: a UUID in lowercase.
var playerUUIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ErrNotOnePlayerUUID is what ReadPlayerUUIDFile's error wraps for a file
// that is there and holds anything but one UUID.
var ErrNotOnePlayerUUID = errors.New("does not hold one UUID")

// ReadPlayerUUID returns the player's UUID from dataDir, and an error that
// is fs.ErrNotExist when none was made yet. The UUID may be followed by
// white space (unicode.IsSpace), such as the CR LF an editor leaves; a
// file that is there and holds anything else is an error too, never a
// reason to make another: a new UUID is a new player to every controller,
// so the operator decides (removing the file makes a new one at the next
// start).
func ReadPlayerUUID(dataDir string) (string, error) {
	id, err := ReadPlayerUUIDFile(filepath.Join(dataDir, PlayerUUIDFile))
	if errors.Is(err, ErrNotOnePlayerUUID) {
		return "", fmt.Errorf("%w: remove it, and the next start makes a new one, which controllers see as a new player", err)
	}
	return id, err
}

// ReadPlayerUUIDFile is ReadPlayerUUID's rule for the player-uuid file at
// path, wherever it is: a snapshot's copy is read by it too, so a restore
// takes exactly what the player would. A file that is there and holds
// anything but one UUID, followed by nothing but white space, gives an
// error wrapping ErrNotOnePlayerUUID; no file gives fs.ErrNotExist.
func ReadPlayerUUIDFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPlayerUUIDFile+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	id := strings.TrimRightFunc(string(b), unicode.IsSpace)
	if len(b) > maxPlayerUUIDFile || !playerUUIDShape.MatchString(id) {
		return "", fmt.Errorf("%s %w", path, ErrNotOnePlayerUUID)
	}
	return id, nil
}

// EnsurePlayerUUID returns the player's UUID, making it the first time:
// created is true when this call made it, through WritePlayerUUIDFile. The
// caller holds the data directory's lock (serve's own, or
// holdDataDirForMutation's), so no other process makes one at the same
// time.
func EnsurePlayerUUID(dataDir string) (id string, created bool, err error) {
	id, err = ReadPlayerUUID(dataDir)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	u, err := uuid.NewRandom()
	if err != nil {
		return "", false, fmt.Errorf("make the player's UUID: %w", err)
	}
	id = u.String()
	if err := fsutil.MkdirAll(dataDir, 0o700); err != nil {
		return "", false, err
	}
	if err := WritePlayerUUIDFile(filepath.Join(dataDir, PlayerUUIDFile), id); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// ForgetPlayerUUID removes dataDir's player-uuid file while it holds id:
// the undo of an EnsurePlayerUUID that made it (created) for a player that
// then did not start, serve's whose engine refused the data directory's
// storage, so the refusal leaves no identity behind on storage that may not
// keep it. A file that holds another UUID, or that is not there, is left
// alone; one that does not read is the reader's error. The caller holds the
// data directory's lock, as it did for the make.
func ForgetPlayerUUID(dataDir, id string) error {
	got, err := ReadPlayerUUID(dataDir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case got != id:
		return nil
	}
	if err := os.Remove(filepath.Join(dataDir, PlayerUUIDFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// WritePlayerUUIDFile writes id and a newline to the player-uuid file at
// path, replacing what is there: the identity file's one writer, which
// EnsurePlayerUUID makes the file with and bridge restore writes a
// snapshot's UUID through. It refuses an id that is not one UUID in
// lowercase, so it never writes a file ReadPlayerUUIDFile refuses. The file
// is written as every config file is, staged beside path, given the owner
// of the file it replaces, or of the directory, when run as root
// (fsutil.KeepOwner), synced and renamed into place, and is 0600 like the
// rest of the data directory, whatever mode the file it replaces had. The
// directory must exist.
func WritePlayerUUIDFile(path, id string) error {
	if !playerUUIDShape.MatchString(id) {
		return fmt.Errorf("write %s: what was to be written is not one UUID in lowercase", path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".player-uuid-*")
	if err != nil {
		return fmt.Errorf("stage %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// Registered after the Remove, so it runs first: Windows will not
	// unlink an open file.
	defer func() { _ = tmp.Close() }()
	if err := fsutil.KeepOwner(tmp, path); err != nil {
		return fmt.Errorf("keep %s's owner: %w", path, err)
	}
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := atomicwrite.RenameWithRetry(tmpName, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	tmpName = ""
	return nil
}
