// `bridge variants` subcommand group (v1.4 PR D2).
//
// One subcommand today:
//   - `bridge variants move --to <path>` — walk every track_variants
//     row, recompute its new sidecar path under <path> using the
//     source-mirrored layout, place the file there while the source
//     name still exists, update the DB row, then remove the source
//     name, and report progress.
//
// Crash-safety contract: each row's operation is independently
// idempotent. The destination holds the bytes before the row moves,
// and the source name is removed only after the row points at the
// destination, so a crash leaves two copies rather than a row whose
// file is already gone. A crash after the second name exists and
// before the row is updated is resumed by the next run: one file
// under two names updates the row and leaves the old name (the
// orphan sweep reaps a hard link), and a copied second inode is
// copied again and the source name removed after the update. The
// copy is a temp file in the destination's directory, fsynced, then
// renamed over the destination, so the final name appears only
// complete and a name that shares an inode with another file is
// replaced as a directory entry. A crash mid-copy leaves the temp.
// A crash after the update and before that removal leaves the extra
// name for the same sweep. Rows already pointing at the destination
// are skipped because the recomputed path equals the recorded one.
// Two paths that are one file (a case-only spelling, a --to that is
// a link to the variants directory) update the row and remove
// nothing: removing the source name would remove the only copy.
//
// FK pre-check: variants whose parent `tracks` row is gone are
// skipped with a warning. CASCADE on track delete would have
// pruned the variant too; an orphan variant DB row is a sign of
// inconsistent state and shouldn't be moved.
//
// Run as root (the usual way to move the variants onto a new disk,
// whose mount point root owns), a move keeps what it moves the
// service's, as mv does: a linked sidecar keeps its owner by itself
// (it is the same inode), a copied one is given the owner of the file
// it replaces (copyAndFsync),
// the --to directory it creates the owner of the variants directory it
// replaces (fsutil.MkdirAllLike), and an album directory beneath the
// owner of the directory it is created in (fsutil.MkdirAll). Before,
// every one of those was root's, and the service could not add a
// rendition to the tree it was pointed at next.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

func variantsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bridge variants <move> [flags]")
		return 2
	}
	switch args[0] {
	case "move":
		return variantsMoveCmd(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, variantsUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown variants subcommand: %s\n\n%s\n", args[0], variantsUsage)
		return 2
	}
}

const variantsUsage = `bridge variants — manage upscaled FLAC sidecars

Subcommands:
  move --to <path>     Relocate every track_variants row's on-disk
                       file to <path> using the source-mirrored
                       layout. Updates DB rows in lockstep. Safe
                       to interrupt and re-run.

Run "bridge variants <subcommand> -h" for subcommand-specific flags.`

func variantsMoveCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("variants move", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	to := fs.String("to", "", "absolute destination directory for variants (required)")
	dryRun := fs.Bool("dry-run", false, "list planned moves without touching files or DB")
	confirm := fs.String("confirm", "", "type MOVE to confirm destructive relocation (skipped under --dry-run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to == "" {
		fmt.Fprintln(stderr, "missing --to <path>")
		return 2
	}
	if !filepath.IsAbs(*to) {
		fmt.Fprintln(stderr, "--to must be an absolute path")
		return 2
	}
	// Typed-phrase confirmation gate (CodeRabbit Major on PR #245).
	// `variants move` unlinks source sidecar files; the rest of the
	// bridge CLI's destructive subcommands (notably `bridge tsnet
	// logout`) require the operator to type WIPE / similar to
	// confirm. Exact-match (not prefix-match) so `M`, `mov`, etc.
	// don't slip past.
	if !*dryRun && *confirm != "MOVE" {
		fmt.Fprintln(stderr, "refusing to proceed without --confirm MOVE")
		fmt.Fprintln(stderr, "(use --dry-run to preview the moves without writing)")
		return 2
	}

	cfg, _, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load: %v\n", err)
		return 2
	}
	if conflictingRoot := fsutil.IsUnderAny(*to, cfg.LibraryRoots); conflictingRoot != "" {
		fmt.Fprintf(stderr, "--to must not be under library root %q\n", conflictingRoot)
		return 2
	}

	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()

	all, err := store.AllVariants(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list variants: %v\n", err)
		return 1
	}
	if len(all) == 0 {
		fmt.Fprintln(stdout, "No variants to move.")
		return 0
	}

	// Not under --dry-run. The flag promises to "list planned moves without
	// touching files or DB", and creating the destination directory is
	// touching the filesystem — on a preview an operator may well be running
	// against a path they have not decided on yet.
	if !*dryRun {
		if err := fsutil.MkdirAllLike(*to, 0o755, cfg.Upscale.EffectiveVariantsDir(cfg.DataDir)); err != nil {
			fmt.Fprintf(stderr, "mkdir destination: %v\n", err)
			return 1
		}
	}

	var (
		moved   int
		skipped int
		failed  int
	)
	for _, v := range all {
		// Stop promptly on Ctrl-C rather than driving every remaining
		// moveOneVariant into a canceled-ctx DB write and flooding
		// stderr with one error per row.
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(stderr, "move interrupted (moved=%d skipped=%d failed=%d)\n", moved, skipped, failed)
			return 130
		}
		newPath := computeNewSidecarPath(*to, v)
		if newPath == v.SidecarPath {
			skipped++
			continue
		}
		if *dryRun {
			fmt.Fprintf(stdout, "[dry-run] %s → %s\n",
				(v.SidecarPath), (newPath))
			continue
		}
		if err := moveOneVariant(ctx, store, v, newPath); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n",
				(v.SidecarPath), err)
			failed++
			continue
		}
		moved++
		if moved%50 == 0 {
			fmt.Fprintf(stdout, "moved %d/%d (failed=%d, skipped=%d)\n",
				moved, len(all), failed, skipped)
		}
	}
	fmt.Fprintf(stdout, "Done. moved=%d skipped=%d failed=%d\n",
		moved, skipped, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// computeNewSidecarPath builds the destination sidecar path under
// <to> matching the v1.4 source-mirrored layout from
// `transcode.JobSpec.SidecarPath`.
//
// We don't have the full JobSpec here (only the persisted VariantRow),
// so we can't call JobSpec.SidecarPath directly — but the layout is ONE
// function, transcode.VariantSidecarPath, shared with the pool writer
// and the integrity probes, so the recomputed path cannot drift from
// the persisted sidecar_path (and from where the reapers look for a
// relocated file). A raw fmt.Sprintf here once skipped the FAT
// sanitization + 255-byte truncation, so a move to a FAT/exFAT target
// (the documented use case) failed on every colon/`?`-bearing classical
// filename, and over-long names hit ENAMETOOLONG even on ext4.
func computeNewSidecarPath(toDir string, v manifest.VariantRow) string {
	return transcode.VariantSidecarPath(toDir, v.SourcePath, v.VariantID)
}

// linkSidecar names the destination as another directory entry of the
// source file. A test points it at a function that fails so the copy
// path runs where a hard link cannot be made.
var linkSidecar = os.Link

// moveBeforeRowUpdate runs after the file step and before the row
// update. Nil in production. The suite runs a watcher tick here.
var moveBeforeRowUpdate func()

// moveOneVariant places the sidecar at newPath and then records that
// path. The destination name exists before the row moves, and the
// source name is removed only after the row points at the destination,
// so a crash leaves two copies rather than a row whose file is already
// gone. Two paths that name one file update the row and remove nothing.
//
// A crash between the second name and the row update leaves both
// copies. The next run sees one file under two names and updates the
// row without removing the old name, or copies again when the second
// name is another inode and removes the source name after the update.
// The copy is renamed into place, so an existing destination is
// replaced as a directory entry: a hard link of another file keeps
// that file's bytes.
// A crash after the update leaves the extra name for the orphan sweep.
// A source already gone with the destination present (a crash of an
// older rename) updates the row and removes nothing.
func moveOneVariant(ctx context.Context, store *manifest.Store, v manifest.VariantRow, newPath string) error {
	srcInfo, err := os.Stat(v.SidecarPath)
	if err != nil {
		if os.IsNotExist(err) {
			if _, derr := os.Stat(newPath); derr == nil {
				return store.UpdateVariantSidecarPath(ctx, v.SourcePath, v.VariantID, newPath)
			}
			return errors.New("source sidecar missing on disk")
		}
		return fmt.Errorf("stat source: %w", err)
	}

	if err := fsutil.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return fmt.Errorf("mkdir destination parent: %w", err)
	}

	// SameFile is asked before any link or copy. It is true for one
	// directory entry spelled two ways and for two hard-link names of
	// one inode. A link this call is about to make must not be judged
	// that way, or the source name would be left in place on purpose
	// and the destination would be the only name the row records.
	removeSource := false
	dstInfo, err := os.Stat(newPath)
	switch {
	case err == nil && os.SameFile(srcInfo, dstInfo):
	case err == nil:
		if err := copyAndFsync(v.SidecarPath, newPath); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		removeSource = true
	case os.IsNotExist(err):
		if err := linkSidecar(v.SidecarPath, newPath); err != nil {
			if err := copyAndFsync(v.SidecarPath, newPath); err != nil {
				return fmt.Errorf("copy: %w", err)
			}
		}
		removeSource = true
	default:
		return fmt.Errorf("stat destination: %w", err)
	}

	if moveBeforeRowUpdate != nil {
		moveBeforeRowUpdate()
	}
	if err := store.UpdateVariantSidecarPath(ctx, v.SourcePath, v.VariantID, newPath); err != nil {
		return err
	}
	if !removeSource {
		return nil
	}
	if err := os.Remove(v.SidecarPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: row updated but unlink of the source name failed: %v\n", err)
	}
	return nil
}

