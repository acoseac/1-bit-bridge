// Package backup snapshots and restores the bridge's state directory.
//
// A snapshot bundles every file an operator would otherwise have to
// re-pair / re-scan to recover after corruption: the manifest SQLite
// database (`bridge.db`), the token store (`tokens.json`), the TLS
// material (`server.crt` + `server.key`), the live config
// (`bridge.yaml`), the public-mode console account (`adminauth.json`,
// when one exists), the Atlas harvest credential and sync cursor
// (`atlas-harvest.json`, when one is stored), and the player's identity
// (`player-uuid`, once the player has one: config.PlayerUUIDFile), plus
// its durable reservation floor (`player-ids.json`, restamped with the captured
// UUID, never the queue). The
// snapshot also writes a small `manifest.json`
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
// main file), so `bridge restore` holds the exclusive data-dir lock and
// refuses a running serve. The confirmation prompt also accepts `--yes`.
//
// Run as root over an install another user owns (`sudo bridge backup`,
// and `sudo bridge restore`, the usual form with the service stopped),
// everything either writes keeps the install's owner: a new directory
// its parent's (fsutil.MkdirAll / fsutil.Mkdir), a copied or restored
// file the owner of what it replaces, or of its directory
// (fsutil.KeepOwner in copyFile and writeManifest), and the snapshot's
// database, which VACUUM INTO writes, the snapshot directory's
// (fsutil.Precreate). Before, a root snapshot was root's 0700, which the
// service's own prune could not remove, and a root restore left the
// config, the database, the token store and the TLS pair root's 0600,
// so the service could not start. The console account and the harvest
// state take that same write. Restore keeps the console account and
// signs its sessions out (adminauth.SignedOutFile) in that one write, and
// writes the player's identity through the identity file's own writer
// (config.WritePlayerUUIDFile), which keeps the owner the same way.
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Snapshot schema version. Bump on incompatible layout changes
// (e.g. new mandatory file, restructured manifest). Restore refuses
// snapshots newer than this constant. Version 1 remains readable. Version 2
// makes older restores refuse before they could restore a UUID without a jump.
const SchemaVersion = 2

// ManifestFile is the metadata file written into every snapshot dir.
const ManifestFile = "manifest.json"

// ManifestDBFileName is the on-disk basename for the SQLite manifest
// inside a backup directory. Three duplicates flagged by go:S1192;
// extracted so a future rename only happens once across Capture,
// captured-list emission, and Restore.
const ManifestDBFileName = "bridge.db"

// AdminAuthFileName is the public-mode console account. Snapshot copies
// the file as stored. Restore rewrites it so the account stays and its
// sessions do not (adminauth.SignedOutFile).
const AdminAuthFileName = "adminauth.json"

// AtlasHarvestFileName is the Atlas harvest credential and its sync
// cursor. Snapshot and restore copy it byte for byte. A missing file is
// skipped, the same way tokens.json is.
const AtlasHarvestFileName = "atlas-harvest.json"

// PlayerUUIDFileName is the player's identity (config.PlayerUUIDFile),
// which controllers know the player by. Snapshot copies it as stored, and a
// missing file is skipped: a box whose player was never enabled has none.
// Restore reads the copy by the player's own rule and writes it through the
// identity file's writer, so the box's player is the device it was (Restore).
const PlayerUUIDFileName = "player-uuid"

// BackupsDirName is the dir under `dataDir` that holds snapshots.
const BackupsDirName = "backups"

// timestampLayout is the snapshot dir name format. Windows-friendly
// (no colons) and lexicographically sortable.
const timestampLayout = "2006-01-02T15-04-05Z"

// orphanReapGrace is how old a manifest-less snapshot dir must be
// before ReapOrphans deletes it. Snapshot writes manifest.json LAST
// (after the DB VACUUM + file copies), so a dir legitimately has no
// manifest for the duration of an in-flight snapshot; the grace
// spares that window while still reclaiming a dir orphaned by a hard
// crash / power-loss (whose frozen mtime ages past the grace). One
// hour is enormous headroom — the manifest SQLite copy takes seconds
// even for a large library.
const orphanReapGrace = 1 * time.Hour

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
	// PlayerFloorRequired records a writer that lists player-ids.json beside
	// every player UUID it captures, as this bridge's Snapshot does (a
	// player-uuid that holds no one UUID is copied alone, for restore to
	// refuse as it stands): restore refuses such a snapshot's UUID without its
	// floor, which only stripping leaves. A writer before reservation capture
	// recorded nothing, and BridgeVersion cannot tell the two apart (it is the
	// build's stamped version, the same 0.0.1 for every build from source).
	PlayerFloorRequired bool `json:"playerFloorRequired,omitempty"`
}

