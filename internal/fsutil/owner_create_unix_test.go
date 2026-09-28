//go:build !windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// modedInfo is a FileInfo with a mode and an owner, which is all
// dirOwnerFor reads of a parent.
type modedInfo struct {
	mode     fs.FileMode
	uid, gid uint32
}

func (i modedInfo) Name() string       { return "parent" }
func (i modedInfo) Size() int64        { return 0 }
func (i modedInfo) Mode() fs.FileMode  { return i.mode }
func (i modedInfo) ModTime() time.Time { return time.Time{} }
func (i modedInfo) IsDir() bool        { return true }
func (i modedInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid, Gid: i.gid} }

// TestDirOwnerForTakesTheParentsOwnerUnlessThePolicyNamesRef pins which
// owner a new directory takes: its parent's, except in a directory anyone
// may create entries in (the shared policy) and for the last directory of
// the path (the like policy), where ref's. An empty ref never overrides,
// and a ref that cannot be looked up is an error, never a guess.
func TestDirOwnerForTakesTheParentsOwnerUnlessThePolicyNamesRef(t *testing.T) {
	root755 := modedInfo{mode: fs.ModeDir | 0o755, uid: 0, gid: 0}
	svc755 := modedInfo{mode: fs.ModeDir | 0o755, uid: 4242, gid: 4243}
	tmp := modedInfo{mode: fs.ModeDir | fs.ModeSticky | 0o777, uid: 0, gid: 0}
	writeNoSearch := modedInfo{mode: fs.ModeDir | 0o772, uid: 0, gid: 0}
	refInfo := modedInfo{mode: fs.ModeDir | 0o755, uid: 5252, gid: 5253}
	lost := errors.New("i/o error")

	for _, tc := range []struct {
		name    string
		parent  modedInfo
		policy  dirOwnerPolicy
		last    bool
		ref     string
		refErr  error
		want    fileOwner
		wantErr error
		askRef  bool
	}{
		{name: "parent: a service directory's owner", parent: svc755, policy: dirOwnerParent, last: true, ref: "/v", want: fileOwner{4242, 4243}},
		{name: "parent: root's /tmp stays root's", parent: tmp, policy: dirOwnerParent, last: true, ref: "/v", want: fileOwner{0, 0}},
		{name: "shared: a directory only its owner writes keeps the parent's", parent: root755, policy: dirOwnerShared, ref: "/v", want: fileOwner{0, 0}},
		{name: "shared: /tmp gives ref's", parent: tmp, policy: dirOwnerShared, ref: "/v", want: fileOwner{5252, 5253}, askRef: true},
		{name: "shared: writable but not searchable is not shared", parent: writeNoSearch, policy: dirOwnerShared, ref: "/v", want: fileOwner{0, 0}},
		{name: "shared: no ref keeps the parent's", parent: tmp, policy: dirOwnerShared, ref: "", want: fileOwner{0, 0}},
		{name: "like: the last directory takes ref's", parent: root755, policy: dirOwnerLastLikeRef, last: true, ref: "/v", want: fileOwner{5252, 5253}, askRef: true},
		{name: "like: a directory on the way keeps the parent's", parent: root755, policy: dirOwnerLastLikeRef, last: false, ref: "/v", want: fileOwner{0, 0}},
		{name: "like: a ref that cannot be looked up is an error", parent: root755, policy: dirOwnerLastLikeRef, last: true, ref: "/v", refErr: lost, wantErr: lost, askRef: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			nearest := func(p string) (fs.FileInfo, error) {
				asked = true
				if p != tc.ref {
					t.Fatalf("looked up %q, want the ref %q", p, tc.ref)
				}
				if tc.refErr != nil {
					return nil, tc.refErr
				}
				return refInfo, nil
			}
			got, err := dirOwnerFor(tc.parent, tc.policy, tc.last, tc.ref, nearest)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("owner = %+v, want %+v", got, tc.want)
			}
			if asked != tc.askRef {
				t.Fatalf("ref looked up = %v, want %v", asked, tc.askRef)
			}
		})
	}
}

// TestNearestExistingClimbsToTheFirstAncestorThere: a ref not made yet (the
// variants directory before its first rendition) answers for its nearest
// ancestor that exists.
func TestNearestExistingClimbsToTheFirstAncestorThere(t *testing.T) {
	dir := t.TempDir()
	want, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, filepath.Join(dir, "a"), filepath.Join(dir, "a", "b", "c")} {
		got, err := nearestExisting(p)
		if err != nil {
			t.Fatalf("nearestExisting(%q): %v", p, err)
		}
		if !os.SameFile(got, want) {
			t.Fatalf("nearestExisting(%q) found %s, want %s", p, got.Name(), dir)
		}
	}
}

