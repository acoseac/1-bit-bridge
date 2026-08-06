package backup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
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
// state out can: the new bytes must land somewhere ELSE and be swapped in
// by a single rename, so the destination holds the complete OLD manifest
// right up to the instant it holds the complete NEW one.
//
// Observed through atomicwrite's rename seam rather than through file
// identity. An earlier version compared os.SameFile across the two writes;
// that is a correct observation on POSIX (a rename-commit installs the
// temp's inode) and UNFALSIFIABLE on Windows, where os.SameFile resolves
// the file id LAZILY AT COMPARISON TIME by re-opening fs.path
// (os.(*fileStat).loadFileId in types_windows.go). Both FileInfos came
// from the same path, so both loads opened the post-write file and the
// ids always matched — the test reported "rewrote in place" for a
// correct implementation. CI on windows-latest caught it.
//
// The seam is strictly stronger anyway: it names the mechanism (one
// rename, from a temp beside the destination) AND lets the destination be
// read at the moment of the swap, which is the anti-truncation property
// itself rather than a proxy for it.
//
// Must not call t.Parallel — atomicwrite's renameFunc is a package-level
// seam, per its docblock.
func TestWriteManifestCommitsByRenameNotInPlaceTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ManifestFile)

	m := Manifest{SchemaVersion: SchemaVersion, Files: []string{ManifestDBFileName}}
	if err := writeManifest(path, m); err != nil {
		t.Fatalf("writeManifest (first): %v", err)
	}
	firstBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first write: %v", err)
	}

	// Record every rename the second write performs, capturing what the
	// DESTINATION held immediately before each one.
	type renameCall struct {
		src, dst    string
		dstAtCommit []byte
		dstReadErr  error
	}
	var calls []renameCall
	prev := atomicwrite.SetRenameFuncForTest(func(src, dst string) error {
		existing, readErr := os.ReadFile(dst)
		calls = append(calls, renameCall{src: src, dst: dst, dstAtCommit: existing, dstReadErr: readErr})
		return os.Rename(src, dst)
	})
	t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(prev) })

	m.Files = append(m.Files, "tokens.json")
	if err := writeManifest(path, m); err != nil {
		t.Fatalf("writeManifest (second): %v", err)
	}
	atomicwrite.SetRenameFuncForTest(prev)

	if len(calls) != 1 {
		t.Fatalf("writeManifest performed %d renames, want exactly 1; "+
			"an in-place rewrite does none, and an interrupted one then leaves a "+
			"truncated manifest that ReapOrphans refuses to reap and Prune reports forever",
			len(calls))
	}
	c := calls[0]
	if c.dst != path {
		t.Errorf("rename destination = %q, want the manifest path %q", c.dst, path)
	}
	if c.src == path {
		t.Error("rename source is the destination itself — that is not a temp-then-swap commit")
	}
	// Same directory, so the rename is a same-filesystem atomic operation:
	// a cross-FS rename degrades to copy-and-delete, which reintroduces
	// exactly the torn-destination window this is here to close.
	if got := filepath.Dir(c.src); got != dir {
		t.Errorf("temp lives in %q, want the destination's own dir %q (cross-FS renames aren't atomic)", got, dir)
	}
	if !strings.HasPrefix(filepath.Base(c.src), ".bridge-bak-manifest-") {
		t.Errorf("temp %q doesn't carry the manifest-writer prefix; a stray temp should name its writer", filepath.Base(c.src))
	}
	// The load-bearing one: at the instant of the swap the destination
	// still held the COMPLETE first manifest. Nothing truncated it.
	if c.dstReadErr != nil {
		t.Errorf("destination unreadable at commit time: %v", c.dstReadErr)
	} else if string(c.dstAtCommit) != string(firstBytes) {
		t.Errorf("destination was modified before the swap\nwant: %s\ngot:  %s", firstBytes, c.dstAtCommit)
	}

	// The commit must still land the new content...
	got, err := readManifest(path)
	if err != nil {
		t.Fatalf("readManifest after commit: %v", err)
	}
	if len(got.Files) != 2 {
		t.Errorf("committed manifest Files = %v, want the 2-entry second write", got.Files)
	}
	// ...at the same owner-only mode the rest of the bundle uses. POSIX
	// only: Go's file mode is advisory on Windows, where it is derived
	// solely from FILE_ATTRIBUTE_READONLY — any writable file reports
	// 0666, which is what windows-latest returned here. Confidentiality
	// there comes from the per-user-profile NTFS ACLs on %LOCALAPPDATA%,
	// the same position CLAUDE.md records for the init-permissions work.
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("manifest mode = %v, want 0600 (it names the bundle's secret-grade contents)", fi.Mode().Perm())
		}
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
// (complete, valid) manifest must survive untouched, and the caller must be
// TOLD the write failed. In-place truncation destroys the old manifest
// before it can discover it will fail — and in the pre-fix shape there was
// no commit step to fail at all, so the write "succeeded" and the operator
// had no signal either way.
//
// The rename is the only step that touches the destination, so failing it
// is the tightest possible injection — and going through the seam rather
// than a read-only parent directory makes the test mean the same thing on
// all three platforms. (The first version chmod'd the parent to 0500 to
// block os.CreateTemp; that gates nothing on Windows, where the test
// silently landed on a skip.)
//
// Must not call t.Parallel — atomicwrite's renameFunc is a package-level
// seam, per its docblock.
func TestWriteManifestLeavesPriorManifestIntactOnFailure(t *testing.T) {
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

	prev := atomicwrite.SetRenameFuncForTest(func(_, _ string) error {
		return errors.New("injected rename failure")
	})
	t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(prev) })

	err = writeManifest(path, Manifest{SchemaVersion: SchemaVersion, Files: []string{"tokens.json"}})
	atomicwrite.SetRenameFuncForTest(prev)

	if err == nil {
		t.Error("writeManifest reported success although its commit could not be performed")
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
	// The abandoned temp must not accumulate — every failed snapshot would
	// otherwise leave one beside the manifest.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bridge-bak-manifest-") {
			t.Errorf("a failed writeManifest left its temp behind: %s", e.Name())
		}
	}
}
