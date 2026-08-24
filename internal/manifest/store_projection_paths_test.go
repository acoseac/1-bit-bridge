package manifest

import (
	"context"
	"encoding/json"
	"testing"
)

// seedSharedDirFixture stages the shape that motivates
// TrackProjectionsForPaths: TWO albums whose tracks sit flat in ONE
// directory. It is not hypothetical — on the reference library
// `2go/Music/Peter Gabriel/Hi-Res Masters/` holds 18 albums this way,
// and 69 of 880 albums share a directory with a neighbour.
//
// The album an action targets is `so/` (two tracks); `us/` is the
// neighbour that must never be touched by it.
func seedSharedDirFixture(t *testing.T, s *Store) (target, neighbour []string) {
	t.Helper()
	tags, err := json.Marshal(map[string]any{
		"sampleRate": 44100.0, "bitsPerSample": 16, "isDSD": false, "codec": "FLAC",
	})
	if err != nil {
		t.Fatalf("marshal tags: %v", err)
	}
	target = []string{
		"Music/Peter Gabriel/Hi-Res Masters/So - 01 Red Rain.flac",
		"Music/Peter Gabriel/Hi-Res Masters/So - 02 Sledgehammer.flac",
	}
	neighbour = []string{
		"Music/Peter Gabriel/Hi-Res Masters/Us - 01 Come Talk To Me.flac",
		"Music/Peter Gabriel/Hi-Res Masters/Us - 02 Love To Be Loved.flac",
	}
	for _, p := range append(append([]string{}, target...), neighbour...) {
		if _, err := s.db.Exec(`
			INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
			VALUES (?,?,?,?,?)`, p, int64(1000), int64(1), tags, int64(1)); err != nil {
			t.Fatalf("insert %q: %v", p, err)
		}
	}
	return target, neighbour
}

func projectionPaths(in []TrackProjection) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.Path)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTrackProjectionsForPathsExcludesDirectoryNeighbours is the
// headline pin for the whole identity-scope feature.
//
// The prefix form is shown failing in the same test, on purpose: it is
// not a bug in ListTrackProjectionsUnderPrefix (a subtree scope is
// exactly what it promises), it is the demonstration that a subtree
// CANNOT express "this album". Without the second assertion a reader
// has to take the motivation on trust.
func TestTrackProjectionsForPathsExcludesDirectoryNeighbours(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	target, neighbour := seedSharedDirFixture(t, s)

	got, err := s.TrackProjectionsForPaths(context.Background(), target, VariantKindPrefixOptimized)
	if err != nil {
		t.Fatalf("TrackProjectionsForPaths: %v", err)
	}
	if want := target; !equalStrings(projectionPaths(got), want) {
		t.Errorf("identity scope projected %v, want exactly %v", projectionPaths(got), want)
	}

	// The same album addressed as its directory sweeps the neighbour in.
	dir := "Music/Peter Gabriel/Hi-Res Masters"
	viaPrefix, err := s.ListTrackProjectionsUnderPrefix(context.Background(), dir, VariantKindPrefixOptimized)
	if err != nil {
		t.Fatalf("ListTrackProjectionsUnderPrefix: %v", err)
	}
	if len(viaPrefix) != len(target)+len(neighbour) {
		t.Fatalf("prefix scope projected %d rows, want %d — fixture no longer models the shared-directory case",
			len(viaPrefix), len(target)+len(neighbour))
	}
}

// TestTrackProjectionsForPathsFindsASingleTrack pins the other half of
// why the prefix form cannot serve identity scopes: subtreeLikePattern
// builds `<base>/%`, which matches strict DESCENDANTS, so a file path
// projects nothing at all. That is why the Inspector's per-track
// "Generate variants" menu item never had anything to enqueue.
func TestTrackProjectionsForPathsFindsASingleTrack(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	target, _ := seedSharedDirFixture(t, s)
	one := target[:1]

	got, err := s.TrackProjectionsForPaths(context.Background(), one, VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("TrackProjectionsForPaths: %v", err)
	}
	if !equalStrings(projectionPaths(got), one) {
		t.Errorf("single-track scope projected %v, want %v", projectionPaths(got), one)
	}

	viaPrefix, err := s.ListTrackProjectionsUnderPrefix(context.Background(), one[0], VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("ListTrackProjectionsUnderPrefix: %v", err)
	}
	if len(viaPrefix) != 0 {
		t.Fatalf("prefix scope on a file path projected %d rows, want 0 — the `<base>/%%` "+
			"pattern this test documents has changed", len(viaPrefix))
	}
}