// dirCall is one directory the seams were asked about.
type dirCall struct {
	path string
	last bool
}

// dirSeams answers want for every directory and records what it was asked
// and what it gave away.
func dirSeams(t *testing.T, want fileOwner, chownErr, targetErr error) (targets *[]bool, given *[]dirCall) {
	t.Helper()
	var asked []bool
	var gave []dirCall
	withOwnerSeams(t, &ownerSeams{
		euid: func() int { return 0 },
		target: func(string) (fileOwner, error) {
			t.Fatal("a file owner asked of a directory helper")
			return fileOwner{}, nil
		},
		chown: func(*os.File, string, fileOwner) error { t.Fatal("a file chowned by a directory helper"); return nil },
		dirTarget: func(_ *os.Root, _ dirOwnerPolicy, last bool, _ string) (fileOwner, error) {
			asked = append(asked, last)
			return want, targetErr
		},
		chownDir: func(_ *os.Root, _, path string, o fileOwner) error {
			if o != want {
				t.Fatalf("gave %s to %+v, want %+v", path, o, want)
			}
			gave = append(gave, dirCall{path: path, last: len(asked) > 0 && asked[len(asked)-1]})
			return chownErr
		},
	})
	return &asked, &gave
}

// TestMkdirAllDecision drives the directory helpers through their seams, so
// the root path runs on every POSIX host: not root is os.MkdirAll and
// nothing else; root gives each directory it creates, and only those, the
// owner the policy names; a directory whose owner is already right is left
// alone; and a directory it cannot give away, or cannot find the owner for,
// is removed again and the error returned.
func TestMkdirAllDecision(t *testing.T) {
	other := fileOwner{uid: os.Getuid() + 1, gid: os.Getgid() + 1}
	refused := errors.New("operation not permitted")

	t.Run("a process that is not root only creates", func(t *testing.T) {
		base := t.TempDir()
		withOwnerSeams(t, &ownerSeams{
			euid: func() int { return 501 },
			dirTarget: func(*os.Root, dirOwnerPolicy, bool, string) (fileOwner, error) {
				t.Fatal("owner asked")
				return fileOwner{}, nil
			},
			chownDir: func(*os.Root, string, string, fileOwner) error { t.Fatal("chowned"); return nil },
		})
		p := filepath.Join(base, "a", "b")
		if err := MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(p); err != nil || !info.IsDir() {
			t.Fatalf("%s not made: %v", p, err)
		}
	})

	t.Run("root gives each directory it creates, and no other, the owner", func(t *testing.T) {
		base := t.TempDir()
		if err := os.Mkdir(filepath.Join(base, "there"), 0o755); err != nil {
			t.Fatal(err)
		}
		targets, gave := dirSeams(t, other, nil, nil)
		p := filepath.Join(base, "there", "a", "b")
		if err := MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		want := []dirCall{
			{path: filepath.Join(base, "there", "a"), last: false},
			{path: p, last: true},
		}
		if len(*gave) != len(want) {
			t.Fatalf("gave away %+v, want %+v (the existing directory must be left alone)", *gave, want)
		}
		for i := range want {
			if (*gave)[i] != want[i] {
				t.Fatalf("gave away %+v, want %+v", *gave, want)
			}
		}
		if len(*targets) != 2 {
			t.Fatalf("owner asked %d times, want once per directory made", len(*targets))
		}
		// And a second run over what is there now does nothing.
		*gave = nil
		if err := MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if len(*gave) != 0 {
			t.Fatalf("a directory already there was given away: %+v", *gave)
		}
	})

	t.Run("an owner that already matches is left alone", func(t *testing.T) {
		base := t.TempDir()
		_, gave := dirSeams(t, fileOwner{uid: os.Getuid(), gid: os.Getgid()}, nil, nil)
		if err := MkdirAll(filepath.Join(base, "a"), 0o755); err != nil {
			t.Fatal(err)
		}
		if len(*gave) != 0 {
			t.Fatalf("chowned %+v to the owner it already has", *gave)
		}
	})

	t.Run("a directory that cannot be given away is removed and the error returned", func(t *testing.T) {
		base := t.TempDir()
		dirSeams(t, other, refused, nil)
		p := filepath.Join(base, "a")
		if err := MkdirAll(p, 0o755); !errors.Is(err, refused) {
			t.Fatalf("err = %v, want the refusal", err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left behind root's: %v", p, err)
		}
	})

	t.Run("a directory whose owner cannot be found is removed and the error returned", func(t *testing.T) {
		base := t.TempDir()
		lost := errors.New("i/o error")
		dirSeams(t, other, nil, lost)
		p := filepath.Join(base, "a")
		if err := MkdirAll(p, 0o755); !errors.Is(err, lost) {
			t.Fatalf("err = %v, want the lookup's error", err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left behind root's: %v", p, err)
		}
	})

	t.Run("a file in the way is ENOTDIR, as with os.MkdirAll", func(t *testing.T) {
		base := t.TempDir()
		file := filepath.Join(base, "f")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, gave := dirSeams(t, other, nil, nil)
		if err := MkdirAll(file, 0o755); !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("MkdirAll over a file: err = %v, want ENOTDIR", err)
		}
		if err := MkdirAll(filepath.Join(file, "x"), 0o755); err == nil {
			t.Fatal("MkdirAll below a file succeeded")
		}
		if len(*gave) != 0 {
			t.Fatalf("gave away %+v", *gave)
		}
	})

	t.Run("Mkdir keeps os.Mkdir's ErrExist and gives nothing away", func(t *testing.T) {
		base := t.TempDir()
		_, gave := dirSeams(t, other, nil, nil)
		p := filepath.Join(base, "snap")
		if err := Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := Mkdir(p, 0o700); !errors.Is(err, os.ErrExist) {
			t.Fatalf("a second Mkdir: err = %v, want os.ErrExist", err)
		}
		if len(*gave) != 1 || (*gave)[0].path != p {
			t.Fatalf("gave away %+v, want %s once", *gave, p)
		}
	})
}

// TestPrecreateDecision drives Precreate through its seams: not root
// creates nothing; root creates the empty file, asks whose file dst is
// BEFORE path exists (so a dst that is path itself is answered by its
// directory), and gives it that owner; an entry at path is os.ErrExist and
// is left alone; and a file it cannot give away is removed.
func TestPrecreateDecision(t *testing.T) {
	other := fileOwner{uid: os.Getuid() + 1, gid: os.Getgid() + 1}
	refused := errors.New("operation not permitted")

	t.Run("a process that is not root creates nothing", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bridge.db")
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 501 },
			target: func(string) (fileOwner, error) { t.Fatal("owner asked"); return fileOwner{}, nil },
			chown:  func(*os.File, string, fileOwner) error { t.Fatal("chowned"); return nil },
		})
		if err := Precreate(p, 0o644, p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a process that is not root created %s: %v", p, err)
		}
	})

	t.Run("root creates it empty with the owner of dst, asked first", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "bridge.db")
		var existedWhenAsked bool
		var gaveDst string
		withOwnerSeams(t, &ownerSeams{
			euid: func() int { return 0 },
			target: func(dst string) (fileOwner, error) {
				_, err := os.Lstat(p)
				existedWhenAsked = err == nil
				return other, nil
			},
			chown: func(_ *os.File, dst string, o fileOwner) error {
				if o != other {
					t.Fatalf("gave %+v, want %+v", o, other)
				}
				gaveDst = dst
				return nil
			},
		})
		if err := Precreate(p, 0o640, p); err != nil {
			t.Fatal(err)
		}
		if existedWhenAsked {
			t.Fatal("the owner was asked after the file was made, so it names the file's own (root's)")
		}
		if gaveDst != p {
			t.Fatalf("chowned for %q, want %q", gaveDst, p)
		}
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			t.Fatalf("%s is not an empty regular file: %v %v", p, info, err)
		}
	})

	t.Run("an owner that already matches is left alone", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "x.tmp")
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return fileOwner{uid: os.Getuid(), gid: os.Getgid()}, nil },
			chown:  func(*os.File, string, fileOwner) error { t.Fatal("chowned to its own owner"); return nil },
		})
		if err := Precreate(p, 0o644, filepath.Join(filepath.Dir(p), "x")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("an entry already there is os.ErrExist and is left alone", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "x.tmp")
		if err := os.WriteFile(p, []byte("debris"), 0o600); err != nil {
			t.Fatal(err)
		}
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return other, nil },
			chown:  func(*os.File, string, fileOwner) error { t.Fatal("chowned"); return nil },
		})
		if err := Precreate(p, 0o644, p); !errors.Is(err, os.ErrExist) {
			t.Fatalf("err = %v, want os.ErrExist", err)
		}
		if got, _ := os.ReadFile(p); string(got) != "debris" {
			t.Fatalf("the entry already there was touched: %q", got)
		}
	})

	t.Run("a file that cannot be given away is removed and the error returned", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "x.tmp")
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return other, nil },
			chown:  func(*os.File, string, fileOwner) error { return refused },
		})
		if err := Precreate(p, 0o644, p); !errors.Is(err, refused) {
			t.Fatalf("err = %v, want the refusal", err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s left behind root's: %v", p, err)
		}
	})

	t.Run("an owner that cannot be found creates nothing", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "x.tmp")
		lost := errors.New("i/o error")
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return fileOwner{}, lost },
			chown:  func(*os.File, string, fileOwner) error { t.Fatal("chowned"); return nil },
		})
		if err := Precreate(p, 0o644, p); !errors.Is(err, lost) {
			t.Fatalf("err = %v, want the lookup's error", err)
		}
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s was made: %v", p, err)
		}
	})
}

