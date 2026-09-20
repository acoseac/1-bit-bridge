package integrity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// seedTree writes each relative path under root with one byte of content.
func seedTree(t *testing.T, root string, rels ...string) []string {
	t.Helper()
	out := make([]string, len(rels))
	for i, rel := range rels {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		out[i] = p
	}
	return out
}

func knownOf(paths ...string) map[string]struct{} {
	k := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		k[strings.ToLower(filepath.Clean(p))] = struct{}{}
	}
	return k
}

// TestTakeSidecarInventoryPartitionsTheTree — the base contract: Known +
// Orphans == Files, the orphan PATHS are the ones a sweep would unlink,
// and a dot-directory is never ours to look inside (a `.Trashes/` full of
// sidecars is files an operator put in the Trash to get back).
func TestTakeSidecarInventoryPartitionsTheTree(t *testing.T) {
	root := t.TempDir()
	paths := seedTree(t, root,
		"Artist/Album/01.flac.upscaled-v2-176400-24.flac",
		"Artist/Album/02.flac.upscaled-v2-176400-24.flac",
		"Artist/Album/03.flac.upscaled-v2-176400-24.flac",
		".Trashes/501/deleted.flac.upscaled-v2-176400-24.flac",
	)
	inv, err := TakeSidecarInventory(context.Background(), root, knownOf(paths[0]), SidecarInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Files != 3 || inv.Known != 1 || inv.Orphans != 2 {
		t.Fatalf("files=%d known=%d orphans=%d, want 3/1/2 (the dot-directory's file is not classified at all)",
			inv.Files, inv.Known, inv.Orphans)
	}
	if inv.Known+inv.Orphans != inv.Files {
		t.Errorf("Known+Orphans=%d does not partition Files=%d", inv.Known+inv.Orphans, inv.Files)
	}
	got := map[string]bool{}
	for _, p := range inv.OrphanPaths {
		got[p] = true
	}
	if !got[paths[1]] || !got[paths[2]] || len(got) != 2 {
		t.Errorf("OrphanPaths = %v, want exactly the two unreferenced sidecars", inv.OrphanPaths)
	}
	if got[paths[3]] {
		t.Error("a file under a dot-directory is in the deletion list")
	}
	if inv.Truncated || inv.Unreadable != 0 {
		t.Errorf("truncated=%v unreadable=%d on a clean tree", inv.Truncated, inv.Unreadable)
	}
}

// TestTakeSidecarInventoryKeepsScratchOutOfTheRatio — a half-written
// `.tmp` is the sweep's own litter, never the operator's data. Counting it
// as an orphan would let a crashed run trip the mass-orphan guard on the
// next one, which is the sweep refusing to clean up after itself.
func TestTakeSidecarInventoryKeepsScratchOutOfTheRatio(t *testing.T) {
	root := t.TempDir()
	seedTree(t, root, "a.waveform.bin", "b.waveform.bin.tmp", "c.waveform.bin.tmp")
	// Consider is nil — accept every file, the shape `upscale --gc` uses
	// — so Scratch is the ONLY thing keeping the two .tmp files out of
	// the ratio. With a Consider that excludes them anyway (what
	// `analyze --gc` passes) this assertion would hold with Scratch
	// deleted, and prove nothing.
	inv, err := TakeSidecarInventory(context.Background(), root, nil, SidecarInventoryOptions{
		Scratch: func(n string) bool { return strings.HasSuffix(n, ".waveform.bin.tmp") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Files != 1 || inv.Orphans != 1 {
		t.Errorf("files=%d orphans=%d, want 1/1 — the two .tmp files are not candidates", inv.Files, inv.Orphans)
	}
	if len(inv.ScratchPaths) != 2 {
		t.Errorf("ScratchPaths=%v, want both .tmp files (their caller removes them unconditionally)", inv.ScratchPaths)
	}
	for _, p := range inv.OrphanPaths {
		if strings.HasSuffix(p, ".tmp") {
			t.Errorf("a scratch file is in the deletion RATIO as an orphan: %s", p)
		}
	}
}

// TestTakeSidecarInventoryBudgetScopesTheAnswer — a reporting caller may
// cap the walk; the cap must be visible in the result, because an answer
// from part of a tree presented as an answer about the tree is the
// confident-wrong-answer shape.
//
// The budget counts TRAVERSED entries, so Files lands BELOW it by however
// many directories the walk crossed. That is the point: the cap is a
// wall-clock bound and a directory costs the same as a file.
func TestTakeSidecarInventoryBudgetScopesTheAnswer(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 30; i++ {
		seedTree(t, root, fmt.Sprintf("d/%02d.flac", i))
	}
	inv, err := TakeSidecarInventory(context.Background(), root, nil, SidecarInventoryOptions{
		MaxEntries: 10, MaxOrphanPaths: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !inv.Truncated {
		t.Error("a walk stopped at its budget must say so")
	}
	// Ten entries: the root, the `d` directory, and eight files.
	if inv.Files != 8 {
		t.Errorf("files=%d, want 8 — the budget of 10 also paid for the root and `d`", inv.Files)
	}
	if len(inv.OrphanPaths) != 3 {
		t.Errorf("OrphanPaths=%d, want the MaxOrphanPaths cap of 3 while Orphans counts %d", len(inv.OrphanPaths), inv.Orphans)
	}
	if inv.Orphans != inv.Files {
		t.Errorf("orphans=%d files=%d: every classified file here is unreferenced", inv.Orphans, inv.Files)
	}
}

// TestTakeSidecarInventoryBudgetCountsWhatItDidNotClassify is the finding
// itself. Gated on inv.Files, the cap bounded only the files Consider
// accepted — so a tree of directories, of ignored files, or of scratch
// files walked without limit under a probe documented as bounded, and the
// guarantee held only because both of today's callers happen to pass a nil
// Consider. `/api/doctor` runs on a settings-page render; an unbounded
// walk there is the cost the budget exists to cap. (CodeRabbit on #940.)
func TestTakeSidecarInventoryBudgetCountsWhatItDidNotClassify(t *testing.T) {
	for _, c := range []struct {
		name string
		seed func(root string)
		opts SidecarInventoryOptions
	}{
		{
			name: "files Consider rejects",
			seed: func(root string) {
				for i := 0; i < 200; i++ {
					seedTree(t, root, fmt.Sprintf("notes/%03d.txt", i))
				}
			},
			opts: SidecarInventoryOptions{
				Consider:   func(n string) bool { return strings.HasSuffix(n, ".flac") },
				MaxEntries: 20,
			},
		},
		{
			name: "scratch files",
			seed: func(root string) {
				for i := 0; i < 200; i++ {
					seedTree(t, root, fmt.Sprintf("tmp/%03d.waveform.bin.tmp", i))
				}
			},
			opts: SidecarInventoryOptions{
				Consider:   func(n string) bool { return strings.HasSuffix(n, ".waveform.bin") },
				Scratch:    func(n string) bool { return strings.HasSuffix(n, ".tmp") },
				MaxEntries: 20,
			},
		},
		{
			name: "directories",
			seed: func(root string) {
				for i := 0; i < 200; i++ {
					if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("a%03d", i)), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			},
			opts: SidecarInventoryOptions{MaxEntries: 20},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			c.seed(root)
			inv, err := TakeSidecarInventory(context.Background(), root, nil, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if !inv.Truncated {
				t.Fatalf("the walk crossed 200 entries under a budget of %d without truncating: %+v",
					c.opts.MaxEntries, inv)
			}
			// ...and the budget must not have been spent on classification
			// it never did: Files stays the count of classified files.
			if inv.Files > c.opts.MaxEntries {
				t.Errorf("files=%d exceeds the budget", inv.Files)
			}
			if len(inv.ScratchPaths) > c.opts.MaxEntries {
				t.Errorf("scratch=%d exceeds the budget", len(inv.ScratchPaths))
			}
		})
	}
}

// TestTakeSidecarInventoryRefusesAnEmptyRoot — WalkDir("") walks the
// process working directory. An inventory of "" must never be an inventory
// of the cwd, which would then be handed to something that unlinks.
func TestTakeSidecarInventoryRefusesAnEmptyRoot(t *testing.T) {
	if _, err := TakeSidecarInventory(context.Background(), "", nil, SidecarInventoryOptions{}); err == nil {
		t.Fatal("an empty root was accepted")
	}
}

// TestTakeSidecarInventoryTreatsAMissingRootAsNothingToDo — a bridge that
// never transcoded anything has no directory, and both sweeps have always
// read that as a clean no-op rather than an error.
func TestTakeSidecarInventoryTreatsAMissingRootAsNothingToDo(t *testing.T) {
	inv, err := TakeSidecarInventory(context.Background(), filepath.Join(t.TempDir(), "never-made"), nil, SidecarInventoryOptions{})
	if err != nil {
		t.Fatalf("a missing root is an error: %v", err)
	}
	if inv.Files != 0 || inv.Orphans != 0 {
		t.Errorf("a missing root produced %+v", inv)
	}
}

// TestTakeSidecarInventoryCountsADirectoryItCannotRead — an unreadable
// subtree can only make the deletion set SMALLER (the known set comes from
// the database, not the walk), so it is reported rather than refused. But
// a report built from part of a tree has to say so.
func TestTakeSidecarInventoryCountsADirectoryItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0 does not deny directory reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory anyway")
	}
	root := t.TempDir()
	seedTree(t, root, "ok/a.flac", "locked/b.flac")
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	inv, err := TakeSidecarInventory(context.Background(), root, nil, SidecarInventoryOptions{})
	if err != nil {
		t.Fatalf("an unreadable subdirectory aborted the walk: %v", err)
	}
	if inv.Unreadable != 1 {
		t.Errorf("unreadable=%d, want 1", inv.Unreadable)
	}
	if inv.Files != 1 {
		t.Errorf("files=%d, want only the readable one — the locked subtree is absent from every count", inv.Files)
	}
	for _, p := range inv.OrphanPaths {
		if strings.Contains(p, "locked") {
			t.Errorf("a file the walk could not see is in the deletion list: %s", p)
		}
	}
}

// TestTakeSidecarInventoryHonoursCancellation — the `--gc` walks map a
// cancelled context onto "interrupted", not onto a failed sweep.
func TestTakeSidecarInventoryHonoursCancellation(t *testing.T) {
	root := t.TempDir()
	seedTree(t, root, "a.flac", "b.flac")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := TakeSidecarInventory(ctx, root, nil, SidecarInventoryOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

// TestMassOrphanRefusalDecidesOnTheTreeNotTheCatalog is the guard's truth
// table. Each row names the shape it stands for; the field-report row is
// the one the whole thing exists for.
func TestMassOrphanRefusalDecidesOnTheTreeNotTheCatalog(t *testing.T) {
	cases := []struct {
		name                         string
		orphans, files, rows, maxPct int
		refuse                       bool
	}{
		{"the 2026-09-20 aftermath: 200 fresh rows over a stranded tree", 10048, 10248, 200, 20, true},
		{"the field report at its worst: nothing left of the catalog", 10248, 10248, 0, 20, true},
		{"a naming-scheme change leaves one old file per current row", 5000, 10000, 5000, 20, false},
		{"an interrupted bulk delete, half the library gone", 4000, 9000, 5000, 20, false},
		{"one more orphan than rows, but under the floor", 9, 12, 3, 20, false},
		{"at the floor, and more orphans than rows", 10, 12, 3, 20, true},
		{"more orphans than rows but a small share of the tree", 11, 1000, 10, 20, false},
		{"maxPct 100 disables the guard, as it does the reverse one", 10048, 10248, 200, 100, false},
		{"maxPct 0 refuses any mass orphan removal", 10, 10000, 3, 0, true},
		{"an empty tree decides nothing", 0, 0, 0, 20, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason := MassOrphanRefusal(c.orphans, c.files, c.rows, c.maxPct)
			if (reason != "") != c.refuse {
				t.Fatalf("MassOrphanRefusal(%d,%d,%d,%d) = %q, want refuse=%v",
					c.orphans, c.files, c.rows, c.maxPct, reason, c.refuse)
			}
			if !c.refuse {
				return
			}
			// The reason is what the caller's refusal is built from, so it
			// has to carry the numbers rather than just say no.
			for _, want := range []string{"referenced by no row", "row(s) the catalog holds"} {
				if !strings.Contains(reason, want) {
					t.Errorf("reason %q does not say %q", reason, want)
				}
			}
		})
	}
}

// TestMassOrphanRefusalAsksADifferentQuestionFromItsReverseTwin — the two
// guards must not be collapsed. On the incident's numbers the reverse
// guard is silent (nothing is missing; the catalog's few rows all have
// their files) and the forward one is the only thing between `--gc` and
// 254 GiB.
func TestMassOrphanRefusalAsksADifferentQuestionFromItsReverseTwin(t *testing.T) {
	dir := t.TempDir()
	seedTree(t, dir, "Artist/Album/01.flac.upscaled-v2-176400-24.flac")
	// 200 rows, none missing — MassDeleteRefusal is asked about zero.
	if reason := MassDeleteRefusal(dir, 0, 200, 20); reason != "" {
		t.Fatalf("the reverse guard fired on a healthy catalog: %q", reason)
	}
	if reason := MassOrphanRefusal(10048, 10248, 200, 20); reason == "" {
		t.Fatal("the forward guard is silent on the exact shape it exists for")
	}
}

// TestMassOrphanRefusalIsNotGatedOnTheTreeProbe records a deliberate
// asymmetry: the reverse guard consults TreeHoldsVariantSidecars to tell a
// relocation from a real deletion, and the forward one must not, because
// here the files ARE the evidence — counted, in hand, about to be
// unlinked. A tree of files that do not look like sidecars still refuses.
func TestMassOrphanRefusalIsNotGatedOnTheTreeProbe(t *testing.T) {
	if reason := MassOrphanRefusal(500, 500, 3, 20); reason == "" {
		t.Fatal("the guard consulted something other than its own counts")
	}
	// And the shared floor really is shared, so the two guards cannot
	// drift on what the smallest "mass" is.
	if massOrphanFloor != massDeleteFloor {
		t.Errorf("massOrphanFloor=%d massDeleteFloor=%d: one number, two guards", massOrphanFloor, massDeleteFloor)
	}
}

// TestSidecarInventoryUsesTheSameKnownSetAsTheSweeps — the inventory is
// only safe because a relocated catalog's canonical spellings are in the
// set it is handed. Built with KnownSidecarSet (what `--gc` and the
// background sweeper both use), a moved tree reads as fully referenced.
func TestSidecarInventoryUsesTheSameKnownSetAsTheSweeps(t *testing.T) {
	newDir := t.TempDir()
	oldDir := filepath.Join(t.TempDir(), "mnt", "bridge-variants")
	rows := make([]VariantSnapshot, 0, 4)
	for _, src := range []string{"A/Al/01.flac", "A/Al/02.flac", "B/Bl/01.flac", "B/Bl/02.flac"} {
		const id = "upscaled-v2-176400-24"
		canonical := CanonicalSidecarPath(newDir, VariantSnapshot{SourcePath: src, VariantID: id})
		seedTree(t, newDir, mustRel(t, newDir, canonical))
		rows = append(rows, VariantSnapshot{
			SourcePath:  src,
			VariantID:   id,
			SidecarPath: CanonicalSidecarPath(oldDir, VariantSnapshot{SourcePath: src, VariantID: id}),
		})
	}
	inv, err := TakeSidecarInventory(context.Background(), newDir, KnownSidecarSet(newDir, rows), SidecarInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Orphans != 0 {
		t.Fatalf("a relocated catalog read as %d orphan(s): %v", inv.Orphans, inv.OrphanPaths)
	}
	// Control on the control: with the RECORDED spellings only, every
	// file of the moved tree is an orphan — the 2026-09-20 shape.
	recordedOnly := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		recordedOnly[strings.ToLower(filepath.Clean(r.SidecarPath))] = struct{}{}
	}
	inv, err = TakeSidecarInventory(context.Background(), newDir, recordedOnly, SidecarInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Orphans != 4 {
		t.Fatalf("recorded-only known set found %d orphans, want all 4 — the fixture does not reproduce the hazard", inv.Orphans)
	}
}

func mustRel(t *testing.T, base, p string) string {
	t.Helper()
	rel, err := filepath.Rel(base, p)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}
