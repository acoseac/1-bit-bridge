//go:build !windows

package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
)

// fileOwner is a file's numeric owner and group.
type fileOwner struct{ uid, gid int }

// ownerSeams holds the three facts keepOwner acts on: who this process is,
// whose file the destination is, and how a file is given away.
// SimulateRootForTest swaps them so a test that does not run as root can
// drive every writer's root path; production always reads realOwnerSeams.
type ownerSeams struct {
	euid   func() int
	target func(dst string) (fileOwner, error)
	chown  func(f *os.File, dst string, o fileOwner) error
}

var realOwnerSeams = &ownerSeams{
	euid:   os.Geteuid,
	target: targetOwner,
	chown:  func(f *os.File, _ string, o fileOwner) error { return f.Chown(o.uid, o.gid) },
}

// activeOwnerSeams is nil outside a test that simulates root. An atomic
// pointer rather than plain vars, because a writer can run on another
// goroutine (a debounced flush) while a test swaps them.
var activeOwnerSeams atomic.Pointer[ownerSeams]

func currentOwnerSeams() *ownerSeams {
	if s := activeOwnerSeams.Load(); s != nil {
		return s
	}
	return realOwnerSeams
}

func keepOwner(staged *os.File, dst string) error {
	s := currentOwnerSeams()
	if s.euid() != 0 {
		return nil
	}
	want, err := s.target(dst)
	if err != nil {
		return fmt.Errorf("find the owner of %s, which its replacement keeps: %w", dst, err)
	}
	have, err := stagedOwner(staged)
	if err != nil {
		return fmt.Errorf("stat the file staged for %s: %w", dst, err)
	}
	if have == want {
		return nil
	}
	if err := s.chown(staged, dst, want); err != nil {
		return fmt.Errorf("give the file staged for %s to uid %d gid %d, the owner of what it replaces: %w",
			dst, want.uid, want.gid, err)
	}
	return nil
}

// targetOwner answers whose file dst is: the owner of the entry at dst when
// there is one, and otherwise the owner of the directory the file will be
// created in.
func targetOwner(dst string) (fileOwner, error) {
	return targetOwnerFrom(dst, os.Lstat, os.Stat)
}

// targetOwnerFrom is targetOwner over injected stat functions, so a test
// can pin the precedence without files owned by two different users. dst
// is looked up with lstat: a symlink at dst is the entry the rename
// replaces, so its own owner is the one kept, never its target's. The
// directory is looked up with stat, because the file is created in the
// directory the path resolves to.
func targetOwnerFrom(dst string, lstat, stat func(string) (fs.FileInfo, error)) (fileOwner, error) {
	info, err := lstat(dst)
	if errors.Is(err, fs.ErrNotExist) {
		info, err = stat(filepath.Dir(dst))
	}
	if err != nil {
		return fileOwner{}, err
	}
	return ownerFromInfo(info)
}

func stagedOwner(f *os.File) (fileOwner, error) {
	info, err := f.Stat()
	if err != nil {
		return fileOwner{}, err
	}
	return ownerFromInfo(info)
}

func ownerFromInfo(info fs.FileInfo) (fileOwner, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return fileOwner{}, fmt.Errorf("the stat of %s carries no owner", info.Name())
	}
	return fileOwner{uid: int(st.Uid), gid: int(st.Gid)}, nil
}

// SimulateRootForTest makes KeepOwner act as a process running as root
// that finds uid:gid as the owner of every destination, and records each
// change it would make instead of making it. It returns the changes
// recorded so far and a function that restores the real behaviour. Tests
// only: it lets a test that does not run as root check that a writer calls
// KeepOwner on its staged file. On Windows KeepOwner does nothing, so such
// a test skips there.
func SimulateRootForTest(uid, gid int) (changes func() []OwnerChange, restore func()) {
	var mu sync.Mutex
	var got []OwnerChange
	s := &ownerSeams{
		euid:   func() int { return 0 },
		target: func(string) (fileOwner, error) { return fileOwner{uid: uid, gid: gid}, nil },
		chown: func(_ *os.File, dst string, o fileOwner) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, OwnerChange{Dst: dst, UID: o.uid, GID: o.gid})
			return nil
		},
	}
	prev := activeOwnerSeams.Swap(s)
	return func() []OwnerChange {
			mu.Lock()
			defer mu.Unlock()
			return append([]OwnerChange(nil), got...)
		}, func() {
			activeOwnerSeams.Store(prev)
		}
}
