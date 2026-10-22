package conf

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// errOutside is a path that resolved outside the root it was read under.
// The file is not opened, and the error names no target path.
var errOutside = errors.New("path leaves the root")

// errNotFound is a file an include looked for and did not find.
// include_optional treats it as success.
var errNotFound = errors.New("file not found")

// errUnreadable is an include path that could not be stat'd for a
// reason other than absence. The host path stays out of the text: a
// path can hold a password, and the problem already quotes the name
// from the file. Both directives report it.
var errUnreadable = errors.New("unreadable")

// errTooLong is a file longer than MaxConfBytes.
var errTooLong = fmt.Errorf("longer than %d bytes", MaxConfBytes)

// Locate is the filesystem MPD's Linux build searches for mpd.conf, rooted
// at Root so a test can hand it a tree captured from a box.
//
// The order is MPD 0.24.4's, from src/CommandLine.cxx ParseCommandLine and
// src/fs/glue/StandardDirectory.cxx GetUserConfigDir, the USE_XDG build a
// Linux box runs (the Apple build looks in ~/Library/Application Support
// instead, and is not what Volumio or moOde runs):
//
//  1. $XDG_CONFIG_HOME/mpd/mpd.conf, when that variable is an absolute
//     path of a directory that exists. If it is set and the directory
//     exists, ~/.config is not also tried, even when the file is absent.
//  2. Otherwise $HOME/.config/mpd/mpd.conf, when $HOME and $HOME/.config
//     both exist as directories.
//  3. $HOME/.mpdconf, when $HOME exists.
//  4. $HOME/.mpd/mpd.conf, when $HOME exists.
//  5. /etc/mpd.conf, the compiled SYSTEM_CONFIG_FILE_LOCATION.
//
// The first of those files that exists is the one MPD reads. Home and
// XDGConfigHome are the logical paths MPD would see ("/home/pi"), and
// Root is the host directory that stands for "/".
type Locate struct {
	Root          string
	Home          string
	XDGConfigHome string
}

func (l Locate) root() string {
	if l.Root == "" {
		return string(filepath.Separator)
	}
	return l.Root
}

// Candidates is the files Locate would try, in MPD's order, whether or
// not each one exists. A directory that does not exist contributes no
// candidate (MPD's TryFile is never called on a null base).
func (l Locate) Candidates() []string {
	var out []string
	homeOK := logicalAbs(l.Home) && l.dirExists(l.Home)
	xdgOK := logicalAbs(l.XDGConfigHome) && l.dirExists(l.XDGConfigHome)
	switch {
	case xdgOK:
		out = append(out, joinLogical(l.XDGConfigHome, "mpd/mpd.conf"))
	case homeOK && l.dirExists(joinLogical(l.Home, ".config")):
		out = append(out, joinLogical(l.Home, ".config/mpd/mpd.conf"))
	}
	if homeOK {
		out = append(out, joinLogical(l.Home, ".mpdconf"), joinLogical(l.Home, ".mpd/mpd.conf"))
	}
	out = append(out, SystemPath)
	return out
}

// Find returns the first candidate inside Root that can be a conf file.
func (l Locate) Find() (logical, host string, ok bool) {
	for _, c := range l.Candidates() {
		h := hostPath(l.root(), c)
		if err := staysInRoot(l.root(), h); err != nil {
			continue
		}
		fi, err := os.Stat(h)
		if err != nil || !confCandidate(fi.Mode()) {
			continue
		}
		return c, h, true
	}
	return "", "", false
}

// ReadHost reads the configuration file at host. When root is set, host
// must be inside it, and the File's path is the logical path under that
// root ("/etc/mpd.conf" for <root>/etc/mpd.conf). An empty root reads the
// host filesystem.
func ReadHost(root, host string) (File, error) {
	if root == "" {
		root = string(filepath.Separator)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return File{}, err
	}
	absHost, err := filepath.Abs(host)
	if err != nil {
		return File{}, err
	}
	// Both sides are resolved before the logical path is computed.
	// filepath.Abs does not follow a symlink, so a root of /private/var
	// and a host of /var (macOS /var → /private/var), or the reverse,
	// make Rel walk out with "../" and the stored path is not the
	// path inside the root.
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return File{}, err
	}
	resolvedHost, err := filepath.EvalSymlinks(absHost)
	if err != nil {
		return File{}, err
	}
	if !lexicalInside(resolvedRoot, resolvedHost) {
		return File{}, errOutside
	}
	return ReadFile(root, logicalFromHost(resolvedRoot, resolvedHost), absHost)
}

