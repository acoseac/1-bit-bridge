package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
)

// aDSDState is a player-dsd.json the app could have written: a choice for
// one DAC, and what this box observed of it.
const aDSDState = `{"schemaVersion": 1, "defaultMode": "auto", "dacs": [{"key": {"vendorId": "20b1", "productId": "3009", "interface": 1, "product": "GO bar"},
  "choice": {"mode": "native", "maxBitRate": 0, "revision": 2, "setAt": "2026-11-05T10:00:00Z", "setBy": "app"}, "dopConfirmed": null, "firstUse": [],
  "listener": [], "evidence": [{"transport": "native", "format": "DSD_U32_BE", "bitRate": 2822400, "bcdDevice": "0123", "streamingObservedAt": "2026-11-05T10:05:00Z"}]}],
  "migratedFrom": null, "warnings": []}`

// durableRestore gives a restore the durable writer over a barrier and a
// medium of the test's (process-global seams: never in parallel).
func durableRestore(t *testing.T) {
	t.Helper()
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
}

// TestASnapshotCopiesTheDSDStateAsItsBytes: a snapshot holds the player's
// DSD state as the file's bytes, never loaded through the state's store,
// whose load writes: a damaged state is copied as it is, and the live file
// is left as it was, with nothing set aside beside it. A box with no state,
// every box before serve's player writes one, snapshots without it.
func TestASnapshotCopiesTheDSDStateAsItsBytes(t *testing.T) {
	for name, body := range map[string]string{"a state": aDSDState, "a damaged state": "{\"schemaVersion\": 1, \"de"} {
		t.Run(name, func(t *testing.T) {
			live := t.TempDir()
			src := primeLiveState(t, live)
			src.PlayerDSD = filepath.Join(live, backup.PlayerDSDFileName)
			if err := os.WriteFile(src.PlayerDSD, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			snap, err := backup.Snapshot(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			if got := readBytes(t, filepath.Join(snap, backup.PlayerDSDFileName)); string(got) != body {
				t.Fatalf("the snapshot holds %q, want the live bytes", got)
			}
			if got := readBytes(t, src.PlayerDSD); string(got) != body {
				t.Fatalf("the snapshot changed the live state: %q", got)
			}
			if aside, _ := filepath.Glob(src.PlayerDSD + ".bad-*"); len(aside) != 0 {
				t.Fatalf("the snapshot loaded the state through its store, which set %v aside", aside)
			}
		})
	}
	live := t.TempDir()
	src := primeLiveState(t, live)
	src.PlayerDSD = filepath.Join(live, backup.PlayerDSDFileName)
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatalf("a box with no DSD state cannot be backed up: %v", err)
	}
	if pathExists(t, filepath.Join(snap, backup.PlayerDSDFileName)) {
		t.Fatal("the snapshot holds a DSD state the box has none of")
	}
	if backup.PlayerDSDFileName != dsdstate.FileName {
		t.Fatalf("backup names %q, the state %q", backup.PlayerDSDFileName, dsdstate.FileName)
	}
}

// TestARestoreOfASnapshotWithoutTheDSDStateLeavesTheBoxsAlone: a snapshot
// taken before the state was in the bundle, or of a box that had none,
// leaves the box's state as it is, as for the console account and the
// player's identity.
func TestARestoreOfASnapshotWithoutTheDSDStateLeavesTheBoxsAlone(t *testing.T) {
	durableRestore(t)
	live := t.TempDir()
	snap, err := backup.Snapshot(t.Context(), primeLiveState(t, live))
	if err != nil {
		t.Fatal(err)
	}
	box := t.TempDir()
	target := filepath.Join(box, backup.PlayerDSDFileName)
	if err := os.WriteFile(target, []byte(aDSDState), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(snap, backup.Targets{TokensJSON: filepath.Join(box, "tokens.json"), PlayerDSD: target}); err != nil {
		t.Fatal(err)
	}
	if got := readBytes(t, target); string(got) != aDSDState {
		t.Fatalf("a restore without the state replaced the box's: %q", got)
	}
}

// TestARestoreInstallsADamagedArchivedStateAsItsBytes: a snapshot's state
// that does not decode is installed as its bytes, so the box's next load
// refuses it, keeps it aside and refuses DSD, never the defaults.
func TestARestoreInstallsADamagedArchivedStateAsItsBytes(t *testing.T) {
	durableRestore(t)
	live := t.TempDir()
	src := primeLiveState(t, live)
	src.PlayerDSD = filepath.Join(live, backup.PlayerDSDFileName)
	const damaged = "{\"schemaVersion\": 1, \"de"
	if err := os.WriteFile(src.PlayerDSD, []byte(damaged), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	box := t.TempDir()
	target := filepath.Join(box, backup.PlayerDSDFileName)
	if err := os.WriteFile(target, []byte(aDSDState), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(snap, backup.Targets{PlayerDSD: target}); err != nil {
		t.Fatal(err)
	}
	if got := readBytes(t, target); string(got) != damaged {
		t.Fatalf("the restore installed %q, want the archived bytes", got)
	}
	s, err := dsdstate.Open(box)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.Report(); r.Outcome != dsdstate.Damaged || !s.State().Unreadable() {
		t.Fatalf("the box's next load = %+v, unreadable %v: want the damaged state refused", r, s.State().Unreadable())
	}
}

// TestARestoreThatCannotWriteTheDSDStateDurablyRestoresNothing: the state
// is written durably or not at all, before the database, the token store and
// the config, so where it cannot be made durable (no directory barrier, as
// on Windows) the restore refuses with nothing else written, and says how to
// restore the rest.
func TestARestoreThatCannotWriteTheDSDStateDurablyRestoresNothing(t *testing.T) {
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	live := t.TempDir()
	src := primeLiveState(t, live)
	src.PlayerDSD = filepath.Join(live, backup.PlayerDSDFileName)
	if err := os.WriteFile(src.PlayerDSD, []byte(aDSDState), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	box := t.TempDir()
	tokens := filepath.Join(box, "tokens.json")
	t.Cleanup(atomicwrite.SetSyncDirForTest(nil))
	err = backup.Restore(snap, backup.Targets{TokensJSON: tokens, PlayerDSD: filepath.Join(box, backup.PlayerDSDFileName)})
	if !errors.Is(err, atomicwrite.ErrNoDirectoryBarrier) || errors.Is(err, atomicwrite.ErrLanded) {
		t.Fatalf("a restore with no directory barrier = %v, want its refusal before anything landed", err)
	}
	for _, want := range []string{"nothing was restored", "remove " + backup.PlayerDSDFileName + " from a copy of the snapshot"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "is now the snapshot's") {
		t.Errorf("the refusal %q says the box's state changed, before anything was written", err)
	}
	entries, _ := os.ReadDir(box)
	if names := entryNames(entries); len(names) != 0 {
		t.Fatalf("the refused restore wrote %v", names)
	}
}

// TestARestoreWhoseBarrierFailsOnceTheStateLandedSaysItChanged: the durable
// writer renames the snapshot's state into the box's place and then syncs
// the directory, so a barrier that fails there fails the restore after the
// box's state has become the snapshot's (CodeRabbit on #1405: the refusal
// said nothing was restored, and to keep this box's DSD state by leaving the
// file out of the snapshot, which by then was no longer there). The refusal
// says the state changed and that whether it survives a crash is uncertain,
// and gives that advice only where nothing was written; nothing after the
// state is restored.
func TestARestoreWhoseBarrierFailsOnceTheStateLandedSaysItChanged(t *testing.T) {
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	live := t.TempDir()
	src := primeLiveState(t, live)
	src.PlayerDSD = filepath.Join(live, backup.PlayerDSDFileName)
	if err := os.WriteFile(src.PlayerDSD, []byte(aDSDState), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	box := t.TempDir()
	tokens := filepath.Join(box, "tokens.json")
	target := filepath.Join(box, backup.PlayerDSDFileName)
	const boxState = `{"schemaVersion": 1, "defaultMode": "off", "dacs": [], "migratedFrom": {"from": "state_unreadable"}, "warnings": [{"code": "state_unreadable", "at": "2026-11-04T09:00:00Z"}]}`
	if err := os.WriteFile(target, []byte(boxState), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return errors.New("the directory would not sync") }))
	err = backup.Restore(snap, backup.Targets{TokensJSON: tokens, PlayerDSD: target})
	if !errors.Is(err, atomicwrite.ErrBarrierFailed) || !errors.Is(err, atomicwrite.ErrLanded) {
		t.Fatalf("a restore whose barrier fails = %v, want the barrier's failure once the state landed", err)
	}
	for _, not := range []string{"nothing was restored", "remove " + backup.PlayerDSDFileName + " from a copy of the snapshot", "keep this box's DSD state"} {
		if strings.Contains(err.Error(), not) {
			t.Errorf("the refusal %q says %q, after the box's state was replaced", err, not)
		}
	}
	for _, want := range []string{"is now the snapshot's", "whether it survives a crash is uncertain"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
	installed, err := dsdstate.ForRestore([]byte(aDSDState))
	if err != nil {
		t.Fatal(err)
	}
	if got := readBytes(t, target); string(got) != string(installed) {
		t.Fatalf("the box's state is %q, want the snapshot's, which the refusal says it is", got)
	}
	if pathExists(t, tokens) {
		t.Fatal("the restore went on past a state it could not make durable")
	}
}

func entryNames(entries []os.DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}
