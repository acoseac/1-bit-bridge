package integrity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVariantsDirSweepBlockReason pins the shared probe's contract
// (2026-07-21 review H4 + M15, backlog B223): missing, empty and
// non-directory variants dirs block a deletion sweep with an
// actionable reason, and so does a directory that holds no RENDITION,
// whatever else is in it; a dir holding one (or a link to a directory,
// which the probe cannot see behind) is healthy and returns "".
//
// Until B223 the table said "a dir holding at least one entry (file or
// subdir) is healthy", and pinned exactly that with a file named
// "sidecar.flac" and a lone subdirectory: the local directory an
// unmount leaves under a mountpoint, holding what was written there
// while the volume was away, which the watcher then read as healthy and
// deleted every row over (40 of 40).
//
// It pins the Empty half of the typed answer in the same table,
// because that flag is what `upscale --gc`'s reverse guard and the
// variant delete handler act on, and "missing" or "unreadable" must
// never set it: those are different facts, and a sweep that emptied the
// directory itself may proceed past one and not the other.
func TestVariantsDirSweepBlockReason(t *testing.T) {
	cases := []struct {
		name string
		// setup returns the dir path to probe.
		setup func(t *testing.T) string
		// wantReason is the substring the block reason must
		// contain; "" means healthy (no block).
		wantReason string
		// wantEmpty is the typed answer's Empty flag: true for the
		// exists-but-holds-nothing case alone.
		wantEmpty bool
	}{
		{
			name: "missing dir blocks",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "never-created")
			},
			wantReason: "missing",
		},
		{
			name: "empty dir blocks",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			wantReason: "empty",
			wantEmpty:  true,
		},
		{
			name: "regular file blocks",
			setup: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "not-a-dir")
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				return p
			},
			wantReason: "not a directory",
		},
		{
			name: "a rendition one directory deep is healthy",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeSidecar(t, filepath.Join(dir, "Artist", "Album", "01.flac.upscaled-v2-176400-24.flac"))
				return dir
			},
			wantReason: "",
		},
		{
			name: "a legacy hash-flat rendition is healthy",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeSidecar(t, filepath.Join(dir, "abc123-optimized-v1-44100-16.flac"))
				return dir
			},
			wantReason: "",
		},
		{
			name: "a file that is not a rendition holds no rendition",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeSidecar(t, filepath.Join(dir, "sidecar.flac"))
				return dir
			},
			wantReason: "holds no rendition",
			wantEmpty:  true,
		},
		{
			name: "a lone subdirectory holds no rendition",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				mkdirAllUnder(t, dir, filepath.Join("Artist", "Album"))
				return dir
			},
			wantReason: "holds no rendition",
			wantEmpty:  true,
		},
		{
			name: "a .DS_Store holds no rendition",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeSidecar(t, filepath.Join(dir, ".DS_Store"))
				return dir
			},
			wantReason: "holds no rendition",
			wantEmpty:  true,
		},
		{
			name: "renditions only in a dot-directory hold no rendition",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeSidecar(t, filepath.Join(dir, ".Trashes", "501", "01.flac.upscaled-v2-176400-24.flac"))
				return dir
			},
			wantReason: "holds no rendition",
			wantEmpty:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			block := VariantsDirSweepBlock(dir)
			if block.Empty != tc.wantEmpty {
				t.Errorf("Empty = %v, want %v (reason %q)", block.Empty, tc.wantEmpty, block.Reason)
			}
			// Info is the directory whose entries the probe read (healthy
			// or empty), which the watcher and the delete handler compare
			// with later (backlog B203), and only that.
			if want := block.Reason == "" || block.Empty; (block.Info != nil) != want {
				t.Errorf("Info set = %v with reason %q, want it set only when the probe read the directory's entries", block.Info != nil, block.Reason)
			}
			if block.Info != nil {
				if now, err := os.Stat(dir); err != nil || !os.SameFile(block.Info, now) {
					t.Errorf("Info does not name the probed directory (stat err %v)", err)
				}
			}
			// The one-line form is a wrapper, and must stay one:
			// two probes that can answer differently is the drift
			// the delegation exists to prevent.
			reason := VariantsDirSweepBlockReason(dir)
			if reason != block.Reason {
				t.Errorf("VariantsDirSweepBlockReason = %q, VariantsDirSweepBlock().Reason = %q", reason, block.Reason)
			}
			if tc.wantReason == "" {
				if reason != "" {
					t.Errorf("healthy dir blocked: %q", reason)
				}
				return
			}
			if reason == "" {
				t.Fatalf("expected a block reason containing %q, got healthy", tc.wantReason)
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("block reason %q does not contain %q", reason, tc.wantReason)
			}
		})
	}
}

