package api

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// newPathsDeleteFixture wires a server over two albums whose tracks
// share ONE directory — the shape that makes a prefix delete unsafe as
// an album action.
func newPathsDeleteFixture(t *testing.T) (*Server, *stubVariantDeleter, []string, []string) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	authStore, err := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	target := []string{"Shared/So - 01.flac", "Shared/So - 02.flac"}
	neighbour := []string{"Shared/Us - 01.flac"}

	byPath := map[string][]VariantSummary{}
	var all []VariantSummary
	for _, p := range append(append([]string{}, target...), neighbour...) {
		row := VariantSummary{
			SourcePath: p, VariantID: "optimized-v2-44100-16",
			SidecarPath: filepath.Join(tmp, strings.ReplaceAll(p, "/", "_")),
			SizeBytes:   100,
		}
		byPath[p] = []VariantSummary{row}
		all = append(all, row)
	}
	deleter := &stubVariantDeleter{all: all, byPath: byPath}
	srv := New(cfg, authStore, nil, "fp").WithVariantDeleter(deleter)
	return srv, deleter, target, neighbour
}

func deletedPathsOf(d *stubVariantDeleter) []string {
	keys := d.deletedKeys()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if i := strings.Index(k, "|"); i >= 0 {
			out = append(out, k[:i])
		}
	}
	sort.Strings(out)
	return out
}

// TestRunVariantDelete_PathsScopeIsExact is the shared-loop half of the
// neighbour-isolation contract. The Paths shape resolves its row set in
// phase 1 and then runs the SAME unlink / DB-delete / event path every
// other shape runs — so this asserts the scoping, and the existing
// tests continue to cover the destructive loop itself.
func TestRunVariantDelete_PathsScopeIsExact(t *testing.T) {
	srv, deleter, target, neighbour := newPathsDeleteFixture(t)

	resp, err := srv.RunVariantDelete(context.Background(), VariantDeleteRequest{Paths: target})
	if err != nil {
		t.Fatalf("RunVariantDelete: %v", err)
	}
	if resp.DeletedCount != len(target) {
		t.Errorf("DeletedCount = %d, want %d", resp.DeletedCount, len(target))
	}
	got := deletedPathsOf(deleter)
	if strings.Join(got, "|") != strings.Join(target, "|") {
		t.Fatalf("deleted %v, want exactly %v", got, target)
	}
	for _, p := range got {
		for _, n := range neighbour {
			if p == n {
				t.Errorf("neighbouring album's sidecar %q was reclaimed by an album-scoped delete", p)
			}
		}
	}
}

// TestRunVariantDelete_PrefixSweepsTheWholeDirectory is the companion:
// the same album addressed as its directory takes the neighbour with
// it. Not a defect in the prefix shape — it is why the Paths shape has
// to exist.
func TestRunVariantDelete_PrefixSweepsTheWholeDirectory(t *testing.T) {
	srv, deleter, target, neighbour := newPathsDeleteFixture(t)

	if _, err := srv.RunVariantDelete(context.Background(),
		VariantDeleteRequest{Prefix: "Shared"}); err != nil {
		t.Fatalf("RunVariantDelete: %v", err)
	}
	if want := len(target) + len(neighbour); len(deletedPathsOf(deleter)) != want {
		t.Fatalf("prefix delete removed %d, want %d — the fixture no longer models "+
			"two albums sharing a directory", len(deletedPathsOf(deleter)), want)
	}
}

