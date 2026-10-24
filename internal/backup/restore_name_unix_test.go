//go:build !windows

package backup_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A restore takes bridge.yaml's turn and writes the snapshot's copy in it.
// A link at the config's name repointed meanwhile (a dotfiles restow, a
// home-manager switch) names a file the restore never locked or read: the
// restore treats it as it treats a config another writer changed, refusing
// with nothing restored before its first write and saying what it restored
// at bridge.yaml's own (B506's fourth round). The link is repointed to a twin
// holding the same bytes, so that only the name's resolution tells the two
// apart.

// linkTheBoxsConfig makes the box's bridge.yaml a link to a file of its own,
// A, holding what bridge.yaml held, beside a twin holding the same bytes, B,
// and returns the link, A and B.
func linkTheBoxsConfig(t *testing.T, b *restorableBox) (link, a, twin string) {
	t.Helper()
	link = b.src.BridgeYAML
	store := filepath.Join(filepath.Dir(link), "store")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := b.live[link]
	a, twin = filepath.Join(store, "actual.yaml"), filepath.Join(store, "switched.yaml")
	for _, p := range []string{a, twin} {
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	return link, a, twin
}

// repointLink makes link lead to to, in one rename, as a restow replaces it.
func repointLink(t *testing.T, link, to string) {
	t.Helper()
	tmp := link + ".repoint"
	if err := os.Symlink(to, tmp); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Error(err)
	}
}

// requireConfigsAsTheyWere fails the test unless A and the twin still hold
// raw and link still leads to the twin.
func requireConfigsAsTheyWere(t *testing.T, raw []byte, link, a, twin string) {
	t.Helper()
	for _, p := range []string{a, twin} {
		if got := readBytes(t, p); !bytes.Equal(got, raw) {
			t.Errorf("the restore wrote %s:\n%s", filepath.Base(p), got)
		}
	}
	if to, err := os.Readlink(link); err != nil || to != twin {
		t.Errorf("%s leads to %q (%v), want %s", link, to, err, twin)
	}
}

// TestARestoreRefusesANameRepointedBeforeItsFirstWrite: a link at the
// config's name repointed once the restore holds its turn, before its first
// write, refuses the restore with nothing restored, as a config another
// writer changed does: the player's reservation, identity and queue, the
// database and every file are as they were.
func TestARestoreRefusesANameRepointedBeforeItsFirstWrite(t *testing.T) {
	b := newRestorableBox(t)
	link, a, twin := linkTheBoxsConfig(t, b)
	raw := b.live[link]
	opts := backup.WithBeforeWritesForTest(backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})}, func() {
		repointLink(t, link, twin)
	})
	err := backup.RestoreWithOptions(b.snap, b.targets, opts)
	if !errors.Is(err, config.ErrNameMoved) || !strings.Contains(err.Error(), "nothing was restored") ||
		!strings.Contains(err.Error(), "run the restore again") {
		t.Errorf("a restore whose config's name moved before its first write = %v, want the refusal of a changed config: "+
			"nothing was restored, run the restore again", err)
	}
	b.requireUntouched(t)
	requireConfigsAsTheyWere(t, raw, link, a, twin)
}

// TestARestoreRefusesANameRepointedDuringItsCheck: a link at the config's
// name repointed while the restore's check, before its first write, reads the
// file its turn locked, which the read finds unchanged, refuses the restore
// with nothing restored: the check resolves the name once that read has
// answered. Until B506's fifth round it resolved the name first, the restore
// wrote its files, and only bridge.yaml's own write refused.
func TestARestoreRefusesANameRepointedDuringItsCheck(t *testing.T) {
	b := newRestorableBox(t)
	link, a, twin := linkTheBoxsConfig(t, b)
	raw := b.live[link]
	locked, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	reads, secondOf := 0, ""
	t.Cleanup(config.SetTurnReadForTest(func(p string) ([]byte, error) {
		if reads++; reads == 2 {
			repointLink(t, link, twin)
			secondOf = p
		}
		return fsutil.ReadAsFile(p)
	}))
	err = backup.RestoreWithOptions(b.snap, b.targets, backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})})
	if secondOf != locked {
		t.Fatalf("premise: the second read was of %q, want the check's, of %s", secondOf, locked)
	}
	if !errors.Is(err, config.ErrNameMoved) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Errorf("a restore whose config's name moved during its check's read = %v, want the refusal of a changed config, "+
			"nothing restored", err)
	}
	b.requireUntouched(t)
	requireConfigsAsTheyWere(t, raw, link, a, twin)
}

// TestARestoreSaysWhatItRestoredBeforeANameRepointedAtItsConfig: a link
// repointed while the files before bridge.yaml are restored leaves
// bridge.yaml unwritten, in neither file, and the refusal names what the
// restore restored and says running it again finishes it, as a config another
// writer changed meanwhile does.
func TestARestoreSaysWhatItRestoredBeforeANameRepointedAtItsConfig(t *testing.T) {
	b := newRestorableBox(t)
	link, a, twin := linkTheBoxsConfig(t, b)
	raw := b.live[link]
	opts := backup.WithBeforeConfigForTest(backup.RestoreOptions{Random: bytes.NewReader([]byte{0, 0, 7})}, func() {
		repointLink(t, link, twin)
	})
	err := backup.RestoreWithOptions(b.snap, b.targets, opts)
	if !errors.Is(err, config.ErrNameMoved) {
		t.Fatalf("a restore whose config's name moved before bridge.yaml's write = %v, want ErrNameMoved", err)
	}
	msg := err.Error()
	for _, want := range []string{"run the restore again", "tokens.json", backup.PlayerUUIDFileName} {
		if !strings.Contains(msg, want) {
			t.Errorf("the late refusal %q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "nothing was restored") {
		t.Errorf("the late refusal %q says nothing was restored, after files were", msg)
	}
	requireConfigsAsTheyWere(t, raw, link, a, twin)
}