// Sources points at the live files that should be captured.
//
// Empty fields are skipped (with the exception of `DataDir`, which
// is required because it determines where the snapshot dir lands).
// Missing-on-disk files are also skipped silently — a freshly
// `bridge init`'d install hasn't pair'd anyone yet, so `tokens.json`
// may not exist.
type Sources struct {
	DataDir      string // required — where `<dataDir>/backups/<stamp>/` is created
	ManifestDB   string // typically `<dataDir>/bridge.db`
	TokensJSON   string // typically `<dataDir>/tokens.json`
	ServerCert   string // typically `<dataDir>/server.crt`
	ServerKey    string // typically `<dataDir>/server.key`
	BridgeYAML   string // path to the bridge config file
	AdminAuth    string // typically `<dataDir>/adminauth.json`; absent on a loopback install
	AtlasHarvest string // typically `<dataDir>/atlas-harvest.json`; absent until a harvest credential is stored
	PlayerUUID   string // typically `<dataDir>/player-uuid`; absent until the player has an identity
}

// Targets points at where each restored file should land. Mirrors
// `Sources` field-for-field. Empty fields skip that file (so the
// caller can choose to exclude e.g. `bridge.yaml` if they want to
// keep the live config untouched).
type Targets struct {
	ManifestDB   string
	TokensJSON   string
	ServerCert   string
	ServerKey    string
	BridgeYAML   string
	AdminAuth    string
	AtlasHarvest string
	PlayerUUID   string
}