// TestRunVariantDelete_ShapeGuardCountsPaths is the load-bearing one.
// The zero-value request wipes the ENTIRE variant cache, and the guard
// is what keeps a caller from reaching that by accident. Adding a
// fourth shape means the guard has to count it — a guard that still
// only knew three would let `{All: true, Paths: …}` through, and the
// switch would take the All arm.
func TestRunVariantDelete_ShapeGuardCountsPaths(t *testing.T) {
	srv, deleter, target, _ := newPathsDeleteFixture(t)

	for _, tc := range []struct {
		name string
		req  VariantDeleteRequest
	}{
		{"paths plus all", VariantDeleteRequest{All: true, Paths: target}},
		{"paths plus prefix", VariantDeleteRequest{Prefix: "Shared", Paths: target}},
		{"paths plus path", VariantDeleteRequest{Path: target[0], Paths: target}},
		{"no shape at all", VariantDeleteRequest{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(deleter.deletedKeys())
			_, err := srv.RunVariantDelete(context.Background(), tc.req)
			if err == nil {
				t.Fatal("got nil error, want a shape rejection")
			}
			if !strings.Contains(err.Error(), "exactly one of") {
				t.Errorf("error = %q, want the exactly-one-shape message", err.Error())
			}
			if after := len(deleter.deletedKeys()); after != before {
				t.Errorf("a rejected shape still deleted %d rows", after-before)
			}
		})
	}
}

// TestRunVariantDelete_PathsHonoursKindNarrowing: the kind filter runs
// after phase 1 for every shape, so an optimize-scoped delete must
// leave an upscale sidecar for the same track alone.
func TestRunVariantDelete_PathsHonoursKindNarrowing(t *testing.T) {
	srv, deleter, target, _ := newPathsDeleteFixture(t)
	// Give the first target track a second, upscale variant.
	up := VariantSummary{
		SourcePath: target[0], VariantID: "upscaled-v2-192000-24",
		SidecarPath: filepath.Join(t.TempDir(), "up.flac"), SizeBytes: 500,
	}
	deleter.byPath[target[0]] = append(deleter.byPath[target[0]], up)
	deleter.all = append(deleter.all, up)

	if _, err := srv.RunVariantDelete(context.Background(),
		VariantDeleteRequest{Paths: target, Kind: "optimize"}); err != nil {
		t.Fatalf("RunVariantDelete: %v", err)
	}
	for _, k := range deleter.deletedKeys() {
		if strings.Contains(k, "upscaled-") {
			t.Errorf("kind=optimize deleted an upscale variant: %s", k)
		}
	}
}

// TestRunVariantDelete_PathsWithNoVariantsIsANoOp: a scope whose tracks
// simply have no variants is a well-formed request whose post-condition
// already holds — not an error, and above all not a fall-through to the
// All shape.
func TestRunVariantDelete_PathsWithNoVariantsIsANoOp(t *testing.T) {
	srv, deleter, _, _ := newPathsDeleteFixture(t)

	resp, err := srv.RunVariantDelete(context.Background(),
		VariantDeleteRequest{Paths: []string{"Shared/Nothing.flac"}})
	if err != nil {
		t.Fatalf("RunVariantDelete: %v", err)
	}
	if resp.DeletedCount != 0 {
		t.Errorf("DeletedCount = %d, want 0", resp.DeletedCount)
	}
	if got := deleter.deletedKeys(); len(got) != 0 {
		t.Fatalf("a no-variant scope deleted %v", got)
	}
}

// TestRunVariantDelete_PathsDeduplicates: RunVariantDelete is exported,
// so it can be handed a path set the admin route's own dedup never saw.
// A duplicate would list the same sidecar twice and the second unlink
// would report a spurious failure for a file the first one correctly
// removed.
func TestRunVariantDelete_PathsDeduplicates(t *testing.T) {
	srv, deleter, target, _ := newPathsDeleteFixture(t)

	dupes := []string{target[0], target[0], target[1], target[0]}
	resp, err := srv.RunVariantDelete(context.Background(), VariantDeleteRequest{Paths: dupes})
	if err != nil {
		t.Fatalf("RunVariantDelete: %v", err)
	}
	if resp.DeletedCount != 2 {
		t.Errorf("DeletedCount = %d, want 2 — one per distinct path", resp.DeletedCount)
	}
	if got := deletedPathsOf(deleter); len(got) != 2 {
		t.Errorf("deleted %v, want two entries", got)
	}
}
