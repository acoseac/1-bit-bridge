//go:build linux || darwin

package conf

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestChangedCountsTheLinkAFileIsNamedBy: a link that names mpd.conf or an
// include is part of what the read depends on, since re-pointing it hands
// the read another file whose own times are old. Its own change counts.
func TestChangedCountsTheLinkAFileIsNamedBy(t *testing.T) {
	later := time.Now().Add(time.Hour).Truncate(time.Second)
	setLink := func(t *testing.T, link string) {
		t.Helper()
		tv := unix.NsecToTimeval(later.UnixNano())
		if err := unix.Lutimes(link, []unix.Timeval{tv, tv}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		link string
	}{
		{"mpd.conf named by a link", "mpd.conf"},
		{"an include named by a link", "inc.conf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mustWrite(t, filepath.Join(root, "real", "mpd.conf"), "include \"/inc.conf\"\nmusic_directory \"/m\"\n")
			mustWrite(t, filepath.Join(root, "real", "inc.conf"), "replaygain \"off\"\n")
			if err := os.Symlink(filepath.Join("real", "mpd.conf"), filepath.Join(root, "mpd.conf")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("real", "inc.conf"), filepath.Join(root, "inc.conf")); err != nil {
				t.Fatal(err)
			}
			setLink(t, filepath.Join(root, tc.link))
			// The main file is named by its link, as /etc/mpd.conf would be.
			f, err := ReadFile(root, "/mpd.conf", filepath.Join(root, "mpd.conf"))
			if err != nil || len(f.Problems) != 0 {
				t.Fatalf("err %v problems %q", err, f.Problems)
			}
			if got := f.Changed(); !got.Equal(later) {
				t.Fatalf("changed %s, want the link's %s", got, later)
			}
		})
	}
}
