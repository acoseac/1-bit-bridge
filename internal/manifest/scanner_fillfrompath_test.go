package manifest

import "testing"

// fillFromPath is the last-resort heuristic for files with no embedded
// tags. The multiRoot bool is the load-bearing signal: in multi-root
// mode `relPath` prefixes every track's library-relative form with the
// root's basename, and the album/artist heuristics MUST strip that
// prefix before evaluating the trailing segments — otherwise an
// untagged file directly under a root named "Alphaville" inherits
// "Alphaville" as the artist for every album in the root. (Field-
// reported "Alphaville bug".)
//
// These tests pin all three paths: the regression case, the deep-path
// positive case, and the single-root no-strip path.

func TestFillFromPath_MultiRootStripsPrefix_ShortPath(t *testing.T) {
	// "Alphaville/Album/Song.flac" under multiRoot=true.
	// After stripping the root basename we have just two segments
	// ("Album/Song.flac") so the artist heuristic (which needs >=3
	// segments) MUST NOT fire — leaving Artist empty is the correct
	// answer, NOT "Alphaville".
	track := &Track{}
	fillFromPath(track, "Alphaville/Album/Song.flac", true)
	if track.Title != "Song" {
		t.Errorf("Title = %q, want %q", track.Title, "Song")
	}
	if track.Album != "Album" {
		t.Errorf("Album = %q, want %q", track.Album, "Album")
	}
	if track.Artist != "" {
		t.Errorf("Artist = %q, want empty (root-prefix bug regression)", track.Artist)
	}
}

func TestFillFromPath_MultiRootStripsPrefix_DeepPath(t *testing.T) {
	// "Music/Genre/Pink Floyd/Dark Side/Money.flac" under multiRoot=true.
	// After stripping "Music" the working slice is
	// "Genre/Pink Floyd/Dark Side/Money.flac" — the artist heuristic
	// then picks the grandparent of the file, which is the artist
	// folder. This is the positive case: deep multi-root paths must
	// reach the same answer single-root paths reach.
	track := &Track{}
	fillFromPath(track, "Music/Genre/Pink Floyd/Dark Side/Money.flac", true)
	if track.Title != "Money" {
		t.Errorf("Title = %q, want %q", track.Title, "Money")
	}
	if track.Album != "Dark Side" {
		t.Errorf("Album = %q, want %q", track.Album, "Dark Side")
	}
	if track.Artist != "Pink Floyd" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Pink Floyd")
	}
}

func TestFillFromPath_SingleRootKeepsAllParts(t *testing.T) {
	// "Pink Floyd/Dark Side/Money.flac" under multiRoot=false.
	// No prefix to strip; the heuristic must operate on the full
	// path. Locks the no-strip behaviour so a future refactor that
	// always-strips flips this red.
	track := &Track{}
	fillFromPath(track, "Pink Floyd/Dark Side/Money.flac", false)
	if track.Title != "Money" {
		t.Errorf("Title = %q, want %q", track.Title, "Money")
	}
	if track.Album != "Dark Side" {
		t.Errorf("Album = %q, want %q", track.Album, "Dark Side")
	}
	if track.Artist != "Pink Floyd" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Pink Floyd")
	}
}

func TestFillFromPath_PreservesExistingTags(t *testing.T) {
	// Heuristic must not overwrite tag-derived fields. Exercises the
	// "if t.X == empty" gates on every field.
	track := &Track{
		Title:  "Real Title",
		Album:  "Real Album",
		Artist: "Real Artist",
	}
	fillFromPath(track, "Music/Pink Floyd/Dark Side/Money.flac", true)
	if track.Title != "Real Title" || track.Album != "Real Album" || track.Artist != "Real Artist" {
		t.Errorf("heuristic overwrote tag-derived fields: title=%q album=%q artist=%q",
			track.Title, track.Album, track.Artist)
	}
}
