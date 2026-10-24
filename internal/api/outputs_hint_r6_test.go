package api

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

func TestHintParentAtHostRootStaysAtRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the canonical POSIX root / has no Windows equivalent")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeRigFile(t, root, "etc/placeholder", "")
	writeRigFile(t, root, "srv/mpd/mpd.conf", "")
	// Climb past /, then descend to the existing file. Keep the raw target:
	// cleaning it would erase the boundary this walk needs to handle.
	target := strings.Repeat("../", len(strings.Split(strings.Trim(root, "/"), "/"))+3) + strings.TrimPrefix(root, "/") + "/srv/mpd/mpd.conf"
	alias := filepath.Join(root, "etc/mpd.conf")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	want, _, err := conf.StatHost("/", alias)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := hintOperandHost("/", alias); !ok || got != want {
		t.Errorf("host-root parent lost operand: got %q, %v; want %q", got, ok, want)
	}
	if err := os.Symlink("../../srv/mpd/mpd.conf", filepath.Join(root, "etc/injected.conf")); err != nil {
		t.Fatal(err)
	}
	if got, ok := hintOperandHost(root, "/etc/injected.conf"); ok {
		t.Errorf("injected-root parent escaped: %q", got)
	}
}

func TestReadingHintLinkToInjectedRoot(t *testing.T) {
	for _, spelling := range []string{"named", "canonical"} {
		t.Run(spelling, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "srv/mpd.conf", "")
			target := root
			if spelling == "canonical" {
				var err error
				target, err = filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, filepath.Join(root, "etc/base")); err != nil {
				t.Skip(err)
			}
			operand := "/etc/base/srv/mpd.conf"
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00"+operand+"\x00")
			wantConfigAndHint(t, provenanceReading(t, r), "different", operand, "no")
		})
	}
}

func TestReadingHintFortyLinks(t *testing.T) {
	r, root, pid := provenanceRig(t)
	writeRigFile(t, root, "etc/end.conf", "")
	for i := 39; i >= 0; i-- {
		next := "end.conf"
		if i < 39 {
			next = fmt.Sprintf("link%d.conf", i+1)
		}
		if err := os.Symlink(next, filepath.Join(root, "etc", fmt.Sprintf("link%d.conf", i))); err != nil {
			t.Skip(err)
		}
	}
	operand := "/etc/link0.conf"
	writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00"+operand+"\x00")
	wantConfigAndHint(t, provenanceReading(t, r), "different", operand, "no")
}

func TestReadingHintValidLinkChains(t *testing.T) {
	for _, kind := range []string{"absolute", "relative_parent"} {
		t.Run(kind, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			writeRigFile(t, root, "srv/x.conf", "")
			writeRigFile(t, root, "srv/nested/placeholder", "")
			// A lexical clean of dir-link/../x.conf reaches this configured
			// file instead of srv/x.conf, changing different into same.
			r.confPath = filepath.Join(root, "etc/x.conf")
			r.writeConf(volumioConf(t, r.music))
			target := filepath.Join(root, "srv/x.conf")
			if kind == "relative_parent" {
				if err := os.Symlink("../srv/nested", filepath.Join(root, "etc/dir-link")); err != nil {
					t.Skip(err)
				}
				target = "dir-link/../x.conf"
			}
			if err := os.Symlink(target, filepath.Join(root, "etc/alias.conf")); err != nil {
				t.Skip(err)
			}
			operand := "/etc/alias.conf"
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00"+operand+"\x00")
			wantHint(t, provenanceReading(t, r), "different", operand)
		})
	}
}

// TestHintLinkToEitherSpellingOfTheRoot: an absolute link to exactly the
// injected root stays inside it, whether it names the root as configured or
// by its canonical path. The root is reached through a link here, so the two
// spellings differ on every OS: a bare t.TempDir has one spelling on Linux,
// where TestReadingHintLinkToInjectedRoot's two subtests are the same test.
func TestHintLinkToEitherSpellingOfTheRoot(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	named := filepath.Join(dir, "alias")
	writeRigFile(t, real, "srv/mpd.conf", "")
	if err := os.MkdirAll(filepath.Join(real, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, named); err != nil {
		t.Skip(err)
	}
	canonical, err := filepath.EvalSymlinks(named)
	if err != nil {
		t.Fatal(err)
	}
	if canonical == named {
		t.Fatalf("premise: the root's two spellings differ, both %q", named)
	}
	want, err := os.Stat(filepath.Join(real, "srv/mpd.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{named, canonical} {
		link := filepath.Join(real, "etc/base")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		got, ok := hintOperandHost(named, "/etc/base/srv/mpd.conf")
		if !ok {
			t.Errorf("a link to the root spelled %q lost the hint", target)
			continue
		}
		if fi, err := os.Stat(got); err != nil || !os.SameFile(fi, want) {
			t.Errorf("a link to the root spelled %q resolved to %q (%v), not the root's srv/mpd.conf", target, got, err)
		}
	}
}
