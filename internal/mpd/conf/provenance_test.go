package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinkedConfigIncludesUseTheNamedDirectory(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "main link", true: "included link"}[nested], func(t *testing.T) {
			root := t.TempDir()
			write := func(p, s string) {
				t.Helper()
				h := filepath.Join(root, p)
				if err := os.MkdirAll(filepath.Dir(h), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(h, []byte(s), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write("opt/mpd/mpd.conf", "include \"x.conf\"\n")
			write("opt/mpd/x.conf", "volume_normalization \"no\"\n")
			write("etc/x.conf", "volume_normalization \"yes\"\n")
			link := filepath.Join(root, "etc/mpd.conf")
			if err := os.Symlink("../opt/mpd/mpd.conf", link); err != nil {
				t.Skip(err)
			}
			if nested {
				write("etc/start.conf", "include \"mpd.conf\"\n")
				link = filepath.Join(root, "etc/start.conf")
			}
			f, err := ReadHost(root, link)
			if err != nil {
				t.Fatal(err)
			}
			if f.VolumeNormalization != "yes" || len(f.Problems) != 0 {
				t.Fatalf("normalization=%q problems=%v", f.VolumeNormalization, f.Problems)
			}
		})
	}
}

func TestLinkedConfigIncludesKeepConfinementAndCycleChecks(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"etc", "opt/mpd"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	main := filepath.Join(root, "opt/mpd/mpd.conf")
	if err := os.WriteFile(main, []byte("bind_to_address \"127.0.0.1\"\ninclude \"mpd.conf\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "etc/mpd.conf")
	if err := os.Symlink("../opt/mpd/mpd.conf", link); err != nil {
		t.Skip(err)
	}
	f, err := ReadHost(root, link)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.BindAddresses) != 1 {
		t.Fatalf("cycle file parsed again: %v", f.BindAddresses)
	}
	if !strings.Contains(strings.Join(f.Problems, " "), "cycle") {
		t.Fatalf("no cycle: %v", f.Problems)
	}
	outside := filepath.Join(t.TempDir(), "x.conf")
	if err := os.WriteFile(outside, []byte("volume_normalization \"yes\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc/x.conf")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("include_optional \"x.conf\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err = ReadHost(root, link)
	if err != nil {
		t.Fatal(err)
	}
	if f.VolumeNormalization != "" || !strings.Contains(strings.Join(f.Problems, " "), "leaves the root") {
		t.Fatalf("outside include: %+v", f)
	}
}
