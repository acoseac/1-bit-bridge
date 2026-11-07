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
