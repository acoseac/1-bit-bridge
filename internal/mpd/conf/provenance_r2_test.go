package conf

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIncludesResolveDotDotPhysicallyAtEveryLevel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows normalizes .. lexically before the filesystem follows a link (Win32 path rules), so MPD's physical .. is a POSIX rule")
	}
	for _, kind := range []string{"main directory link", "child directory link", "main file link", "child file link"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			write := func(p, s string) {
				t.Helper()
				p = filepath.Join(root, p)
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(s), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write("opt/mpd/sub/child.conf", "include \"../x.conf\"\n")
			write("opt/mpd/x.conf", "include \"sub/../answer.conf\"\n")
			write("opt/mpd/answer.conf", "volume_normalization \"yes\"\n")
			write("etc/x.conf", "volume_normalization \"no\"\n")
			if err := os.Symlink("../opt/mpd/sub", filepath.Join(root, "etc/linked")); err != nil {
				t.Skip(err)
			}
			main := "etc/linked/child.conf"
			switch kind {
			case "child directory link":
				write("etc/start.conf", "include \"linked/child.conf\"\n")
				main = "etc/start.conf"
			case "main file link", "child file link":
				write("real/file.conf", "include \"../linked/../x.conf\"\n")
				write("etc/named/placeholder", "")
				if err := os.Symlink("../../real/file.conf", filepath.Join(root, "etc/named/main.conf")); err != nil {
					t.Skip(err)
				}
				main = "etc/named/main.conf"
				if kind == "child file link" {
					write("etc/start.conf", "include \"named/main.conf\"\n")
					main = "etc/start.conf"
				}
			}
			f, err := ReadHost(root, filepath.ToSlash(filepath.Join(root, main)))
			if err != nil {
				t.Fatal(err)
			}
			if f.VolumeNormalization != "yes" || len(f.Problems) != 0 {
				t.Fatalf("normalization=%q problems=%v", f.VolumeNormalization, f.Problems)
			}
		})
	}
}
