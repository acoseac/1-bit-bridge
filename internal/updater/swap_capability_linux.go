//go:build linux

package updater

import (
	"errors"

	"golang.org/x/sys/unix"
)

// capabilityAttr is the extended attribute that holds a file's capabilities
// (setcap's), which the kernel grants a process that execs the file: a
// binary given cap_net_bind_service binds a port below 1024 as its service
// user.
const capabilityAttr = "security.capability"

// capabilityXattr reads and sets a file's capabilities. A test stands in for
// both, since only a process with CAP_SETFCAP may set them.
var capabilityXattr = struct {
	get func(path string) ([]byte, error)
	set func(path string, value []byte) error
}{get: readCapability, set: writeCapability}

// FileCapabilities is the file capabilities path carries, nil where it
// carries none (backlog B660's second round): serve compares its binary's
// with the backup's when a bind is refused for a permission, since a backup
// that carries them binds where the binary may not.
func FileCapabilities(path string) ([]byte, error) { return readCapability(path) }

// carryCapabilities gives staged, the new binary beside dst, the file
// capabilities dst carries, before the swap renames it into place: an update
// must not take away the operator's setcap (cap_net_bind_service, for a port
// below 1024). Only a process with CAP_SETFCAP may set them, so the service
// user's auto-installer cannot, nor can a filesystem without extended
// attributes keep them: the swap goes on without them, and serve's bind check
// (cmd/bridge's bindRefusalIsTheHosts) counts the start a refused bind ends,
// since the backup, which keeps them, would bind. A read of dst that fails
// sets nothing. The rest of what the swap does with dst's attributes is
// backlog B760's.
func carryCapabilities(dst, staged string) {
	caps, err := capabilityXattr.get(dst)
	if err != nil || len(caps) == 0 {
		return
	}
	_ = capabilityXattr.set(staged, caps)
}

// readCapability is path's capabilities, nil where it carries none: a
// filesystem that keeps no extended attributes carries none either.
func readCapability(path string) ([]byte, error) {
	buf := make([]byte, 64)
	for {
		n, err := unix.Getxattr(path, capabilityAttr, buf)
		switch {
		case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
			return nil, nil
		case errors.Is(err, unix.ERANGE):
			buf = make([]byte, 2*len(buf))
			continue
		case err != nil:
			return nil, err
		}
		return buf[:n], nil
	}
}

// writeCapability gives path the capabilities value.
func writeCapability(path string, value []byte) error {
	return unix.Setxattr(path, capabilityAttr, value, 0)
}
