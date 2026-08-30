package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestArtistImageCacheControlVerifiesTheToken is the load-bearing half of
// portrait versioning.
//
// The point of the token is to serve `immutable` for a year. The danger
// of the token is serving `immutable` for a year with the WRONG bytes:
// the client's token comes from a TTL-cached directory listing, so a
// portrait replaced inside that window is requested under the previous
// token. Trusting any `v=` would freeze the old image in that viewer's
// cache until they cleared it by hand — a year-long bug reachable by a
// perfectly ordinary race.
//
// So the token is recomputed from the file being served and compared. A
// mismatch degrades to the short max-age, which is precisely the
// behaviour every portrait had before this existed: correct, just slow,
// and self-healing on the client's next catalog read.
func TestArtistImageCacheControlVerifiesTheToken(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "artist-x.jpg")
	if err := os.WriteFile(src, []byte("portrait-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	good := manifest.ArtworkFileVersion(fi.ModTime(), fi.Size())

	immutable := func(s string) bool { return strings.Contains(s, "immutable") }

	if got := artistImageCacheControl(src, good); !immutable(got) {
		t.Errorf("matching token: Cache-Control = %q, want immutable — without this "+
			"the whole feature does nothing", got)
	}
	if got := artistImageCacheControl(src, "0000000000000000"); immutable(got) {
		t.Errorf("stale token: Cache-Control = %q, must NOT be immutable — a viewer "+
			"would hold the previous portrait for a year", got)
	}
	if got := artistImageCacheControl(src, ""); immutable(got) {
		t.Errorf("no token: Cache-Control = %q, want the short max-age", got)
	}
	if got := artistImageCacheControl(filepath.Join(dir, "gone.jpg"), good); immutable(got) {
		t.Errorf("unstattable source: Cache-Control = %q, must fail closed to the "+
			"short max-age", got)
	}

	// And the token must actually move when the file does — a constant
	// would satisfy every assertion above while pinning nothing.
	later := fi.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(src, later, later); err != nil {
		t.Fatal(err)
	}
	if got := artistImageCacheControl(src, good); immutable(got) {
		t.Errorf("token after the file changed: Cache-Control = %q, must not be "+
			"immutable — this is the exact race the verification exists for", got)
	}
}

// TestArtworkFileVersionMovesWithTheFile pins the derivation itself:
// same inputs, same token; either input moving, different token.
func TestArtworkFileVersionMovesWithTheFile(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	a := manifest.ArtworkFileVersion(base, 1234)
	if a != manifest.ArtworkFileVersion(base, 1234) {
		t.Error("not deterministic")
	}
	if a == manifest.ArtworkFileVersion(base.Add(time.Nanosecond), 1234) {
		t.Error("mtime change did not move the token")
	}
	if a == manifest.ArtworkFileVersion(base, 1235) {
		t.Error("size change did not move the token")
	}
	if len(a) != 16 {
		t.Errorf("token = %q (len %d), want 16 hex chars to match the artworkVersion "+
			"alias shape used elsewhere", a, len(a))
	}
}
