// Package backup snapshots and restores the bridge's state directory.
//
// A snapshot bundles every file an operator would otherwise have to
// re-pair / re-scan to recover after corruption: the manifest SQLite
// database (`bridge.db`), the token store (`tokens.json`), the TLS
// material (`server.crt` + `server.key`), and the live config
// (`bridge.yaml`). The snapshot also writes a small `manifest.json`
// describing the bridge version and protocol version that produced it
// — Restore refuses incompatible snapshots so a future schema change
// doesn't get silently rolled back into.
//
// Snapshots are written to `<dataDir>/backups/<timestamp>/`. The
// directory and files are permissioned 0700 / 0600 because the bundle
// contains the TLS private key and token hashes — anyone with read
// access could impersonate the bridge or forge an authenticated
// request. Operators are warned about this on every backup.
//
// The manifest copy uses SQLite's `VACUUM INTO`, which produces a
// clean, atomic copy of a WAL-mode database without locking out the
// running bridge. Restoring the manifest db while the bridge is
// running is unsafe (the WAL would be inconsistent with the new
// main file), so `bridge restore` warns + requires `--yes`.
package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/version"
	_ "modernc.org/sqlite" // register "sqlite" driver
)

// Snapshot schema version. Bump on incompatible layout changes
// (e.g. new mandatory file, restructured manifest). Restore refuses
// snapshots whose `SchemaVersion` doesn't match this constant.
const SchemaVersion = 1

// ManifestFile is the metadata file written into every snapshot dir.
const ManifestFile = "manifest.json"

// ManifestDBFileName is the on-disk basename for the SQLite manifest
// inside a backup directory. Three duplicates flagged by go:S1192;
// extracted so a future rename only happens once across Capture,
// captured-list emission, and Restore.
const ManifestDBFileName = "bridge.db"

// BackupsDirName is the dir under `dataDir` that holds snapshots.
const BackupsDirName = "backups"

// timestampLayout is the snapshot dir name format. Windows-friendly
// (no colons) and lexicographically sortable.
const timestampLayout = "2006-01-02T15-04-05Z"

// Manifest is the metadata file written alongside each snapshot. It
// records what produced the snapshot and which files are inside —
// the file list lets Restore copy back exactly what was captured
// without over-restoring (e.g. don't try to restore `server.crt`
// from a snapshot that didn't include one).
type Manifest struct {
	SchemaVersion   int       `json:"schemaVersion"`
	BridgeVersion   string    `json:"bridgeVersion"`
	ProtocolVersion int       `json:"protocolVersion"`
	CreatedAt       time.Time `json:"createdAt"`
	Files           []string  `json:"files"`
}

// Sources points at the live files that should be captured.
//
// Empty fields are skipped (with the exception of `DataDir`, which
// is required because it determines where the snapshot dir lands).
// Missing-on-disk files are also skipped silently — a freshly
// `bridge init`'d install hasn't pair'd anyone yet, so `tokens.json`
// may not exist.
type Sources struct {
	DataDir    string // required — where `<dataDir>/backups/<stamp>/` is created
	ManifestDB string // typically `<dataDir>/bridge.db`
	TokensJSON string // typically `<dataDir>/tokens.json`
	ServerCert string // typically `<dataDir>/server.crt`
	ServerKey  string // typically `<dataDir>/server.key`
	BridgeYAML string // path to the bridge config file
}

// Targets points at where each restored file should land. Mirrors
// `Sources` field-for-field. Empty fields skip that file (so the
// caller can choose to exclude e.g. `bridge.yaml` if they want to
// keep the live config untouched).
type Targets struct {
	ManifestDB string
	TokensJSON string
	ServerCert string
	ServerKey  string
	BridgeYAML string
}

