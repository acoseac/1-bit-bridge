package packaging

import (
	"fmt"
	"net"
	"time"
)

// IsListening returns true if something accepts TCP on host:port within
// a short dial timeout. Used before `SpawnDetached` so a re-run of
// `bridge init` while an SCM-installed service (or a previously-spawned
// launcher) is already bound doesn't produce a port-bind error that
// ends up in the log as a permanent-looking failure.
func IsListening(host string, port int) bool {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// WaitForListen polls host:port until something accepts, or the timeout
// expires. Used after `SpawnDetached` so `openInBrowser` fires only
// after the admin server is ready to answer. Returns true if a
// connection landed within the budget.
func WaitForListen(host string, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if IsListening(host, port) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
