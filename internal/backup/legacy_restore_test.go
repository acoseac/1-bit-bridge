package backup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestOlderRestoresRejectA5a0Snapshot(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	source := t.TempDir()
	p := filepath.Join(source, PlayerUUIDFileName)
	if err := config.WritePlayerUUIDFile(p, id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "player-ids.json"), []byte(`{"version":1,"ids":0,"tokens":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	snap, err := Snapshot(t.Context(), Sources{DataDir: source, PlayerUUID: p})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		restore func(string, Targets) error
	}{
		{"v0.2.1", restoreV021}, {"main_before_5a0", restoreBefore5a0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			target := Targets{PlayerUUID: filepath.Join(dst, PlayerUUIDFileName)}
			if err := tc.restore(snap, target); err == nil {
				t.Fatal("older restore accepted a 5a0 snapshot")
			}
			entries, err := os.ReadDir(dst)
			if err != nil || len(entries) != 0 {
				t.Fatalf("old restore wrote before refusing: %v %v", entries, err)
			}
		})
	}
}

func TestOlderSchemaOneRestoresExplainTheReleaseCoupling(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	snap := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(snap, PlayerUUIDFileName), id); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "player-ids.json"), []byte(`{"version":2,"uuid":"`+id+`","ids":10000,"tokens":30000}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(filepath.Join(snap, ManifestFile), Manifest{SchemaVersion: 1, Files: []string{PlayerUUIDFileName, "player-ids.json", "future-unknown-file"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		restore  func(string, Targets) error
		identity bool
	}{
		{"v0.2.1", restoreV021, false}, {"main_before_5a0", restoreBefore5a0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			if err := tc.restore(snap, Targets{PlayerUUID: filepath.Join(dst, PlayerUUIDFileName)}); err != nil {
				t.Fatal(err)
			}
			got, err := config.ReadPlayerUUID(dst)
			if tc.identity && (err != nil || got != id) || !tc.identity && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old identity behavior: %q %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(dst, "player-ids.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("old restore handled an unknown reservation")
			}
		})
	}
}

// restoreV021 captures Restore from v0.2.1. Only the function name and
// references to that revision's schema constant (1) were changed; review
// names in comments were omitted.
func restoreV021(snapshotDir string, dst Targets) error {
	m, err := readManifest(filepath.Join(snapshotDir, ManifestFile))
	if err != nil {
		return fmt.Errorf("read snapshot manifest: %w", err)
	}
	if m.SchemaVersion != 1 {
		return fmt.Errorf("snapshot schema v%d not supported (this bridge supports v%d)",
			m.SchemaVersion, 1)
	}

	// Pair the manifest's file list with the caller's targets.
	// Files in the snapshot but unmapped by the caller are skipped
	// (caller chose to exclude); files mapped but missing from the
	// snapshot are skipped silently (the snapshot just didn't
	// capture that one).
	mapping := map[string]string{
		ManifestDBFileName: dst.ManifestDB,
		"tokens.json":      dst.TokensJSON,
		"server.crt":       dst.ServerCert,
		"server.key":       dst.ServerKey,
		"bridge.yaml":      dst.BridgeYAML,
	}
	for _, name := range m.Files {
		target, ok := mapping[name]
		if !ok || target == "" {
			continue
		}
		srcPath := filepath.Join(snapshotDir, name)
		if _, err := os.Stat(srcPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			// A non-ErrNotExist stat error (permission flap, transient
			// I/O, symlink loop) MUST abort here — NOT fall through to
			// the bridge.db block below, which removes the live -wal/-shm
			// BEFORE copyFile and would then fail at its own os.Open,
			// deleting the WAL for a source we never actually read. This
			// mirrors Snapshot's non-ErrNotExist stat handling.
			return fmt.Errorf("stat snapshot file %s: %w", name, err)
		}
		// Restore at 0o600 across the board — the snapshot bundle was
		// stored that way, and bridge.yaml may carry sensitive data
		// (paths, future secrets). Maintaining 0o600 on the live file
		// matches what `Snapshot` writes back into the bundle and what
		// `config.Save` would land if the operator hand-edited via
		// the admin console.
		var mode os.FileMode = 0o600
		if err := fsutil.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create dir for %s: %w", name, err)
		}
		if name == ManifestDBFileName {
			// Drop the live pre-restore WAL/SHM BEFORE overwriting the
			// main DB. Snapshot's VACUUM INTO produces a clean single-
			// file bridge.db (no -wal in the bundle), so the only WAL on
			// disk belongs to the DB we're about to replace — SQLite
			// would otherwise replay those stale frames onto the restored
			// (older, different) main file on next open and corrupt it.
			// Order is load-bearing: copyFile is an atomic temp+rename,
			// so removing WAL/SHM first means a crash in the gap leaves
			// the OLD db with no wal (clean, restore just didn't finish —
			// re-runnable) rather than NEW db + OLD wal (corruption).
			// Restore requires the bridge stopped (docblock), so there is
			// no concurrent writer. A non-ErrNotExist failure (permission,
			// path-is-a-dir) must ABORT — proceeding would leave the stale
			// WAL in place while copyFile overwrites the DB, reproducing
			// the exact corruption this block prevents (PR #476).
			for _, sidecar := range []string{target + "-wal", target + "-shm"} {
				if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove stale sidecar %s: %w", sidecar, err)
				}
			}
		}
		if err := copyFile(srcPath, target, mode); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
	}
	return nil
}

