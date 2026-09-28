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

// ownerSeams holds the facts the owner-keeping helpers act on: who this
// process is, whose file a destination is, which owner a new directory
// takes, and how a file or a directory is given away.
// SimulateRootForTest swaps them so a test that does not run as root can
// drive every writer's root path; production always reads realOwnerSeams.
type ownerSeams struct {
	euid   func() int
	target func(dst string) (fileOwner, error)
	chown  func(f *os.File, dst string, o fileOwner) error
	// dirTarget answers which owner a directory created in r's directory
	// takes, under policy; last reports whether it is the last directory
	// of the path the caller asked for.
	dirTarget func(r *os.Root, policy dirOwnerPolicy, last bool, ref string) (fileOwner, error)
	// chownDir gives the directory name, created in r, the owner o. path
	// is that directory as the caller spelled it, for errors and records.
	chownDir func(r *os.Root, name, path string, o fileOwner) error
}

var realOwnerSeams = &ownerSeams{
	euid:   os.Geteuid,
	target: targetOwner,
	chown:  func(f *os.File, _ string, o fileOwner) error { return f.Chown(o.uid, o.gid) },
	dirTarget: func(r *os.Root, policy dirOwnerPolicy, last bool, ref string) (fileOwner, error) {
		// "." through the root is the directory the new one was created
		// in, by its descriptor: the owner is read from where the
		// directory really is, whatever its path resolves to now.
		parent, err := r.Stat(".")
		if err != nil {
			return fileOwner{}, err
		}
		return dirOwnerFor(parent, policy, last, ref, nearestExisting)
	},
	// Lchown, so a symlink put where the directory was is changed itself,
	// never followed: it could only be put there by the owner of the
	// directory holding it, who is the owner it is given (or, in a shared
	// directory, anybody, who could have made that entry too).
	chownDir: func(r *os.Root, name, _ string, o fileOwner) error { return r.Lchown(name, o.uid, o.gid) },
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

// dirOwnerFor answers which owner a directory created in the directory
// parent describes takes, under policy: parent's own owner, or ref's
// (through nearest, which finds ref or its nearest existing ancestor) where
// the policy says the parent's owner is not the evidence. An empty ref
// never overrides.
func dirOwnerFor(parent fs.FileInfo, policy dirOwnerPolicy, last bool, ref string,
	nearest func(string) (fs.FileInfo, error)) (fileOwner, error) {
	takeRef := false
	switch policy {
	case dirOwnerShared:
		// Anyone may create an entry here (others can write and search),
		// so the directory's owner says nothing about who uses one.
		takeRef = parent.Mode().Perm()&0o003 == 0o003
	case dirOwnerLastLikeRef:
		takeRef = last
	}
	if !takeRef || ref == "" {
		return ownerFromInfo(parent)
	}
	info, err := nearest(ref)
	if err != nil {
		return fileOwner{}, fmt.Errorf("find the owner of %s: %w", ref, err)
	}
	return ownerFromInfo(info)
}

// nearestExisting stats p, or, while p does not exist, its nearest
// ancestor that does: the owner a directory serving p takes before p has
// been made (the variants directory before its first rendition).
func nearestExisting(p string) (fs.FileInfo, error) {
	for {
		info, err := os.Stat(p)
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return info, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil, err
		}
		p = parent
	}
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

func mkdirAll(path string, perm os.FileMode, policy dirOwnerPolicy, ref string) error {
	s := currentOwnerSeams()
	if s.euid() != 0 {
		return os.MkdirAll(path, perm)
	}
	return mkdirAllAsRoot(s, path, perm, policy, ref, true)
}

func mkdir(path string, perm os.FileMode, policy dirOwnerPolicy, ref string, last bool) error {
	s := currentOwnerSeams()
	if s.euid() != 0 {
		return os.Mkdir(path, perm)
	}
	return mkdirAsRoot(s, path, perm, policy, ref, last)
}

// mkdirAllAsRoot is os.MkdirAll's own walk (a directory already there is
// done, anything else there is ENOTDIR, the parent first, then this
// directory), with mkdirAsRoot in place of os.Mkdir.
func mkdirAllAsRoot(s *ownerSeams, path string, perm os.FileMode, policy dirOwnerPolicy, ref string, last bool) error {
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return nil
		}
		return &fs.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}
	i := len(path)
	for i > 0 && os.IsPathSeparator(path[i-1]) { // trailing separators
		i--
	}
	j := i
	for j > 0 && !os.IsPathSeparator(path[j-1]) { // the last element
		j--
	}
	if j > 1 {
		if err := mkdirAllAsRoot(s, path[:j-1], perm, policy, ref, false); err != nil {
			return err
		}
	}
	if err := mkdirAsRoot(s, path, perm, policy, ref, last); err != nil {
		// "foo/." and a directory somebody made meanwhile, as os.MkdirAll.
		if info, lerr := os.Lstat(path); lerr == nil && info.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

// mkdirAsRoot creates the one directory path through an os.Root on its
// parent, and gives it the owner dirTarget names, read through that same
// root. Anything it cannot finish leaves no directory of its own behind.
func mkdirAsRoot(s *ownerSeams, path string, perm os.FileMode, policy dirOwnerPolicy, ref string, last bool) error {
	clean := filepath.Clean(path)
	dir, name := filepath.Dir(clean), filepath.Base(clean)
	r, err := os.OpenRoot(dir)
	if err != nil {
		return mkdirErr(err, path)
	}
	defer r.Close()
	if err := r.Mkdir(name, perm); err != nil {
		// os.Mkdir's error, whose os.ErrExist a caller may test for:
		// an entry somebody else made is not ours to give away.
		return mkdirErr(err, path)
	}
	fail := func(err error) error {
		if info, lerr := r.Lstat(name); lerr == nil && info.IsDir() {
			_ = r.Remove(name) // empty: it is the one just made
		}
		return err
	}
	want, err := s.dirTarget(r, policy, last, ref)
	if err != nil {
		return fail(fmt.Errorf("find the owner the new directory %s takes: %w", path, err))
	}
	info, err := r.Lstat(name)
	if err != nil {
		return fail(fmt.Errorf("stat the new directory %s: %w", path, err))
	}
	have, err := ownerFromInfo(info)
	if err != nil {
		return fail(err)
	}
	if have == want {
		return nil
	}
	if err := s.chownDir(r, name, path, want); err != nil {
		return fail(fmt.Errorf("give the new directory %s to uid %d gid %d: %w", path, want.uid, want.gid, err))
	}
	return nil
}

// mkdirErr puts the caller's own spelling of path, and os.Mkdir's op, on
// an error the root reported against the last element alone.
func mkdirErr(err error, path string) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return &fs.PathError{Op: "mkdir", Path: path, Err: pe.Err}
	}
	return err
}

