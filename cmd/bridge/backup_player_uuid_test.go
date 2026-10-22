package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// runBackup runs bridge backup on cfgPath and returns the snapshot it wrote.
func runBackup(t *testing.T, cfgPath string) string {
	t.Helper()
	var stdout, stderr strings.Builder
	if code := backupCmd([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("bridge backup = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	_, rest, ok := strings.Cut(stdout.String(), "Snapshot written: ")
	snap, _, _ := strings.Cut(rest, "\n")
	if !ok || snap == "" {
		t.Fatalf("bridge backup named no snapshot:\n%s", stdout.String())
	}
	return snap
}

// TestBackupAndRestoreGiveThePlayerTheIdentityItHad: bridge backup carries
// the player's UUID, and bridge restore gives it back, both over a box whose
// player has taken another identity since and onto a box with none, so the
// controllers that knew the player see the same device. The restore runs
// through the real CLI, as an operator runs it.
func TestBackupAndRestoreGiveThePlayerTheIdentityItHad(t *testing.T) {
	cfgPath, dataDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	id, created, err := config.EnsurePlayerUUID(dataDir)
	if err != nil || !created {
		t.Fatalf("EnsurePlayerUUID = %q, %v, %v", id, created, err)
	}
	snap := runBackup(t, cfgPath)
	var m backup.Manifest
	raw, err := os.ReadFile(filepath.Join(snap, backup.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Files, config.PlayerUUIDFile) {
		t.Fatalf("the snapshot lists %v, without %s", m.Files, config.PlayerUUIDFile)
	}

	live := filepath.Join(dataDir, config.PlayerUUIDFile)
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	other, _, err := config.EnsurePlayerUUID(dataDir)
	if err != nil || other == id {
		t.Fatalf("the box's next identity = %q, %v", other, err)
	}
	code, stdout, stderr := runRestoreYes(t, cfgPath, snap)
	if code != 0 {
		t.Fatalf("restore = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got, err := config.ReadPlayerUUID(dataDir); err != nil || got != id {
		t.Fatalf("after a restore over another identity the player is %q, %v; want %s, the identity it had", got, err, id)
	}

	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runRestoreYes(t, cfgPath, snap)
	if code != 0 {
		t.Fatalf("restore = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got, err := config.ReadPlayerUUID(dataDir); err != nil || got != id {
		t.Fatalf("after a restore onto a box with no identity the player is %q, %v; want %s", got, err, id)
	}
}

// TestARestoreRefusesAPlayerUUIDThatIsNotOneUUIDAndSaysWhatToDo: a snapshot
// whose player-uuid does not hold one UUID is refused before the restore
// writes anything, the database included, which the restore writes first
// (Codex on B505's round 1: the test watched the config and the identity
// alone), and the refusal names the file and the way through. Removing the
// file from the snapshot is that way: the restore then keeps the box's
// identity, as for a snapshot taken before the file was part of the bundle.
func TestARestoreRefusesAPlayerUUIDThatIsNotOneUUIDAndSaysWhatToDo(t *testing.T) {
	cfgPath, dataDir, _ := seedRestorableInstall(t, closedAdminAddr(t))
	if _, _, err := config.EnsurePlayerUUID(dataDir); err != nil {
		t.Fatal(err)
	}
	snap := runBackup(t, cfgPath)
	snapID := filepath.Join(snap, config.PlayerUUIDFile)
	if err := os.WriteFile(snapID, []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dataDir, config.PlayerUUIDFile)
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	boxID, _, err := config.EnsurePlayerUUID(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	// The install moves on after the snapshot, so a restore that wrote the
	// config or the database shows.
	edit := []byte("# written after the snapshot\n")
	if err := os.WriteFile(cfgPath, append(readFileT(t, cfgPath), edit...), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBefore := readFileT(t, cfgPath)
	const afterSnapshot = "Artist/Album/After the snapshot.flac"
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: afterSnapshot, Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: "After",
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runRestoreYes(t, cfgPath, snap)
	if code == 0 || strings.Contains(stdout, "Restore complete") {
		t.Fatalf("restore took a player-uuid that is not one UUID, exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, snapID) || !strings.Contains(stderr, "remove") {
		t.Fatalf("the refusal does not name the snapshot's file and the way through:\n%s", stderr)
	}
	if got := readFileT(t, cfgPath); string(got) != string(cfgBefore) {
		t.Fatalf("the refused restore wrote bridge.yaml:\n%s", got)
	}
	if !trackExists(t, filepath.Dir(dataDir), afterSnapshot) {
		t.Fatal("the refused restore replaced the database with the snapshot's")
	}
	if got, err := config.ReadPlayerUUID(dataDir); err != nil || got != boxID {
		t.Fatalf("the refused restore changed the player's identity: %q, %v", got, err)
	}

	if err := os.Remove(snapID); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runRestoreYes(t, cfgPath, snap)
	if code != 0 {
		t.Fatalf("restore without the snapshot's player-uuid = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if got, err := config.ReadPlayerUUID(dataDir); err != nil || got != boxID {
		t.Fatalf("a restore without the snapshot's player-uuid changed the box's identity: %q, %v; want %s", got, err, boxID)
	}
}

func readFileT(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
