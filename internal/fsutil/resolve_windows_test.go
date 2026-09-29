//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestStripVerbatimPrefixGivesAnOrdinaryPath: GetFinalPathNameByHandle
// answers with the `\\?\` prefix, which ResolveLinks takes off so the answer
// compares with the drive-letter and UNC spellings everything else uses. A
// volume GUID path, or anything else unexpected, is an error here;
// nameFromHandle asks for the GUID form separately.
func TestStripVerbatimPrefixGivesAnOrdinaryPath(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{`\\?\C:\Users\x\Music`, `C:\Users\x\Music`, true},
		{`\\?\C:\`, `C:\`, true},
		{`\\?\UNC\nas\share\music`, `\\nas\share\music`, true},
		{`\\?\Volume{0b2cd1a1-0000-0000-0000-100000000000}\music`, "", false},
		{`C:\Users\x\Music`, "", false},
	} {
		got, err := stripVerbatimPrefix(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("stripVerbatimPrefix(%s) = %q, %v; want %q, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

// TestNameFromHandleFallsBackToTheVolumeGUIDPath: a volume mounted only in a
// folder has no drive letter for VOLUME_NAME_DOS to answer with (CodeRabbit
// on #1090), and its GUID path is what reaches the files below the mount
// point, so that is the answer then, as it is. No drive-letter answer and no
// GUID answer leaves the drive-letter error, for resolveWith to judge.
func TestNameFromHandleFallsBackToTheVolumeGUIDPath(t *testing.T) {
	const guid = `\\?\Volume{0b2cd1a1-0000-0000-0000-100000000000}\music`
	errNoDOS := errors.New("no drive letter")
	for _, c := range []struct {
		name     string
		dos      string
		dosErr   error
		guid     string
		guidErr  error
		want     string
		wantErr  error
	}{
		{name: "a drive letter", dos: `\\?\D:\music`, guid: guid, want: `D:\music`},
		{name: "no drive letter", dosErr: errNoDOS, guid: guid, want: guid},
		{name: "neither", dosErr: errNoDOS, guidErr: errors.New("no GUID either"), wantErr: errNoDOS},
		{name: "a GUID answer that is not one", dosErr: errNoDOS, guid: `\\?\C:\music`, wantErr: errNoDOS},
	} {
		got, err := nameFromHandle(func(flags uint32) (string, error) {
			if flags == volumeNameGUID {
				return c.guid, c.guidErr
			}
			return c.dos, c.dosErr
		})
		if got != c.want || !errors.Is(err, c.wantErr) || (c.wantErr == nil && err != nil) {
			t.Errorf("%s: nameFromHandle = %q, %v; want %q, %v", c.name, got, err, c.want, c.wantErr)
		}
	}
}

// TestResolveWithRefusesALinkItCouldNotResolve: a path that is there and that
// no handle could name gets EvalSymlinks's answer, unless that answer still
// ends at a link to a directory (a junction or a mounted folder
// EvalSymlinks leaves as it is): answered as resolved, a walk of it saw one
// entry (CodeRabbit on #1090), so it is refused instead. A plain directory,
// and a link that leads nowhere it can stat (a loop), keep EvalSymlinks's
// answer; a path that is not there is a not-exist error.
func TestResolveWithRefusesALinkItCouldNotResolve(t *testing.T) {
	p := `C:\lib\mnt`
	dir := stubInfo(fs.ModeDir | 0o755)
	link := stubInfo(fs.ModeIrregular | 0o666)
	errCannotName := errors.New("cannot be named")
	unnamed := func(string) (string, error) { return "", errCannotName }
	same := func(q string) (string, error) { return q, nil }
	infos := func(fi fs.FileInfo, err error) func(string) (fs.FileInfo, error) {
		return func(string) (fs.FileInfo, error) { return fi, err }
	}
	for _, c := range []struct {
		name        string
		ops         resolveOps
		want        string
		wantErr     error
		wantAnError bool
	}{
		{name: "named by its handle", want: `D:\real`, ops: resolveOps{
			finalPath: func(string) (string, error) { return `D:\real`, nil },
		}},
		{name: "not there", wantErr: fs.ErrNotExist, ops: resolveOps{
			finalPath: unnamed, stat: infos(nil, fs.ErrNotExist),
		}},
		{name: "unnamed, a plain directory", want: p, ops: resolveOps{
			finalPath: unnamed, stat: infos(dir, nil), lstat: infos(dir, nil), evalSymlinks: same,
		}},
		{name: "unnamed, a link to a directory left as it is", wantErr: errLinkNotResolved, ops: resolveOps{
			finalPath: unnamed, stat: infos(dir, nil), lstat: infos(link, nil), evalSymlinks: same,
		}},
		{name: "unnamed, a link that leads nowhere (a loop)", want: p, ops: resolveOps{
			finalPath: unnamed, stat: infos(nil, errors.New("cannot resolve")), lstat: infos(link, nil), evalSymlinks: same,
		}},
		{name: "unnamed, and EvalSymlinks fails", wantAnError: true, ops: resolveOps{
			finalPath: unnamed, stat: infos(dir, nil), lstat: infos(link, nil),
			evalSymlinks: func(string) (string, error) { return "", errors.New("not a directory") },
		}},
	} {
		got, err := resolveWith(p, c.ops)
		switch {
		case c.wantErr != nil:
			if !errors.Is(err, c.wantErr) || got != "" {
				t.Errorf("%s: resolveWith = %q, %v; want \"\" and %v", c.name, got, err, c.wantErr)
			}
		case c.wantAnError:
			if err == nil || got != "" {
				t.Errorf("%s: resolveWith = %q, %v; want \"\" and an error", c.name, got, err)
			}
		default:
			if err != nil || got != c.want {
				t.Errorf("%s: resolveWith = %q, %v; want %q", c.name, got, err, c.want)
			}
		}
	}
}

// TestAVolumeGUIDPathIsWalkable: the GUID form nameFromHandle falls back to is
// a path the rest of the bridge can use as it is. Opened the way
// finalPathName opens, a real directory's GUID path reaches its files through
// filepath.WalkDir, and a comparison made with it agrees with the
// drive-letter spelling of the same directory.
func TestAVolumeGUIDPathIsWalkable(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "Artist", "01.flac")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := openFollowing(root)
	if err != nil {
		t.Fatal(err)
	}
	guidRoot, err := finalPathNameByHandle(h, volumeNameGUID)
	windows.CloseHandle(h)
	if err != nil || !strings.HasPrefix(guidRoot, `\\?\Volume{`) {
		t.Fatalf("premise: the GUID path of %s = %q, %v", root, guidRoot, err)
	}
	var files []string
	if err := filepath.WalkDir(guidRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("WalkDir(%s): %v", guidRoot, err)
	}
	if want := filepath.Join(guidRoot, "Artist", "01.flac"); len(files) != 1 || files[0] != want {
		t.Fatalf("a walk of the GUID path found %v, want [%s]", files, want)
	}
	if IsUnderAny(filepath.Join(guidRoot, "Artist"), []string{root}) == "" {
		t.Errorf("a GUID-spelled path below %s reads as outside it", root)
	}
}