// TestTrackProjectionsForPathsBindingOrder is the twin of
// TestListTrackProjectionsUnderPrefix_BindingOrder. The shared SELECT
// block's two placeholders (variant LIKE, suppression cutoff) sit
// inside the SELECT list and so bind BEFORE this query's own `?` — the
// JSON path array. Swapping them makes SQLite compare track paths
// against `upscaled-%` and return nothing, silently.
func TestTrackProjectionsForPathsBindingOrder(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	target, _ := seedSharedDirFixture(t, s)

	got, err := s.TrackProjectionsForPaths(context.Background(), target, VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("TrackProjectionsForPaths: %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("zero rows — binding order regressed (variant LIKE and the path array swapped?)")
	}
}

// TestTrackProjectionsForPathsKindScopedHasVariant mirrors the prefix
// form's kind-scoping pin: a track carrying ONLY an upscale variant
// must read as uncovered under an optimize projection, or the optimize
// submit skips it as "already done".
func TestTrackProjectionsForPathsKindScopedHasVariant(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	target, _ := seedSharedDirFixture(t, s)

	if err := s.UpsertVariant(context.Background(), VariantRow{
		SourcePath: target[0], VariantID: "upscaled-v2-176400-24",
		SidecarPath: "x.flac", Format: "FLAC", SampleRate: 176400,
		BitsPerSample: 24, SizeBytes: 10, SourceMTimeNS: 1, SourceSize: 1000,
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	for _, tc := range []struct {
		kind string
		want bool
	}{
		{VariantKindPrefixUpscaled, true},
		{VariantKindPrefixOptimized, false},
	} {
		got, err := s.TrackProjectionsForPaths(context.Background(), target[:1], tc.kind)
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: %d rows, want 1", tc.kind, len(got))
		}
		if got[0].HasVariant != tc.want {
			t.Errorf("%s: HasVariant = %v, want %v", tc.kind, got[0].HasVariant, tc.want)
		}
	}
}

// TestTrackProjectionsForPathsIsOrderedAndDoesNotMutateInput covers the
// two contracts a caller depends on: results come back in path order
// regardless of the order (or chunking) of the request, and the
// caller's slice is untouched — the usual caller hands over
// librarycat.Album.TrackPaths straight out of an immutable catalog
// snapshot that other readers still hold.
func TestTrackProjectionsForPathsIsOrderedAndDoesNotMutateInput(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	target, neighbour := seedSharedDirFixture(t, s)

	// Reverse order, and interleave the two albums, so a
	// preserve-input-order implementation would fail the sort check.
	input := []string{neighbour[1], target[1], neighbour[0], target[0]}
	before := append([]string(nil), input...)

	got, err := s.TrackProjectionsForPaths(context.Background(), input, VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("TrackProjectionsForPaths: %v", err)
	}
	want := []string{target[0], target[1], neighbour[0], neighbour[1]}
	if !equalStrings(projectionPaths(got), want) {
		t.Errorf("order = %v, want %v (sorted by path)", projectionPaths(got), want)
	}
	if !equalStrings(input, before) {
		t.Errorf("input slice was mutated: %v, want %v", input, before)
	}
}

// TestTrackProjectionsForPathsEmptyAndMissing: an empty set is a valid
// empty scope, not an error, and unknown paths are dropped rather than
// faulting — a track deleted between the catalog snapshot and the
// submit is the ordinary case, not a client bug.
func TestTrackProjectionsForPathsEmptyAndMissing(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	target, _ := seedSharedDirFixture(t, s)

	empty, err := s.TrackProjectionsForPaths(context.Background(), nil, VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("empty scope: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("empty scope returned %d rows, want 0", len(empty))
	}

	mixed := []string{target[0], "Music/Gone/Deleted.flac"}
	got, err := s.TrackProjectionsForPaths(context.Background(), mixed, VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("mixed scope: %v", err)
	}
	if !equalStrings(projectionPaths(got), target[:1]) {
		t.Errorf("mixed scope = %v, want %v", projectionPaths(got), target[:1])
	}
}

// TestTrackProjectionsForPathsSpansChunks proves the chunking loop
// stitches results together and keeps them globally ordered. Seeds more
// paths than trackProjectionChunk so at least two statements run.
func TestTrackProjectionsForPathsSpansChunks(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	tags, err := json.Marshal(map[string]any{
		"sampleRate": 44100.0, "bitsPerSample": 16, "isDSD": false, "codec": "FLAC",
	})
	if err != nil {
		t.Fatalf("marshal tags: %v", err)
	}
	const n = trackProjectionChunk + 37
	paths := make([]string, 0, n)
	for i := 0; i < n; i++ {
		// Zero-padded so lexical order matches numeric order.
		p := "Big/Album/" + pad4(i) + ".flac"
		paths = append(paths, p)
		if _, err := s.db.Exec(`
			INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
			VALUES (?,?,?,?,?)`, p, int64(10), int64(1), tags, int64(1)); err != nil {
			t.Fatalf("insert %q: %v", p, err)
		}
	}
	// Hand them over reversed: a chunking bug that emitted per-chunk
	// results in request order would surface here.
	reversed := make([]string, n)
	for i, p := range paths {
		reversed[n-1-i] = p
	}
	got, err := s.TrackProjectionsForPaths(context.Background(), reversed, VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("TrackProjectionsForPaths: %v", err)
	}
	if !equalStrings(projectionPaths(got), paths) {
		t.Fatalf("chunked result is not globally path-ordered (got %d rows)", len(got))
	}
}

func pad4(i int) string {
	digits := []byte{'0', '0', '0', '0'}
	for pos := 3; pos >= 0 && i > 0; pos-- {
		digits[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(digits)
}