func precreate(path string, perm os.FileMode, dst string) error {
	s := currentOwnerSeams()
	if s.euid() != 0 {
		return nil
	}
	// Before path exists: when dst is path itself, its owner is the
	// directory's, not the empty file's about to be made.
	want, err := s.target(dst)
	if err != nil {
		return fmt.Errorf("find the owner of %s, which %s takes: %w", dst, path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	have, err := stagedOwner(f)
	if err != nil {
		err = fmt.Errorf("stat %s: %w", path, err)
	} else if have != want {
		if cerr := s.chown(f, dst, want); cerr != nil {
			err = fmt.Errorf("give %s to uid %d gid %d, the owner of %s: %w", path, want.uid, want.gid, dst, cerr)
		}
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// SimulateRootForTest makes KeepOwner, Precreate and the directory helpers
// act as a process running as root that finds uid:gid as the owner of every
// destination and every new directory, and records each change it would
// make instead of making it. It returns the changes recorded so far and a
// function that restores the real behaviour. Tests only: it lets a test
// that does not run as root check that a writer gives what it creates away.
// The directories and files are still created, as the test's own user. On
// Windows these helpers change no owner, so such a test skips there.
func SimulateRootForTest(uid, gid int) (changes func() []OwnerChange, restore func()) {
	var mu sync.Mutex
	var got []OwnerChange
	record := func(dst string, o fileOwner) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, OwnerChange{Dst: dst, UID: o.uid, GID: o.gid})
	}
	s := &ownerSeams{
		euid:   func() int { return 0 },
		target: func(string) (fileOwner, error) { return fileOwner{uid: uid, gid: gid}, nil },
		chown: func(_ *os.File, dst string, o fileOwner) error {
			record(dst, o)
			return nil
		},
		dirTarget: func(*os.Root, dirOwnerPolicy, bool, string) (fileOwner, error) {
			return fileOwner{uid: uid, gid: gid}, nil
		},
		chownDir: func(_ *os.Root, _, path string, o fileOwner) error {
			record(path, o)
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
