package conf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMissingIncludesKeepNotFound(t *testing.T) {
	for _, kind := range []string{"missing", "dangling link"} {
		for _, directive := range []string{"include", "include_optional"} {
			t.Run(kind+"/"+directive, func(t *testing.T) {
				root := t.TempDir()
				child := filepath.Join(root, "child.conf")
				if kind == "dangling link" {
					if err := os.Symlink("absent.conf", child); err != nil {
						t.Skip(err)
					}
				}
				physicalRoot, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				l := &loader{root: physicalRoot}
				if _, err := l.open(child); !errors.Is(err, errNotFound) {
					t.Errorf("open: %v, want not found", err)
				}
				main := filepath.Join(root, "mpd.conf")
				if err := os.WriteFile(main, []byte(directive+" \"child.conf\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f, err := ReadHost(root, main)
				if err != nil {
					t.Fatal(err)
				}
				if directive == "include_optional" {
					if len(f.Problems) != 0 {
						t.Errorf("optional missing: %v", f.Problems)
					}
				} else if len(f.Problems) != 1 || !strings.Contains(f.Problems[0], "file not found") {
					t.Errorf("plain missing: %v", f.Problems)
				}
			})
		}
	}
}

func TestMissingIncludeOutsideRootIsStillRefused(t *testing.T) {
	for _, kind := range []string{"dangling file", "directory link"} {
		t.Run(kind, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			target := filepath.Join(outside, "missing.conf")
			name := "link.conf"
			if kind == "directory link" {
				target = outside
				name = "link.conf/missing.conf"
			}
			if err := os.Symlink(target, filepath.Join(root, "link.conf")); err != nil {
				t.Skip(err)
			}
			main := filepath.Join(root, "mpd.conf")
			if err := os.WriteFile(main, []byte("include_optional \""+name+"\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f, err := ReadHost(root, main)
			if err != nil {
				t.Fatal(err)
			}
			if len(f.Problems) != 1 || !strings.Contains(f.Problems[0], "leaves the root") {
				t.Errorf("outside absence swallowed: %v", f.Problems)
			}
			if strings.Contains(strings.Join(f.Problems, " "), outside) {
				t.Error("host path leaked")
			}
		})
	}
}
