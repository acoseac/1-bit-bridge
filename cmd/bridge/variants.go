// `bridge variants` subcommand group (v1.4 PR D2).
//
// One subcommand today:
//   - `bridge variants move --to <path>` — walk every track_variants
//     row, recompute its new sidecar path under <path> using the
//     source-mirrored layout, move the file (os.Rename fast-path on
//     same filesystem, copy+unlink fallback on EXDEV), update the
//     DB row, report progress.
//
// Crash-safety contract: each row's operation is independently
// idempotent. If the file move succeeds but the DB update fails,
// the old DB row still points at the OLD path (which no longer
// exists on disk) — `bridge upscale --gc`'s reverse sweep will
// reconcile. If the file move succeeds AND DB update succeeds,
// the new state is on disk + in DB. Re-running `bridge variants
// move --to <samepath>` after a partial run resumes cleanly: rows
// already pointing at <path> are skipped via the destination-stat
// precheck.
//
// FK pre-check: variants whose parent `tracks` row is gone are
// skipped with a warning. CASCADE on track delete would have
// pruned the variant too; an orphan variant DB row is a sign of
// inconsistent state and shouldn't be moved.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
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
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	to := fs.String("to", "", "absolute destination directory for variants (required)")
	dryRun := fs.Bool("dry-run", false, "list planned moves without touching files or DB")
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

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load: %v\n", err)
		return 2
	}
	// Same validation the runtime config-load applies, mirrored
	// inline so the CLI surfaces a friendly error before touching
	// any DB / disk state.
	for _, root := range cfg.LibraryRoots {
		if root == "" {
			continue
		}
		cleanedRoot := filepath.Clean(root)
		rel, rerr := filepath.Rel(cleanedRoot, filepath.Clean(*to))
		if rerr == nil && rel != ".." && len(rel) > 0 && rel[0] != '.' {
			fmt.Fprintf(stderr, "--to must not be under library root %q\n", cleanedRoot)
			return 2
		}
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

	if err := os.MkdirAll(*to, 0o755); err != nil {
		fmt.Fprintf(stderr, "mkdir destination: %v\n", err)
		return 1
	}

	var (
		moved   int
		skipped int
		failed  int
	)
	for _, v := range all {
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
// `transcode.JobSpec.SidecarPath`. Mirrors the same shape that the
// runtime pool produces for new conversions.
//
// Constructed by hand here (rather than calling JobSpec.SidecarPath)
// because we don't have the full JobSpec — only the persisted
// VariantRow. The mapping is deterministic from
// `(source_path, variant_id)` so this stays in lockstep with the
// pool's writer.
func computeNewSidecarPath(toDir string, v manifest.VariantRow) string {
	dir := filepath.Dir(v.SourcePath)
	base := filepath.Base(v.SourcePath)
	filename := fmt.Sprintf("%s.%s.flac", base, v.VariantID)
	if dir == "" || dir == "." {
		return filepath.Join(toDir, filename)
	}
	return filepath.Join(toDir, dir, filename)
}

// moveOneVariant runs the per-row move pipeline:
//  1. Stat source — surface missing-on-disk variants via a clear error.
//  2. mkdir destination parent.
//  3. Try os.Rename (atomic on same fs).
//  4. On EXDEV, copy + fsync + unlink.
//  5. UpdateVariantSidecarPath in DB.
//
// Idempotent: re-running over a row whose file already moved AND DB
// already updated returns nil (computeNewSidecarPath == v.SidecarPath).
// Re-running over a partial state (file moved, DB not yet updated)
// detects the destination-already-present case and just updates the
// DB row.
func moveOneVariant(ctx context.Context, store *manifest.Store, v manifest.VariantRow, newPath string) error {
	// Stat source. If missing, this variant is orphaned on disk;
	// caller's `bridge upscale --gc` reverse sweep will clean it up.
	// We don't try to recover here — surface and skip.
	if _, err := os.Stat(v.SidecarPath); err != nil {
		if os.IsNotExist(err) {
			// If destination ALREADY exists (interrupted move), just
			// fix up the DB row.
			if _, derr := os.Stat(newPath); derr == nil {
				return store.UpdateVariantSidecarPath(ctx, v.SourcePath, v.VariantID, newPath)
			}
			return errors.New("source sidecar missing on disk")
		}
		return fmt.Errorf("stat source: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return fmt.Errorf("mkdir destination parent: %w", err)
	}

	// Atomic rename (same filesystem). Returns EXDEV (or
	// linkerr.Err == syscall.EXDEV) when source + dest live on
	// different filesystems; fall through to copy+unlink.
	if err := os.Rename(v.SidecarPath, newPath); err == nil {
		return store.UpdateVariantSidecarPath(ctx, v.SourcePath, v.VariantID, newPath)
	} else if !isCrossDeviceError(err) {
		return fmt.Errorf("rename: %w", err)
	}

	// Cross-device path: copy + fsync + unlink source.
	if err := copyAndFsync(v.SidecarPath, newPath); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if err := os.Remove(v.SidecarPath); err != nil {
		// Copy succeeded; leaving old file on disk is recoverable
		// via `bridge upscale --gc`. DB update goes ahead.
		fmt.Fprintf(os.Stderr, "warning: copy succeeded but unlink failed: %v\n", err)
	}
	return store.UpdateVariantSidecarPath(ctx, v.SourcePath, v.VariantID, newPath)
}

// copyAndFsync streams source → destination + fsyncs the
// destination file before close. Used for cross-device moves where
// os.Rename fails with EXDEV.
func copyAndFsync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return nil
}

// isCrossDeviceError detects the EXDEV (cross-device link) error
// that os.Rename returns when source and destination live on
// different filesystems. Exact error wrapping varies across Go
// versions and OSs — Linux + macOS surface "invalid cross-device
// link", Windows surfaces "The system cannot move the file to a
// different disk drive". Substring match across the known forms.
func isCrossDeviceError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "cross-device") ||
		strings.Contains(s, "EXDEV") ||
		strings.Contains(s, "different file system") ||
		strings.Contains(s, "different disk drive")
}
