package fsutil

// ResolveLinks returns p with every link in it resolved, the way
// filepath.EvalSymlinks does, and on Windows also through a directory
// junction or a volume mounted in a folder.
//
// Since Go 1.23 filepath.EvalSymlinks follows only what os.Lstat reports as
// ModeSymlink, and on Windows a junction (`mklink /J`, the ordinary way to
// point a folder at another volume there, since it needs no privilege) and a
// mounted folder are ModeIrregular instead: at the end of a path it returns
// the junction as it is, and through one it fails with ENOTDIR. So a
// containment check built on it compared a junction'd library root by its
// own spelling, and a variants directory named by its real path under the
// junction's target (`D:\real\variants` against a root `C:\lib -> D:\real`)
// read as not nested; and a sidecar walk of a junction'd variants directory
// started AT the junction and saw one entry.
//
// On Windows an absolute p is opened, following every link in it, and named
// by the handle (GetFinalPathNameByHandle, the resolution the OS itself
// makes), with the `\\?\` prefix taken off so the answer is an ordinary
// drive-letter or UNC path; one the call cannot name by a drive letter (a
// volume mounted only in a folder can be one; not measured) is named by its
// volume GUID path (`\\?\Volume{…}\…`), which Go's os and filepath
// functions take as it is (measured). A p that is not there is an error
// that errors.Is reads as fs.ErrNotExist, as EvalSymlinks's is. A p that is
// there but cannot be named that way (two junctions pointing at each other,
// whose open fails with ERROR_CANT_RESOLVE_FILENAME; a filesystem that does
// not support the call) gets EvalSymlinks's answer, which is what every
// caller had before, unless that answer still ends at a link to a directory:
// that is an error, since answered as resolved it is one entry to a walk and
// its own spelling to a comparison. A relative p is always EvalSymlinks's, so
// it stays relative. Elsewhere this IS filepath.EvalSymlinks.
//
// The answer can spell the same directory differently from EvalSymlinks on
// Windows: a drive made with SUBST resolves to the path it stands for
// (measured: `Q:\sub` answers the directory's `C:\…` path, where
// EvalSymlinks keeps `Q:\sub`), and so, by the same call, does a mapped
// network drive. Both sides of a comparison go through the same function,
// so they agree; a caller that needs the configured spelling back maps it
// itself, as the sidecar walks do.
func ResolveLinks(p string) (string, error) {
	return resolveLinks(p)
}