// TestCreatingAsRootKeepsTheOwner gives real directories and files to other
// owners, so it needs root: run it in a container (CLAUDE.md, dido) with
// `go test ./internal/fsutil/ -run TestCreatingAsRootKeepsTheOwner -count=1`
// as root. It pins the owners the real seams read: through the new
// directory's parent descriptor, and a shared directory's and a move
// target's reference.
func TestCreatingAsRootKeepsTheOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it hands directories and files to other uids")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ownerOf := func(p string) fileOwner {
		t.Helper()
		info, err := os.Lstat(p)
		must(err)
		o, err := ownerFromInfo(info)
		must(err)
		return o
	}
	base := t.TempDir()
	svc := filepath.Join(base, "install")
	must(os.Mkdir(svc, 0o700))
	must(os.Chown(svc, 4242, 4243))
	variants := filepath.Join(svc, "transcoded") // not made yet: its nearest ancestor answers
	shared := filepath.Join(base, "tmp")
	must(os.Mkdir(shared, 0o755))
	must(os.Chmod(shared, 0o777|os.ModeSticky))
	mnt := filepath.Join(base, "mnt")
	must(os.Mkdir(mnt, 0o755))

	must(MkdirAll(filepath.Join(svc, "artwork", "thumbs"), 0o700))
	must(MkdirAllShared(filepath.Join(shared, "made", "1-bit-bridge-render"), 0o700, variants))
	must(MkdirAllShared(filepath.Join(mnt, "scratch"), 0o700, variants))
	must(MkdirAllLike(filepath.Join(mnt, "disk", "variants"), 0o755, variants))
	existing := filepath.Join(svc, "old.flac")
	must(os.WriteFile(existing, nil, 0o644))
	must(os.Chown(existing, 4244, 4245))
	must(Precreate(filepath.Join(svc, "bridge.db"), 0o644, filepath.Join(svc, "bridge.db")))
	must(Precreate(filepath.Join(svc, "old.flac.tmp"), 0o644, existing))

	for _, tc := range []struct {
		path string
		want fileOwner
	}{
		{filepath.Join(svc, "artwork"), fileOwner{4242, 4243}},
		{filepath.Join(svc, "artwork", "thumbs"), fileOwner{4242, 4243}},
		{filepath.Join(shared, "made"), fileOwner{4242, 4243}},                        // in the shared dir: the install's
		{filepath.Join(shared, "made", "1-bit-bridge-render"), fileOwner{4242, 4243}}, // in the install's: its parent's
		{filepath.Join(mnt, "scratch"), fileOwner{0, 0}},                              // root's own directory: its parent's
		{filepath.Join(mnt, "disk"), fileOwner{0, 0}},                                 // on the way: its parent's
		{filepath.Join(mnt, "disk", "variants"), fileOwner{4242, 4243}},               // the last: the ref's
		{filepath.Join(svc, "bridge.db"), fileOwner{4242, 4243}},                      // new: its directory's
		{filepath.Join(svc, "old.flac.tmp"), fileOwner{4244, 4245}},                   // replaces: the replaced file's
	} {
		if got := ownerOf(tc.path); got != tc.want {
			t.Errorf("%s: owner %+v, want %+v", tc.path, got, tc.want)
		}
	}
}
