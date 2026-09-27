package fsutil

import "os"

// KeepOwner gives a file staged for a rename-into-place the owner of the
// file it will replace, when this process runs as root. Callers are the
// temp-file + rename writers of the small files a CLI rewrites beside a
// running bridge: tokens.json, adminauth.json and its login tickets,
// bridge.yaml, the TLS pair and update-state.json. Each calls it on the
// staged file, while it is open and before the rename, as one more of its
// own steps (CLAUDE.md, "Atomic writes": each site keeps its own Chmod /
// Sync / parent-dir fsync).
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

// OwnerChange is one change KeepOwner made, as SimulateRootForTest records
// it: the path the staged file was going to replace, and the owner it was
// given.
type OwnerChange struct {
	Dst      string
	UID, GID int
}
