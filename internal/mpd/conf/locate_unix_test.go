//go:build unix

package conf

import (
	"os"
	"syscall"
	"testing"
)

func TestFindSkipsAFifo(t *testing.T) {
	root := t.TempDir()
	home := "/home/pi"
	if err := os.MkdirAll(hostPath(root, home), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(hostPath(root, home+"/.mpdconf"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, hostPath(root, "/etc/mpd.conf"), "music_directory \"/etc-one\"\n")
	logical, _, ok := Locate{Root: root, Home: home}.Find()
	if !ok || logical != SystemPath {
		t.Fatalf("found %q ok %v, want %s", logical, ok, SystemPath)
	}
}
