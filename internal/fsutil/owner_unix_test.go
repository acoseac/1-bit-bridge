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

// ownedInfo is a FileInfo whose Sys carries only an owner, which is all
// targetOwnerFrom reads.
type ownedInfo struct {
	name     string
	uid, gid uint32
}

func (i ownedInfo) Name() string       { return i.name }
func (i ownedInfo) Size() int64        { return 0 }
func (i ownedInfo) Mode() fs.FileMode  { return 0o600 }
func (i ownedInfo) ModTime() time.Time { return time.Time{} }
func (i ownedInfo) IsDir() bool        { return false }
func (i ownedInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid, Gid: i.gid} }

// TestTargetOwnerIsTheReplacedEntryThenItsDirectory pins the precedence:
// the entry at dst decides when it exists (asked with lstat, so a symlink's
// own owner), the directory only when there is no entry, and any other
// lookup error is an error rather than a guess.
func TestTargetOwnerIsTheReplacedEntryThenItsDirectory(t *testing.T) {
	dst := filepath.Join("data", "tokens.json")
	entry := ownedInfo{name: "tokens.json", uid: 4242, gid: 4243}
	dir := ownedInfo{name: "data", uid: 5252, gid: 5253}
	failing := errors.New("i/o error")

	for _, tc := range []struct {
		name     string
		lstatErr error
		want     fileOwner
		wantErr  error
		wantDir  bool
	}{
		{name: "an existing entry keeps its own owner", want: fileOwner{4242, 4243}},
		{name: "a new file takes its directory's owner", lstatErr: fs.ErrNotExist, want: fileOwner{5252, 5253}, wantDir: true},
		{name: "a failed lookup is an error, not a guess", lstatErr: failing, wantErr: failing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var askedDir string
			lstat := func(p string) (fs.FileInfo, error) {
				if p != dst {
					t.Fatalf("lstat(%q), want lstat(%q)", p, dst)
				}
				if tc.lstatErr != nil {
					return nil, &fs.PathError{Op: "lstat", Path: p, Err: tc.lstatErr}
				}
				return entry, nil
			}
			stat := func(p string) (fs.FileInfo, error) {
				askedDir = p
				return dir, nil
			}
			got, err := targetOwnerFrom(dst, lstat, stat)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("owner = %+v, want %+v", got, tc.want)
			}
			if tc.wantDir && askedDir != filepath.Dir(dst) {
				t.Fatalf("stat(%q), want the directory %q", askedDir, filepath.Dir(dst))
			}
			if !tc.wantDir && askedDir != "" {
				t.Fatalf("stat(%q) asked, but the entry at dst decides", askedDir)
			}
		})
	}
}

// withOwnerSeams installs s for the length of the test.
func withOwnerSeams(t *testing.T, s *ownerSeams) {
	t.Helper()
	prev := activeOwnerSeams.Swap(s)
	t.Cleanup(func() { activeOwnerSeams.Store(prev) })
}

func stageFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), ".staged-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func ownerOfStaged(t *testing.T, f *os.File) fileOwner {
	t.Helper()
	o, err := stagedOwner(f)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// TestKeepOwnerDecision drives keepOwner's decisions through its seams, so
// the root path runs on every host: not root changes nothing, root gives
// the staged file the target's owner, an owner that already matches is
// left alone, and a refused chown or an owner that cannot be found is an
// error, which makes the caller abandon its write.
func TestKeepOwnerDecision(t *testing.T) {
	const dst = "/data/tokens.json"
	refused := errors.New("operation not permitted")

	t.Run("a process that is not root changes nothing", func(t *testing.T) {
		f := stageFile(t)
		called := false
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 501 },
			target: func(string) (fileOwner, error) { t.Fatal("target asked"); return fileOwner{}, nil },
			chown:  func(*os.File, string, fileOwner) error { called = true; return nil },
		})
		if err := KeepOwner(f, dst); err != nil {
			t.Fatal(err)
		}
		if called {
			t.Fatal("chown called for a process that is not root")
		}
	})

	t.Run("root gives the staged file the replaced file's owner", func(t *testing.T) {
		f := stageFile(t)
		var got []fileOwner
		var gotDst string
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return fileOwner{4242, 4243}, nil },
			chown: func(_ *os.File, d string, o fileOwner) error {
				gotDst = d
				got = append(got, o)
				return nil
			},
		})
		if err := KeepOwner(f, dst); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != (fileOwner{4242, 4243}) || gotDst != dst {
			t.Fatalf("chown calls = %+v for %q, want one to 4242:4243 for %q", got, gotDst, dst)
		}
	})

	t.Run("an owner that already matches is left alone", func(t *testing.T) {
		f := stageFile(t)
		mine := ownerOfStaged(t, f)
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return mine, nil },
			chown:  func(*os.File, string, fileOwner) error { t.Fatal("chown called"); return nil },
		})
		if err := KeepOwner(f, dst); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a refused chown is an error", func(t *testing.T) {
		f := stageFile(t)
		mine := ownerOfStaged(t, f)
		other := fileOwner{uid: mine.uid + 1, gid: mine.gid + 1}
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return other, nil },
			chown:  func(*os.File, string, fileOwner) error { return refused },
		})
		if err := KeepOwner(f, dst); !errors.Is(err, refused) {
			t.Fatalf("err = %v, want the refusal", err)
		}
	})

	t.Run("an owner that cannot be found is an error", func(t *testing.T) {
		f := stageFile(t)
		lost := errors.New("i/o error")
		withOwnerSeams(t, &ownerSeams{
			euid:   func() int { return 0 },
			target: func(string) (fileOwner, error) { return fileOwner{}, lost },
			chown:  func(*os.File, string, fileOwner) error { t.Fatal("chown called"); return nil },
		})
		if err := KeepOwner(f, dst); !errors.Is(err, lost) {
			t.Fatalf("err = %v, want the lookup's error", err)
		}
	})
}

// TestKeepOwnerAsRoot gives real files to other owners, so it needs root:
// run it in a container (CLAUDE.md, dido) with
// `go test ./internal/fsutil/ -run TestKeepOwnerAsRoot -count=1` as root.
func TestKeepOwnerAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it hands files to other uids")
	}
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Chown(dir, 4242, 4242))
	existing := filepath.Join(dir, "tokens.json")
	must(os.WriteFile(existing, []byte("[]\n"), 0o600))
	must(os.Chown(existing, 4243, 4244))
	link := filepath.Join(dir, "linked.json")
	must(os.Symlink(existing, link))
	must(os.Lchown(link, 4245, 4246))

	for _, tc := range []struct {
		name string
		dst  string
		want fileOwner
	}{
		{name: "an existing file keeps its owner", dst: existing, want: fileOwner{4243, 4244}},
		{name: "a new file takes the directory's owner", dst: filepath.Join(dir, "new.json"), want: fileOwner{4242, 4242}},
		{name: "a symlink's own owner, never its target's", dst: link, want: fileOwner{4245, 4246}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp(dir, ".staged-*")
			must(err)
			defer f.Close()
			defer os.Remove(f.Name())
			if got := ownerOfStaged(t, f); got != (fileOwner{0, 0}) {
				t.Fatalf("staged as root owned by %+v, want 0:0", got)
			}
			must(KeepOwner(f, tc.dst))
			if got := ownerOfStaged(t, f); got != tc.want {
				t.Fatalf("owner = %+v, want %+v", got, tc.want)
			}
		})
	}
}
