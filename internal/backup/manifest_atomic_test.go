package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteManifestCommitsByRenameNotInPlaceTruncation pins the property
// that makes a truncated `manifest.json` impossible to produce.
//
// The manifest is written LAST in a snapshot, after a possibly-multi-GB
// DB copy, and it is the ONLY thing distinguishing a complete snapshot
// from a crash orphan. A plain in-place `os.WriteFile` (O_CREATE|O_TRUNC)
// interrupted by SIGKILL / power-loss leaves a partially-written file, and
// that state is unrecoverable BY DESIGN everywhere downstream:
// `readManifest` returns a decode error, so `List` skips the dir, so
// `Prune`'s keep-policy can never select it, and `ReapOrphans` refuses to
// delete it (a read error is deliberately not treated as evidence of a
// crash orphan — the AV-handle window produces the same signature on a
// perfectly good backup). Nothing in this package could ever remove it.
//
// A crash can't be tested, but the structural property that rules the
// state out can: the destination must be REPLACED by rename, never
// truncated in place. File identity is the direct observation — an
// in-place rewrite keeps the same inode / file index, a rename-commit
// installs the temp file's.
func TestWriteManifestCommitsByRenameNotInPlaceTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFile)

	m := Manifest{SchemaVersion: SchemaVersion, Files: []string{ManifestDBFileName}}
	if err := writeManifest(path, m); err != nil {
		t.Fatalf("writeManifest (first): %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first write: %v", err)
	}

	m.Files = append(m.Files, "tokens.json")
	if err := writeManifest(path, m); err != nil {
		t.Fatalf("writeManifest (second): %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second write: %v", err)
	}

	if os.SameFile(first, second) {
		t.Error("writeManifest rewrote the manifest IN PLACE (same file identity); " +
			"an interrupted write then leaves a truncated manifest that ReapOrphans " +
			"refuses to reap and Prune reports forever")
	}

	// The commit must still land the new content...
	got, err := readManifest(path)
	if err != nil {
		t.Fatalf("readManifest after commit: %v", err)
	}
	if len(got.Files) != 2 {
		t.Errorf("committed manifest Files = %v, want the 2-entry second write", got.Files)
	}
	// ...at the same owner-only mode the rest of the bundle uses...
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("manifest mode = %v, want 0600 (it names the bundle's secret-grade contents)", fi.Mode().Perm())
	}
	// ...and leave no temp file behind in the snapshot dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bridge-bak-manifest-") {
			t.Errorf("writeManifest left a temp file behind: %s", e.Name())
		}
	}
}

// TestWriteManifestLeavesPriorManifestIntactOnFailure is the other half of
// the atomicity contract: when the commit CANNOT complete, the previous
// (complete, valid) manifest must survive untouched. In-place truncation
// destroys it before it can discover it will fail.
//
// A read-only parent directory is the deterministic injection: it blocks
// `os.CreateTemp` while leaving an existing file writable (POSIX directory
// permissions gate create/unlink, not opening an existing entry for write
// — which is exactly why the pre-fix `os.WriteFile` SUCCEEDS here).
func TestWriteManifestLeavesPriorManifestIntactOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory mode bits, so the failure can't be injected")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFile)

	good := Manifest{SchemaVersion: SchemaVersion, Files: []string{ManifestDBFileName}}
	if err := writeManifest(path, good); err != nil {
		t.Fatalf("writeManifest (baseline): %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err = writeManifest(path, Manifest{SchemaVersion: SchemaVersion, Files: []string{"tokens.json"}})
	if err == nil {
		t.Skip("writeManifest succeeded despite a 0500 parent (perms-ignoring FS); injection didn't take")
	}

	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("prior manifest unreadable after a failed write: %v", readErr)
	}
	if string(after) != string(before) {
		t.Errorf("a failed writeManifest modified the prior manifest\nbefore: %s\nafter:  %s", before, after)
	}
	var m Manifest
	if err := json.Unmarshal(after, &m); err != nil {
		t.Errorf("prior manifest is no longer valid JSON after a failed write: %v", err)
	}
}
