//go:build !windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The data dir's record is beside the file serve's boot read of the config was
// of (config.LoadFileAt), never beside the file the config's name opens by the
// time serve checks the record or writes it (Codex on #1397): a link at the
// name repointed between the two, a dotfiles restow or a home-manager switch
// during serve's boot, had serve check another config's record for its own
// data dir, and write its own data dir into that config's record.

// recordLinkLayout is two configs in directories of their own, each with a
// data dir of its own, the second's record naming its data dir, and a link at
// the name serve is given that leads to the first.
type recordLinkLayout struct {
	link, first, second, firstData, secondData string
}

func newRecordLinkLayout(t *testing.T) recordLinkLayout {
	t.Helper()
	base := resolvedTempDir(t)
	lib := filepath.Join(base, "Music")
	for _, d := range []string{lib, filepath.Join(base, "first"), filepath.Join(base, "second"), filepath.Join(base, "usb")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l := recordLinkLayout{
		link:       filepath.Join(base, "bridge.yaml"),
		firstData:  filepath.Join(base, "usb", "bridge"),
		secondData: filepath.Join(base, "second-data"),
	}
	l.first = writeServeConfig(t, filepath.Join(base, "first", "bridge.yaml"), lib, l.firstData, "127.0.0.1:0", "127.0.0.1:0")
	l.second = writeServeConfig(t, filepath.Join(base, "second", "bridge.yaml"), lib, l.secondData, "127.0.0.1:0", "127.0.0.1:0")
	writeDataDirRecord(t, l.second, l.secondData)
	if err := os.Symlink(l.first, l.link); err != nil {
		t.Fatal(err)
	}
	return l
}

// TestServeRecordsItsDataDirBesideTheFileItsBootReadOpened: a link at the
// config's name repointed to another config just after serve's boot read
// leaves that config's record as it was, and serve records its data dir
// beside the file it read.
func TestServeRecordsItsDataDirBesideTheFileItsBootReadOpened(t *testing.T) {
	l := newRecordLinkLayout(t)
	_, served, _, stderr := runServeWith(t, serveOpts{configPath: l.link,
		afterConfigRead: func() { repointLink(t, l.link, l.second) }})
	if !served {
		t.Fatalf("serve did not serve; stderr=%s", stderr)
	}
	requireLinkTo(t, l.link, l.second)
	if got := recordedDataDir(t, l.first); got != l.firstData {
		t.Errorf("the record beside the file serve's boot read opened names %q, want %q", got, l.firstData)
	}
	if got := recordedDataDir(t, l.second); got != l.secondData {
		t.Errorf("the record of the config the name opened after serve's read names %q, want %q, as it was", got, l.secondData)
	}
}

// TestServeChecksTheRecordBesideTheFileItsBootReadOpened: serve refuses the
// data dir that the record beside the file its boot read opened names, while
// that data dir holds neither its database nor its lock file, however the
// config's name is repointed after the read; the record beside the file the
// name opens by then, naming another data dir, refuses nothing of this one.
func TestServeChecksTheRecordBesideTheFileItsBootReadOpened(t *testing.T) {
	l := newRecordLinkLayout(t)
	writeDataDirRecord(t, l.first, l.firstData)
	code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: l.link,
		afterConfigRead: func() { repointLink(t, l.link, l.second) }})
	switch {
	case served:
		t.Errorf("serve served over the data dir its record names while it holds neither file; stdout=%s", stdout)
	case code != 1:
		t.Errorf("serve exited %d, want 1; stderr=%s", code, stderr)
	case !strings.Contains(stderr, "serve: the data directory "):
		t.Errorf("serve's refusal is not the data directory's; stderr=%s", stderr)
	default:
		mustRefuseAsConfigured(t, stderr, l.firstData, dataDirRecordFile(l.first))
	}
	if _, err := os.Lstat(l.firstData); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("serve made the data dir its record names (%v)", err)
	}
	requireLinkTo(t, l.link, l.second)
}
