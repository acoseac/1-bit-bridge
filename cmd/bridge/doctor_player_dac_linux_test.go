//go:build linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestAccessForPlaybackAsksTheKernelAsAnOpenWould runs the player-dac line's
// probe, the raw faccessat2 call with R_OK|W_OK and AT_EACCESS, on this
// host's kernel over files of its own (B676): a file this process may read
// and write is allowed; one it may only read, only write, or neither, is
// EACCES, as the player's O_RDWR open would be refused; one that is not
// there is ENOENT. statDACNode is lstat's answer: the file's own group, no
// character device, and a link stat'ed as the link, as the player's open
// refuses one. No test asks the host's own nodes.
func TestAccessForPlaybackAsksTheKernelAsAnOpenWould(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may open any file: the refusals need a user the mode bits refuse")
	}
	dir := t.TempDir()
	node := filepath.Join(dir, "pcmC5D0p")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	switch err := accessForPlayback(node); {
	case errors.Is(err, syscall.ENOSYS), errors.Is(err, syscall.EPERM):
		t.Skipf("this kernel, or a policy over this test, answers faccessat2 with %v", err)
	case err != nil:
		t.Fatalf("a file this process may read and write: %v, want nil", err)
	}
	for _, mode := range []os.FileMode{0o400, 0o200, 0o000} {
		if err := os.Chmod(node, mode); err != nil {
			t.Fatal(err)
		}
		if err := accessForPlayback(node); !errors.Is(err, syscall.EACCES) {
			t.Errorf("a file of mode %v: %v, want EACCES", mode, err)
		}
	}
	if err := accessForPlayback(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file that is not there: %v, want ENOENT", err)
	}

	info, err := os.Lstat(node)
	if err != nil {
		t.Fatal(err)
	}
	n, err := statDACNode(node)
	if err != nil || n.char || n.gid != info.Sys().(*syscall.Stat_t).Gid {
		t.Errorf("statDACNode of a regular file: %+v, %v; want its own gid %d and no character device", n, err, info.Sys().(*syscall.Stat_t).Gid)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	if n, err := statDACNode(link); err != nil || n.char {
		t.Errorf("statDACNode of a link to a character device: %+v, %v; want the link itself, no character device", n, err)
	}
	if _, err := statDACNode(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("statDACNode of a node that is not there: %v, want ENOENT", err)
	}
}
