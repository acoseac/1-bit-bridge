package fsutil

import "os"

// KeepOwner gives a file staged for a rename-into-place the owner of the
// file it will replace, when this process runs as root. Callers are the
// temp-file + rename writers of the small files a CLI rewrites beside a
// running bridge: tokens.json, adminauth.json and its login tickets,
// bridge.yaml, the TLS pair and update-state.json. The job and database
// CLIs call it too, on the files they write themselves: a cached cover
// (atomicwrite.WriteBytes), a waveform, a backup snapshot's copies and
// manifest, and a restored file (the backup package's copyFile). Each
// calls it on the staged file, while it is open and before the rename,
// as one more of its own steps (CLAUDE.md, "Atomic writes": each site
// keeps its own Chmod / Sync / parent-dir fsync).
//
// Why: the realistic way to run `bridge pair` or `bridge admin
// reset-password` on a service install is with sudo, because `bridge init`
// makes the data dir 0700 and the service user owns it. Staged as root, the
// replacement was root-owned and 0600, and the bridge running as the
// service user could no longer read the file it had just been told about:
// every console request answered 503 after a sign-out, bearer tokens were
// checked against a stale list, and the next restart could not open either
// store. The owner a write keeps is the one the replaced entry already has
// (os.Lstat, so a symlink's own owner and never its target's), and for a
// file that does not exist yet, the owner of the directory it is created
// in. A process that is not root changes nothing here: it cannot give a
// file away, and every file it creates is already its own.
//
// dst is the path the staged file will be renamed onto. On Windows, where
// a file takes its directory's ACL, KeepOwner does nothing.
//
// It returns an error only when running as root and the staged file cannot
// be given the owner it needs (root_squash NFS, where root's files belong
// to nobody, is one way): the caller then abandons the write, so the file
// the service reads stays one it can read. A filesystem that reports one
// owner for every file (vfat or exFAT mounted for one uid) never reaches
// the chown: the staged file already reports the owner wanted.
func KeepOwner(staged *os.File, dst string) error {
	return keepOwner(staged, dst)
}

// MkdirAll creates path and any parents it lacks, as os.MkdirAll does.
// Run as root, it gives each directory it creates the owner of the
// directory it creates it in, which is how the job and database CLIs run
// with sudo over a service install leave directories the service can
// write: the artwork cache, the variants tree, the waveforms, the backups
// and a store's own directory. A process that is not root calls
// os.MkdirAll and nothing else.
//
// The owner is read through the descriptor the directory is created
// through (an os.Root on its parent), never from a path looked up
// beforehand: whatever the path resolves to when the directory is made,
// the new directory takes the owner of the directory that holds it. So
// the owner of a directory can gain nothing from it that it did not have:
// it could have made that directory itself. A directory it creates and
// then cannot give away (root_squash NFS) is removed again, empty, and the
// error returned, so nothing root's is left where the service writes. A
// directory somebody else made in the meantime is not ours to give, and is
// left as it is. On Windows, where a directory takes its parent's ACL, it
// is os.MkdirAll.
func MkdirAll(path string, perm os.FileMode) error {
	return mkdirAll(path, perm, dirOwnerParent, "")
}

// Mkdir is os.Mkdir with MkdirAll's owner: run as root, the directory
// takes the owner of the directory it is created in. Its error is
// os.Mkdir's, so a caller can still tell os.ErrExist apart (the backup
// package's unique snapshot directory does).
func Mkdir(path string, perm os.FileMode) error {
	return mkdir(path, perm, dirOwnerParent, "", true)
}

// MkdirAllShared is MkdirAll for a directory the install keeps OUTSIDE
// itself, in a shared directory: the DSD render's Stage A scratch, whose
// default home is the OS temp dir. A directory it creates in a directory
// anyone may create entries in (world-writable and searchable, as /tmp
// is) takes the owner of ref, the install directory it serves, or of
// ref's nearest ancestor while ref does not exist yet. There the parent's
// owner (root, for /tmp) says nothing about who uses the entry, and
// MkdirAll's own answer left a root-owned 0700 scratch directory in which
// the service could create nothing, so every DSD render of the running
// bridge failed. Anybody could have created that directory in such a
// parent, so giving it to ref's owner gives nobody anything. Every other
// directory takes its parent's owner, as with MkdirAll, which is what a
// configured temp dir the service owns gets.
func MkdirAllShared(path string, perm os.FileMode, ref string) error {
	return mkdirAll(path, perm, dirOwnerShared, ref)
}

// MkdirAllLike is MkdirAll whose LAST directory, when it creates it,
// takes the owner of ref (or of ref's nearest ancestor while ref does not
// exist): for a directory that replaces ref somewhere ref's owner does not
// own, as a `bridge variants move --to` destination under a mount point
// root owns replaces the variants directory. That is what mv keeps when it
// moves a directory as root. The directories between take their parent's
// owner, as with MkdirAll. Only the command line names such a path, never
// the service's config, which the service user can write.
func MkdirAllLike(path string, perm os.FileMode, ref string) error {
	return mkdirAll(path, perm, dirOwnerLastLikeRef, ref)
}

// Precreate creates the empty file path for a writer that is not this
// process (a child process such as sox, or SQLite) and, run as root, gives
// it the owner KeepOwner would give a file staged for dst: the owner of the
// entry at dst, or of dst's directory when there is none. The writer then
// fills the file it finds, which keeps the owner: sox opens its output with
// O_TRUNC, SQLite opens an existing database file, and VACUUM INTO writes
// into an empty one. Without it, a file root's child creates is root's
// (every rendition `sudo bridge upscale` wrote was), and so is a database
// SQLite creates, with the -wal and -shm SQLite then gives the database's
// owner. dst may be path itself; its owner is looked up before path is
// created. The file is created O_EXCL, so an entry already at path is an
// error that wraps os.ErrExist and nothing is written through a symlink.
//
// A process that is not root does nothing here and returns nil: whatever
// its writer creates is already its own. On Windows it does nothing.
func Precreate(path string, perm os.FileMode, dst string) error {
	return precreate(path, perm, dst)
}

// dirOwnerPolicy says which owner a directory the helpers create takes,
// when this process is root.
type dirOwnerPolicy int

const (
	// dirOwnerParent: the owner of the directory it is created in
	// (MkdirAll, Mkdir).
	dirOwnerParent dirOwnerPolicy = iota
	// dirOwnerShared: as dirOwnerParent, except in a directory anyone may
	// create entries in, where ref's (MkdirAllShared).
	dirOwnerShared
	// dirOwnerLastLikeRef: as dirOwnerParent, except the last directory
	// of the path, which takes ref's (MkdirAllLike).
	dirOwnerLastLikeRef
)

// OwnerChange is one change KeepOwner, Precreate or a directory helper
// made, as SimulateRootForTest records it: the path the file was going to
// replace (KeepOwner, Precreate) or the directory created (MkdirAll and its
// kin), and the owner it was given.
type OwnerChange struct {
	Dst      string
	UID, GID int
}