// Snapshot captures every present file in `src` into a fresh
// timestamped directory under `<src.DataDir>/backups/`. Returns the
// absolute path of the created directory.
//
// `ctx` is forwarded to the SQLite VACUUM INTO step and read retries so the
// periodic ticker can cancel an in-flight snapshot on bridge shutdown. Pass
// `context.Background()` if you don't have a richer scope.
func Snapshot(ctx context.Context, src Sources) (snapDir string, retErr error) {
	if src.PlayerUUID != "" {
		if err := completeCapturedPlayerSeal(filepath.Dir(src.PlayerUUID)); err != nil {
			// Local repair is not part of a snapshot. The captured UUID/floor
			// pairing checks below still decide whether its identity is safe.
			// Only a busy lock defers the repair; any other failure recurs at
			// the next serve boot or start, and is logged as the failure it is.
			if errors.Is(err, errSealCompletionBusy) {
				logging.Component("backup").Warn("player seal completion deferred", "reason", err.Error())
			} else {
				logging.Component("backup").Warn("player seal completion failed", "reason", err.Error())
			}
		}
	}
	if src.DataDir == "" {
		return "", errors.New("backup: DataDir is required")
	}
	backupsRoot := filepath.Join(src.DataDir, BackupsDirName)
	if err := fsutil.MkdirAll(backupsRoot, 0o700); err != nil {
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
		{src.AdminAuth, AdminAuthFileName},
		{src.AtlasHarvest, AtlasHarvestFileName},
		{src.PlayerUUID, PlayerUUIDFileName},
	} {
		if pair.src == "" {
			continue
		}
		err := atomicwrite.ReadWithRetryCtx(ctx, func() error {
			_, err := os.Stat(pair.src)
			return err
		})
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return "", fmt.Errorf("stat %s: %w", pair.name, err)
		}
		if err := copyFileCtx(ctx, pair.src, filepath.Join(dst, pair.name), 0o600); err != nil {
			return "", fmt.Errorf("copy %s: %w", pair.name, err)
		}
		captured = append(captured, pair.name)
	}

	// Capture the UUID before its floor and require the same named identity.
	// A discard and mint between these reads must not pair old UUID/new floor.
	if slices.Contains(captured, PlayerUUIDFileName) {
		var id string
		idErr := atomicwrite.ReadWithRetryCtx(ctx, func() error {
			var err error
			id, err = config.ReadPlayerUUIDFile(filepath.Join(dst, PlayerUUIDFileName))
			return err
		})
		if idErr != nil && !errors.Is(idErr, config.ErrNotOnePlayerUUID) {
			return "", fmt.Errorf("backup: read captured %s: %w", PlayerUUIDFileName, idErr)
		}
		var mark engine.Reservation
		readFloor := func() error {
			return atomicwrite.ReadWithRetryCtx(ctx, func() error {
				var err error
				mark, err = engine.ReadReservation(filepath.Join(filepath.Dir(src.PlayerUUID), engine.ReservationFileName))
				return err
			})
		}
		err := readFloor()
		if errors.Is(err, os.ErrNotExist) {
			if idErr == nil {
				if upgradeErr := upgradeCapturedPlayer(filepath.Dir(src.PlayerUUID), id); upgradeErr != nil {
					return "", fmt.Errorf("backup: %w", upgradeErr)
				}
				err = readFloor()
			}
			if errors.Is(err, os.ErrNotExist) {
				if _, statErr := os.Lstat(src.PlayerUUID); errors.Is(statErr, os.ErrNotExist) {
					return "", fmt.Errorf("backup: player UUID vanished during capture; run the backup again: %w", err)
				} else if statErr != nil {
					return "", fmt.Errorf("backup: check player-uuid: %w", statErr)
				}
				advice := playerResetAdvice(false)
				if marked, err := playerEntry(filepath.Dir(src.PlayerUUID), playerInitialized); err != nil || marked {
					advice = keepInitializedMarker + "; " + advice
				}
				return "", fmt.Errorf("backup: player floor is missing; %s", advice)
			}
		}
		if err != nil {
			if errors.Is(err, atomicwrite.ErrReadBusy) {
				return "", fmt.Errorf("backup: read %s: %w; retry the backup", engine.ReservationFileName, err)
			}
			var pathErr *os.PathError
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &pathErr) {
				return "", fmt.Errorf("backup: read %s: %w", engine.ReservationFileName, err)
			}
			// Only a file read successfully but refused as data justifies
			// giving up the identity. A failed read establishes no damage.
			return "", fmt.Errorf("backup: read %s: %w; %s", engine.ReservationFileName, err, playerResetAdvice(false))
		}
		if idErr == nil {
			if mark.UUID != id {
				return "", errors.New("backup: player identity changed or the floor does not name the captured UUID; run bridge reset-player before backing up again")
			}
			mark.Version, mark.UUID = 2, id
			data, err := json.Marshal(mark)
			if err != nil {
				return "", err
			}
			if err := writeFrom(filepath.Join(dst, engine.ReservationFileName), bytes.NewReader(data), 0o600); err != nil {
				return "", err
			}
			captured = append(captured, engine.ReservationFileName)
		}
	}

	// Every captured player UUID above travels with its floor, or the
	// snapshot was refused: the manifest says so to restore.
	m := Manifest{
		SchemaVersion:       SchemaVersion,
		BridgeVersion:       version.ServerVersion,
		ProtocolVersion:     version.ProtocolVersion,
		CreatedAt:           time.Now().UTC(),
		Files:               captured,
		PlayerFloorRequired: true,
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
// restore is the most useful failure mode anyway. The console account is
// the exception: it is rewritten, not copied, and one that does not parse
// fails the restore before any file is written (signedOutAdminAuth). So is
// the player's identity: it is written through the identity file's writer
// (config.WritePlayerUUIDFile), and a copy that does not hold one UUID
// fails the restore before any file is written (snapshotPlayerUUID).
//
// The snapshot's identity wins over the one the box has. A restore makes
// the box the bridge the snapshot was taken from, its database, tokens, TLS
// pair and config included, and that bridge's player is the device its
// controllers know by that UUID: keeping the box's would put a new player
// beside a restored bridge. A snapshot that holds none (taken before the
// file was in the bundle, or of a box whose player had none) leaves the
// box's identity as it is.
// A restored identity first gets a merged, durable reservation, past both
// the snapshot's and the live counters by 2^24 plus one random 24-bit draw.
// The snapshot reservation is only an input, never copied to the live path.
// A snapshot that lists no floor counts as 2^31 + 2^24, the adoption
// ceiling, when an older writer made it, and is refused when this bridge
// did (floorlessSnapshotBound).
func Restore(snapshotDir string, dst Targets) error {
	return RestoreWithOptions(snapshotDir, dst, RestoreOptions{})
}

// RestoreWithOptions restores with an optional warning sink and random source.
// Like Restore, its caller must hold the data directory's exclusive lock.
func RestoreWithOptions(snapshotDir string, dst Targets, opts RestoreOptions) error {
	var completionErr error
	if dst.PlayerUUID != "" {
		completionErr = completePlayerSeal(filepath.Dir(dst.PlayerUUID))
	}
	m, err := readManifest(filepath.Join(snapshotDir, ManifestFile))
	if err != nil {
		return fmt.Errorf("read snapshot manifest: %w", err)
	}
	if m.SchemaVersion < 1 || m.SchemaVersion > SchemaVersion {
		return fmt.Errorf("snapshot schema v%d not supported (this bridge supports v%d)",
			m.SchemaVersion, SchemaVersion)
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
	// (CodeRabbit on PR #1200).
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
	if playerUUIDRead {
		if completionErr != nil {
			return completionErr
		}
		if err := restorePlayerReservation(snapshotDir, dst.PlayerUUID, playerUUID, m, opts); err != nil {
			return err
		}
		// A replacement identity must never expose this box's old entries,
		// including the UUID-less queue format on development boxes.
		if err := discardReplacedPlayerQueue(dst.PlayerUUID, playerUUID); err != nil {
			return err
		}
		// This comes after the reservation and before all other identity
		// files, regardless of their order in the manifest. A failed restore
		// may leave a raised floor; nothing rolls it back.
		if err := config.WritePlayerUUIDFile(dst.PlayerUUID, playerUUID); err != nil {
			return fmt.Errorf("restore %s: %w", PlayerUUIDFileName, err)
		}
	}
	for _, name := range m.Files {
		// The reservation is an input only, never a copied snapshot file.
		if name == PlayerUUIDFileName || name == engine.ReservationFileName {
			continue
		}
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
			// the exact corruption this block prevents (Gemini + CodeRabbit
			// on PR #476).
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

// PruneResult reports what a Prune call did.
//
// `ReapOrphans`'s outcome is carried HERE rather than folded into Prune's
// error return, and that separation is the point. The orphan sweep can
// report a directory it could not classify — most importantly one whose
// `manifest.json` is unreadable rather than absent, which it deliberately
// refuses to delete — and that condition is (a) permanent for as long as
// the directory exists and (b) not a failure of the prune the caller
// actually asked for. Joined into the error return it made `bridge backup`
// exit 1 on EVERY run thereafter, and made `runBackupTicker` skip its
// "pruned N" line, even though the snapshot AND the keep-policy prune had
// both succeeded. Callers should surface ReapErr as a warning and keep
// going.
type PruneResult struct {
	// Deleted counts snapshot dirs removed by the keep-policy.
	Deleted int
	// Reaped counts manifest-less orphan dirs removed by the sweep.
	Reaped int
	// ReapErr is the orphan sweep's non-fatal outcome. Never nil-checked
	// as a prune failure — see the type docblock.
	ReapErr error
}

// Prune is PruneContext with a background context. Kept for the CLI and
// any caller with no scope to inherit.
func Prune(backupsRoot string, keep int) (PruneResult, error) {
	return PruneContext(context.Background(), backupsRoot, keep)
}

// PruneContext deletes the oldest snapshots in `backupsRoot`, keeping at
// most `keep` most recent. A `keep` of zero or below is treated as
// "retain everything"; the caller is responsible for clamping if
// they want a different floor.
//
// **`keep <= 0` disables the KEEP-POLICY, not the orphan sweep.** Callers
// must therefore invoke this unconditionally rather than guarding it with
// their own `keep > 0` check — an operator who has turned retention off
// still needs crash orphans reclaimed (they carry a near-full DB copy and
// nothing else in this package can ever remove them), and still needs the
// sweep's warnings surfaced.
//
// The context bounds the two genuinely unbounded loops: the per-directory
// manifest reads (`ListContext` exists precisely because a root with many
// snapshots — or one on a stalled network mount — takes arbitrarily long)
// and the delete loop. The serve-side ticker calls this from a ctx-bound
// goroutine that is joined at shutdown, so a prune stuck on a wedged mount
// would otherwise burn the whole shutdown grace.
//
// The returned error covers only the keep-policy prune (plus cancellation).
// Orphan-sweep problems ride on PruneResult.ReapErr — see PruneResult.
func PruneContext(ctx context.Context, backupsRoot string, keep int) (PruneResult, error) {
	// Reap crash-orphaned partial snapshots first — dirs with no
	// manifest.json that `List` (and therefore the keep-policy below)
	// can never see, so the keep-policy would leave them forever. Runs
	// regardless of `keep` (an orphan is never a snapshot worth
	// retaining). Best-effort: a reap error is REPORTED but never blocks
	// the keep-policy prune. Prune is the natural wiring point — every
	// snapshot path (ticker, admin, CLI) calls it right after Snapshot.
	reaped, reapErr := reapOrphans(ctx, backupsRoot, orphanReapGrace)
	res := PruneResult{Reaped: reaped, ReapErr: reapErr}
	if keep <= 0 {
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	manifests, err := ListContext(ctx, backupsRoot)
	if err != nil {
		return res, err
	}
	if len(manifests) <= keep {
		return res, nil
	}
	// `manifests` is newest-first; the trailing entries are the
	// oldest and the ones to delete. Use the on-disk DirName so the
	// dedup suffix from `uniqueDir` is preserved (rebuilding from
	// CreatedAt would miss "<stamp>-1" / "<stamp>-2" collisions).
	var errs []error
	for _, m := range manifests[keep:] {
		// Between removals, not mid-removal: os.RemoveAll isn't
		// interruptible, and abandoning a half-removed snapshot dir would
		// manufacture exactly the manifest-less orphan the sweep above
		// exists to clean up.
		if err := ctx.Err(); err != nil {
			return res, errors.Join(errors.Join(errs...), err)
		}
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
		res.Deleted++
	}
	return res, errors.Join(errs...)
}

// ReapOrphans deletes snapshot subdirectories under backupsRoot that
// have no readable manifest.json AND whose directory mtime is older
// than `grace`. These are the residue of a snapshot whose writer died
// (SIGKILL / power-loss) between the DB copy and the manifest write:
// they carry a near-full bridge.db copy, `List` skips them (no
// manifest), and `Prune`'s keep-policy therefore can never reclaim
// them — so they accumulate unbounded across hard crashes. (The
// Snapshot deferred cleanup only covers graceful returns.)
//
// Returns the count reaped. Best-effort: a single un-removable dir
// doesn't block the others (errors are joined). A `grace` of zero or
// below reaps every manifest-less dir regardless of age (test
// affordance; production passes orphanReapGrace via Prune).
//
// A dir whose manifest is UNREADABLE (as opposed to absent) is REPORTED
// AND KEPT — permanently, with no "eventually reap it anyway" escape
// hatch. That is a deliberate choice, not an omission: an unreadable
// manifest is not evidence of a crash orphan (the AV-handle window above
// produces it on a perfectly good backup), the directory still holds a
// full bridge.db an operator can recover by hand, and a persistent EACCES
// would otherwise turn a healthy backup into a scheduled deletion. Since
// writeManifest is now atomic, a truncated manifest is no longer a state
// this package can produce; a persistent read error means something
// outside it needs the operator's attention, which is exactly what
// surfacing it through PruneResult.ReapErr does — where it is visible on
// every prune without failing the prune.
func ReapOrphans(backupsRoot string, grace time.Duration) (int, error) {
	return reapOrphans(context.Background(), backupsRoot, grace)
}

// reapOrphans is ReapOrphans bounded by ctx. Cancellation is checked
// BETWEEN directories — os.RemoveAll isn't interruptible, and stopping
// part-way through one would leave behind exactly the manifest-less
// residue this function exists to reclaim.
func reapOrphans(ctx context.Context, backupsRoot string, grace time.Duration) (int, error) {
	// Refuse an empty root. Not because os.ReadDir("") reads the working
	// directory, as the post-merge review of #531 said: it fails with ENOENT
	// on every platform (measured with go1.26.6 on macOS, Linux and Windows),
	// so an empty root would fall into the not-exist branch below and reap
	// nothing, silently. It is refused because an empty backupsRoot is a
	// misconfiguration the caller should hear about (PruneResult.ReapErr),
	// and because this check runs before anything can turn "" into the
	// working directory: filepath.Clean("") is ".", and filepath.Join("",
	// name) is relative to it, so a root resolved before the listing, or a
	// name joined onto an empty one, WOULD reap unrelated directories next to
	// wherever the bridge runs. TestReapOrphansRefusesAnEmptyRoot pins it.
	if strings.TrimSpace(backupsRoot) == "" {
		return 0, errors.New("backup: ReapOrphans requires a non-empty backups root")
	}
	entries, err := os.ReadDir(backupsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	now := time.Now()
	reaped := 0
	var errs []error
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return reaped, errors.Join(errors.Join(errs...), err)
		}
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(backupsRoot, e.Name())
		// Reap only when the manifest is genuinely ABSENT — that is what
		// marks a snapshot whose writer died mid-run. Any OTHER read error
		// (EACCES, EIO, Windows ERROR_SHARING_VIOLATION while Defender or
		// the Search Indexer holds the file) means we simply couldn't tell,
		// and the pre-fix `err == nil` gate treated that as "crash orphan"
		// and os.RemoveAll'd a COMPLETE backup — silently, since Prune
		// discards the count and this package has no logger.
		//
		// That AV-holds-a-handle window is not hypothetical here; it is the
		// same one renameWithRetry exists for. RemoveAll would unlink the
		// manifest (AV handles usually carry FILE_SHARE_DELETE) then fail
		// the rmdir, leaving a dir that now genuinely has no manifest — so
		// the NEXT prune reaps it outright. A good backup died over two
		// cycles. Surface the error instead and keep the directory.
		if _, err := readManifest(filepath.Join(dir, ManifestFile)); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				errs = append(errs, fmt.Errorf("read manifest %s: %w", dir, err))
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			// Vanished between ReadDir and Info (a concurrent prune / the
			// operator cleaning up) — nothing to reap.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			errs = append(errs, fmt.Errorf("stat %s: %w", dir, err))
			continue
		}
		// Spare a dir younger than the grace — it may be an in-progress
		// snapshot that hasn't written its manifest yet. The dir mtime
		// bumps as Snapshot adds files and freezes at the crash instant.
		if grace > 0 && now.Sub(info.ModTime()) < grace {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("reap orphan %s: %w", dir, err))
			continue
		}
		reaped++
	}
	return reaped, errors.Join(errs...)
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
//
// Equivalent to ListContext with a background context. Kept for callers
// with no scope to inherit (the CLI) and for the ones where the listing
// is the whole point of the request.
func List(backupsRoot string) ([]snapshotEntry, error) {
	return ListContext(context.Background(), backupsRoot)
}

// ListContext is List, bounded by ctx.
//
// The cancellation point is per-directory, which is where the cost is:
// each entry means an open + read + JSON parse of its manifest, so a root
// holding many snapshots — or one on a stalled network mount — can take
// arbitrarily long. Callers on a request path (the 10s-polled jobs
// endpoint) need to be able to stop.
func ListContext(ctx context.Context, backupsRoot string) ([]snapshotEntry, error) {
	entries, err := os.ReadDir(backupsRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]snapshotEntry, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
//
// vacuumInto, the manifest database's copy, is in vacuum.go.

// copyFile is an atomic byte-for-byte copy: write to `<dir>/.tmp-*`
// then rename onto `dstPath`. The intermediate file is mode `mode`
// so a power-loss after rename leaves the right perms.
func copyFile(srcPath, dstPath string, mode os.FileMode) error {
	return copyFileCtx(context.Background(), srcPath, dstPath, mode)
}

// copyFileCtx lets a snapshot cancel a sharing-violation backoff. Only the
// source open is retried; the copy and its commit still happen exactly once.
// OpenAsFile refuses a named pipe without waiting for a writer.
func copyFileCtx(ctx context.Context, srcPath, dstPath string, mode os.FileMode) error {
	var in *os.File
	err := atomicwrite.ReadWithRetryCtx(ctx, func() error {
		var err error
		in, _, err = fsutil.OpenAsFile(srcPath)
		return err
	})
	if err != nil {
		return err
	}
	defer in.Close()
	return writeFrom(dstPath, in, mode)
}

// writeFrom is copyFile's commit: one temp file, one KeepOwner, then
// rename onto dstPath. It streams r, so Restore's database copy never
// holds the file in memory. Restore hands it the signed-out account
// through bytes.NewReader, so that account is a single write.
func writeFrom(dstPath string, r io.Reader, mode os.FileMode) error {
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
	// The owner of what this replaces (a restore's live file), or of the
	// directory (a snapshot's copy): the service must read both.
	if err := fsutil.KeepOwner(tmp, dstPath); err != nil {
		return err
	}

	if _, err := io.Copy(tmp, r); err != nil {
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
	if err := atomicwrite.RenameWithRetry(tmpName, dstPath); err != nil {
		return err
	}
	committed = true
	return nil
}

// signedOutAdminAuth reads a snapshot's console account and returns it
// with its sessions signed out (adminauth.SignedOutFile). Restore calls it
// before its loop, so an account that does not parse fails the restore
// before any file is written. read is false when the snapshot lists the
// file but does not hold it; the loop skips such a file the same way.
func signedOutAdminAuth(srcPath string) (out []byte, read bool, err error) {
	raw, err := os.ReadFile(srcPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("restore %s: %w", AdminAuthFileName, err)
	}
	out, _, err = adminauth.SignedOutFile(raw, time.Now())
	if err != nil {
		return nil, false, fmt.Errorf("restore %s: %w", AdminAuthFileName, err)
	}
	return out, true, nil
}

// snapshotPlayerUUID reads a snapshot's copy of the player's UUID by the
// rule the player reads its own with (config.ReadPlayerUUIDFile). Restore
// calls it before its loop, so a copy that does not hold one UUID fails the
// restore before any file is written: neither the box's identity nor any
// other file is replaced, and no identity is made from it. read is false
// when the snapshot lists the file but does not hold it, which the loop
// skips the same way; removing the copy is how an operator restores the
// rest of such a snapshot and leaves the box's identity as it is.
func snapshotPlayerUUID(srcPath string) (id string, read bool, err error) {
	id, err = config.ReadPlayerUUIDFile(srcPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	case errors.Is(err, config.ErrNotOnePlayerUUID):
		return "", false, fmt.Errorf("restore %s: %w, so the player cannot have the identity it had; nothing was restored: "+
			"remove it from the snapshot to restore the rest and leave this box's player identity as it is", PlayerUUIDFileName, err)
	case err != nil:
		return "", false, fmt.Errorf("restore %s: %w; nothing was restored", PlayerUUIDFileName, err)
	}
	return id, true, nil
}

// writeManifest commits the snapshot metadata ATOMICALLY — temp file in
// the same directory, then rename — rather than truncating the
// destination in place.
//
// This is load-bearing, not hygiene. `manifest.json` is the file that
// distinguishes a complete snapshot from a crash orphan, and it is written
// LAST, after a possibly-multi-GB DB copy. A hard crash (SIGKILL,
// power-loss) partway through a plain `os.WriteFile` leaves a TRUNCATED
// file: `readManifest` then returns a decode error, which is neither
// "present" nor `os.ErrNotExist`, so `List` skips the dir, `Prune`'s
// keep-policy can never select it, and `ReapOrphans` deliberately refuses
// to delete it (a read error is not evidence of a crash orphan — see its
// docblock). Nothing in this package could ever remove that directory, and
// every subsequent `Prune` returned its error forever, which the CLI
// reports as a failed `bridge backup` (exit 1) on every run thereafter.
//
// With the rename, a crash leaves either NO manifest (a genuine orphan,
// reaped after the grace) or a complete one. There is no truncated state
// to produce.
//
// Mirrors copyFile's shape in this package (CreateTemp → write → Sync →
// Chmod → atomicwrite.RenameWithRetry, which absorbs the Windows
// AV-scan-on-close rename window). The distinct temp prefix makes a stray
// temp identifiable as manifest-side rather than file-copy-side.
func writeManifest(path string, m Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-bak-manifest-*")
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
	if err := fsutil.KeepOwner(tmp, path); err != nil {
		return err
	}

	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := atomicwrite.RenameWithRetry(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
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
	// Test candidates lazily — the base path succeeds >99% of the time,
	// so building all 100 fallback strings up front is wasted work.
	for i := 0; i < 100; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		if err := fsutil.Mkdir(candidate, 0o700); err == nil {
			return candidate, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("backup: 100 snapshot dirs already exist for %s", base)
}

// EnsureBackupsDir is a small convenience that creates the backups
// root with the right permissions if it doesn't yet exist. Useful
// for the periodic ticker on a fresh `bridge init`.
func EnsureBackupsDir(dataDir string) error {
	return fsutil.MkdirAll(filepath.Join(dataDir, BackupsDirName), 0o700)
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