// TestOrphanIsRenditionReadsTheName — `upscale --gc`'s reverse guard
// explains a directory that holds no rendition after its forward sweep
// only by the renditions that sweep unlinked (backlog B223), and asks
// OrphanIsRendition of each: with every orphan counted, a run that
// unlinked a .DS_Store from under an unmounted mountpoint read the
// emptiness as its own work.
func TestOrphanIsRenditionReadsTheName(t *testing.T) {
	inv := SidecarInventory{OrphanPaths: []string{
		filepath.Join("v", ".DS_Store"),
		filepath.Join("v", "Artist", "Album", "01.flac"),
		filepath.Join("v", "Artist", "Album", "01.flac.upscaled-v2-176400-24.flac"),
		filepath.Join("v", "abc123-optimized-v1-44100-16.flac"),
	}}
	want := []bool{false, false, true, true}
	for i, w := range want {
		if got := inv.OrphanIsRendition(i); got != w {
			t.Errorf("OrphanIsRendition(%d) (%s) = %v, want %v", i, inv.OrphanPaths[i], got, w)
		}
	}
}

// TestTheVariantsDirProbeReadsLinksAndDirectoriesItCannotList pins the
// probe's rules for what it cannot look into (backlog B223). A link to a
// directory may lead to renditions the scan does not follow, so it keeps
// the directory healthy, as any entry did before; a dot-named one is pruned
// like a dot-directory. A link to a rendition counts, a dangling one does
// not. The filesystem's lost+found, which this user cannot list, is no
// evidence either way (IsFilesystemLostFound), so a fresh volume holding
// only that holds no rendition. Any other directory it cannot list is a
// refusal of its own, never Empty, unless a rendition turned up elsewhere:
// the scan goes on past it, whichever way the names sort.
func TestTheVariantsDirProbeReadsLinksAndDirectoriesItCannotList(t *testing.T) {
	symlink := func(t *testing.T, target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink: %v", err)
		}
	}
	requireBlock := func(t *testing.T, dir, wantReason string, wantEmpty bool) {
		t.Helper()
		block := VariantsDirSweepBlock(dir)
		if block.Empty != wantEmpty {
			t.Errorf("Empty = %v, want %v (reason %q)", block.Empty, wantEmpty, block.Reason)
		}
		switch {
		case wantReason == "" && block.Reason != "":
			t.Errorf("healthy directory blocked: %q", block.Reason)
		case wantReason != "" && !strings.Contains(block.Reason, wantReason):
			t.Errorf("reason %q does not contain %q", block.Reason, wantReason)
		}
	}
	t.Run("a link to a directory keeps it healthy", func(t *testing.T) {
		dir := t.TempDir()
		symlink(t, t.TempDir(), filepath.Join(dir, "Artist"))
		requireBlock(t, dir, "", false)
	})
	t.Run("a dot-named link to a directory is pruned", func(t *testing.T) {
		dir := t.TempDir()
		symlink(t, t.TempDir(), filepath.Join(dir, ".Trashes"))
		requireBlock(t, dir, "holds no rendition", true)
	})
	t.Run("a link to a rendition counts", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "real.flac")
		writeSidecar(t, target)
		symlink(t, target, filepath.Join(dir, "01.flac.upscaled-v2-176400-24.flac"))
		requireBlock(t, dir, "", false)
	})
	t.Run("a dangling link named like a rendition does not", func(t *testing.T) {
		dir := t.TempDir()
		symlink(t, filepath.Join(dir, "gone.flac"), filepath.Join(dir, "01.flac.upscaled-v2-176400-24.flac"))
		requireBlock(t, dir, "holds no rendition", true)
	})
	t.Run("the filesystem's lost+found alone holds no rendition", func(t *testing.T) {
		skipWhereModesDenyNothing(t)
		dir := t.TempDir()
		lockDir(t, mkdirAllUnder(t, dir, "lost+found"))
		requireBlock(t, dir, "holds no rendition", true)
	})
	t.Run("another directory it cannot list is a refusal, not Empty", func(t *testing.T) {
		skipWhereModesDenyNothing(t)
		dir := t.TempDir()
		lockDir(t, mkdirAllUnder(t, dir, "Locked"))
		requireBlock(t, dir, "cannot read", false)
	})
	for _, locked := range []string{"AAA-first", "zzz-last"} {
		t.Run("a rendition beside a directory it cannot list, "+locked, func(t *testing.T) {
			skipWhereModesDenyNothing(t)
			dir := t.TempDir()
			writeSidecar(t, filepath.Join(dir, "Mozart", "Requiem", "01.flac.upscaled-v2-176400-24.flac"))
			lockDir(t, mkdirAllUnder(t, dir, locked))
			requireBlock(t, dir, "", false)
			if holds, err := TreeHoldsVariantSidecars(dir); err != nil || !holds {
				t.Errorf("TreeHoldsVariantSidecars = %v, %v; want true, nil", holds, err)
			}
		})
	}
}