// ReadFile reads the configuration file at host, following includes, and
// refuses a path that leaves root. logical is the path stored on the File
// ("/etc/mpd.conf").
func ReadFile(root, logical, host string) (File, error) {
	if root == "" {
		root = string(filepath.Separator)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return File{}, err
	}
	host, err = filepath.Abs(host)
	if err != nil {
		return File{}, err
	}
	if err := staysInRoot(root, host); err != nil {
		return File{}, err
	}
	// Stack keys are symlink-resolved, so an include that names the same
	// file by another path is the same entry. On macOS that also folds
	// /tmp onto /private/tmp.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return File{}, err
	}
	host, err = filepath.EvalSymlinks(host)
	if err != nil {
		return File{}, err
	}
	text, err := readCapped(host)
	if err != nil {
		return File{}, err
	}
	load := &loader{root: root, stack: map[string]struct{}{host: {}}}
	f := parseFile(text, logical, filepath.Dir(host), load)
	f.Path = logical
	f.finish()
	return f, nil
}

// open reads the include at path, a path resolve gave. A path that is not
// there is errNotFound, and one that cannot be looked at or opened is
// errUnreadable: the open's error names the host path, an include problem
// already quotes the name from the file, and that path can hold a password.
func (l *loader) open(path string) (string, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errNotFound
		}
		return "", errUnreadable
	}
	if err := staysInRoot(l.root, path); err != nil {
		return "", err
	}
	text, err := readCapped(path)
	if err != nil && !errors.Is(err, errNotFound) && !errors.Is(err, errTooLong) {
		return "", errUnreadable
	}
	return text, err
}

// confCandidate reports whether a stat mode can be an mpd.conf.
// A directory, a pipe, a socket or a device cannot. A Windows cloud
// placeholder is ModeIrregular and is still a file, so this is not
// IsRegular: that would skip every OneDrive file on demand. os.Stat
// follows a symlink, so the mode is the target's.
func confCandidate(m fs.FileMode) bool {
	return m&(fs.ModeDir|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeDevice) == 0
}

func (l Locate) dirExists(logical string) bool {
	h := hostPath(l.root(), logical)
	if err := staysInRoot(l.root(), h); err != nil {
		return false
	}
	fi, err := os.Stat(h)
	return err == nil && fi.IsDir()
}

func (l *loader) resolve(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(name, 0) {
		return "", errors.New("invalid path")
	}
	var host string
	if strings.HasPrefix(name, "/") {
		host = hostPath(l.root, name)
	} else {
		if dir == "" {
			return "", errors.New("no base file")
		}
		host = filepath.Join(dir, filepath.FromSlash(name))
	}
	host = filepath.Clean(host)
	if !lexicalInside(l.root, host) {
		return "", errOutside
	}
	// A symlink is the file it names. Refuse one that leaves the root,
	// and let a missing file through as itself so include_optional can
	// ignore it.
	if _, err := os.Lstat(host); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return host, nil
		}
		return "", errUnreadable
	}
	if err := staysInRoot(l.root, host); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(host)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func logicalAbs(p string) bool {
	return strings.HasPrefix(p, "/")
}

func joinLogical(base, rel string) string {
	return strings.TrimRight(base, "/") + "/" + rel
}

// hostPath maps a logical absolute path ("/etc/mpd.conf") into root.
func hostPath(root, logical string) string {
	logical = filepath.ToSlash(logical)
	if !strings.HasPrefix(logical, "/") {
		logical = "/" + logical
	}
	return filepath.Join(root, filepath.FromSlash(logical[1:]))
}

func logicalFromHost(root, host string) string {
	rel, err := filepath.Rel(root, host)
	if err != nil {
		return filepath.ToSlash(host)
	}
	return "/" + filepath.ToSlash(rel)
}

func lexicalInside(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// staysInRoot reports whether path, after following symlinks, is the root
// or a file inside it. Both sides are made absolute first.
func staysInRoot(root, path string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if !lexicalInside(resolvedRoot, resolved) {
		return errOutside
	}
	return nil
}

// Exists reports whether logical (an absolute path like "/volumio") is
// inside root after symlinks are followed. A link that leaves root does
// not count.
func Exists(root, logical string) bool {
	if root == "" {
		root = string(filepath.Separator)
	}
	h := hostPath(root, logical)
	if err := staysInRoot(root, h); err != nil {
		return false
	}
	_, err := os.Stat(h)
	return err == nil
}

// ReadLogical reads a file inside root, refusing one that leaves it, and
// stops at limit bytes.
func ReadLogical(root, logical string, limit int64) ([]byte, error) {
	if root == "" {
		root = string(filepath.Separator)
	}
	if limit <= 0 {
		limit = MaxConfBytes
	}
	h := hostPath(root, logical)
	if err := staysInRoot(root, h); err != nil {
		return nil, err
	}
	f, _, err := fsutil.OpenAsFile(h)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLong
	}
	return b, nil
}

// readCapped reads the file at path whole, at most MaxConfBytes, opened as
// a file (fsutil.OpenAsFile): a named pipe, a device or a directory is
// refused at once and never waited on, since nothing cancels an open that
// waits for a pipe's writer.
func readCapped(path string) (string, error) {
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errNotFound
		}
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, MaxConfBytes+1))
	if err != nil {
		return "", err
	}
	if len(b) > MaxConfBytes {
		return "", errTooLong
	}
	return string(b), nil
}
