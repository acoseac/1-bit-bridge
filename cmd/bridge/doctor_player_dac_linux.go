//go:build linux

package main

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// statDACNode is lstat's answer for the node at path, its link not followed,
// as the player's open looks at it (alsapcm refuses a node that is a link):
// the group that owns it, and whether it is a character device.
func statDACNode(path string) (dacNode, error) {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return dacNode{}, &fs.PathError{Op: "lstat", Path: path, Err: err}
	}
	return dacNode{gid: st.Gid, char: st.Mode&unix.S_IFMT == unix.S_IFCHR}, nil
}

// accessForPlayback asks the kernel whether this process may open the node
// at path for reading and writing, as the player's open does (alsapcm opens
// it O_RDWR), by the raw faccessat2 system call with R_OK|W_OK and
// AT_EACCESS: judged by the effective uid and gid and the supplementary
// groups, and by the node's ACL, as an open is, and opening nothing, so no
// stream the player or another process holds is disturbed. Never through
// unix.Faccessat, which on ENOSYS or EPERM falls back to the mode bits and
// so ignores an ACL: here ENOSYS (a kernel before 5.8) and EPERM (a seccomp
// filter, say) come back as they are, for the line to call the check
// inconclusive.
func accessForPlayback(path string) error {
	if err := unix.Faccessat2(unix.AT_FDCWD, path, unix.R_OK|unix.W_OK, unix.AT_EACCESS); err != nil {
		return &fs.PathError{Op: "faccessat2", Path: path, Err: err}
	}
	return nil
}
