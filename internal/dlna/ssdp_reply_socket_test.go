//go:build darwin || windows

package dlna

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
)

// TestStrictReplyWithoutSocketControlFailsClosed exercises the platform's
// actual pin function: a socket without control cannot become a routed reply.
func TestStrictReplyWithoutSocketControlFailsClosed(t *testing.T) {
	c := &round2Unicast{rawErr: errInjected}
	host := hostSSDPTransport{openReply: func(*net.UDPAddr) (ssdpUnicastSocket, error) { return c, nil }}
	from := SSDPEndpointSnapshot{Interface: SSDPInterface{Index: 2}, Local: netip.MustParsePrefix("192.168.1.20/24")}
	r, err := host.dialReply(netip.MustParseAddrPort(searchSrc), &from)
	if r != nil || !errors.Is(err, ErrSSDPStrictCapability) || !errors.Is(err, errInjected) {
		t.Fatalf("pin refusal: reply=%v err=%v", r, err)
	}
	if !c.closed || c.writes != 0 {
		t.Error("pin refusal leaked a socket or a packet")
	}
}

type round2RawConn struct{ err error }

func (c round2RawConn) Control(f func(uintptr)) error {
	if c.err != nil {
		return c.err
	}
	f(42)
	return nil
}
func (round2RawConn) Read(func(uintptr) bool) error  { return nil }
func (round2RawConn) Write(func(uintptr) bool) error { return nil }

type round2ControlledSocket struct {
	round2Unicast
	raw round2RawConn
}

func (c *round2ControlledSocket) SyscallConn() (syscall.RawConn, error) { return c.raw, nil }

// TestStrictReplyPropagatesControlAndOptionFailures distinguishes a failed
// RawConn.Control from a setsockopt failure inside a successful Control.
func TestStrictReplyPropagatesControlAndOptionFailures(t *testing.T) {
	for _, controlFails := range []bool{false, true} {
		c := &round2ControlledSocket{}
		if controlFails {
			c.raw.err = errInjected
		}
		called := false
		err := setStrictReplyInterface(c, 7, func(fd uintptr, index int) error {
			called = true
			if fd != 42 || index != 7 {
				t.Error("wrong socket or interface")
			}
			return errInjected
		})
		if !errors.Is(err, errInjected) || called == controlFails {
			t.Errorf("controlFails=%v called=%v err=%v", controlFails, called, err)
		}
	}
}