// Snapshot captures every present file in `src` into a fresh
// timestamped directory under `<src.DataDir>/backups/`. Returns the
// absolute path of the created directory.
//
// `ctx` is forwarded to the SQLite VACUUM INTO step so the periodic
// ticker can cancel an in-flight snapshot on bridge shutdown. Pass
// `context.Background()` if you don't have a richer scope.
func Snapshot(ctx context.Context, src Sources) (snapDir string, retErr error) {
	if src.DataDir == "" {
		return "", errors.New("backup: DataDir is required")
	}
	backupsRoot := filepath.Join(src.DataDir, BackupsDirName)
	if err := os.MkdirAll(backupsRoot, 0o700); err != nil {
		return "", fmt.Errorf("create backups root: %w", err)
	}
	dst, err := createUniqueSnapshotDir(backupsRoot, time.Now().UTC())
	if err != nil {
		return "", fmt.Errorf("create snapshot dir: %w", err)
	}
	// Reap the partial dir on any failure past this point. A failed /
	// cancelled snapshot never wrote `manifest.json`, and `List` skips
	// manifest-less dirs — so without this cleanup the partial dir
	// (containing a full DB copy) would be invisible to Prune forever
	// and accumulate unbounded across failures.
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(dst)
		}
	}()

	var captured []string

	if src.ManifestDB != "" {
		if _, err := os.Stat(src.ManifestDB); err == nil {
			if err := vacuumInto(ctx, src.ManifestDB, filepath.Join(dst, ManifestDBFileName)); err != nil {
				return "", fmt.Errorf("vacuum manifest db: %w", err)
			}
			captured = append(captured, ManifestDBFileName)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat manifest db: %w", err)
		}
	}

	for _, pair := range []struct {
		src, name string
	}{
		{src.TokensJSON, "tokens.json"},
		{src.ServerCert, "server.crt"},
		{src.ServerKey, "server.key"},
		{src.BridgeYAML, "bridge.yaml"},
	} {
		if pair.src == "" {
			continue
		}
		if _, err := os.Stat(pair.src); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("stat %s: %w", pair.name, err)
		}
		if err := copyFile(pair.src, filepath.Join(dst, pair.name), 0o600); err != nil {
			return "", fmt.Errorf("copy %s: %w", pair.name, err)
		}
		captured = append(captured, pair.name)
	}

	m := Manifest{
		SchemaVersion:   SchemaVersion,
		BridgeVersion:   version.ServerVersion,
		ProtocolVersion: version.ProtocolVersion,
		CreatedAt:       time.Now().UTC(),
		Files:           captured,
	}
	if err := writeManifest(filepath.Join(dst, ManifestFile), m); err != nil {
		return "", fmt.Errorf("write snapshot manifest: %w", err)
	}
	return dst, nil
}

// Restore copies files from a snapshot directory back into place.
// The caller is responsible for stopping the running bridge first
// — restoring the manifest db while serve is up will leave the
// WAL inconsistent with the new main file (the running connection
// is unaware of the swap).
//
// Per-file copies are atomic (temp + rename); if any copy fails the
// previously-restored files are NOT rolled back, but each individual
// target stays internally consistent. In practice the whole-dir
// snapshot was atomic (created in one Snapshot call) so a partial
// restore is the most useful failure mode anyway.
func Restore(snapshotDir string, dst Targets) error {
	m, err := readManifest(filepath.Join(snapshotDir, ManifestFile))
	if err != nil {
		return fmt.Errorf("read snapshot manifest: %w", err)
	}
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("snapshot schema v%d not supported (this bridge supports v%d)",
			m.SchemaVersion, SchemaVersion)
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
		}
		// Restore at 0o600 across the board — the snapshot bundle was
		// stored that way, and bridge.yaml may carry sensitive data
		// (paths, future secrets). Maintaining 0o600 on the live file
		// matches what `Snapshot` writes back into the bundle and what
		// `config.Save` would land if the operator hand-edited via
		// the admin console.
		var mode os.FileMode = 0o600
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create dir for %s: %w", name, err)
		}
		if err := copyFile(srcPath, target, mode); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
	}
	return nil
}

// Prune deletes the oldest snapshots in `backupsRoot`, keeping at
// most `keep` most recent. A `keep` of zero or below is treated as
// "retain everything"; the caller is responsible for clamping if
// they want a different floor. Returns the count of dirs deleted.
func Prune(backupsRoot string, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	manifests, err := List(backupsRoot)
	if err != nil {
		return 0, err
	}
	if len(manifests) <= keep {
		return 0, nil
	}
	// `manifests` is newest-first; the trailing entries are the
	// oldest and the ones to delete. Use the on-disk DirName so the
	// dedup suffix from `uniqueDir` is preserved (rebuilding from
	// CreatedAt would miss "<stamp>-1" / "<stamp>-2" collisions).
	deleted := 0
	var errs []error
	for _, m := range manifests[keep:] {
		full := filepath.Join(backupsRoot, m.DirName)
		if err := os.RemoveAll(full); err != nil {
			// Best-effort: a single locked / permission-drifted snapshot
			// dir must not block reclaiming the older ones behind it, or
			// disk usage grows unbounded on a long-running bridge (a
			// fail-fast return left every snapshot past the first failure
			// un-pruned forever). Collect and keep going.
			errs = append(errs, fmt.Errorf("remove %s: %w", full, err))
			continue
		}
		deleted++
	}
	return deleted, errors.Join(errs...)
}

// snapshotEntry is List's return shape — Manifest plus the on-disk
// directory path so callers (admin UI, prune) can act on the entry
// without re-reading the directory list.
type snapshotEntry struct {
	Manifest
	DirName string `json:"dirName"`
}

