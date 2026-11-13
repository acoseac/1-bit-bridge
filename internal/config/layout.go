package config

import (
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The bridge's own files against its library roots (backlog B884). A library
// root is often a network or removable mount, and while one stops answering,
// every bridge command that reads what is inside it waits on it: serve, the
// doctor and status among them, which read the config file and the data
// directory (the database, the install marker, the player's storage, the
// tokens). So a config file or a data directory inside a root takes every one
// of those commands down with the mount. bridge init, bridge doctor and serve
// say so, each in its own place (cmd/bridge's install_layout.go), and none of
// them refuses: an install keeps working whatever its layout.
//
// The check is lexical, and touches nothing: no stat, no link resolved, no
// file opened, so it cannot wait on a root whose mount does not answer. Each
// path is made absolute as Load makes it, against the config file's directory
// (ResolvePath), and cleaned; a path is inside a root when the root's names
// are its first ones, compared as the OS spells and compares paths
// (pathForms), never by a string prefix, so /music2 is not inside /music. What
// the check does not resolve it does not see: a layout joined only through a
// symbolic link, a Windows junction or short name, a bind mount, or a case a
// macOS volume folds, is missed. This file runs on strings alone, which
// TestTheLayoutCheckRunsOnStringsAlone holds it to: it calls nothing of os,
// path/filepath or fsutil, and parses each OS's forms itself, so its table
// runs every OS's forms on any host.

// InstallLayout is where the config file and the data directory lie against
// the library roots, as LayoutOf found them: each of the two inside the first
// root, in libraryRoots order, that holds it, or inside none.
type InstallLayout struct {
	configFile, dataDir string
	roots               []string
	forms               pathForms
	// configIn and dataIn are the place in roots of the first root that holds
	// the config file and the data directory, -1 for none.
	configIn, dataIn int
}

// LayoutOf compares the config file at configFile, the data directory dataDir
// and the library roots, as this OS spells and compares paths. configFile is
// the config's name made absolute, as the caller opened it; dataDir and roots
// are as Load gives them, a relative one resolved against configFile's
// directory as Load would resolve it. A path that cannot be made absolute (a
// relative one beside a configFile that is not absolute, or none at all) is
// inside no root.
func LayoutOf(configFile, dataDir string, roots []string) InstallLayout {
	return layoutOn(runtime.GOOS, configFile, dataDir, roots)
}

// layoutOn is LayoutOf for the OS goos: its forms, whatever the host's.
func layoutOn(goos, configFile, dataDir string, roots []string) InstallLayout {
	f := formsOf(goos)
	l := InstallLayout{configFile: configFile, dataDir: dataDir, roots: roots, forms: f, configIn: -1, dataIn: -1}
	cfg, base := "", ""
	if f.isAbs(configFile) {
		cfg = f.clean(configFile)
		base = f.dir(cfg)
	}
	data, dataOK := f.absolute(base, dataDir)
	for i, r := range roots {
		root, ok := f.absolute(base, r)
		if !ok {
			continue
		}
		if l.configIn < 0 && cfg != "" && f.within(cfg, root) {
			l.configIn = i
		}
		if l.dataIn < 0 && dataOK && f.within(data, root) {
			l.dataIn = i
		}
	}
	return l
}

// Inside says whether the config file or the data directory lies inside a
// library root.
func (l InstallLayout) Inside() bool { return l.configIn >= 0 || l.dataIn >= 0 }

// Summary names what lies inside which root, "" where nothing does: the
// doctor's install-layout line. byPath names the config file, the data
// directory and the root by their paths, the root beside its place in
// libraryRoots: the CLI's own output, where B505's rule takes a path the
// operator configured. Without it they are named by their settings alone, as
// every other line about the config in the journal names them.
func (l InstallLayout) Summary(byPath bool) string {
	switch {
	case l.configIn >= 0 && l.dataIn == l.configIn:
		return l.configNamed(byPath) + " and " + l.dataNamed(byPath) + " are inside " + l.rootNamed(l.configIn, byPath)
	case l.configIn >= 0 && l.dataIn >= 0:
		return l.configNamed(byPath) + " is inside " + l.rootNamed(l.configIn, byPath) +
			", and " + l.dataNamed(byPath) + " inside " + l.rootNamed(l.dataIn, byPath)
	case l.configIn >= 0:
		return l.configNamed(byPath) + " is inside " + l.rootNamed(l.configIn, byPath)
	case l.dataIn >= 0:
		return l.dataNamed(byPath) + " is inside " + l.rootNamed(l.dataIn, byPath)
	}
	return ""
}

// layoutRisk is why a file of the bridge's own inside a root is a risk.
const layoutRisk = "a library root is often a mount, and while one stops answering, every bridge command " +
	"that reads what is inside it waits on it, serve, doctor and status among them"

// Advice is why the layout Summary names is a risk, and what to do, "" where
// nothing lies inside a root: the doctor's hint. The fix is for what lies
// inside: the config file is moved, keeping where its relative paths point,
// since dataDir is DefaultDataDir beside it unless set; the data directory is
// moved, and dataDir set to it. defaultDir, the platform's default config
// directory or "", is offered as one place to move to (the data directory's
// default below it), where it lies outside every root; a suggestion, never a
// promise that it fixes anything.
func (l InstallLayout) Advice(byPath bool, defaultDir string) string {
	var fix string
	switch {
	case l.configIn >= 0 && l.dataIn >= 0:
		fix = "move bridge.yaml and the data directory outside every library root" + l.suggestion(defaultDir, "") +
			", keeping dataDir pointed at the data directory: a relative dataDir (" + DefaultDataDir +
			" unless set) resolves beside bridge.yaml"
	case l.configIn >= 0:
		fix = "move bridge.yaml outside every library root" + l.suggestion(defaultDir, "") +
			", first setting dataDir in it to the data directory's absolute path" + l.pathIf(byPath, l.dataDir) +
			": a relative dataDir resolves beside bridge.yaml"
	case l.dataIn >= 0:
		fix = "move the data directory outside every library root" + l.suggestion(defaultDir, "data") +
			", and set dataDir in bridge.yaml to its new path"
	default:
		return ""
	}
	return layoutRisk + "; " + fix
}

// Line is the layout's one line, Summary and Advice: what bridge init prints
// and serve logs. "" where nothing lies inside a root.
func (l InstallLayout) Line(byPath bool, defaultDir string) string {
	if !l.Inside() {
		return ""
	}
	return l.Summary(byPath) + ": " + l.Advice(byPath, defaultDir)
}

func (l InstallLayout) configNamed(byPath bool) string {
	return "the config file" + l.pathIf(byPath, l.configFile)
}

func (l InstallLayout) dataNamed(byPath bool) string {
	if byPath {
		return "the data directory " + l.dataDir
	}
	return "the data directory (dataDir)"
}

// rootNamed names the root at i in libraryRoots by its place there, and by
// its path where byPath asks.
func (l InstallLayout) rootNamed(i int, byPath bool) string {
	place := "libraryRoots[" + strconv.Itoa(i) + "]"
	if byPath {
		return "library root " + l.roots[i] + " (" + place + ")"
	}
	return "library root " + place
}

func (l InstallLayout) pathIf(byPath bool, p string) string {
	if !byPath || p == "" {
		return ""
	}
	return " " + p
}

// suggestion offers dir, or the entry below it named below, as one place to
// move to, where it is an absolute path outside every root, and nothing
// otherwise.
func (l InstallLayout) suggestion(dir, below string) string {
	f := l.forms
	if dir == "" || !f.isAbs(dir) {
		return ""
	}
	place := f.clean(dir)
	if below != "" {
		place = f.join(place, below)
	}
	base := ""
	if f.isAbs(l.configFile) {
		base = f.dir(f.clean(l.configFile))
	}
	for _, r := range l.roots {
		if root, ok := f.absolute(base, r); ok && f.within(place, root) {
			return ""
		}
	}
	return ", to " + place + " for example (the platform's default)"
}

// pathForms is how one OS spells paths and compares names, parsed here from
// strings alone: POSIX's, one root and the slash, compared byte for byte, or
// Windows', volume names (a drive letter, a UNC share, a device path) and
// both slashes, compared as its volumes compare names. It is a port of Go's
// own (internal/filepathlite and path/filepath, go1.27.3) for IsAbs, Clean,
// Dir and Join, which Load resolves paths with, taken whatever the host:
// filepath's functions take the host's forms alone.
type pathForms struct{ windows bool }

// formsOf is goos's forms: Windows' for "windows", POSIX's for any other.
func formsOf(goos string) pathForms { return pathForms{windows: goos == "windows"} }

func (f pathForms) isSep(c byte) bool { return c == '/' || f.windows && c == '\\' }

func (f pathForms) sep() byte {
	if f.windows {
		return '\\'
	}
	return '/'
}

func (f pathForms) fromSlash(p string) string {
	if !f.windows {
		return p
	}
	return strings.ReplaceAll(p, "/", `\`)
}

// isAbs is filepath.IsAbs.
func (f pathForms) isAbs(p string) bool {
	if !f.windows {
		return strings.HasPrefix(p, "/")
	}
	l := f.volumeLen(p)
	if l == 0 {
		return false
	}
	// A volume name that starts with two separators (UNC, a device path) is
	// absolute whatever follows; a drive letter only with a separator after.
	if f.isSep(p[0]) && f.isSep(p[1]) {
		return true
	}
	p = p[l:]
	return p != "" && f.isSep(p[0])
}

// volumeLen is the length of p's volume name: 0 on POSIX; on Windows a drive
// letter's two bytes, a UNC path's host and share, or a device path's prefix
// and the name after it (filepathlite's volumeNameLen).
func (f pathForms) volumeLen(p string) int {
	if !f.windows {
		return 0
	}
	switch {
	case len(p) >= 2 && p[1] == ':':
		return 2
	case len(p) == 0 || !f.isSep(p[0]):
		return 0
	case f.hasPrefixFold(p, `\\.`) || f.hasPrefixFold(p, `\\?`) || f.hasPrefixFold(p, `\??`):
		switch {
		case len(p) == 3:
			return 3
		case f.hasPrefixFold(p[4:], `UNC`):
			return f.validVolumeLen(p, f.uncLen(p, len(`\\.\UNC\`)))
		}
		_, rest, ok := f.cut(p[4:])
		if !ok {
			return f.validVolumeLen(p, len(p))
		}
		return f.validVolumeLen(p, len(p)-len(rest)-1)
	case len(p) >= 2 && f.isSep(p[1]):
		return f.validVolumeLen(p, f.uncLen(p, 2))
	}
	return 0
}

// validVolumeLen is n, or 0 where p[:n] holds a ".." name.
func (f pathForms) validVolumeLen(p string, n int) int {
	for rest := p[:n]; rest != ""; {
		var name string
		name, rest, _ = f.cut(rest)
		if name == ".." {
			return 0
		}
	}
	return n
}

// hasPrefixFold says s begins with prefix, ignoring ASCII case and taking
// either slash for a separator, and is that prefix or has a separator after
// it.
func (f pathForms) hasPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if f.isSep(prefix[i]) {
			if !f.isSep(s[i]) {
				return false
			}
		} else if asciiUpper(prefix[i]) != asciiUpper(s[i]) {
			return false
		}
	}
	return len(s) == len(prefix) || f.isSep(s[len(prefix)])
}

// uncLen is the length of a UNC volume whose host starts at prefixLen: up to
// the separator after its share, or the whole of p.
func (f pathForms) uncLen(p string, prefixLen int) int {
	count := 0
	for i := prefixLen; i < len(p); i++ {
		if f.isSep(p[i]) {
			count++
			if count == 2 {
				return i
			}
		}
	}
	return len(p)
}

// cut is p around its first separator.
func (f pathForms) cut(p string) (before, after string, found bool) {
	for i := 0; i < len(p); i++ {
		if f.isSep(p[i]) {
			return p[:i], p[i+1:], true
		}
	}
	return p, "", false
}

// cleanBuf is filepathlite's lazybuf: what Clean writes, read from the path
// until it first differs from it, and a buffer the path's length after,
// whose bytes past what is written stay as they were, which Windows'
// postClean reads (as Go's does).
type cleanBuf struct {
	path string
	buf  []byte
	w    int
}

func (b *cleanBuf) at(i int) byte {
	if b.buf != nil {
		return b.buf[i]
	}
	return b.path[i]
}

func (b *cleanBuf) add(c byte) {
	if b.buf == nil {
		if b.w < len(b.path) && b.path[b.w] == c {
			b.w++
			return
		}
		b.buf = make([]byte, len(b.path))
		copy(b.buf, b.path[:b.w])
	}
	if b.w < len(b.buf) {
		b.buf[b.w] = c
	} else {
		b.buf = append(b.buf, c)
	}
	b.w++
}

func (b *cleanBuf) prepend(prefix ...byte) {
	b.buf = append(append(make([]byte, 0, len(prefix)+len(b.buf)), prefix...), b.buf...)
	b.w += len(prefix)
}

func (b *cleanBuf) result(orig string, volLen int) string {
	if b.buf == nil {
		return orig[:volLen+b.w]
	}
	return orig[:volLen] + string(b.buf[:b.w])
}

// clean is filepath.Clean.
func (f pathForms) clean(p string) string {
	orig := p
	volLen := f.volumeLen(p)
	p = p[volLen:]
	if p == "" {
		if volLen > 1 && f.isSep(orig[0]) && f.isSep(orig[1]) {
			return f.fromSlash(orig)
		}
		return orig + "."
	}
	rooted := f.isSep(p[0])
	n := len(p)
	out := cleanBuf{path: p}
	r, dotdot := 0, 0
	if rooted {
		out.add(f.sep())
		r, dotdot = 1, 1
	}
	for r < n {
		switch {
		case f.isSep(p[r]):
			r++
		case p[r] == '.' && (r+1 == n || f.isSep(p[r+1])):
			r++
		case p[r] == '.' && p[r+1] == '.' && (r+2 == n || f.isSep(p[r+2])):
			r += 2
			switch {
			case out.w > dotdot:
				out.w--
				for out.w > dotdot && !f.isSep(out.at(out.w)) {
					out.w--
				}
			case !rooted:
				if out.w > 0 {
					out.add(f.sep())
				}
				out.add('.')
				out.add('.')
				dotdot = out.w
			}
		default:
			if rooted && out.w != 1 || !rooted && out.w != 0 {
				out.add(f.sep())
			}
			for ; r < n && !f.isSep(p[r]); r++ {
				out.add(p[r])
			}
		}
	}
	if out.w == 0 {
		out.add('.')
	}
	f.postClean(&out, volLen)
	return f.fromSlash(out.result(orig, volLen))
}

// postClean is Windows' own: a relative path that Clean wrote whose first
// name holds a colon is given a .\ in front, and one that begins \?? a \. in
// front, so neither reads as a volume.
func (f pathForms) postClean(out *cleanBuf, volLen int) {
	if !f.windows || volLen != 0 || out.buf == nil {
		return
	}
	for _, c := range out.buf {
		if f.isSep(c) {
			break
		}
		if c == ':' {
			out.prepend('.', '\\')
			return
		}
	}
	if len(out.buf) >= 3 && f.isSep(out.buf[0]) && out.buf[1] == '?' && out.buf[2] == '?' {
		out.prepend('\\', '.')
	}
}

// dir is filepath.Dir.
func (f pathForms) dir(p string) string {
	volLen := f.volumeLen(p)
	vol := f.fromSlash(p[:volLen])
	i := len(p) - 1
	for i >= volLen && !f.isSep(p[i]) {
		i--
	}
	d := f.clean(p[volLen : i+1])
	if d == "." && volLen > 2 {
		return vol
	}
	return vol + d
}

// join is filepath.Join of base, which is not empty, and p, which is not
// either.
func (f pathForms) join(base, p string) string {
	if !f.windows {
		return f.clean(base + "/" + p)
	}
	var b strings.Builder
	b.WriteString(base)
	switch last := base[len(base)-1]; {
	case f.isSep(last):
		// Leading separators of p are dropped rather than make a UNC path
		// of names that are none, and \ before ?? becomes \.\?? rather than a
		// device path.
		for len(p) > 0 && f.isSep(p[0]) {
			p = p[1:]
		}
		if b.Len() == 1 && strings.HasPrefix(p, "??") && (len(p) == 2 || f.isSep(p[2])) {
			b.WriteString(`.\`)
		}
	case last == ':':
		// A drive's own directory: C: and f are C:f.
	default:
		b.WriteByte('\\')
	}
	b.WriteString(p)
	return f.clean(b.String())
}

// absolute is p as Load resolves it against base, the config file's
// directory (ResolvePath): cleaned where it is absolute, joined to base where
// it is not, and false for an empty p or a relative one with no absolute base
// to resolve it against.
func (f pathForms) absolute(base, p string) (string, bool) {
	switch {
	case p == "":
		return "", false
	case f.isAbs(p):
		return f.clean(p), true
	case base == "" || !f.isAbs(base):
		return "", false
	}
	return f.join(base, p), true
}

// within is the predicate: p is root, or lies inside it. Both are absolute and
// cleaned, and compared name by name below their volumes, never as strings:
// /music2 is not inside /music.
func (f pathForms) within(p, root string) bool {
	if !f.isAbs(p) || !f.isAbs(root) {
		return false
	}
	pVol, pNames := f.split(p)
	rootVol, rootNames := f.split(root)
	if !f.sameName(f.canonicalVolume(pVol), f.canonicalVolume(rootVol)) || len(rootNames) > len(pNames) {
		return false
	}
	for i, name := range rootNames {
		if !f.sameName(name, pNames[i]) {
			return false
		}
	}
	return true
}

// split is p's volume and the names below it.
func (f pathForms) split(p string) (vol string, names []string) {
	n := f.volumeLen(p)
	for rest := p[n:]; rest != ""; {
		var name string
		name, rest, _ = f.cut(rest)
		if name != "" {
			names = append(names, name)
		}
	}
	return p[:n], names
}

// canonicalVolume is how Windows reads a volume name: a device path of a
// drive letter (\\?\C:, \\.\C:, \??\C:) is that drive, and one of a UNC share
// (\\?\UNC\host\share) is \\host\share; any other is as it is. POSIX has none.
func (f pathForms) canonicalVolume(vol string) string {
	if !f.windows || len(vol) <= 4 ||
		!(f.hasPrefixFold(vol, `\\.`) || f.hasPrefixFold(vol, `\\?`) || f.hasPrefixFold(vol, `\??`)) {
		return vol
	}
	rest := vol[4:]
	switch {
	case len(rest) == 2 && rest[1] == ':':
		return rest
	case len(rest) > 4 && f.hasPrefixFold(rest, `UNC`):
		return `\\` + rest[4:]
	}
	return vol
}

// sameName says a and b name one entry: byte for byte on POSIX; on Windows as
// its volumes compare names, each character of the Basic Multilingual Plane
// upper-cased by Unicode's simple mapping (NTFS's own table maps UTF-16 code
// units alone), and a byte that is not UTF-8 compared as it is. A case a
// macOS volume folds is not folded: that is the volume's to say, and asking
// it is a filesystem touch.
func (f pathForms) sameName(a, b string) bool {
	if !f.windows {
		return a == b
	}
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		switch {
		case ra == utf8.RuneError && na == 1, rb == utf8.RuneError && nb == 1:
			if a[:na] != b[:nb] {
				return false
			}
		case upcase(ra) != upcase(rb):
			return false
		}
		a, b = a[na:], b[nb:]
	}
	return a == "" && b == ""
}

func upcase(r rune) rune {
	if r > 0xFFFF {
		return r
	}
	return unicode.ToUpper(r)
}

func asciiUpper(c byte) byte {
	if 'a' <= c && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}
