package fsutil

// ResolveParent splits path into the directory a create or a rename of path
// lands in, named as the OS resolves path, and path's last element. Two
// spellings of one destination answer one directory, so a lock taken beside
// a file, and a file staged to be renamed onto it, go where the file is
// whichever spelling a writer was given.
//
// On unix a link in the directory part is followed where it stands and a
// `..` after it climbs from where the link leads, as the kernel walks a
// path: with cfg/link leading to real/sub, cfg/link/../bridge.yaml is in
// real. filepath.Dir cleans that name to cfg, a directory the file is not
// in, so the directory part is never cleaned before it is resolved; a
// relative one is resolved from the working directory, by the directory's
// own links. On Windows the OS takes a `..` by the name alone, before any
// link is followed (Win32 normalises a path before the filesystem sees it),
// so the directory is filepath.Abs's, which is that same normalisation; a
// link in it is left as it is, since opening through it reaches the same
// directory.
//
// The directory must exist. A path whose last element names a directory
// (none at all, ".", "..") is refused, an *fs.PathError wrapping
// syscall.EISDIR.
func ResolveParent(path string) (dir, name string, err error) {
	return resolveParent(path)
}