// copyAndFsync streams the source into a temp file in the destination's
// directory, fsyncs it, and renames it over dst. Both copy sites use it:
// a destination that already exists as a different file, and a link that
// failed. The rename replaces the directory entry. It does not truncate
// an inode another name still points at, and the final name never holds
// a partial copy.
//
// Close's error is checked. A deferred Close remains for the paths that
// return before that, and a deferred Remove drops the temp unless the
// rename has landed. Close on an already-closed file is a no-op.
//
// The copy stands in for src, which the move then unlinks. Run as root
// it keeps src's owner, as a rename would have. A new file is created
// 0644, as before; a file being replaced keeps that file's permission
// bits on the new inode.
func copyAndFsync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	replacing, err := os.Lstat(dst)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out, tmp, err := openMoveCopy(filepath.Dir(dst), src)
	if err != nil {
		return err
	}
	// Remove is registered first so it runs after Close. The temp name
	// is dropped unless the rename has published it.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp)
		}
	}()
	defer out.Close()
	if replacing != nil && replacing.Mode().IsRegular() {
		if err := out.Chmod(replacing.Mode().Perm()); err != nil {
			return err
		}
	}
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close destination: %w", err)
	}
	if err := atomicwrite.RenameWithRetry(tmp, dst); err != nil {
		return err
	}
	committed = true
	return nil
}

// openMoveCopy creates an empty 0644 file in dir whose name ends in
// .tmp, so a crash leftover is the sidecar sweep's scratch and not a
// rendition under its final name. It gives the new file the owner of
// ownerPath: KeepOwner reads the entry at that path, and the copy stands
// in for the source the move then unlinks.
func openMoveCopy(dir, ownerPath string) (*os.File, string, error) {
	var rnd [8]byte
	for range 100 {
		if _, err := rand.Read(rnd[:]); err != nil {
			return nil, "", err
		}
		name := filepath.Join(dir, ".bridge-move-"+hex.EncodeToString(rnd[:])+".tmp")
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			if !os.IsExist(err) {
				return nil, "", err
			}
			continue
		}
		if err := fsutil.KeepOwner(f, ownerPath); err != nil {
			_ = f.Close()
			_ = os.Remove(name)
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", fmt.Errorf("create temp copy in %s", dir)
}
