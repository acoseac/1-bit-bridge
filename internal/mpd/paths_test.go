package mpd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProveRootsStates(t *testing.T) {
	t.Run("music directory itself", func(t *testing.T) {
		music := t.TempDir()
		var asked []string
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: "Library", Path: music, Tracks: []string{"Album/t.flac", "b.flac"},
		}}, func(_ context.Context, path string) (bool, error) {
			asked = append(asked, path)
			return true, nil
		})
		want := []PathRule{{
			Root: "Library", StripBridgePrefix: "", PrependMpdPrefix: "",
			State: PathProven, Checked: 2, Found: 2,
		}}
		if !reflect.DeepEqual(rules, want) {
			t.Fatalf("rules %#v", rules)
		}
		if !reflect.DeepEqual(asked, []string{"Album/t.flac", "b.flac"}) {
			t.Fatalf("asked %q", asked)
		}
	})

	t.Run("child directory", func(t *testing.T) {
		music := t.TempDir()
		root := filepath.Join(music, "INTERNAL")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		var asked []string
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: "INTERNAL", Path: root, Tracks: []string{"Album/t.flac"},
		}}, func(_ context.Context, path string) (bool, error) {
			asked = append(asked, path)
			return true, nil
		})
		if rules[0].State != PathProven || rules[0].PrependMpdPrefix != "INTERNAL/" || rules[0].StripBridgePrefix != "" {
			t.Fatalf("rule %#v", rules[0])
		}
		if !reflect.DeepEqual(asked, []string{"INTERNAL/Album/t.flac"}) {
			t.Fatalf("asked %q", asked)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		music := t.TempDir()
		root := t.TempDir()
		link := filepath.Join(music, "INTERNAL")
		if err := os.Symlink(root, link); err != nil {
			t.Skip(err)
		}
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: filepath.Base(root), Path: root, Tracks: []string{"a.flac"},
		}}, func(_ context.Context, path string) (bool, error) {
			if path != "INTERNAL/a.flac" {
				t.Errorf("asked %q", path)
			}
			return true, nil
		})
		if rules[0].State != PathProven || rules[0].PrependMpdPrefix != "INTERNAL/" {
			t.Fatalf("rule %#v", rules[0])
		}
	})

	t.Run("same basename is not the same file", func(t *testing.T) {
		music := t.TempDir()
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(music, filepath.Base(root)), 0o755); err != nil {
			t.Fatal(err)
		}
		called := 0
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: filepath.Base(root), Path: root, Tracks: []string{"a.flac"},
		}}, func(context.Context, string) (bool, error) {
			called++
			return true, nil
		})
		if rules[0].State != PathUnproven || rules[0].Checked != 0 || rules[0].Found != 0 {
			t.Fatalf("rule %#v", rules[0])
		}
		if rules[0].StripBridgePrefix != "" || rules[0].PrependMpdPrefix != "" {
			t.Fatalf("prefixes filled on an unproven rule: %#v", rules[0])
		}
		if called != 0 {
			t.Fatalf("find was called %d times", called)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		music := t.TempDir()
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: "Library", Path: music, Tracks: []string{"a.flac", "b.flac"},
		}}, func(_ context.Context, path string) (bool, error) {
			return path == "a.flac", nil
		})
		if rules[0].State != PathMismatch || rules[0].Checked != 2 || rules[0].Found != 1 {
			t.Fatalf("rule %#v", rules[0])
		}
		if rules[0].StripBridgePrefix != "" || rules[0].PrependMpdPrefix != "" {
			t.Fatalf("matched prefixes dropped: %#v", rules[0])
		}
	})

	t.Run("find error", func(t *testing.T) {
		music := t.TempDir()
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: "Library", Path: music, Tracks: []string{"a.flac"},
		}}, func(context.Context, string) (bool, error) {
			return false, errors.New("mpd: closed")
		})
		if rules[0].State != PathMismatch || rules[0].Checked != 1 || rules[0].Found != 0 {
			t.Fatalf("rule %#v", rules[0])
		}
	})

	t.Run("no tracks", func(t *testing.T) {
		music := t.TempDir()
		root := filepath.Join(music, "INTERNAL")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: "INTERNAL", Path: root,
		}}, func(context.Context, string) (bool, error) {
			t.Fatal("find with no tracks")
			return false, nil
		})
		if rules[0].State != PathUnchecked || rules[0].PrependMpdPrefix != "INTERNAL/" || rules[0].Checked != 0 {
			t.Fatalf("rule %#v", rules[0])
		}
	})

	t.Run("no music directory", func(t *testing.T) {
		rules := ProveRoots(context.Background(), "", []LibraryRoot{{
			ID: "Library", Path: t.TempDir(), Tracks: []string{"a.flac"},
		}}, func(context.Context, string) (bool, error) {
			t.Fatal("find without a music directory")
			return false, nil
		})
		if rules[0].State != PathUnchecked || rules[0].PrependMpdPrefix != "" || rules[0].StripBridgePrefix != "" {
			t.Fatalf("rule %#v", rules[0])
		}
	})

	t.Run("multi-root prefixes", func(t *testing.T) {
		music := t.TempDir()
		root := filepath.Join(music, "INTERNAL")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		var asked []string
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: "Disk", Path: root, Multi: true, Tracks: []string{"Disk/Album/t.flac"},
		}}, func(_ context.Context, path string) (bool, error) {
			asked = append(asked, path)
			return true, nil
		})
		if rules[0].State != PathProven || rules[0].StripBridgePrefix != "Disk/" || rules[0].PrependMpdPrefix != "INTERNAL/" {
			t.Fatalf("rule %#v", rules[0])
		}
		if !reflect.DeepEqual(asked, []string{"INTERNAL/Album/t.flac"}) {
			t.Fatalf("asked %q", asked)
		}
	})

	t.Run("twenty of twenty five", func(t *testing.T) {
		music := t.TempDir()
		tracks := make([]string, 25)
		for i := range tracks {
			tracks[i] = string(rune('a'+i)) + ".flac"
		}
		var asked []string
		rules := ProveRoots(context.Background(), music, []LibraryRoot{{
			ID: "Library", Path: music, Tracks: tracks,
		}}, func(_ context.Context, path string) (bool, error) {
			asked = append(asked, path)
			return true, nil
		})
		if rules[0].State != PathProven || rules[0].Checked != 20 || rules[0].Found != 20 {
			t.Fatalf("rule %#v", rules[0])
		}
		if len(asked) != 20 || asked[0] != "a.flac" || asked[19] != "x.flac" {
			t.Fatalf("sample %q", asked)
		}
	})
}

func TestProveRootsAmbiguousMatchIsUnproven(t *testing.T) {
	music := t.TempDir()
	link := filepath.Join(music, "AGAIN")
	if err := os.Symlink(music, link); err != nil {
		t.Skip(err)
	}
	called := 0
	rules := ProveRoots(context.Background(), music, []LibraryRoot{{
		ID: "Library", Path: music, Tracks: []string{"a.flac"},
	}}, func(context.Context, string) (bool, error) {
		called++
		return true, nil
	})
	if rules[0].State != PathUnproven || called != 0 {
		t.Fatalf("rule %#v calls %d", rules[0], called)
	}
}
