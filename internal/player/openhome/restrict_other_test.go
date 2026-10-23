//go:build !linux && !darwin

package openhome

import (
	"net"
	"testing"
)

// boundInterface has nothing to read back on a platform with no option.
func boundInterface(*testing.T, net.Listener) (int, bool) { return 0, false }
