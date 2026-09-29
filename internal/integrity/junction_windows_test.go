//go:build windows

package integrity

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// makeJunction makes link a directory junction to target. `mklink /J` needs
// no privilege, where a symbolic link does, so a junction is how a Windows
// operator points a variants directory at another volume, and this runs on
// any Windows host.
func makeJunction(t *testing.T, target, link string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
}

// TestSidecarInventoryResolvesAJunctionedRoot is
// TestSidecarInventoryResolvesASymlinkedRoot for a junction, which is the
// shape that test skips on a host that cannot make a symbolic link. The
// root was resolved with filepath.EvalSymlinks, which since Go 1.23 leaves a
// junction as it is, so the walk started AT the junction, saw one entry
// that is not a directory, and inventoried nothing (measured: 0 files of
// 2). Resolved through the junction, the walk descends, lists each file
// under the configured spelling the known set keys on, and pairs it with
// the path it walked, which is under the junction's TARGET: a sweep unlinks
// that one, so a junction repointed between the walk and the unlinks cannot
// send them into a tree the walk never counted.
func TestSidecarInventoryResolvesAJunctionedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	paths := seedTree(t, real,
		"Artist/Album/01.flac.upscaled-v2-176400-24.flac",
		"Artist/Album/02.flac.upscaled-v2-176400-24.flac",
	)
	link := filepath.Join(base, "variants")
	makeJunction(t, real, link)
	canonReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	known := knownOf(filepath.Join(link, "Artist", "Album", filepath.Base(paths[0])))
	inv, err := TakeSidecarInventory(context.Background(), link, known, SidecarInventoryOptions{})
	if err != nil {
		t.Fatalf("TakeSidecarInventory through a junction: %v", err)
	}
	if inv.Files != 2 || inv.Known != 1 || inv.Orphans != 1 {
		t.Fatalf("Files/Known/Orphans = %d/%d/%d, want 2/1/1: the walk did not descend through the junction",
			inv.Files, inv.Known, inv.Orphans)
	}
	want := filepath.Join(link, "Artist", "Album", filepath.Base(paths[1]))
	if len(inv.OrphanPaths) != 1 || inv.OrphanPaths[0] != want {
		t.Fatalf("OrphanPaths = %v, want [%s] in the configured spelling", inv.OrphanPaths, want)
	}
	walked := filepath.Join(canonReal, "Artist", "Album", filepath.Base(paths[1]))
	if len(inv.OrphanWalkedPaths) != 1 || inv.OrphanWalkedPaths[0] != walked {
		t.Fatalf("OrphanWalkedPaths = %v, want [%s], under the junction's target", inv.OrphanWalkedPaths, walked)
	}
	if strings.HasPrefix(strings.ToLower(inv.OrphanWalkedPaths[0]), strings.ToLower(link)+`\`) {
		t.Fatalf("the path a sweep would unlink goes through the junction: %s", inv.OrphanWalkedPaths[0])
	}
}

// TestTreeHoldsVariantSidecarsThroughAJunction: the reverse guard's evidence
// that a tree still holds sidecars (MassDeleteRefusal reads it) came back
// false for a junction'd variants directory full of them, which is the
// reading that lets a mass reap through.
func TestTreeHoldsVariantSidecarsThroughAJunction(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	seedTree(t, real, "Artist/Album/01.flac.upscaled-v2-176400-24.flac")
	link := filepath.Join(base, "variants")
	makeJunction(t, real, link)

	holds, err := TreeHoldsVariantSidecars(link)
	if err != nil {
		t.Fatalf("TreeHoldsVariantSidecars through a junction: %v", err)
	}
	if !holds {
		t.Fatal("a junction'd variants directory holding a sidecar reads as holding none")
	}
}