// restoreBefore5a0 captures Restore from 71af3f26. Only the function name and
// references to that revision's schema constant (1) were changed; review
// names in comments were omitted.
func restoreBefore5a0(snapshotDir string, dst Targets) error {
	m, err := readManifest(filepath.Join(snapshotDir, ManifestFile))
	if err != nil {
		return fmt.Errorf("read snapshot manifest: %w", err)
	}
	if m.SchemaVersion != 1 {
		return fmt.Errorf("snapshot schema v%d not supported (this bridge supports v%d)",
			m.SchemaVersion, 1)
	}

	// Pair the manifest's file list with the caller's targets.
	// Files in the snapshot but unmapped by the caller are skipped
	// (caller chose to exclude); files mapped but missing from the
	// snapshot are skipped silently (the snapshot just didn't
	// capture that one).
	mapping := map[string]string{
		ManifestDBFileName:   dst.ManifestDB,
		"tokens.json":        dst.TokensJSON,
		"server.crt":         dst.ServerCert,
		"server.key":         dst.ServerKey,
		"bridge.yaml":        dst.BridgeYAML,
		AdminAuthFileName:    dst.AdminAuth,
		AtlasHarvestFileName: dst.AtlasHarvest,
		PlayerUUIDFileName:   dst.PlayerUUID,
	}
	// Sign the console account out before the loop. The manifest lists it
	// after bridge.db, the token store, the TLS pair and bridge.yaml, so a
	// check made when the loop reached it came after those were replaced,
	// and an account that does not parse left a half-restored install
	// (PR #1200).
	var account []byte
	accountRead := false
	if dst.AdminAuth != "" && slices.Contains(m.Files, AdminAuthFileName) {
		account, accountRead, err = signedOutAdminAuth(filepath.Join(snapshotDir, AdminAuthFileName))
		if err != nil {
			return err
		}
	}
	// The player's identity is read before the loop for the same reason:
	// the manifest lists it last.
	var playerUUID string
	playerUUIDRead := false
	if dst.PlayerUUID != "" && slices.Contains(m.Files, PlayerUUIDFileName) {
		playerUUID, playerUUIDRead, err = snapshotPlayerUUID(filepath.Join(snapshotDir, PlayerUUIDFileName))
		if err != nil {
			return err
		}
	}
	for _, name := range m.Files {
		target, ok := mapping[name]
		if !ok || target == "" {
			continue
		}
		srcPath := filepath.Join(snapshotDir, name)
		if _, err := os.Stat(srcPath); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			// A non-ErrNotExist stat error (permission flap, transient
			// I/O, symlink loop) MUST abort here — NOT fall through to
			// the bridge.db block below, which removes the live -wal/-shm
			// BEFORE copyFile and would then fail at its own os.Open,
			// deleting the WAL for a source we never actually read. This
			// mirrors Snapshot's non-ErrNotExist stat handling.
			return fmt.Errorf("stat snapshot file %s: %w", name, err)
		}
		// Restore at 0o600 across the board — the snapshot bundle was
		// stored that way, and bridge.yaml may carry sensitive data
		// (paths, future secrets). Maintaining 0o600 on the live file
		// matches what `Snapshot` writes back into the bundle and what
		// `config.Save` would land if the operator hand-edited via
		// the admin console.
		var mode os.FileMode = 0o600
		if err := fsutil.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create dir for %s: %w", name, err)
		}
		if name == AdminAuthFileName {
			// One write of the account signed out before the loop. copyFile
			// would land the live sessions, and a second write to strip
			// them would hand the file to its owner twice.
			if !accountRead {
				return fmt.Errorf("restore %s: the snapshot changed during the restore", AdminAuthFileName)
			}
			if err := writeFrom(target, bytes.NewReader(account), 0o600); err != nil {
				return fmt.Errorf("restore %s: %w", AdminAuthFileName, err)
			}
			continue
		}
		if name == PlayerUUIDFileName {
			// The UUID read before the loop, written the way the player
			// writes its own file, never a copy of the snapshot's bytes.
			if !playerUUIDRead {
				return fmt.Errorf("restore %s: the snapshot changed during the restore", PlayerUUIDFileName)
			}
			if err := config.WritePlayerUUIDFile(target, playerUUID); err != nil {
				return fmt.Errorf("restore %s: %w", PlayerUUIDFileName, err)
			}
			continue
		}
		if name == ManifestDBFileName {
			// Drop the live pre-restore WAL/SHM BEFORE overwriting the
			// main DB. Snapshot's VACUUM INTO produces a clean single-
			// file bridge.db (no -wal in the bundle), so the only WAL on
			// disk belongs to the DB we're about to replace — SQLite
			// would otherwise replay those stale frames onto the restored
			// (older, different) main file on next open and corrupt it.
			// Order is load-bearing: copyFile is an atomic temp+rename,
			// so removing WAL/SHM first means a crash in the gap leaves
			// the OLD db with no wal (clean, restore just didn't finish —
			// re-runnable) rather than NEW db + OLD wal (corruption).
			// Restore requires the bridge stopped (docblock), so there is
			// no concurrent writer. A non-ErrNotExist failure (permission,
			// path-is-a-dir) must ABORT — proceeding would leave the stale
			// WAL in place while copyFile overwrites the DB, reproducing
			// the exact corruption this block prevents (PR #476).
			for _, sidecar := range []string{target + "-wal", target + "-shm"} {
				if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove stale sidecar %s: %w", sidecar, err)
				}
			}
		}
		if err := copyFile(srcPath, target, mode); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
		if name == ManifestDBFileName {
			// A restored library is a new backup generation. Minting
			// after the copy, then dropping the WAL the mint left,
			// keeps the epoch in the main file.
			if err := manifest.ResetBackupEpoch(target); err != nil {
				return fmt.Errorf("mint backup epoch: %w", err)
			}
			for _, sidecar := range []string{target + "-wal", target + "-shm"} {
				if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("remove epoch sidecar %s: %w", sidecar, err)
				}
			}
		}
	}
	return nil
}
