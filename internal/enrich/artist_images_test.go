package enrich

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCachedArtistImageMBIDs pins the artist-image cache enumeration: only
// strict `artist-<uuid>.jpg` files count — the name-hashed canonicals
// (`artist-name-<sha256>.jpg`), `<mbid>-<size>.jpg` album covers sharing the
// directory, and malformed names are all excluded; keys come back lowercase.
func TestCachedArtistImageMBIDs(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	touch("artist-11111111-1111-4111-8111-111111111111.jpg")
	touch("artist-ABCDEF01-2345-4678-89AB-CDEF01234567.jpg") // uppercase → lowercased key
	touch("artist-name-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.jpg")
	touch("22222222-2222-4222-8222-222222222222-500.jpg") // album cover
	touch("artist-not-a-uuid.jpg")
	touch("artist-33333333-3333-4333-8333-333333333333.png") // wrong extension
	if err := os.MkdirAll(filepath.Join(dir, "artist-44444444-4444-4444-8444-444444444444.jpg"), 0o700); err != nil {
		t.Fatal(err) // a directory with a matching name must be skipped
	}

	got, err := CachedArtistImages(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"11111111-1111-4111-8111-111111111111",
		"abcdef01-2345-4678-89ab-cdef01234567",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries (%v), want %d", len(got), got, len(want))
	}
	for _, m := range want {
		if _, ok := got[m]; !ok {
			t.Errorf("missing expected mbid %s", m)
		}
	}
}

// TestCachedArtistImageMBIDsMissingDir pins the fresh-install shape: a
// nonexistent cache dir is "no images", not an error.
func TestCachedArtistImageMBIDsMissingDir(t *testing.T) {
	got, err := CachedArtistImages(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing dir: err = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("missing dir: got %v, want empty", got)
	}
}