// List returns the snapshots under `backupsRoot`, newest-first.
// Directories without a readable manifest are skipped (likely
// in-progress or corrupt — operator can clean them up by hand).
func List(backupsRoot string) ([]snapshotEntry, error) {
	entries, err := os.ReadDir(backupsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]snapshotEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mPath := filepath.Join(backupsRoot, e.Name(), ManifestFile)
		m, err := readManifest(mPath)
		if err != nil {
			continue
		}
		out = append(out, snapshotEntry{Manifest: m, DirName: e.Name()})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// --- Internal helpers ---

// vacuumInto runs SQLite's VACUUM INTO to make a clean atomic copy
// of a WAL-mode database. Read-only connection so it can't disturb
// a running writer. Context-aware so a periodic snapshot can be
// cancelled on bridge shutdown.
func vacuumInto(ctx context.Context, srcDB, dstDB string) error {
	uri := dsn.File(srcDB, "mode=ro&_pragma=busy_timeout(5000)")
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	// VACUUM INTO refuses if the destination already exists; clear
	// it so re-running a snapshot in the same second (collision-
	// suffix paths) doesn't bail. The parent dir is already 0700.
	_ = os.Remove(dstDB)
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dstDB); err != nil {
		// A failed VACUUM INTO can leave a partial/corrupt fragment on
		// disk. Remove it so the snapshot dir doesn't accumulate broken
		// DB files (and a later reader can't mistake one for a good copy).
		_ = os.Remove(dstDB)
		return err
	}
	// VACUUM INTO writes the destination at the umask-default mode
	// (typically 0644). The file contains hashed-token references
	// and is sensitive enough to warrant the same 0600 the rest of
	// the snapshot uses. Chmod after the write so a tester reading
	// `ls -l` sees consistent perms across the bundle.
	if err := os.Chmod(dstDB, 0o600); err != nil {
		// Don't leave a 0644 copy of token-hash data behind if we
		// couldn't lock it down — unlink it and surface the error.
		_ = os.Remove(dstDB)
		return err
	}
	return nil
}

// copyFile is an atomic byte-for-byte copy: write to `<dir>/.tmp-*`
// then rename onto `dstPath`. The intermediate file is mode `mode`
// so a power-loss after rename leaves the right perms.
func copyFile(srcPath, dstPath string, mode os.FileMode) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), ".bridge-bak-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	// Panic-safety FD close (LIFO order — runs before Remove). See
	// internal/auth/auth.go for the rationale.
	defer func() { _ = tmp.Close() }()

	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func writeManifest(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func readManifest(path string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("decode %s: %w", path, err)
	}
	return m, nil
}

// createUniqueSnapshotDir is the atomic alternative to a stat-then-
// mkdir uniqueness check. Two snapshots that hit the same second
// (rare but possible from a tight test loop) would otherwise race
// the existence check; this loop relies on `os.Mkdir` returning
// `os.ErrExist` on collision (kernel-atomic) and tries successive
// "-1", "-2" suffixes until one succeeds.
func createUniqueSnapshotDir(backupsRoot string, t time.Time) (string, error) {
	base := filepath.Join(backupsRoot, t.UTC().Format(timestampLayout))
	candidates := []string{base}
	for i := 1; i < 100; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", base, i))
	}
	for _, c := range candidates {
		if err := os.Mkdir(c, 0o700); err == nil {
			return c, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("backup: 100 snapshot dirs already exist for %s", base)
}

// EnsureFreshDataDirSibling helps tests construct a writable scratch
// `dataDir` that isn't the live bridge state. Returns the path on
// success; callers `defer os.RemoveAll(path)` to clean up.
func EnsureFreshDataDirSibling(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// EnsureBackupsDir is a small convenience that creates the backups
// root with the right permissions if it doesn't yet exist. Useful
// for the periodic ticker on a fresh `bridge init`.
func EnsureBackupsDir(dataDir string) error {
	return os.MkdirAll(filepath.Join(dataDir, BackupsDirName), 0o700)
}

// SensitivityNotice is the warning Snapshot writes to the operator
// — exposed as a constant so the CLI, the admin UI, and the docs
// all show the same wording.
const SensitivityNotice = "WARNING: backups contain the TLS private key and token hashes. Treat as secret-grade material — store with the same care as the live bridge state."

// LooksLikeSnapshotDir returns true when `path` contains a readable,
// well-formed `manifest.json` (any positive schema version). Used by
// the Restore CLI's pre-flight validation to surface a clear "this
// isn't a snapshot directory" error before loading config — the
// schema-version compatibility check happens later in Restore so
// that a future-version snapshot produces a precise mismatch error
// rather than a generic "not recognized" rejection.
func LooksLikeSnapshotDir(path string) bool {
	m, err := readManifest(filepath.Join(path, ManifestFile))
	if err != nil {
		return false
	}
	return m.SchemaVersion > 0
}

// trimTrailingSlash makes "<path>/" and "<path>" equivalent for the
// CLI restore arg. Cosmetic — the underlying ReadFile / ReadDir
// calls accept either, but accepting both keeps shell-completion
// idiosyncrasies from confusing operators.
func trimTrailingSlash(p string) string {
	return strings.TrimRight(p, string(os.PathSeparator))
}

// CleanSnapshotPath normalizes the operator-supplied snapshot path
// before use. Exported for the CLI; tests use it too so the canonical
// shape is one definition.
func CleanSnapshotPath(p string) string {
	return trimTrailingSlash(filepath.Clean(p))
}
