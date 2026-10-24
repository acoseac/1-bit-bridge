//go:build !linux && !darwin

package openhome

import "syscall"

// restrictToInterface leaves the socket unrestricted: this platform offers
// no option an unprivileged process may set that restricts a listening
// socket to an interface (Windows' IP_UNICAST_IF scopes what a socket
// sends, and the player runs on Linux alone). Admission's prefix check holds
// all the same.
func restrictToInterface(syscall.RawConn, int) string {
	return "none: this platform has no unprivileged option that restricts a socket to its interface"
}
